package devnet

import (
	"testing"

	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A node that adopts a chain somebody else wrote has to arrive at the state that
// chain describes, and the only way it can is by executing it exactly as the
// author did. This is the property the two-validator run depends on and it is not
// something the chain itself checks: the tip can be adopted perfectly while the
// state underneath it is a different one, and then every state request the node
// answers is wrong and nobody notices, because a node that has the right block
// numbers looks healthy.
//
// The two paths are genuinely different code. Producing runs a timeslot and
// settles whatever the timeslot produced. Rebuilding submits the work a block
// names and then runs the timeslot, and clears the numbers it had handed out
// when it is done. Both have to leave the same state behind.
func TestRebuildingAChainReproducesTheStateThatWroteIt(t *testing.T) {
	const slots = 8
	start := jamtime.Timeslot(9193000)

	// The author: run the chain and keep the work every timeslot settled.
	author := newTestRuntime(t)
	type step struct {
		work []BlockWork
		root crypto.Hash
	}
	steps := make([]step, 0, slots)
	for i := 0; i < slots; i++ {
		work, err := author.Run(start + jamtime.Timeslot(i))
		require.NoError(t, err, "the author must be able to run timeslot %d", i)
		steps = append(steps, step{work: work, root: author.Root()})
	}

	// The follower: same genesis, and the chain as the blocks described it. The
	// root each block was built on is the root before that timeslot, so those are
	// the roots to check the rebuild against.
	follower := newTestRuntime(t)
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, follower.Rebuild(s.work), "rebuild timeslot %d", slot)
		require.NoError(t, follower.Step(slot), "step timeslot %d", slot)
		assert.Equal(t, s.root, follower.Root(),
			"timeslot %d: the rebuilt state is not the state that was written", slot)
	}
	follower.FinishRebuild()

	assert.Equal(t, author.Root(), follower.Root(),
		"a node that adopted this chain would be answering state questions from a state the chain never described")
}

// Rebuilding a chain that this node itself wrote, which is what a restart does,
// has to land on the same state too. If these two disagree then a node that
// restarts is not on the chain it was on before it stopped, and every comparison
// between a running node and a restarted one is meaningless.
func TestRebuildAfterRewindMatchesRunning(t *testing.T) {
	const slots = 6
	start := jamtime.Timeslot(9194000)

	author := newTestRuntime(t)
	type step struct {
		work []BlockWork
		root crypto.Hash
	}
	steps := make([]step, 0, slots)
	for i := 0; i < slots; i++ {
		work, err := author.Run(start + jamtime.Timeslot(i))
		require.NoError(t, err)
		steps = append(steps, step{work: work, root: author.Root()})
	}

	// Rewind and rebuild is the path a node takes when it decides the chain it
	// was on is not the chain, so this is the same comparison as above reached
	// from a state that has already moved.
	follower := newTestRuntime(t)
	require.NoError(t, follower.Rewind())
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, follower.Rebuild(s.work))
		require.NoError(t, follower.Step(slot))
	}
	follower.FinishRebuild()

	assert.Equal(t, author.Root(), follower.Root(),
		"rewinding and replaying a chain has to give the state that ran it")
}

// This is the shape a two-validator run actually has, and it is not the shape
// the tests above cover. A node runs the timeslots it authors and rebuilds the
// ones another validator wrote, and it alternates between the two for the whole
// life of the chain. Each rebuild finishes by clearing the numbers the node had
// handed out, on the grounds that a rebuild is where a node starts over. That is
// true of a node resuming its own chain after a restart and false of a node that
// is running and adopts one block from a peer, and the two cases are the same
// function.
func TestInterleavingRunAndRebuildAgreesWithRunning(t *testing.T) {
	const slots = 6
	start := jamtime.Timeslot(9195000)

	// The author runs every timeslot and keeps what each one settled.
	author := newTestRuntime(t)
	type step struct {
		work []BlockWork
		root crypto.Hash
	}
	steps := make([]step, 0, slots)
	for i := 0; i < slots; i++ {
		work, err := author.Run(start + jamtime.Timeslot(i))
		require.NoError(t, err)
		steps = append(steps, step{work: work, root: author.Root()})
	}

	// The follower authors the even timeslots and has to rebuild the odd ones,
	// which is what a two-validator chain does on both nodes in turn.
	follower := newTestRuntime(t)
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		if i%2 == 0 {
			_, err := follower.Run(slot)
			require.NoError(t, err)
		} else {
			require.NoError(t, follower.Rebuild(s.work))
			require.NoError(t, follower.Step(slot))
			follower.FinishRebuild()
		}
		assert.Equal(t, s.root, follower.Root(),
			"timeslot %d: alternating between running and rebuilding gives a different state", slot)
	}
}

// And the same again on the node that has to adopt a chain it was not building,
// which is the node that comes back after being away.
func TestAdoptingOneBlockMidRunKeepsTheState(t *testing.T) {
	const slots = 6
	start := jamtime.Timeslot(9196000)

	author := newTestRuntime(t)
	type step struct {
		work []BlockWork
		root crypto.Hash
	}
	steps := make([]step, 0, slots)
	for i := 0; i < slots; i++ {
		work, err := author.Run(start + jamtime.Timeslot(i))
		require.NoError(t, err)
		steps = append(steps, step{work: work, root: author.Root()})
	}

	follower := newTestRuntime(t)
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		if i == 3 {
			// This is the whole of a peer's block arriving in a chain this node
			// is already running.
			require.NoError(t, follower.Rebuild(s.work))
			require.NoError(t, follower.Step(slot))
			follower.FinishRebuild()
		} else {
			_, err := follower.Run(slot)
			require.NoError(t, err)
		}
		assert.Equal(t, s.root, follower.Root(),
			"timeslot %d: adopting one block in the middle changed the state", slot)
	}
}
