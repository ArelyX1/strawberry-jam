package papucoin

import (
	"context"
	"fmt"
	"math/big"

	svc "github.com/eigerco/strawberry/sdk"
)

// The transfer fee is a price, not a tariff. What the genesis names is where it
// starts, and from there it moves with the load: a block that asks for more work
// than a quiet one leaves the next block charging more, and a block that sat
// idle brings the price back down. A fee frozen at genesis is a number that is
// wrong the moment the chain is busy, and wrong in the direction that lets
// anybody flood it for free.
//
// All of it is in raw units, and all of it is far below one PAPU. Twelve
// decimals make a whole unit a million times too coarse to be a fee here: at one
// unit a transfer of a thousandth would be charged a thousand times what it
// moved. The floor is small enough that a small transfer is not priced out of
// existence, and the ceiling is small enough that a busy block never makes the
// transfer it is pricing out of reach.
const (
	// FeeFloor is the smallest fee the chain will charge: a thousandth of a
	// millionth of a PAPU, which is small enough not to matter to anyone.
	FeeFloor = int64(1_000)
	// FeeCeiling is the largest: a thousandth of a PAPU. Enough to price a
	// burst, small enough that it stays a rounding error next to a real amount.
	FeeCeiling = int64(1_000_000_000)
	// FeeBusyTarget is how many transfers in one block counts as busy. Above it
	// the price rises, and a block that does nothing at all brings it down.
	FeeBusyTarget = 8
	// FeeUpNumerator and FeeUpDenominator move the price up by a third, and
	// FeeDownNumerator and FeeDownDenominator move it down by a third, rounded
	// away from zero so that a price can never stall between two numbers.
	FeeUpNumerator     = int64(3)
	FeeUpDenominator   = int64(2)
	FeeDownNumerator   = int64(2)
	FeeDownDenominator = int64(3)
)

// feeLevelKey is the storage key the current fee lives under.
func feeLevelKey() []byte { return []byte{keyFeeLevel} }

// currentFee reads the fee the next block charges. A chain that has never
// charged one falls back to what the genesis named, so the first transfer costs
// what the chain was configured with rather than nothing.
func currentFee(ctx svc.AccumulateContext, params Params) (*big.Int, error) {
	stored, _, err := ctx.Read(feeLevelKey())
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return new(big.Int).Set(params.TransferFee), nil
	}
	fee, err := decodeAmount(stored)
	if err != nil {
		return nil, err
	}
	return clampFee(fee), nil
}

// nextFee moves the price in the direction the load asks for and writes it back.
// It is called once per block, after the transfers of that block have been
// applied, so what a block is charged is the price the one before it settled on
// and a block never charges a price that has not been published.
func nextFee(ctx svc.AccumulateContext, params Params, transfers int) error {
	current, err := currentFee(ctx, params)
	if err != nil {
		return err
	}

	var next *big.Int
	switch {
	case transfers > FeeBusyTarget:
		next = scaleFee(current, FeeUpNumerator, FeeUpDenominator)
	case transfers == 0:
		next = scaleFee(current, FeeDownNumerator, FeeDownDenominator)
	default:
		next = current
	}
	next = clampFee(next)
	return writeAmount(ctx, feeLevelKey(), next)
}

// scaleFee multiplies a fee and rounds down, so rounding can only ever make the
// price smaller and a transfer is never charged more than was published.
func scaleFee(fee *big.Int, num, den int64) *big.Int {
	scaled := new(big.Int).Mul(fee, big.NewInt(num))
	return scaled.Quo(scaled, big.NewInt(den))
}

// clampFee keeps the price inside the band it is allowed to occupy.
func clampFee(fee *big.Int) *big.Int {
	floor, ceiling := big.NewInt(FeeFloor), big.NewInt(FeeCeiling)
	if fee.Cmp(floor) < 0 {
		return floor
	}
	if fee.Cmp(ceiling) > 0 {
		return ceiling
	}
	return fee
}

// CurrentFee is what a client asks to be told: the price the next block charges.
// A wallet that reads this is quoting the chain rather than a constant, which is
// the whole point of the price moving.
func CurrentFee(ctx context.Context, view *View, params Params) *big.Int {
	level := new(big.Int).Set(params.TransferFee)
	if view != nil {
		if stored, ok := view.Raw(feeLevelKey()); ok {
			if decoded, err := decodeAmount(stored); err == nil {
				level = clampFee(decoded)
			}
		}
	}
	return level
}

var _ = fmt.Sprintf

// refineFee reads the price a report should quote, falling back to the genesis
// figure when the chain has never settled on one. Refine may not write, so this
// is the published price rather than a new one: a report quotes what the block
// it lands in will charge.
func refineFee(ctx svc.RefineContext, params Params) (*big.Int, error) {
	stored, _, err := ctx.Read(feeLevelKey())
	if err != nil {
		return nil, err
	}
	if len(stored) == 0 {
		return new(big.Int).Set(params.TransferFee), nil
	}
	fee, err := decodeAmount(stored)
	if err != nil {
		return nil, err
	}
	return clampFee(fee), nil
}
