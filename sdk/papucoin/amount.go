// Package papucoin is PAPU, the economy of the chain, expressed as a JAM
// service on top of the svc SDK.
//
// Amounts are arbitrary precision because the maximum supply of 10^9 PAPU with
// 12 decimals is 10^21, which does not fit a uint64. Balances are stored as
// fixed 16 byte big endian values rather than decimal strings: the storage
// footprint is priced per octet, so a fixed width keeps state small and makes
// comparison and lookup cheap.
package papucoin

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
)

// Params are the PAPU coin parameters.
type Params struct {
	Symbol        string
	Name          string
	Decimals      uint8
	MaxSupply     *big.Int
	TransferFee   *big.Int
	FaucetAmount  *big.Int
	WelcomeAmount *big.Int
	// FirstNonce is the nonce an account starts at, matching the original
	// chain where an unseen account is expected to submit nonce 1.
	FirstNonce uint64
}

// DefaultParams returns the parameters of the original chain.
func DefaultParams() Params {
	const decimals = 12
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(decimals), nil)

	maxSupply := new(big.Int).Mul(big.NewInt(1_000_000_000), unit)
	faucet := new(big.Int).Mul(big.NewInt(10_000), unit)
	welcome := new(big.Int).Mul(big.NewInt(5), unit)

	return Params{
		Symbol:        "PAPU",
		Name:          "CRYPTOPAPU",
		Decimals:      decimals,
		MaxSupply:     maxSupply,
		TransferFee:   big.NewInt(1_000_000), // 1 micro-PAPU
		FaucetAmount:  faucet,
		WelcomeAmount: welcome,
		FirstNonce:    1,
	}
}

var (
	// ErrInvalidAmount is returned for a malformed decimal amount.
	ErrInvalidAmount = errors.New("papucoin: invalid amount")
	// ErrAmountOverflow is returned when an amount cannot be stored.
	ErrAmountOverflow = errors.New("papucoin: amount exceeds 128 bits")
	// ErrNegativeAmount is returned for a negative amount.
	ErrNegativeAmount = errors.New("papucoin: negative amount")
)

// amountOctets is the fixed width of a stored amount.
const amountOctets = 16

// encodeAmount renders an amount as 16 big endian bytes.
func encodeAmount(v *big.Int) ([]byte, error) {
	if v.Sign() < 0 {
		return nil, ErrNegativeAmount
	}
	if v.BitLen() > amountOctets*8 {
		return nil, fmt.Errorf("%w: %d bits", ErrAmountOverflow, v.BitLen())
	}

	out := make([]byte, amountOctets)
	v.FillBytes(out)
	return out, nil
}

// decodeAmount parses a stored amount. An absent entry decodes to zero, which
// matches the original chain defaulting missing balances to 0.
func decodeAmount(stored []byte) (*big.Int, error) {
	if len(stored) == 0 {
		return new(big.Int), nil
	}
	if len(stored) > amountOctets {
		return nil, fmt.Errorf("%w: %d octets", ErrAmountOverflow, len(stored))
	}
	return new(big.Int).SetBytes(stored), nil
}

// FormatRaw renders a raw amount with its decimals, trimming trailing zeros.
func FormatRaw(raw *big.Int, decimals uint8) string {
	negative := raw.Sign() < 0
	abs := new(big.Int).Abs(raw)
	digits := abs.String()
	if decimals == 0 {
		if negative {
			return "-" + digits
		}
		return digits
	}

	if len(digits) <= int(decimals) {
		digits = strings.Repeat("0", int(decimals)-len(digits)+1) + digits
	}
	split := len(digits) - int(decimals)
	whole := strings.TrimLeft(digits[:split], "0")
	if whole == "" {
		whole = "0"
	}
	fraction := strings.TrimRight(digits[split:], "0")

	out := whole
	if fraction != "" {
		out += "." + fraction
	}
	if negative {
		return "-" + out
	}
	return out
}

// ParseAmount parses a decimal string such as "123.000001" into a raw amount.
func ParseAmount(input string, decimals uint8) (*big.Int, error) {
	s := strings.TrimSpace(input)
	if strings.HasPrefix(s, "+") {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, input)
	}

	parts := strings.Split(s, ".")
	switch len(parts) {
	case 1:
	case 2:
		if len(parts[1]) > int(decimals) {
			return nil, fmt.Errorf("%w: %q has more than %d decimals", ErrInvalidAmount, input, decimals)
		}
	default:
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, input)
	}

	whole, fraction := parts[0], ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	if whole == "" && fraction == "" {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, input)
	}
	for _, part := range []string{whole, fraction} {
		for _, r := range part {
			if r < '0' || r > '9' {
				return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, input)
			}
		}
	}

	// Padding the fraction to full width already places the value on the raw
	// scale, so the digits are used as they are. Scaling again here would
	// double count the decimals.
	if len(fraction) < int(decimals) {
		fraction += strings.Repeat("0", int(decimals)-len(fraction))
	}

	value, ok := new(big.Int).SetString(whole+fraction, 10)
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrInvalidAmount, input)
	}
	return value, nil
}
