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

// Blocks do not arrive in order. The stretch behind an announced head comes back
// newest first, so every block of it finds its parent not yet stored.
//
// Each one used to leave a tip behind, and the set of tips is what the node calls
// its chain: after a few hundred blocks a node was choosing between hundreds of
// tips, and it called a timeslot already written because one of them named it,
// so the author stood down and the chain stopped on a timeslot no node had a
// block for.
func TestOutOfOrderImportLeavesOneTip(t *testing.T) {
	bs := newTestService(t)

	genesisHash := genesisIn(t, bs)

	// The chain, built in order and then handed over backwards.
	const n = 30
	headers := make([]block.Header, 0, n)
	parent := genesisHash
	for i := 1; i <= n; i++ {
		h := testChain(parent, futureSlot(t, bs, jamtime.Timeslot(i)), 0)
		headers = append(headers, h)
		hash, err := h.Hash()
		require.NoError(t, err)
		parent = hash
	}

	for i := n - 1; i >= 0; i-- {
		require.NoError(t, bs.StoreImportedBlock(block.Block{Header: headers[i]}))
	}

	// Newest first means every block becomes a tip before its parent turns up and
	// can take the tip back, so the set is allowed to hold more than one. What it
	// is not allowed to do is grow without bound, because the sync loop walks it
	// on every pass and every block sorts it.
	leaves := bs.Leaves()
	require.LessOrEqual(t, len(leaves), 64,
		"the set of tips is walked on every pass of the sync loop, so it is bounded")

	// And the tip it follows is the end of the chain, not the block that happened
	// to arrive first, which is the one the node would otherwise be stuck behind.
	newest := jamtime.Timeslot(0)
	for _, leaf := range leaves {
		if leaf.Slot > newest {
			newest = leaf.Slot
		}
	}
	assert.Equal(t, futureSlot(t, bs, n), newest,
		"and it follows the end of it, not the block that arrived first")
}

// The timeslot an author is responsible for must only count as written when the
// block is really there. A header is not a block, and one that arrived without
// its block used to stop the author of that timeslot from writing it, which is a
// chain that stops with every node convinced the timeslot was dealt with.
func TestATimeslotWithOnlyAHeaderIsNotTakenForWritten(t *testing.T) {
	bs := newTestService(t)

	genesisHash := genesisIn(t, bs)

	// The header for the next timeslot is known. Its block has not arrived.
	announced := testChain(genesisHash, futureSlot(t, bs, 1), 0)
	announcedHash, err := announced.Hash()
	require.NoError(t, err)
	require.NoError(t, bs.Store.PutHeader(announced))
	bs.AddLeaf(announcedHash, announced.TimeSlotIndex)

	// It shows up as a leaf, so anything that reads the leaf set alone will call
	// that timeslot written.
	require.NotEmpty(t, bs.Leaves())

	// And the block store, which is what has to decide, has nothing for it.
	_, err = bs.Store.GetBlock(announcedHash)
	require.Error(t, err, "a header on its own is not a block, and the difference is the whole point")
}
//
// It used to be written into the store and nothing else, with only a header
// arriving by announcement ever touching the leaf set. A block whose header came
// separately, or never came, was therefore in the store and unknown to the tip:
// the author waiting on that very block held its timeslot for ever, and the walk
// for gaps started from the older tip and found nothing, because the chain below
// it was whole. The mesh stops with every node healthy and nothing logged as an
// error.
func TestImportedBlockBecomesTheTip(t *testing.T) {
	bs := newTestService(t)

	genesisHash := genesisIn(t, bs)

	// A block that arrives without its header ever having been announced.
	imported := testChain(genesisHash, futureSlot(t, bs, 1), 0)
	importedHash, err := imported.Hash()
	require.NoError(t, err)

	require.NoError(t, bs.StoreImportedBlock(block.Block{Header: imported}))

	leaves := bs.Leaves()
	require.Len(t, leaves, 1, "the imported block has to be the tip, not left out of it")
	assert.Equal(t, importedHash, func() crypto.Hash {
		h, herr := leaves[0].Header.Hash()
		require.NoError(t, herr)
		return h
	}(), "the tip is the block that arrived")

	// A block with a parent already there replaces it, rather than both sitting
	// in the leaf set as two tips.
	child := testChain(importedHash, futureSlot(t, bs, 2), 0)
	require.NoError(t, bs.StoreImportedBlock(block.Block{Header: child}))

	leaves = bs.Leaves()
	require.Len(t, leaves, 1, "a chain of imported blocks is one tip, not one per block")
	assert.Equal(t, futureSlot(t, bs, 2), leaves[0].Slot, "the tip is the newest of them")
}

// A chain that arrived block by block has to look whole: the tip is the block
// that is really there, and there is nothing left to ask for.
//
// Registering the leaf on import is only correct if it does not then report the
// blocks it registered as missing, and the author waiting on a parent has to be
// released by exactly this: a block arriving is what tells it the parent it was
// holding a timeslot for is here.
func TestImportedChainIsWholeAndFreesTheAuthorWaitingOnIt(t *testing.T) {
	bs := newTestService(t)

	genesisHash := genesisIn(t, bs)

	parent := genesisHash
	for i := 1; i <= 4; i++ {
		h := testChain(parent, futureSlot(t, bs, jamtime.Timeslot(i)), 0)
		require.NoError(t, bs.StoreImportedBlock(block.Block{Header: h}))
		hash, err := h.Hash()
		require.NoError(t, err)
		parent = hash
	}

	assert.Empty(t, bs.BackfillHashes(10),
		"blocks that arrived are not gaps, so the author is not sent looking for them")

	// And the tip is the last of them, which is what lets the author of the
	// timeslot after it write.
	leaves := bs.Leaves()
	require.Len(t, leaves, 1)
	assert.Equal(t, futureSlot(t, bs, 4), leaves[0].Slot,
		"the author of the next timeslot builds on the block that arrived last")
}
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
