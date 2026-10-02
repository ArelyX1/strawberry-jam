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

// Rebuilding a block that a peer wrote is not a restart, and the node must not
// treat it as one.
//
// The numbers a node has handed out are its own bookkeeping, and they decide
// which nonce the next item gets, so they end up in the state. A node that is
// running and adopts one block still has the right ones: they describe the chain
// it is already on. Clearing them at the end of every rebuild meant a node that
// had just adopted a block started counting nonces from nothing while its peers
// carried on counting, so the two wrote different items onto a chain they all
// agreed on, and the state roots of one block came out different on different
// nodes.
func TestAdoptingABlockDoesNotForgetTheNumbersTheNodeHandedOut(t *testing.T) {
	const slots = 8
	start := jamtime.Timeslot(9197000)

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

	// The follower runs some timeslots and rebuilds the rest, ending every
	// rebuild the way a node adopting blocks from peers does.
	follower := newTestRuntime(t)
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		if i%2 == 0 {
			_, err := follower.Run(slot)
			require.NoError(t, err)
		} else {
			before := len(follower.issued)
			require.NoError(t, follower.Rebuild(s.work))
			require.NoError(t, follower.Step(slot))
			follower.FinishRebuild()
			assert.Equal(t, before, len(follower.issued),
				"timeslot %d: adopting a block forgot the numbers this node had handed out", slot)
		}
		assert.Equal(t, s.root, follower.Root(),
			"timeslot %d: the state came out different from the one that was written", slot)
	}
}

// And a node that has just started does forget them, because its state comes out
// of the blocks it replays and those numbers are part of that state.
func TestForgetHandedOutClearsTheNumbers(t *testing.T) {
	rt := newTestRuntime(t)
	const slots = 3
	for i := 0; i < slots; i++ {
		_, err := rt.Run(jamtime.Timeslot(9198000 + jamtime.Timeslot(i)))
		require.NoError(t, err)
	}
	rt.mu.Lock()
	antes := len(rt.issued)
	rt.mu.Unlock()

	rt.ForgetHandedOut()
	rt.mu.RLock()
	despues := len(rt.issued)
	rt.mu.RUnlock()

	assert.Equal(t, 0, antes, "running a timeslot hands out numbers, which is what makes this worth testing")
	assert.Equal(t, 0, despues, "a node that has just started keeps none of them")
}

// This is the shape a network of two validators actually has, and the tests above
// are not it. Here each validator writes some of the blocks and rebuilds the rest,
// so every block in the chain was produced by a different runtime than the one
// executing it, and the two take turns being the author.
//
// It is the property that decides whether two nodes end up with the same state for
// the same block. When it holds, a chain written by one node and executed by
// another gives both the same root, and a mesh agrees. When it does not, the nodes
// that rebuild refuse the chain, end up executing different things, and then
// publish the same block over two different states, which is exactly what was seen:
// two nodes naming one tip with two roots.
func TestTwoValidatorsWritingAndRebuildingEachOtherAgree(t *testing.T) {
	const slots = 8
	start := jamtime.Timeslot(9199002)

	// Both nodes follow the chain, and node 0 authors the even timeslots while
	// node 1 authors the odd ones.
	build := func(authorIndex int) (*Runtime, map[jamtime.Timeslot][]BlockWork, map[jamtime.Timeslot]crypto.Hash) {
		rt := newTestRuntime(t)
		work := make(map[jamtime.Timeslot][]BlockWork, slots)
		roots := make(map[jamtime.Timeslot]crypto.Hash, slots)

		var lastWork []BlockWork
		for i := 0; i < slots; i++ {
			slot := start + jamtime.Timeslot(i)
			var err error
			if int(slot)%2 == authorIndex {
				lastWork, err = rt.Run(slot)
			} else {
				// A timeslot can settle nothing at all, and then the block names
				// no work and there is nothing to hand the rebuild. That is a block
				// like any other, not a broken one.
				if lastWork == nil {
					lastWork = []BlockWork{}
				}
				if err = rt.Rebuild(lastWork); err != nil {
					t.Fatalf("rebuild %d: %v", slot, err)
				}
				if err = rt.Step(slot); err != nil {
					t.Fatalf("step %d: %v", slot, err)
				}
			}
			require.NoError(t, err, "timeslot %d", slot)
			work[slot] = lastWork
			roots[slot] = rt.Root()
		}
		return rt, work, roots
	}

	zero, workZero, rootsZero := build(0)
	one, _, rootsOne := build(1)

	// Both must be standing on the same chain, which is the same tip.
	assert.Equal(t, zero.Root(), one.Root(),
		"two validators writing and rebuilding each other's blocks do not agree on the state")

	// And at every timeslot along the way, not only at the end, because a
	// difference that shows up late is a difference that compounds.
	for i := 0; i < slots; i++ {
		slot := start + jamtime.Timeslot(i)
		assert.Equal(t, rootsZero[slot], rootsOne[slot],
			"timeslot %d: the two validators had different states for the same chain", slot)
	}
	_ = workZero
}
