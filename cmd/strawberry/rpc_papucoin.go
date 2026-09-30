package main

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// papucoinHandlers are the PAPU methods, which read the state the runtime owns.
// Every answer comes from the service storage itself, so a client sees what the
// chain committed to rather than a number the node made up.
type papucoinHandlers struct {
	runtime *devnet.Runtime
	// chain is the block chain the runtime is producing, which is where the header
	// of the newest block comes from.
	chain *rpcServer
}

func newPapucoinHandlers(runtime *devnet.Runtime, chain *rpcServer) *papucoinHandlers {
	return &papucoinHandlers{runtime: runtime, chain: chain}
}

// paramsOf reads a positional parameter list.
func paramsOf(raw json.RawMessage) ([]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var params []json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		return nil, fmt.Errorf("params must be a list: %w", err)
	}
	return params, nil
}

// stringParam reads one string parameter.
// optionalString reads a parameter a caller may leave out, which is what makes
// eth_getBlockByNumber("latest") and eth_getBlockByNumber() the same question.
func optionalString(params []json.RawMessage, index int) string {
	if index >= len(params) {
		return ""
	}
	var value string
	if err := json.Unmarshal(params[index], &value); err != nil {
		return ""
	}
	return value
}

func stringParam(params []json.RawMessage, index int, name string) (string, error) {
	if index >= len(params) {
		return "", fmt.Errorf("%s is missing", name)
	}
	var value string
	if err := json.Unmarshal(params[index], &value); err != nil {
		return "", fmt.Errorf("%s must be a string: %w", name, err)
	}
	return value, nil
}

// chainParams describes the chain the way a wallet needs to see it.
func (s *papucoinHandlers) chainParams() (map[string]interface{}, error) {
	genesis := s.runtime.Genesis()
	view := s.runtime.View()
	issuer, err := view.Issuer()
	if err != nil {
		return nil, err
	}
	queued, slots := s.runtime.Pending()

	return map[string]interface{}{
		"network":       genesis.Network,
		"serviceId":     s.runtime.PapucoinID(),
		"serviceName":   genesis.Service.Name,
		"symbol":        genesis.Service.Symbol,
		"decimals":      genesis.Service.Decimals,
		"issuer":        issuer,
		"bridgeAddress": s.runtime.BridgeAddress(),
		"evm": map[string]interface{}{
			"chainId":  genesis.EVM.ChainID,
			"name":     genesis.EVM.Name,
			"symbol":   genesis.EVM.Symbol,
			"decimals": genesis.EVM.Decimals,
		},
		"queue": map[string]interface{}{
			"pending": queued,
			"slots":   slots,
		},
	}, nil
}

// balance reads one account.
func (s *papucoinHandlers) balance(params []json.RawMessage) (map[string]interface{}, error) {
	address, err := stringParam(params, 0, "address")
	if err != nil {
		return nil, err
	}
	view := s.runtime.View()
	raw, err := view.Balance(address)
	if err != nil {
		return nil, err
	}
	nonce, err := view.Nonce(address)
	if err != nil {
		return nil, err
	}
	decimals := s.runtime.Genesis().Service.Decimals
	return map[string]interface{}{
		"address":  address,
		"balance":  papucoin.FormatRaw(raw, decimals),
		"raw":      raw.String(),
		"decimals": decimals,
		"nonce":    nonce,
	}, nil
}

// supply is the amount of PAPU in existence.
func (s *papucoinHandlers) supply() (map[string]interface{}, error) {
	view := s.runtime.View()
	raw, err := view.Supply()
	if err != nil {
		return nil, err
	}
	items, octets := s.runtime.View().StorageFootprint()
	decimals := s.runtime.Genesis().Service.Decimals
	return map[string]interface{}{
		"supply":       papucoin.FormatRaw(raw, decimals),
		"raw":          raw.String(),
		"decimals":     decimals,
		"maxSupply":    papucoin.FormatRaw(s.runtime.Params().MaxSupply, decimals),
		"maxSupplyRaw": s.runtime.Params().MaxSupply.String(),
		// The price the next block charges, which moves with load. Reporting the
		// genesis figure here would tell a client a number the chain is not
		// charging.
		"transferFee": papucoin.FormatRaw(
			papucoin.CurrentFee(context.Background(), s.runtime.View(), s.runtime.Params()), decimals),
		"firstNonce":    s.runtime.Params().FirstNonce,
		"stateRoot":     hashToHex(crypto.Hash(s.runtime.Root())),
		"storageItems":  items,
		"storageOctets": octets,
	}, nil
}

