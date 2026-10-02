package main

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testRuntimeFor(t *testing.T) *devnet.Runtime {
	t.Helper()
	genesis, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)
	genesis.GenesisTimeslot = 9199500
	rt, err := devnet.New(devnet.Options{Genesis: genesis})
	require.NoError(t, err)
	return rt
}

// A timeslot with no block is a timeslot nobody has described, and the replay used
// to run it as if it had settled nothing at all.
//
// That is how two nodes ended up on the same block with two different states. A
// node that is behind executes everything that has arrived in one go, and the loop
// walked every timeslot up to the tip including the ones with no block. A node that
// had all the blocks ran those timeslots for real. Same chain, different states,
// and both then published the same block over each of theirs.
//
// What happened in a timeslot with no block cannot be guessed, so the replay stops
// there: it executes the part it can prove and waits for the rest.
func TestReplayStopsAtATimeslotWithNoBlock(t *testing.T) {
	const start = jamtime.Timeslot(9199501)

	// The author runs every timeslot. The root before each is what the block for
	// that timeslot was built on, and the root after is the state that timeslot
	// left behind.
	author := testRuntimeFor(t)
	blocks := map[jamtime.Timeslot]block.Block{}
	antesDe := map[jamtime.Timeslot]crypto.Hash{}
	despuesDe := map[jamtime.Timeslot]crypto.Hash{}
	for i := 0; i < 5; i++ {
		slot := start + jamtime.Timeslot(i)
		antesDe[slot] = author.Root()
		_, err := author.Run(slot)
		require.NoError(t, err, "timeslot %d", slot)
		despuesDe[slot] = author.Root()
		blocks[slot] = block.Block{Header: block.Header{
			ParentHash:       antesDe[slot],
			PriorStateRoot:   antesDe[slot],
			TimeSlotIndex:    slot,
			BlockAuthorIndex: uint16(i % 2),
		}}
	}
	through := start + jamtime.Timeslot(4)

	for _, hueco := range []jamtime.Timeslot{start + 2, start + 3} {
		t.Run(fmt.Sprintf("hueco-en-%d", uint64(hueco)), func(t *testing.T) {
			present := map[jamtime.Timeslot]block.Block{}
			for slot, b := range blocks {
				if slot == hueco {
					continue
				}
				present[slot] = b
			}

			follower := testRuntimeFor(t)
			bp := &blockProducer{runtime: follower}

			hechos, err := bp.replay(start, through, present)
			require.NoError(t, err, "a hole is a stop, not a failure")

			assert.Equal(t, uint64(hueco-start), hechos,
				"the replay must stop at the timeslot with no block, having run the %d before it",
				uint64(hueco-start))

			// The state it reached is the author's state at the timeslot before the
			// hole. Not the author's final state, and not the state it would have
			// reached by running the missing timeslot as an empty one.
			assert.Equal(t, despuesDe[hueco-1], follower.Root(),
				"stopping at a hole leaves the state the author had at that point")
		})
	}
}

// A stretch with no holes is run all the way through: stopping must not turn into
// a node that never catches up.
func TestReplayRunsAWholeUnbrokenStretch(t *testing.T) {
	const start = jamtime.Timeslot(9199601)

	author := testRuntimeFor(t)
	blocks := map[jamtime.Timeslot]block.Block{}
	for i := 0; i < 4; i++ {
		slot := start + jamtime.Timeslot(i)
		before := author.Root()
		_, err := author.Run(slot)
		require.NoError(t, err)
		blocks[slot] = block.Block{Header: block.Header{
			ParentHash:     before,
			PriorStateRoot: before,
			TimeSlotIndex:  slot,
		}}
	}
	through := start + jamtime.Timeslot(3)

	follower := testRuntimeFor(t)
	bp := &blockProducer{runtime: follower}

	hechos, err := bp.replay(start, through, blocks)
	require.NoError(t, err)
	assert.Equal(t, uint64(4), hechos, "an unbroken stretch is run whole")
	assert.Equal(t, author.Root(), follower.Root())
}
