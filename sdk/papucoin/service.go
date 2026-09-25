package papucoin

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strings"

	"github.com/eigerco/strawberry/sdk"
)

// Storage key domains. A one byte domain keeps keys compact while staying
// unambiguous, since addresses are variable length and concatenated raw.
const (
	keySupply    byte = 0x01
	keyBalance   byte = 0x02
	keyNonce     byte = 0x03
	keyEVMNonce  byte = 0x04
	keyFaucet    byte = 0x05
	keyWelcome   byte = 0x06
	keyIssuer    byte = 0x07
	keyAssetName byte = 0x08
)

// Methods accepted by the service.
const (
	MethodTransfer = "transfer"
	MethodMint     = "mint"
	MethodBurn     = "burn"
	MethodFaucet   = "faucet"
	MethodWelcome  = "welcome"
)

// Item is the wire format of a work item. The EVM gateway builds these, and the
// same shape is what a future packed PVM codec would replace.
type Item struct {
	Method string `json:"method"`
	Sender string `json:"sender"`
	Nonce  uint64 `json:"nonce"`
	To     string `json:"to,omitempty"`
	Amount string `json:"amount,omitempty"`
	Memo   string `json:"memo,omitempty"`
	// Raw is a relayed EVM transaction, hex encoded. It is only meaningful
	// for a transfer, and only when a RelayVerifier is configured.
	Raw string `json:"raw,omitempty"`
}

// Op is the refined result of an item. Every core in the assigned set computes
// this independently and their agreement is what allows accumulation, so it must
// be a pure function of the item.
type Op struct {
	Op string `json:"op"`
	// Actor is the address whose item nonce this operation consumes. For a
	// relayed transfer that is the relaying wallet, which is what the
	// original chain sequenced on.
	Actor string `json:"actor"`
	// Sender is the account that is debited. For a relayed transfer this is
	// the signer recovered from the transaction, and may differ from Actor.
	Sender   string  `json:"sender"`
	Nonce    uint64  `json:"nonce"`
	To       string  `json:"to,omitempty"`
	Amount   string  `json:"amount,omitempty"`
	Fee      string  `json:"fee,omitempty"`
	Memo     string  `json:"memo,omitempty"`
	EVMNonce *uint64 `json:"evmNonce,omitempty"`
}

// EVMFields are the parts of a relayed EVM transaction the service needs.
type EVMFields struct {
	From  string
	To    string
	Nonce uint64
	// Value is in wei.
	Value *big.Int
}

// RelayVerifier recovers the signer of a relayed EVM transaction. Without one,
// refine rejects relayed transfers rather than trusting the gateway that built
// the item, because a forged `from` on a value bearing item is a mint.
type RelayVerifier interface {
	Recover(rawHex string) (EVMFields, error)
}

var (
	// ErrRelayUnconfigured is returned when a relayed transfer arrives with
	// no verifier configured.
	ErrRelayUnconfigured = errors.New("papucoin: relayed transfers need a relay verifier")
	// ErrRelayMismatch is returned when an item disagrees with the
	// transaction it claims to relay.
	ErrRelayMismatch = errors.New("papucoin: item does not match relayed transaction")
	// ErrBadMethod is returned for an unknown method.
	ErrBadMethod = errors.New("papucoin: unknown method")
	// ErrNoConfig is returned when the service has not been initialised.
	ErrNoConfig = errors.New("papucoin: service not initialised")
	// ErrEmptyValue is returned when a relayed transaction transfers nothing.
	ErrEmptyValue = errors.New("papucoin: amount must be greater than zero")
)

// weiGranularity is the value a wei amount must divide into for the relay
// path. One PAPU is 1e12 raw units and 1e18 wei, so a whole micro-PAPU is 1e6
// wei. The original chain only accepted transfers at this granularity.
var weiGranularity = big.NewInt(1_000_000)

// WeiFromRaw converts an amount in raw units into the wei a relayed Ethereum
// transaction would carry for the same value.
func WeiFromRaw(raw *big.Int) *big.Int {
	return new(big.Int).Mul(raw, weiGranularity)
}