// submit queues an item. The item has to arrive signed, and the node checks the
// signature before it occupies a slot in a timeslot.
func (s *papucoinHandlers) submit(params []json.RawMessage) (map[string]interface{}, error) {
	raw, err := stringParam(params, 0, "item")
	if err != nil {
		return nil, err
	}
	encoded, err := hexOrBase64(raw)
	if err != nil {
		return nil, err
	}

	var item papucoin.Item
	if err := json.Unmarshal(encoded, &item); err != nil {
		return nil, fmt.Errorf("item is not a PAPU item: %w", err)
	}
	if err := s.runtime.Submit(item); err != nil {
		return nil, err
	}

	queued, slots := s.runtime.Pending()
	return map[string]interface{}{
		"accepted": true,
		"sender":   item.Sender,
		"nonce":    item.Nonce,
		"method":   item.Method,
		"pending":  queued,
		"slots":    slots,
	}, nil
}

// sendRawTransaction queues a transfer that arrives as a raw Ethereum
// transaction, which is the shape a wallet that only speaks Ethereum has.
//
// The transaction proves itself: the service recovers the sender from its
// signature and takes the destination and the amount from the signed bytes, so
// nothing this node is told about the transfer is believed.
//
// The answer is the hash of the transaction and nothing else, because that is
// what Ethereum promises this method: a client that got an object here would
// read the hash out of a field that is not there. What the chain did with the
// transaction is asked for afterwards, with papucoin_balance, or read off the
// blocks, where the extrinsic that carried it is committed.
func (s *papucoinHandlers) sendRawTransaction(params []json.RawMessage) (string, error) {
	raw, err := stringParam(params, 0, "transaction")
	if err != nil {
		return "", err
	}
	if _, err := s.runtime.SubmitRelayed(raw); err != nil {
		return "", err
	}
	return transactionHash(raw), nil
}

// evmEstimateGas answers a wallet that has to put a gas limit in a transfer
// before it signs one.
//
// This chain does not meter gas: it charges the fee the genesis names, once per
// transfer, and a gas price of zero means a gas limit of any size costs the same
// nothing. The number below is therefore the intrinsic cost Ethereum charges a
// transfer, which is what a client needs in order to build the transaction, and
// nothing is being claimed about what the chain would do with the difference.
func (s *papucoinHandlers) evmEstimateGas(params []json.RawMessage) (string, error) {
	if len(params) == 0 {
		return "", errors.New("a transaction is what to estimate")
	}
	var call struct {
		To    *string `json:"to"`
		Data  *string `json:"data"`
		Input *string `json:"input"`
	}
	if err := json.Unmarshal(params[0], &call); err != nil {
		return "", err
	}

	// A transaction that creates a contract, or that carries data to one, is not
	// a transfer, and this chain has no honest number for it.
	data := call.Data
	if data == nil {
		data = call.Input
	}
	if call.To == nil || *call.To == "" {
		return "", errors.New("this chain does not deploy contracts, so there is no gas to estimate for one")
	}
	if data != nil && *data != "" && *data != "0x" {
		return "", errors.New("this chain does not run contract code, so a transaction with data has no gas to estimate")
	}

	return "0x5208", nil // 21000, the intrinsic cost of a transfer.
}

// transactionHash is the hash Ethereum gives a transaction, so a wallet that
// asked for one can look it up in the same place it would look up any other.
func transactionHash(rawTx string) string {
	raw, err := hex.DecodeString(strings.TrimPrefix(rawTx, "0x"))
	if err != nil {
		return ""
	}
	return fmt.Sprintf("0x%x", crypto.KeccakData(raw))
}

