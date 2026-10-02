// Package devnet builds a runnable JAM state out of a genesis file: the
// services that exist, the accounts that hold PAPU, and a scheduler that turns
// queued items into blocks. It is the layer the dev node runs on, and the layer
// the runtime tests assert against.
package devnet

import (
	"github.com/eigerco/strawberry/internal/jamtime"

	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/eigerco/strawberry/sdk/papucoin"
)

// Genesis is the network definition of a dev chain, as written by
// tools/genesis-from-ts.mjs.
//
// Service is a pointer so that a genesis without a service section is rejected
// on load instead of defaulting into a chain with no PAPU.
type Genesis struct {
	Network     string `json:"network"`
	Description string `json:"description"`
	// GeneratedFrom records which genesis this was derived from, so a state
	// root can always be traced back to an economy definition.
	GeneratedFrom string `json:"generatedFrom"`
	TimeslotSecs  int    `json:"timeslotSecs"`
	// GenesisTimeslot is the timeslot the chain is founded at, and every node
	// that joins has to found it at the same one.
	//
	// Dating the genesis when the node starts is what made a network of nodes
	// impossible: two nodes started seconds apart founded two different chains,
	// with different genesis blocks at different timeslots and so different
	// hashes. No block one of them wrote could descend from the other's genesis,
	// so every header the other sent was rejected as not being a descendant of
	// the finalized block, and the two never agreed on anything. The chain's
	// first block has to be the same block for everybody.
	GenesisTimeslot jamtime.Timeslot `json:"genesisTimeslot"`
	Service         *ServiceCfg      `json:"service"`
	EVM             *EVMCfg          `json:"evm"`
	// Extra only exists so a test can prove an unknown genesis field is a load
	// error rather than something silently dropped.
	Extra int `json:"extra,omitempty"`
}

// ServiceCfg is the PAPU service as it is defined at genesis.
type ServiceCfg struct {
	ID            uint16 `json:"id"`
	Name          string `json:"name"`
	Symbol        string `json:"symbol"`
	Decimals      uint8  `json:"decimals"`
	MaxSupply     string `json:"maxSupply"`
	TransferFee   string `json:"transferFee"`
	FaucetAmount  string `json:"faucetAmount"`
	WelcomeAmount string `json:"welcomeAmount"`
	FirstNonce    uint64 `json:"firstNonce"`
	Endowment     string `json:"endowment"`
	// Code names a polkavm guest blob to run the economy with, instead of the
	// native Go handlers. The service account is the same either way, so the two
	// can be swapped without migrating state.
	Code string `json:"code,omitempty"`
	// Issuer is the hex encoded Ed25519 public key of the only address that may
	// mint. Genesis names keys rather than addresses because the address
	// checksum is derived: the node renders each address itself, so the two
	// implementations cannot disagree about it.
	Issuer string `json:"issuer"`
	// Accounts are the opening holders of PAPU, named by public key.
	Accounts []GenesisAccount `json:"accounts"`
}

// GenesisAccount is one opening balance.
type GenesisAccount struct {
	PublicKey string `json:"publicKey"`
	Amount    string `json:"amount"`
}

// EVMCfg describes the chain as the EVM gateway presents it to MetaMask.
type EVMCfg struct {
	ChainID  int64  `json:"chainId"`
	Name     string `json:"name"`
	Symbol   string `json:"symbol"`
	Decimals uint8  `json:"decimals"`
}

// LoadGenesis reads and validates a genesis file.
func LoadGenesis(path string) (*Genesis, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("devnet: cannot read genesis %s: %w", path, err)
	}

	var genesis Genesis
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&genesis); err != nil {
		return nil, fmt.Errorf("devnet: genesis %s is not valid: %w", path, err)
	}

	if err := genesis.validate(); err != nil {
		return nil, fmt.Errorf("devnet: genesis %s is not usable: %w", path, err)
	}
	return &genesis, nil
}