// New returns the PAPU service. issuer is the only address allowed to mint.
// initialBalances seeds the opening state and is applied by the init handler.
// relay may be nil, in which case relayed transfers are refused.
func New(params Params, issuer string, initialBalances map[string]string, relay RelayVerifier) svc.Service {
	canonicalIssuer, err := NormalizeAddress(issuer)
	if err != nil {
		panic(fmt.Sprintf("papucoin: bad issuer %q: %v", issuer, err))
	}

	return svc.Service{
		Name: params.Symbol,
		Init: func(ctx svc.AccumulateContext) error {
			return seed(ctx, params, canonicalIssuer, initialBalances)
		},
		Refine: func(ctx svc.RefineContext, payload []byte) ([]byte, error) {
			return refineItem(ctx, payload, params, canonicalIssuer, relay)
		},
		Accumulate: func(ctx svc.AccumulateContext, items []svc.RefinedItem) ([]byte, error) {
			return accumulateItems(ctx, items, params)
		},
	}
}

// seed writes the opening configuration and balances. It must run before the
// service handles any work item.
func seed(ctx svc.AccumulateContext, params Params, issuer string, initialBalances map[string]string) error {
	if err := ctx.Write([]byte{keyIssuer}, []byte(issuer)); err != nil {
		return err
	}
	if err := ctx.Write([]byte{keyAssetName}, []byte(params.Symbol)); err != nil {
		return err
	}

	total := new(big.Int)
	type opening struct {
		address string
		amount  *big.Int
	}
	seeds := make([]opening, 0, len(initialBalances))
	for address, amount := range initialBalances {
		canonical, err := NormalizeAddress(address)
		if err != nil {
			return fmt.Errorf("genesis: %w", err)
		}
		raw, err := ParseAmount(amount, params.Decimals)
		if err != nil {
			return fmt.Errorf("genesis: %s: %w", address, err)
		}
		total.Add(total, raw)
		seeds = append(seeds, opening{canonical, raw})
	}

	if total.Cmp(params.MaxSupply) > 0 {
		return fmt.Errorf("genesis: initial balances %s exceed max supply %s",
			FormatRaw(total, params.Decimals), FormatRaw(params.MaxSupply, params.Decimals))
	}

	// Seed in a stable order so the storage footprint does not depend on Go's
	// map iteration order.
	slices.SortFunc(seeds, func(a, b opening) int { return strings.Compare(a.address, b.address) })
	for _, s := range seeds {
		if err := writeAmount(ctx, balanceKey(s.address), s.amount); err != nil {
			return err
		}
	}

	return writeAmount(ctx, []byte{keySupply}, total)
}

func refineItem(ctx svc.RefineContext, payload []byte, params Params, issuer string, relay RelayVerifier) ([]byte, error) {
	var item Item
	if err := json.Unmarshal(payload, &item); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadMethod, err)
	}

	sender, err := NormalizeAddress(item.Sender)
	if err != nil {
		return nil, err
	}

	op := Op{Op: item.Method, Actor: sender, Sender: sender, Nonce: item.Nonce, Memo: item.Memo}

	switch item.Method {
	case MethodTransfer:
		if item.Raw != "" {
			if relay == nil {
				return nil, ErrRelayUnconfigured
			}
			fields, err := relay.Recover(item.Raw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrRelayMismatch, err)
			}

			from, err := NormalizeAddress(fields.From)
			if err != nil {
				return nil, err
			}
			to, err := NormalizeAddress(fields.To)
			if err != nil {
				return nil, err
			}
			if to != strings.ToLower(item.To) {
				return nil, fmt.Errorf("%w: destination disagrees with the signed transaction", ErrRelayMismatch)
			}
			if fields.Value.Sign() <= 0 {
				return nil, ErrEmptyValue
			}
			if new(big.Int).Mod(fields.Value, weiGranularity).Sign() != 0 {
				return nil, fmt.Errorf("%w: value is not a whole number of micro-PAPU", ErrRelayMismatch)
			}

			amount := new(big.Int).Quo(fields.Value, weiGranularity)
			evmNonce := fields.Nonce
			op.Op, op.Sender, op.To, op.Fee = MethodTransfer, from, to, params.TransferFee.String()
			op.Amount, op.EVMNonce = amount.String(), &evmNonce
			break
		}

		to, err := NormalizeAddress(item.To)
		if err != nil {
			return nil, err
		}
		amount, err := positiveAmount(item.Amount, params.Decimals)
		if err != nil {
			return nil, err
		}
		op.To, op.Amount, op.Fee = to, amount.String(), params.TransferFee.String()

	case MethodMint:
		to, err := NormalizeAddress(item.To)
		if err != nil {
			return nil, err
		}
		amount, err := positiveAmount(item.Amount, params.Decimals)
		if err != nil {
			return nil, err
		}
		op.To, op.Amount = to, amount.String()

	case MethodBurn:
		amount, err := positiveAmount(item.Amount, params.Decimals)
		if err != nil {
			return nil, err
		}
		op.Amount = amount.String()

	case MethodFaucet, MethodWelcome:
		to := sender
		if item.To != "" {
			if to, err = NormalizeAddress(item.To); err != nil {
				return nil, err
			}
		}
		op.To = to

	default:
		return nil, fmt.Errorf("%w: %q", ErrBadMethod, item.Method)
	}

	ctx.Log(svc.LogDebug, "papucoin refined "+op.Op)
	report, err := json.Marshal(op)
	if err != nil {
		return nil, err
	}
	return report, ctx.Emit(report)
}