// faucet queues a claim on the chain's faucet, paid out of the node's own
// account. The amount is the one the genesis names, and only the first claim of an
// address is paid, so the answer says what the chain would pay rather than what
// was asked for.
func (s *papucoinHandlers) faucet(params []json.RawMessage) (map[string]interface{}, error) {
	address, err := stringParam(params, 0, "address")
	if err != nil {
		return nil, err
	}

	item, err := s.runtime.Faucet(address)
	if err != nil {
		return nil, err
	}
	queued, _ := s.runtime.Pending()
	return map[string]interface{}{
		"accepted": true,
		"item":     item,
		"amount":   papucoin.FormatRaw(s.runtime.Params().FaucetAmount, s.runtime.Params().Decimals),
		"once":     true,
		"pending":  queued,
	}, nil
}

// header is the newest block of the chain, as the chain has it, together with the
// state the node has arrived at.
//
// The Substrate shaped headers the chain_* methods answer are for the clients that
// were written before this chain existed. A header of a JAM block is read by naming
// the timeslot it sits at and the state it was built on, so that is what this
// returns, and the state the node is at besides, because the root the next block
// will name is that one and a client that can see both can tell whether they agree.
func (s *papucoinHandlers) header() (map[string]interface{}, error) {
	tip, found := s.chain.loadTip()
	if !found {
		return nil, fmt.Errorf("the chain has not produced a block yet")
	}
	header, err := s.chain.chainStore.GetHeader(tip.hash)
	if err != nil {
		return nil, err
	}
	// The tip is already read, so the body comes back with it: how much work the
	// block named is worth knowing, because a block that names none is a block a
	// restart cannot rebuild the state from.
	work := 0
	if b, err := s.chain.chainStore.GetBlock(tip.hash); err == nil {
		work = len(b.Extrinsic.EP)
	}
	return map[string]interface{}{
		"hash":               hashToHex(tip.hash),
		"parentHash":         hashToHex(header.ParentHash),
		"stateRoot":          hashToHex(header.PriorStateRoot),
		"resultingStateRoot": hashToHex(crypto.Hash(s.runtime.Root())),
		"extrinsicHash":      hashToHex(header.ExtrinsicHash),
		"extrinsics":         work,
		"timeSlotIndex":      uint32(header.TimeSlotIndex),
		"number":             tip.number,
		"blockAuthorIndex":   header.BlockAuthorIndex,
		"epoch":              uint64(header.TimeSlotIndex.ToEpoch()),
	}, nil
}

// The eth_ methods are what a wallet that only speaks Ethereum asks for. Each one
// answers from the state the runtime owns and the chain the node has produced, and
// says so in the units Ethereum uses. A method this chain cannot answer truthfully
// is not answered at all, and says why, because a wallet shown a number nobody
// computed is worse off than one shown an error.

// evmChainID is the chain id the genesis names, in the hex form Ethereum uses.
func (s *papucoinHandlers) evmChainID() (string, error) {
	return fmt.Sprintf("0x%x", s.runtime.Genesis().EVM.ChainID), nil
}

// evmBlockNumber is the number of the newest block the node produced.
//
// The number is read out of the same lookup the other block answers use, so a
// wallet is told a block that exists rather than the first number a producer
// happened to have counted by the time a new block was already in the store.
func (s *papucoinHandlers) evmBlockNumber() (string, error) {
	tip, found := s.chain.loadTip()
	if !found {
		return "0x0", nil
	}
	return fmt.Sprintf("0x%x", tip.number), nil
}

// evmGetBalance answers in wei, because that is what a wallet reads, and it
// refuses an address this chain cannot know about rather than answering zero,
// which is what an unknown address and an empty one would otherwise look like.
func (s *papucoinHandlers) evmGetBalance(params []json.RawMessage) (string, error) {
	address, err := stringParam(params, 0, "address")
	if err != nil {
		return "", err
	}
	raw, err := s.runtime.View().Balance(address)
	if err != nil {
		return "", err
	}
	return weiOf(raw, s.runtime.WeiPerRaw()), nil
}

// evmGetTransactionCount is the number of Ethereum transactions this chain has
// already accepted from an address, which is the number the next one has to
// carry. It is the chain's own record of that address's EVM nonce rather than
// its PAPU nonce: a wallet signs the EVM sequence, and answering with the other
// one would have it sign a number the service never asked for.
func (s *papucoinHandlers) evmGetTransactionCount(params []json.RawMessage) (string, error) {
	address, err := stringParam(params, 0, "address")
	if err != nil {
		return "", err
	}
	nonce, err := s.runtime.View().EVMNonce(address)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("0x%x", nonce), nil
}