func (g *Genesis) validate() error {
	switch {
	case g.Network == "":
		return fmt.Errorf("network is missing")
	case g.TimeslotSecs <= 0:
		return fmt.Errorf("timeslotSecs is %d, want a positive duration", g.TimeslotSecs)
	case g.Service == nil:
		return fmt.Errorf("service is missing, so the chain would have no PAPU")
	}

	service := g.Service
	switch {
	case service.Name == "":
		return fmt.Errorf("service.name is missing")
	case service.Symbol == "":
		return fmt.Errorf("service.symbol is missing")
	case service.Issuer == "":
		return fmt.Errorf("service.issuer is missing, so nothing could ever be minted")
	case len(service.Accounts) == 0:
		return fmt.Errorf("service.accounts is empty, so the chain would start with no money")
	case service.Decimals == 0 || service.Decimals > 18:
		return fmt.Errorf("service.decimals is %d, want 1..18", service.Decimals)
	}

	for _, field := range []struct{ name, value string }{
		{"maxSupply", service.MaxSupply},
		{"transferFee", service.TransferFee},
		{"faucetAmount", service.FaucetAmount},
		{"welcomeAmount", service.WelcomeAmount},
	} {
		if !isDecimalInteger(field.value) {
			return fmt.Errorf("service.%s is %q, want an amount in %s", field.name, field.value, service.Symbol)
		}
	}
	if !isDecimalInteger(service.Endowment) {
		return fmt.Errorf("service.endowment is %q, want a whole number of JAM units", service.Endowment)
	}

	// The EVM section is optional: a chain without one is not presented as an
	// Ethereum chain, and a wallet has nothing to talk to. When it is there, the
	// ratio between the two sets of decimal places has to leave something for a
	// wei to be worth, or a balance in wei could not be counted in the currency
	// the chain keeps.
	if g.EVM != nil {
		switch {
		case g.EVM.ChainID <= 0:
			return fmt.Errorf("evm.chainId is %d, want a positive chain id", g.EVM.ChainID)
		case g.EVM.Decimals == 0 || g.EVM.Decimals > 18:
			return fmt.Errorf("evm.decimals is %d, want 1..18", g.EVM.Decimals)
		case g.EVM.Decimals < service.Decimals:
			return fmt.Errorf("evm.decimals is %d and service.decimals is %d, so a wei would be worth less than the smallest unit of %s",
				g.EVM.Decimals, service.Decimals, service.Symbol)
		case g.EVM.Symbol == "":
			return fmt.Errorf("evm.symbol is missing, so a wallet would have no name for the money")
		}
	}

	if _, err := ParsePublicKey(service.Issuer); err != nil {
		return fmt.Errorf("service.issuer: %w", err)
	}
	seen := make(map[string]bool, len(service.Accounts))
	for index, account := range service.Accounts {
		if _, err := ParsePublicKey(account.PublicKey); err != nil {
			return fmt.Errorf("service.accounts[%d]: %w", index, err)
		}
		if seen[account.PublicKey] {
			return fmt.Errorf("service.accounts[%d] repeats a public key", index)
		}
		seen[account.PublicKey] = true
		if !isDecimalInteger(account.Amount) {
			return fmt.Errorf("service.accounts[%d].amount is %q, want an amount in %s", index, account.Amount, service.Symbol)
		}
	}
	return nil
}

// ParsePublicKey reads the hex encoded Ed25519 public key a genesis names an
// account by.
func ParsePublicKey(hexKey string) (ed25519.PublicKey, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimSpace(hexKey), "0x"))
	if err != nil {
		return nil, fmt.Errorf("public key %q is not hex: %w", hexKey, err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(raw), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(raw), nil
}

// IssuerAddress is the address of the only account the service lets mint.
func (s *ServiceCfg) IssuerAddress() (string, error) {
	publicKey, err := ParsePublicKey(s.Issuer)
	if err != nil {
		return "", err
	}
	return papucoin.AddressFromPublicKey(publicKey)
}

// InitialBalances maps every opening account to the amount it starts with, keyed
// by the address the node derives from its public key.
func (s *ServiceCfg) InitialBalances() (map[string]string, error) {
	balances := make(map[string]string, len(s.Accounts))
	for index, account := range s.Accounts {
		publicKey, err := ParsePublicKey(account.PublicKey)
		if err != nil {
			return nil, fmt.Errorf("service.accounts[%d]: %w", index, err)
		}
		address, err := papucoin.AddressFromPublicKey(publicKey)
		if err != nil {
			return nil, fmt.Errorf("service.accounts[%d]: %w", index, err)
		}
		balances[address] = account.Amount
	}
	return balances, nil
}

// isDecimalInteger reports whether value is a plain non negative base ten number.
// Amounts are written in whole PAPU, with no sign and no exponent, because the
// SDK parses them with the same rule.
func isDecimalInteger(value string) bool {
	if value == "" {
		return false
	}
	seenDot := false
	seenDigit := false
	for _, r := range value {
		switch {
		case r >= '0' && r <= '9':
			seenDigit = true
		case r == '.' && !seenDot && seenDigit:
			seenDot = true
		default:
			return false
		}
	}
	return seenDigit && !strings.HasSuffix(value, ".")
}
