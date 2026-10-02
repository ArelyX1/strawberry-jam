package chain

import (
	"errors"
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/store"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T) *BlockService {
	t.Helper()
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := NewBlockService(db, 0)
	require.NoError(t, err)
	return bs
}

// genesisIn stores the header this node finalizes at startup, so that a chain
// built from it can be walked back to the finalized point.
func genesisIn(t *testing.T, bs *BlockService) crypto.Hash {
	t.Helper()
	fin := bs.GetLatestFinalized()
	g := testChain(fin.Hash, fin.TimeSlotIndex, 0)
	require.NoError(t, bs.Store.PutHeader(g))
	return fin.Hash
}

// testChain builds a header that builds on parent at the given slot.
func testChain(parent crypto.Hash, slot jamtime.Timeslot, author uint16) block.Header {
	return block.Header{
		ParentHash:       parent,
		PriorStateRoot:   crypto.Hash{byte(slot)},
		ExtrinsicHash:    crypto.Hash{0xaa},
		TimeSlotIndex:    slot,
		BlockAuthorIndex: author,
	}
}

// A fresh service finalizes the current timeslot, so anything this node can
// still accept has to be a slot above that. Testing below it would pass for the
// wrong reason: a header in the past is not a descendant either, and that is a
// rejection rather than a gap.
func futureSlot(t *testing.T, bs *BlockService, ahead jamtime.Timeslot) jamtime.Timeslot {
	t.Helper()
	return bs.GetLatestFinalized().TimeSlotIndex + ahead
}

// A header whose ancestors this node does not have must be kept, not thrown
// away: it is the only thing that says there is a gap and the only handle to
// ask for what is missing. It used to be discarded as a bad header, so a node
// that was behind never caught up no matter how long it waited.
func TestHeaderWithMissingAncestorsIsKeptAndReported(t *testing.T) {
	bs := newTestService(t)

	// A block whose parent was never stored.
	orphan := testChain(crypto.Hash{0xde, 0xad}, futureSlot(t, bs, 10), 0)
	orphanHash, err := orphan.Hash()
	require.NoError(t, err)

	err = bs.HandleNewHeader(&orphan)
	require.Error(t, err, "a header over a gap cannot be placed")
	assert.True(t, IsMissingHeader(err),
		"a gap must be distinguishable from a header that does not belong, got: %v", err)

	assert.Equal(t, 1, bs.PendingGaps(), "the header has to be kept for later")

	// And the gap has to be visible as something to go and fetch.
	wanted := bs.BackfillHashes(10)
	assert.Contains(t, wanted, orphanHash, "the block itself is wanted")
	assert.Contains(t, wanted, orphan.ParentHash, "and so is the hole behind it")
}

// A header that is not a descendant of anything this node finalized is a
// different animal and must not be kept: it does not become a gap to fill.
func TestHeaderThatIsNotADescendantIsNotTreatedAsAGap(t *testing.T) {
	bs := newTestService(t)

	// A header from before the finalized point, with its parent present. The walk
	// never runs, so what rejects it is that it is not a descendant rather than
	// that anything is missing, and it must not be kept for a fill.
	old := testChain(genesisIn(t, bs), bs.GetLatestFinalized().TimeSlotIndex-3, 0)

	err := bs.HandleNewHeader(&old)
	require.Error(t, err, "a header older than the finalized block is not a descendant")
	assert.False(t, IsMissingHeader(err),
		"a header that is not a descendant is not a gap, got: %v", err)
	assert.Equal(t, 0, bs.PendingGaps(), "it must not be kept for a fill that would never help")
}

// When the missing blocks arrive, everything waiting on them must be placed.
func TestPendingHeadersArePlacedOnceTheGapIsFilled(t *testing.T) {
	bs := newTestService(t)

	genesisHash := genesisIn(t, bs)

	first := testChain(genesisHash, futureSlot(t, bs, 1), 0)
	firstHash, err := first.Hash()
	require.NoError(t, err)
	second := testChain(firstHash, futureSlot(t, bs, 2), 0)

	// The head arrives first, with nothing behind it.
	err = bs.HandleNewHeader(&second)
	require.Error(t, err)
	require.True(t, IsMissingHeader(err))
	require.Equal(t, 1, bs.PendingGaps())

	// The block that closes the gap lands.
	require.NoError(t, bs.Store.PutBlock(block.Block{Header: first}))

	placed := bs.RetryPending()
	assert.Equal(t, 1, placed, "the waiting header should be placeable now")
	assert.Equal(t, 0, bs.PendingGaps())

	stored, err := bs.Store.GetHeader(firstHash)
	require.NoError(t, err)
	assert.Equal(t, futureSlot(t, bs, 1), stored.TimeSlotIndex)
}

