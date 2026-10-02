package main

import (
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Adopting a peer's block has to rebuild from the whole chain, not from the last
// few blocks. The walk that produces the chain goes by parent hash from the tip,
// so asking for a bounded number of them yields a suffix: the tip and whatever is
// just below it, and nothing further back. The replay starts at the timeslot the
// node's own state stands at, which is nowhere near the tip when the node is
// behind, so every timeslot in between arrives with no block and nothing to check
// it against. The node then steps those timeslots on its own, lands on a state no
// block was built on, and refuses the chain it just adopted.
//
// This is what the two-validator run was doing, and it is why a node that came
// back never managed to catch up no matter how long it was given.
func TestAdoptingAPeerBlockNeedsTheBlocksBeforeItNotJustTheTip(t *testing.T) {
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := chain.NewBlockService(db, 0)
	require.NoError(t, err)

	fin := bs.GetLatestFinalized()
	genesis := fin.Hash

	// Six blocks on one chain, every one stored, so nothing is missing and the
	// only thing that can go wrong is looking at too few of them.
	prev := genesis
	hashes := make([]crypto.Hash, 0, 6)
	slots := make([]jamtime.Timeslot, 0, 6)
	for i := 1; i <= 6; i++ {
		slot := fin.TimeSlotIndex + jamtime.Timeslot(i)
		h := block.Header{
			ParentHash:       prev,
			PriorStateRoot:   crypto.Hash{byte(i)},
			ExtrinsicHash:    crypto.Hash{0xaa},
			TimeSlotIndex:    slot,
			BlockAuthorIndex: uint16(i % 2),
		}
		hash, herr := h.Hash()
		require.NoError(t, herr)
		require.NoError(t, bs.Store.PutBlock(block.Block{Header: h}))
		hashes = append(hashes, hash)
		slots = append(slots, slot)
		prev = hash
	}
	tip := hashes[len(hashes)-1]

	// The suffix: the tip and the block under it, which is what a bounded walk
	// gives a node whose state stands five blocks back.
	suffix, err := bs.CanonicalChain(tip, 2)
	require.NoError(t, err)
	require.Len(t, suffix, 2, "the tip and the block under it, and nothing before that")

	// The whole chain, which is what the replay needs to cover the timeslots the
	// node's state stands before.
	whole, err := bs.CanonicalChain(tip, 0)
	require.NoError(t, err)
	require.Greater(t, len(whole), len(suffix),
		"the full walk has to reach further back than the bounded one")

	// Every timeslot the node has to step has to be in the chain it was given,
	// or it is stepping on nothing.
	covered := make(map[jamtime.Timeslot]bool, len(whole))
	for _, b := range whole {
		covered[b.Header.TimeSlotIndex] = true
	}
	for i := 1; i <= 6; i++ {
		slot := slots[i-1]
		assert.True(t, covered[slot], "timeslot %d has to be in the chain given to the replay", slot)
	}

	// And the work a block names has to come back out of it, because that work is
	// the only description of the timeslot the block settles. A block whose
	// preimages cannot be read back is a block nobody can rebuild from.
	withWork := block.Block{
		Header: block.Header{
			ParentHash:     genesis,
			PriorStateRoot: crypto.Hash{0x01},
			TimeSlotIndex:  fin.TimeSlotIndex + 7,
		},
		Extrinsic: block.Extrinsic{EP: block.PreimageExtrinsic{
			{ServiceIndex: 0, Data: []byte("trabajo")},
			{ServiceIndex: 1, Data: []byte("mas trabajo")},
		}},
	}
	work := blockWork(withWork)
	require.Len(t, work, 2, "both preimages have to come out, in the order the block lists them")
	assert.Equal(t, devnet.BlockWork{ServiceID: 0, Payload: []byte("trabajo")}, work[0])
	assert.Equal(t, devnet.BlockWork{ServiceID: 1, Payload: []byte("mas trabajo")}, work[1])

}

// The block a timeslot settles has to be in the chain the node replays, keyed by
// the timeslot it names, because that is the key the replay looks it up by.
func TestChainByTimeslotCoversEverySlotBetweenTheParentAndTheTip(t *testing.T) {
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := chain.NewBlockService(db, 0)
	require.NoError(t, err)

	fin := bs.GetLatestFinalized()
	prev := fin.Hash
	for i := 1; i <= 4; i++ {
		h := block.Header{
			ParentHash:       prev,
			PriorStateRoot:   crypto.Hash{byte(i)},
			ExtrinsicHash:    crypto.Hash{0xaa},
			TimeSlotIndex:    fin.TimeSlotIndex + jamtime.Timeslot(i),
			BlockAuthorIndex: uint16(i % 2),
		}
		hash, herr := h.Hash()
		require.NoError(t, herr)
		require.NoError(t, bs.Store.PutBlock(block.Block{Header: h}))
		prev = hash
	}

	whole, err := bs.CanonicalChain(prev, 0)
	require.NoError(t, err)

	bySlot := make(map[jamtime.Timeslot]block.Block, len(whole))
	for _, b := range whole {
		bySlot[b.Header.TimeSlotIndex] = b
	}
	// The node's state stands at the first of these timeslots and it has to step
	// every one up to the tip, so every one of them has to be in the map.
	for i := 1; i <= 4; i++ {
		slot := fin.TimeSlotIndex + jamtime.Timeslot(i)
		_, ok := bySlot[slot]
		assert.True(t, ok, "timeslot %d is missing from the replay map", slot)
	}
}
