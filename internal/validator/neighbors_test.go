package validator

import (
	"testing"

	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/crypto/ed25519"
	"github.com/stretchr/testify/assert"
)

// The validator set is a fixed-size array, so a run with fewer validators
// configured than the chain allows leaves the tail of it as zero keys. The grid
// hands those out as neighbours like any other, and one of them was enough to
// stop a node before it had dialled anybody: it came up with no peers at all.
func TestEmptyValidatorsAreNotNeighbours(t *testing.T) {
	real := ed25519.NewKeyFromSeed(bytesOf(1)).Public().(ed25519.PublicKey)
	empty := ed25519.PublicKey{}

	got := dropEmpty([]crypto.ValidatorKey{
		{Ed25519: real},
		{Ed25519: empty},
		{Ed25519: real},
		{Ed25519: empty},
	})

	assert.Len(t, got, 2, "only the validators that are really configured")
	for _, v := range got {
		assert.False(t, ed25519.IsEmpty(v.Ed25519), "a zero key is not a neighbour")
	}
}

// And with nothing configured there are no neighbours, not a set of them.
func TestDropEmptyOfNothingIsNothing(t *testing.T) {
	assert.Empty(t, dropEmpty(nil))
	empty := ed25519.PublicKey{}
	assert.Empty(t, dropEmpty([]crypto.ValidatorKey{{Ed25519: empty}}))
}

func bytesOf(b byte) []byte {
	s := make([]byte, ed25519.SeedSize)
	for i := range s {
		s[i] = b
	}
	return s
}