// evmGasPrice quotes the price the next block charges. It used to be zero on the
// grounds that the fee is part of the transfer rather than a price per unit of
// gas, and that is still true of the shape. But a wallet that shows a price of
// zero shows a price, and the one the chain will actually charge is not zero, so
// the figure reported was not a zero price, it was a wrong one.
func (s *papucoinHandlers) evmGasPrice() (string, error) {
	return currentFeeHex(s), nil
}

// evmGetBlockByNumber describes a block of this chain in the fields Ethereum
// clients read, so a wallet can watch the chain advance without a second
// implementation of it.
//
// A block is asked for by the number this node counts, the same number
// eth_blockNumber answers, or by one of the three tags a client uses for "the
// one I can see", "the one I can expect" and "the first there was". The blocks
// before the tip are found by walking the chain's own parent links, which is how
// far back this node can go; past that it says so rather than answering with the
// tip and letting the caller believe it is reading history.
func (s *papucoinHandlers) evmGetBlockByNumber(params []json.RawMessage) (map[string]interface{}, error) {
	want := optionalString(params, 0)

	tip, found := s.chain.loadTip()
	if !found {
		return nil, nil
	}

	answered, err := s.blockByNumber(want, tip)
	if err != nil {
		return nil, err
	}
	if answered == nil {
		return nil, nil
	}
	header := answered.header
	pending, _ := s.runtime.Pending()

	return map[string]interface{}{
		"number":        fmt.Sprintf("0x%x", answered.number),
		"hash":          hashToHex(answered.hash),
		"parentHash":    hashToHex(header.ParentHash),
		"timestamp":     fmt.Sprintf("0x%x", uint64(header.TimeSlotIndex.TimeslotStart().ToTime().Unix())),
		"stateRoot":     hashToHex(header.PriorStateRoot),
		"gasUsed":       "0x0",
		"gasLimit":      "0x0",
		"baseFeePerGas": currentFeeHex(s),
		"extraData":     "0x",
		"transactions":  []interface{}{},
		// The timeslot is this chain's clock, and a client that has to guess where
		// it is cannot tell a stalled chain from a slow one.
		"timeSlotIndex":       uint32(header.TimeSlotIndex),
		"transactionsPending": pending,
	}, nil
}

// answeredBlock is a block a client asked for: its number, its hash and its
// header, which all have to describe the same block.
type answeredBlock struct {
	number uint
	hash   crypto.Hash
	header block.Header
}

// evmBlockHistory is how far back this method walks the chain. Going further
// than this is a query this node answers with a limit rather than with a guess.
const evmBlockHistory = 4096

// blockByNumber finds the block a client asked for. A nil block with no error
// means the chain has no block for it yet, which is what a client reads as null.
func (s *papucoinHandlers) blockByNumber(want string, newest tip) (*answeredBlock, error) {
	switch want {
	case "", "latest", "pending":
		header, err := s.chain.chainStore.GetHeader(newest.hash)
		if err != nil {
			return nil, err
		}
		return &answeredBlock{number: newest.number, hash: newest.hash, header: header}, nil
	case "earliest":
		genesis, hash, found := s.chain.bs.GenesisHeader()
		if !found {
			return nil, nil
		}
		return &answeredBlock{number: 0, hash: hash, header: genesis}, nil
	}

	number, err := parseBlockNumber(want)
	if err != nil {
		return nil, err
	}
	if number > newest.number {
		return nil, nil // A block this node has not produced yet.
	}
	if newest.number-number > evmBlockHistory {
		return nil, fmt.Errorf("this method reaches the last %d blocks, and block %d is further back than that",
			evmBlockHistory, number)
	}

	// The chain stores a block under its own hash and keeps no index by number, so
	// an older block is reached by following parent links from the newest one.
	blocks, err := s.chain.chainStore.GetBlockSequence(newest.hash, false, uint32(newest.number-number)+1)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		return nil, nil
	}
	found := blocks[len(blocks)-1]
	hash, err := found.Header.Hash()
	if err != nil {
		return nil, err
	}
	return &answeredBlock{number: number, hash: hash, header: found.Header}, nil
}

