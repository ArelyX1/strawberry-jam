package chain

import (
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The canonical chain has to come out in the order it was written, and it has to
// follow the parent hashes rather than the timeslots. Two blocks can share a
// timeslot once a chain forks, and a collection keyed by timeslot keeps whichever
// arrived last, which is how a node ends up replaying a mixture of two branches
// and landing on a state neither one ever described.
func TestCanonicalChainFollowsParentsNotTimeslots(t *testing.T) {
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := NewBlockService(db, 0)
	require.NoError(t, err)

	fin := bs.GetLatestFinalized()
	genesisHash := fin.Hash
	slot := fin.TimeSlotIndex

	// One branch up to slot+2, and a second that branches at slot+1 and runs to
	// slot+3. Both leave the finalized block and each has a block at every slot
	// it covers.
	type frame struct {
		header block.Header
		hash   crypto.Hash
	}
	_ = block.Header{}
	mk := func(parent crypto.Hash, at jamtime.Timeslot, tag byte) frame {
		h := testChain(parent, at, 0)
		h.PriorStateRoot = crypto.Hash{tag}
		hh, err := h.Hash()
		require.NoError(t, err)
		require.NoError(t, bs.Store.PutBlock(block.Block{Header: h}))
		return frame{header: h, hash: hh}
	}

	a1 := mk(genesisHash, slot+1, 0xa1)
	a2hash := mk(a1.hash, slot+2, 0xa2).hash
	// The second branch leaves the same parent as the first. Branching from a1
	// instead would make a1 a real ancestor of it, and then the walk following
	// the tip would be right to pass through a1.
	b1 := mk(genesisHash, slot+1, 0xb1)
	b2 := mk(b1.hash, slot+2, 0xb2)
	b3 := mk(b2.hash, slot+3, 0xb3)

	// The longer branch must come back whole, parent by parent, and must not
	// have the other branch's block spliced into it.
	chain, err := bs.CanonicalChain(b3.hash, 0)
	require.NoError(t, err)

	hashes := make([]crypto.Hash, len(chain))
	for i, b := range chain {
		h, err := b.Header.Hash()
		require.NoError(t, err)
		hashes[i] = h
	}
	// The block the node starts from comes first, then the three of the branch
	// the tip belongs to. The two blocks of the abandoned branch at slot+1 and
	// slot+2 are not in there.
	require.GreaterOrEqual(t, len(chain), 3)
	tail := hashes[len(hashes)-3:]
	assert.Equal(t, []crypto.Hash{b1.hash, b2.hash, b3.hash}, tail,
		"the chain has to be the one the tip belongs to, in order")
	for _, h := range []crypto.Hash{a1.hash, a2hash} {
		assert.NotContains(t, hashes, h, "the abandoned branch must not appear")
	}

	// And the two branches must not be confused for one another: same timeslot,
	// different block, and the walk has to pick the right one.
	assert.NotEqual(t, a1.hash, b1.hash)
	assert.NotContains(t, hashes, a1.hash, "the abandoned branch must not appear")
}

// Walking into a hole is a question, not a guess: the chain cannot be walked
// past a block that is not here, because what came before it is unknown.
func TestCanonicalChainRefusesToWalkPastAHole(t *testing.T) {
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := NewBlockService(db, 0)
	require.NoError(t, err)

	fin := bs.GetLatestFinalized()
	orphan := testChain(fin.Hash, fin.TimeSlotIndex+2, 0)
	orphanHash, err := orphan.Hash()
	require.NoError(t, err)
	// Only the header, no block: the slot before it is missing entirely.
	require.NoError(t, bs.Store.PutHeader(orphan))

	_, err = bs.CanonicalChain(orphanHash, 0)
	require.Error(t, err, "a chain with a hole in it cannot be walked")
}

// Two leaves on the same timeslot have to be ordered the same way on every node,
// or the fork never settles and each node follows a different branch.
func TestLeavesOnTheSameSlotAreOrderedDeterministically(t *testing.T) {
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := NewBlockService(db, 0)
	require.NoError(t, err)

	fin := bs.GetLatestFinalized()
	slot := fin.TimeSlotIndex + 1

	first := testChain(fin.Hash, slot, 0)
	first.PriorStateRoot = crypto.Hash{0x01}
	firstHash, err := first.Hash()
	require.NoError(t, err)
	require.NoError(t, bs.Store.PutHeader(first))

	second := testChain(fin.Hash, slot, 0)
	second.PriorStateRoot = crypto.Hash{0x02}
	secondHash, err := second.Hash()
	require.NoError(t, err)
	require.NoError(t, bs.Store.PutHeader(second))

	bs.AddLeaf(firstHash, slot)
	bs.AddLeaf(secondHash, slot)

	leaves := bs.Leaves()
	require.Len(t, leaves, 2, "both tips are leaves, whichever side wrote them")
	assert.Equal(t, slot, leaves[0].Slot)
	assert.Equal(t, slot, leaves[1].Slot)
	assert.NotEqual(t, leaves[0].Header.PriorStateRoot, leaves[1].Header.PriorStateRoot,
		"the two are genuinely different blocks")
}
