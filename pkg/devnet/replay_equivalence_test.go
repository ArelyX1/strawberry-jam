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
