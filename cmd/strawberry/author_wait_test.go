package main

import (
	"path/filepath"
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

func chainServiceFor(t *testing.T) *chain.BlockService {
	t.Helper()
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := chain.NewBlockService(db, 0)
	require.NoError(t, err)
	return bs
}

// putTip stores a block and makes it the only leaf, which is what a node that has
// just written it has.
func putTip(t *testing.T, bs *chain.BlockService, parent crypto.Hash, slot jamtime.Timeslot, author uint16) crypto.Hash {
	t.Helper()
	h := block.Header{
		ParentHash:       parent,
		PriorStateRoot:   parent,
		ExtrinsicHash:    crypto.Hash{0xaa},
		TimeSlotIndex:    slot,
		BlockAuthorIndex: author,
	}
	hash, err := h.Hash()
	require.NoError(t, err)
	require.NoError(t, bs.Store.PutHeader(h))
	bs.ResetLeaves()
	bs.AddLeaf(hash, slot)
	return hash
}

// An author is one timeslot behind by the time its turn comes. That is the shape of
// the schedule, not a fault: the timeslot before is written by the other validator
// while it runs, so this node cannot have written it and may not have received it
// yet.
//
// Asking "am I level with the best block I know of" cannot notice that, because the
// best block this node knows of is the one it wrote itself, which it has executed by
// definition. The two sides are always equal, the check always passes, and the node
// writes on top of its own last block.
//
// With two validators this is a deadlock, and the first symptom is a chain that
// stops with every node still at it. With a mesh it is worse: each author skips
// the timeslots of the others, so no node ever builds on another's block, and the
// one that waited honestly ends up too far behind for an announcement to reach it.
func TestAuthorWaitsForTheParentTimeslotItDidNotWrite(t *testing.T) {
	const genesis = jamtime.Timeslot(9199500)
	const mine = genesis + 4 // an even timeslot, this node's turn with two authors
	const parent = mine - 1

	genesisCfg, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)
	genesisCfg.GenesisTimeslot = genesis

	rt, err := devnet.New(devnet.Options{Genesis: genesisCfg})
	require.NoError(t, err)

	// This node wrote up to "mine - 2" and has not seen the block of the timeslot
	// before its turn, which is the moment every author is in.
	for slot := genesis; slot <= mine-2; slot++ {
		_, err := rt.Run(slot)
		require.NoError(t, err, "timeslot %d", slot)
	}

	bs := chainServiceFor(t)
	putTip(t, bs, crypto.Hash{0x01}, mine-2, 0)

	bp := &blockProducer{bs: bs, runtime: rt, authorIndex: 0, authorCount: 2}

	assert.False(t, bp.atTipFor(mine),
		"this node has not executed the timeslot it has to build on, so it must wait")

	// And the wait gives up rather than blocking for ever, so the timeslot can be
	// held and tried again instead of the node going quiet.
	assert.False(t, bp.waitUntilAtTip(mine),
		"waiting has to end even while the block it needs is still missing")

	// The other validator's block arrives and is executed. Now the parent is here.
	_, err = rt.Run(parent)
	require.NoError(t, err)
	putTip(t, bs, crypto.Hash{0x02}, parent, 1)

	assert.True(t, bp.atTipFor(mine),
		"with the parent timeslot executed, the author can write")
	assert.True(t, bp.waitUntilAtTip(mine),
		"and it no longer has to wait for it")
}

// The genesis has no timeslot before it, and a block that arrived from a peer
// before this node got its turn is already the tip. Demanding a parent timeslot in
// either case asks for something that cannot exist and the chain never starts.
func TestAtTipForAcceptsGenesisAndABlockThatAlreadyArrived(t *testing.T) {
	const genesis = jamtime.Timeslot(9199500)

	genesisCfg, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)
	genesisCfg.GenesisTimeslot = genesis
	rt, err := devnet.New(devnet.Options{Genesis: genesisCfg})
	require.NoError(t, err)

	bs := chainServiceFor(t)

	bp := &blockProducer{bs: bs, runtime: rt, authorIndex: 0, authorCount: 2}

	// Nothing produced anywhere yet: whoever's turn it is writes the first block.
	assert.True(t, bp.atTipFor(genesis), "the first block has no parent timeslot")

	_, err = rt.Run(genesis)
	require.NoError(t, err)
	putTip(t, bs, crypto.Hash{0x01}, genesis, 0)

	assert.True(t, bp.atTipFor(genesis),
		"the tip is already this timeslot, so there is nothing to wait for")

	// A block for this timeslot arriving from a peer is the same situation.
	assert.True(t, bp.atTipFor(genesis-1),
		"a timeslot behind the tip needs no parent wait either")
}

// A mesh that is starting up has a chain at the genesis and a clock already past
// it, and the timeslots in between have no block and no author who has written one.
//
// Skipping them is what left two nodes holding a timeslot and neither writing it:
// each waited for a block from before the present that the other had been told to
// skip, so nothing was ever written again and the chain stood still from the
// genesis. The skip is only sound once the chain has actually reached the present.
func TestCannotSkipTimeslotsNobodyHasWritten(t *testing.T) {
	const genesis = jamtime.Timeslot(9199500)

	// Fresh mesh: the chain is at the genesis and the clock has moved on. The
	// timeslots between are unclaimed, so the node has to start at the tip and
	// write them with its peers.
	assert.False(t, canCarryOnFromThePresent(genesis, genesis+5),
		"a chain that does not reach the present has to be written up to it")

	// Node that was away: its peers wrote the timeslots it slept through, so it
	// does not write them a second time.
	assert.True(t, canCarryOnFromThePresent(genesis+5, genesis+5),
		"the tip is the present, so there is nothing to skip")

	// One timeslot of slack, for a node whose tip is the timeslot before the
	// present and is about to author the present itself.
	assert.True(t, canCarryOnFromThePresent(genesis+5, genesis+6),
		"the author of the present can start there")

	// And still not when the clock is ahead of the tip by two.
	assert.False(t, canCarryOnFromThePresent(genesis+5, genesis+7),
		"a tip more than one timeslot behind the present means unwritten timeslots")
}