func accumulateItems(ctx svc.AccumulateContext, items []svc.RefinedItem, params Params) ([]byte, error) {
	applied := 0
	for _, refined := range items {
		if len(refined.Report) == 0 {
			continue
		}

		var op Op
		if err := json.Unmarshal(refined.Report, &op); err != nil {
			return nil, err
		}

		ok, err := applyOp(ctx, op, params)
		if err != nil {
			return nil, err
		}
		if ok {
			applied++
		}
	}

	ctx.Log(svc.LogDebug, fmt.Sprintf("papucoin applied %d of %d ops", applied, len(items)))
	return nil, nil
}

// applyOp applies one refined operation. It reports whether the operation took
// effect; an operation that fails a check is skipped rather than aborting the
// block, matching the original chain.
func applyOp(ctx svc.AccumulateContext, op Op, params Params) (bool, error) {
	expected, err := readNonce(ctx, op.Actor, params.FirstNonce)
	if err != nil {
		return false, err
	}
	if op.Nonce != expected {
		return false, nil
	}

	advance := func() error {
		encoded, err := encodeAmount(new(big.Int).SetUint64(op.Nonce + 1))
		if err != nil {
			return err
		}
		return ctx.Write(nonceKey(op.Actor), encoded)
	}

	switch op.Op {
	case MethodTransfer:
		amount, err := amountFromString(op.Amount)
		if err != nil {
			return false, err
		}
		fee, err := amountFromString(op.Fee)
		if err != nil {
			return false, err
		}

		if op.EVMNonce != nil {
			current, err := readEVMNonce(ctx, op.Sender)
			if err != nil {
				return false, err
			}
			if *op.EVMNonce != current {
				return false, nil
			}
		}

		from, err := balanceOf(ctx, op.Sender)
		if err != nil {
			return false, err
		}
		total := new(big.Int).Add(amount, fee)
		if from.Cmp(total) < 0 {
			return false, nil
		}

		to, err := balanceOf(ctx, op.To)
		if err != nil {
			return false, err
		}
		if err := writeAmount(ctx, balanceKey(op.Sender), new(big.Int).Sub(from, total)); err != nil {
			return false, err
		}
		if err := writeAmount(ctx, balanceKey(op.To), new(big.Int).Add(to, amount)); err != nil {
			return false, err
		}

		// The fee is burned, so total supply falls by the fee.
		if fee.Sign() > 0 {
			supply, err := supplyOf(ctx)
			if err != nil {
				return false, err
			}
			next := new(big.Int).Sub(supply, fee)
			if next.Sign() < 0 {
				next = new(big.Int)
			}
			if err := writeAmount(ctx, []byte{keySupply}, next); err != nil {
				return false, err
			}
		}

		if op.EVMNonce != nil {
			if err := writeAmount(ctx, evmNonceKey(op.Sender), new(big.Int).SetUint64(*op.EVMNonce+1)); err != nil {
				return false, err
			}
		}
		return true, advance()

	case MethodMint:
		issuer, err := issuerOf(ctx)
		if err != nil {
			return false, err
		}
		if op.Actor != issuer {
			return false, nil
		}
		amount, err := amountFromString(op.Amount)
		if err != nil {
			return false, err
		}
		supply, err := supplyOf(ctx)
		if err != nil {
			return false, err
		}
		next := new(big.Int).Add(supply, amount)
		if next.Cmp(params.MaxSupply) > 0 {
			return false, nil
		}
		if err := credit(ctx, op.To, amount); err != nil {
			return false, err
		}
		if err := writeAmount(ctx, []byte{keySupply}, next); err != nil {
			return false, err
		}
		return true, advance()

	case MethodBurn:
		amount, err := amountFromString(op.Amount)
		if err != nil {
			return false, err
		}
		from, err := balanceOf(ctx, op.Sender)
		if err != nil {
			return false, err
		}
		if from.Cmp(amount) < 0 {
			return false, nil
		}
		if err := writeAmount(ctx, balanceKey(op.Sender), new(big.Int).Sub(from, amount)); err != nil {
			return false, err
		}
		supply, err := supplyOf(ctx)
		if err != nil {
			return false, err
		}
		next := new(big.Int).Sub(supply, amount)
		if next.Sign() < 0 {
			next = new(big.Int)
		}
		if err := writeAmount(ctx, []byte{keySupply}, next); err != nil {
			return false, err
		}
		return true, advance()

	case MethodFaucet, MethodWelcome:
		claimKey := []byte{keyFaucet}
		amount := params.FaucetAmount
		if op.Op == MethodWelcome {
			claimKey = []byte{keyWelcome}
			amount = params.WelcomeAmount
		}

		claimed, _, err := ctx.Read(append(claimKey, op.To...))
		if err != nil {
			return false, err
		}
		if len(claimed) > 0 {
			return false, nil
		}

		supply, err := supplyOf(ctx)
		if err != nil {
			return false, err
		}
		next := new(big.Int).Add(supply, amount)
		if next.Cmp(params.MaxSupply) > 0 {
			return false, nil
		}
		if err := credit(ctx, op.To, amount); err != nil {
			return false, err
		}
		if err := writeAmount(ctx, []byte{keySupply}, next); err != nil {
			return false, err
		}
		if err := ctx.Write(append(claimKey, op.To...), []byte{1}); err != nil {
			return false, err
		}
		return true, advance()

	default:
		return false, nil
	}
}