// Nothing missing means nothing to ask for, which is what stops the sync loop
// from requesting blocks it already has.
func TestBackfillReportsNothingWhenTheChainIsWhole(t *testing.T) {
	bs := newTestService(t)

	genesisHash := genesisIn(t, bs)

	prev := genesisHash
	for i := 1; i <= 4; i++ {
		slot := futureSlot(t, bs, jamtime.Timeslot(i))
		h := testChain(prev, slot, 0)
		require.NoError(t, bs.Store.PutBlock(block.Block{Header: h}))
		hash, err := h.Hash()
		require.NoError(t, err)
		prev = hash
		bs.AddLeaf(hash, slot)
	}

	assert.Empty(t, bs.BackfillHashes(10), "a whole chain has no gaps to fill")
	assert.Equal(t, 0, bs.PendingGaps())
}

// IsMissingHeader must not be fooled by a plain not-found for something else.
func TestIsMissingHeaderIsAboutAncestorsOnly(t *testing.T) {
	assert.True(t, IsMissingHeader(store.ErrHeaderNotFound))
	assert.True(t, IsMissingHeader(ErrMissingAncestors))
	assert.False(t, IsMissingHeader(nil))
	assert.False(t, IsMissingHeader(errors.New("something else")))
}

// A node that fell behind has the headers for a stretch of chain and not the
// blocks, because announcements carry headers and blocks have to be asked for
// separately. It used to look for the first parent it had a header for, find one
// straight away, and stop: it had every header in a row and none of the blocks
// under them, so it asked for nothing and filled nothing while reporting the same
// two blocks missing forever.
func TestBackfillAsksForBlocksUnderKnownHeaders(t *testing.T) {
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := NewBlockService(db, 0)
	require.NoError(t, err)

	fin := bs.GetLatestFinalized()
	cur := fin.Hash
	hashes := make([]crypto.Hash, 0, 5)
	// Five blocks: every header is stored, not one block. This is exactly the
	// state of a node that heard the announcements and never got the blocks.
	for i := 1; i <= 5; i++ {
		h := testChain(cur, fin.TimeSlotIndex+jamtime.Timeslot(i), 0)
		hash, herr := h.Hash()
		require.NoError(t, herr)
		require.NoError(t, bs.Store.PutHeader(h))
		hashes = append(hashes, hash)
		cur = hash
	}

	bs.AddLeaf(cur, fin.TimeSlotIndex+5)

	wanted := bs.BackfillHashes(32)
	require.NotEmpty(t, wanted, "blocks under known headers are still missing blocks")

	// Every one of the five has to be asked for, and the walk has to reach the
	// bottom of the stretch rather than stopping under the leaf.
	for _, h := range hashes {
		assert.Contains(t, wanted, h, "the walk must go past the first known header")
	}
	assert.Len(t, wanted, 5, "nothing below the leaf is present, so all five are wanted")
}

// The walk stops where the blocks start, and does not go on asking for history
// the node already has.
func TestBackfillStopsAtTheFirstBlockItHas(t *testing.T) {
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := NewBlockService(db, 0)
	require.NoError(t, err)

	fin := bs.GetLatestFinalized()
	// Two blocks with the blocks present, the third header only.
	prev := fin.Hash
	present := make([]crypto.Hash, 0, 2)
	for i := 1; i <= 2; i++ {
		h := testChain(prev, fin.TimeSlotIndex+jamtime.Timeslot(i), 0)
		hash, herr := h.Hash()
		require.NoError(t, herr)
		require.NoError(t, bs.Store.PutHeader(h))
		require.NoError(t, bs.Store.PutBlock(block.Block{Header: h}))
		present = append(present, hash)
		prev = hash
	}
	tip := testChain(prev, fin.TimeSlotIndex+3, 0)
	tipHash, err := tip.Hash()
	require.NoError(t, err)
	require.NoError(t, bs.Store.PutHeader(tip))
	bs.AddLeaf(tipHash, fin.TimeSlotIndex+3)

	wanted := bs.BackfillHashes(32)
	assert.Equal(t, []crypto.Hash{tipHash}, wanted,
		"only the missing block is wanted, not the two that are already here")
	for _, h := range present {
		assert.NotContains(t, wanted, h)
	}
}