// parseBlockNumber reads the hex form Ethereum uses for a block number, and
// says so when the value is not one.
func parseBlockNumber(value string) (uint, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(value), "0x")
	if trimmed == "" {
		return 0, fmt.Errorf("this chain numbers its blocks by timeslot, so %q is not a block it has", value)
	}
	number, err := strconv.ParseUint(trimmed, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("block number %q is not hex: %w", value, err)
	}
	return uint(number), nil
}

// evmCall has no answer here.
//
// A call runs contract code, and this chain does not run contract code yet: that
// is the PVM, and until it exists a number this method returned would be a lie
// with a balance attached to it. A wallet that only reads balances and sends
// transfers works without it.
func (s *papucoinHandlers) evmCall() (map[string]interface{}, error) {
	return nil, fmt.Errorf("this chain does not run contract code yet, so eth_call has no honest answer; balances and transfers work")
}

// weiOf counts a PAPU amount in wei, which is the unit a wallet that only speaks
// Ethereum counts in.
func weiOf(raw *big.Int, scale *big.Int) string {
	return fmt.Sprintf("0x%x", new(big.Int).Mul(raw, scale))
}

// stateRoot is the root of the state the node has committed.
func (s *papucoinHandlers) stateRoot() (string, error) {
	return hashToHex(crypto.Hash(s.runtime.Root())), nil
}

// serviceAccount is the JAM account of the PAPU service, which is what pays for
// its storage.
func (s *papucoinHandlers) serviceAccount() (map[string]interface{}, error) {
	account, ok := s.runtime.Account(s.runtime.PapucoinID())
	if !ok {
		return nil, fmt.Errorf("service %d has no account", s.runtime.PapucoinID())
	}
	threshold, err := account.ThresholdBalance()
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"serviceId": s.runtime.PapucoinID(),
		"balance":   account.Balance,
		"threshold": threshold,
		"items":     account.GetTotalNumberOfItems(),
		"octets":    account.GetTotalNumberOfOctets(),
		"codeHash":  hashToHex(account.CodeHash),
	}, nil
}

// hexOrBase64 accepts either encoding, because a client that signs an item
// naturally has it as hex and a browser client naturally has it as base64.
func hexOrBase64(value string) ([]byte, error) {
	trimmed := value
	if len(trimmed) > 2 && trimmed[:2] == "0x" {
		trimmed = trimmed[2:]
	}
	if decoded, err := hex.DecodeString(trimmed); err == nil {
		return decoded, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("item is neither hex nor base64: %w", err)
	}
	return decoded, nil
}

// storage reads a raw PAPU storage key, which is what the JAM state is made of.
func (s *papucoinHandlers) storage(params []json.RawMessage) (map[string]interface{}, error) {
	rawKey, err := stringParam(params, 0, "key")
	if err != nil {
		return nil, err
	}
	key, err := hexOrBase64(rawKey)
	if err != nil {
		return nil, fmt.Errorf("key: %w", err)
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("key is empty")
	}

	value, ok := s.runtime.View().Raw(key)
	if !ok {
		return nil, fmt.Errorf("key 0x%x is not set in service %d", key, s.runtime.PapucoinID())
	}
	return map[string]interface{}{
		"key":   "0x" + hex.EncodeToString(key),
		"value": "0x" + hex.EncodeToString(value),
		"size":  len(value),
	}, nil
}

// currentFeeHex is the price in the form an Ethereum client reads, which is a
// quantity of wei per unit of gas rather than an amount of PAPU. A block of
// this chain spends no gas a client can meter, so the whole transfer fee is
// priced against the one intrinsic unit of gas a transfer is charged.
func currentFeeHex(s *papucoinHandlers) string {
	fee := papucoin.CurrentFee(context.Background(), s.runtime.View(), s.runtime.Params())
	// The fee is kept in raw units. A client prices in wei, and a raw unit is
	// worth however many wei one PAPU's decimals say it is, so that is the factor
	// to scale by. Scaling by the decimals instead would report a figure 10^6
	// too large for a chain with twelve decimals and eighteen.
	perGas := new(big.Int).Mul(fee, s.runtime.WeiPerRaw())
	return "0x" + perGas.Text(16)
}