func credit(ctx svc.AccumulateContext, address string, amount *big.Int) error {
	balance, err := balanceOf(ctx, address)
	if err != nil {
		return err
	}
	return writeAmount(ctx, balanceKey(address), new(big.Int).Add(balance, amount))
}

func balanceOf(ctx svc.AccumulateContext, address string) (*big.Int, error) {
	amount, _, err := readAmount(ctx, balanceKey(address))
	return amount, err
}

func supplyOf(ctx svc.AccumulateContext) (*big.Int, error) {
	amount, _, err := readAmount(ctx, []byte{keySupply})
	return amount, err
}

func readNonce(ctx svc.AccumulateContext, address string, first uint64) (uint64, error) {
	amount, found, err := readAmount(ctx, nonceKey(address))
	if err != nil || !found {
		return first, err
	}
	return amount.Uint64(), nil
}

func readEVMNonce(ctx svc.AccumulateContext, address string) (uint64, error) {
	amount, found, err := readAmount(ctx, evmNonceKey(address))
	if err != nil || !found {
		return 0, err
	}
	return amount.Uint64(), nil
}

func issuerOf(ctx svc.AccumulateContext) (string, error) {
	stored, _, err := ctx.Read([]byte{keyIssuer})
	if err != nil {
		return "", err
	}
	if len(stored) == 0 {
		return "", ErrNoConfig
	}
	return string(stored), nil
}

func readAmount(ctx svc.AccumulateContext, key []byte) (*big.Int, bool, error) {
	stored, found, err := ctx.Read(key)
	if err != nil || !found {
		return new(big.Int), false, err
	}
	amount, err := decodeAmount(stored)
	return amount, true, err
}

func writeAmount(ctx svc.AccumulateContext, key []byte, amount *big.Int) error {
	encoded, err := encodeAmount(amount)
	if err != nil {
		return err
	}
	return ctx.Write(key, encoded)
}

func balanceKey(address string) []byte { return append([]byte{keyBalance}, address...) }
func nonceKey(address string) []byte   { return append([]byte{keyNonce}, address...) }
func evmNonceKey(address string) []byte {
	return append([]byte{keyEVMNonce}, address...)
}

func amountFromString(s string) (*big.Int, error) {
	value, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, s)
	}
	return value, nil
}

func positiveAmount(input string, decimals uint8) (*big.Int, error) {
	amount, err := ParseAmount(input, decimals)
	if err != nil {
		return nil, err
	}
	if amount.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount must be greater than zero", ErrInvalidAmount)
	}
	return amount, nil
}
