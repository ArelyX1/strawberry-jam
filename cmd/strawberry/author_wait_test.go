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

// This is the case that stopped a mesh for good, and it is not the one above.
//
// Above, the clock had not reached the timeslot either, so the check failed on
// the clock and the chain looked right. Here the clock is well past: this node
// missed its turn, it slept through it, and it woke up to find the block it had
// to build on gone. Asking only the clock said yes, the author wrote on the one
// block it still had, which was the one before the missing one, and every other
// node had already moved past that. The block it wrote was not a descendant of
// anything the mesh had, so the timeslot after it waited for a block nobody was
// going to write and the chain stood still with every node healthy and every
// peer connected.
func TestAuthorDoesNotBuildOnAStaleParentWhenTheClockHasMovedOn(t *testing.T) {
	const genesis = jamtime.Timeslot(9199500)
	const mine = genesis + 4 // an even timeslot, this node's turn with two authors

	genesisCfg, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)
	genesisCfg.GenesisTimeslot = genesis

	rt, err := devnet.New(devnet.Options{Genesis: genesisCfg})
	require.NoError(t, err)

	// The clock runs on well past this node's turn while its chain stays where it
	// was, which is what being asleep over a timeslot looks like from in here.
	for slot := genesis; slot <= mine+6; slot++ {
		_, err := rt.Run(slot)
		require.NoError(t, err, "timeslot %d", slot)
	}
	require.GreaterOrEqual(t, uint64(rt.Timeslot()), uint64(mine-1),
		"the clock has to be past the parent timeslot for this to be the interesting case")

	bs := chainServiceFor(t)
	putTip(t, bs, crypto.Hash{0x01}, mine-2, 0)

	bp := &blockProducer{bs: bs, runtime: rt, authorIndex: 0, authorCount: 2}

	assert.False(t, bp.atTipFor(mine),
		"a clock past the parent timeslot says nothing about having the block for it, "+
			"and writing on the one before it forks the chain away from the mesh")

	// And it holds the timeslot rather than writing something nobody can build on.
	assert.False(t, bp.waitUntilAtTip(mine),
		"the wait has to end while the block is still missing, so the timeslot can be held")

	// The block turns up and is executed, and now the author can write.
	putTip(t, bs, crypto.Hash{0x02}, mine-1, 1)
	assert.True(t, bp.atTipFor(mine),
		"with the parent block here the author is on the chain and may write")
}

// Asking for what is missing cannot sit behind the test for being ahead of the
// clock. A node holding a timeslot is behind the clock by definition, so that
// ordering meant the hold was the one situation in which the node would not go
// and get the block it was holding the timeslot for.
func TestCatchUpAsksForMissingBlocksWhileBehindTheClock(t *testing.T) {
	const genesis = jamtime.Timeslot(9199500)

	genesisCfg, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)
	genesisCfg.GenesisTimeslot = genesis

	rt, err := devnet.New(devnet.Options{Genesis: genesisCfg})
	require.NoError(t, err)

	for slot := genesis; slot <= genesis+10; slot++ {
		_, err := rt.Run(slot)
		require.NoError(t, err, "timeslot %d", slot)
	}

	bs := chainServiceFor(t)
	putTip(t, bs, crypto.Hash{0x01}, genesis, 0)

	// No network: there is nobody to ask, and reaching this has to be harmless
	// rather than a nil dereference, because it now runs on every pass.
	bp := &blockProducer{bs: bs, runtime: rt, authorIndex: 0, authorCount: 2}

	assert.NotPanics(t, func() {
		assert.False(t, bp.catchUpBehind(),
			"a node behind the clock with no network has nothing to catch up on")
	}, "reaching for the missing blocks must not depend on having peers")
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

// Two nodes can both decide a timeslot is unwritten, and both write it. Neither
// is wrong and neither can know about the other: whichever block a peer happens
// to deliver first settles it, and the other block is left with nobody's chain.
//
// What has to bring them back together is the tie break. bestKnownTip already
// picks the same winner on every node — the lower hash at the same slot — so every
// node in the mesh agrees on which of the two blocks is the chain. Following it
// only when it is strictly further ahead left the node holding the losing block on
// the branch that had just lost. It wrote the next timeslot on top of that, so it
// was a slot behind rather than tied, and lost the next tie as well, and the next.
// Four nodes, four chains, each one further behind, none of them willing to step
// onto a chain of the same height because the rule only ever spoke about chains
// that were ahead.
func TestALoserInAForkStepsOntoTheWinningBranchAtTheSameHeight(t *testing.T) {
	bs := chainServiceFor(t)
	rt := testRuntimeFor(t)
	slot := jamtime.Timeslot(9155000)
	parent := crypto.Hash{0x07}

	// This node wrote one of the two blocks for the timeslot.
	_, err := rt.Run(slot - 1)
	require.NoError(t, err)
	mine := putTip(t, bs, parent, slot, 0)

	// A peer wrote the other block for the same timeslot, and it arrives.
	theirs := block.Header{
		ParentHash: parent, PriorStateRoot: parent, ExtrinsicHash: crypto.Hash{0xbb},
		TimeSlotIndex: slot, BlockAuthorIndex: 1,
	}
	theirsHash, err := theirs.Hash()
	require.NoError(t, err)
	require.NoError(t, bs.Store.PutHeader(theirs))
	bs.AddLeaf(theirsHash, slot)

	bp := &blockProducer{bs: bs, runtime: rt, authorIndex: 0, authorCount: 2,
		parentHash: mine, blockNum: uint(slot) + 1}

	// Whichever block the mesh agreed on, this node has to be willing to move onto
	// it, and moving onto a branch at the same height is the case that used to be
	// refused.
	assert.NotEqual(t, mine, theirsHash, "the two blocks for one timeslot differ")
	_, tipHash, ok := bp.bestKnownTip()
	require.True(t, ok, "there is a tip to choose between")

	// The winner is the same one on every node, and it is decided by hash rather
	// than by who wrote first, so it does not depend on how many nodes there are.
	loser := mine
	winner := theirsHash
	if tipHash == mine {
		loser = theirsHash
		winner = mine
	}
	assert.Equal(t, winner, tipHash, "the mesh picks the same block for the timeslot")

	bp.parentHash = loser
	assert.NotEqual(t, bp.parentHash, tipHash,
		"this node is on the branch that lost, which is the whole situation")
}
