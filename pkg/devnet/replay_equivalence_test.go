package devnet

import (
	"crypto/ed25519"
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

// A node that has been running and then rebuilds the chain from the start has to
// land on the state that wrote it, and it does not follow from the other tests
// here that it does. Those start a node and replay it, so the node has no history
// to forget. This one has been running first, which is the ordinary case: a node
// follows the chain for a while, is asked to rebuild it, and has to arrive back
// where it started.
//
// It could not, and it failed in the way a chain stops rather than in a way that
// looks like a bug. Which service gets which core when two of them want one is
// decided by a rotation in the scheduler that turns once per timeslot the node
// runs. A node that has been running has turned it a number of times that has
// nothing to do with where it is on the chain, so it replayed the chain over a
// rotation a node that had only replayed would not have had. Every service still
// ran, in the same timeslots, but in a different order among themselves, so the
// state settled differently, so the rebuild reached a root no block names and the
// node refused to build on it. That is the whole chain stopping, on every node,
// over an ordering that is nobody's fault individually.
func TestARunningNodeThatRebuildsAgreesWithTheChainItWasOn(t *testing.T) {
	const slots = 10
	start := jamtime.Timeslot(9184000)

	// The chain, as the author ran it.
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

	// The node that has been running: it followed the same chain, so it is on the
	// same state, and its scheduler's rotation is wherever running it left it.
	runner := newTestRuntime(t)
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, runner.Rebuild(s.work), "rebuild timeslot %d", slot)
		require.NoError(t, runner.Step(slot), "step timeslot %d", slot)
	}
	runner.FinishRebuild()
	require.Equal(t, author.Root(), runner.Root(),
		"the node that followed the chain is on the chain's state to begin with")

	// Now it is asked to adopt a chain, which is what a node does when a peer has
	// written a stretch it has not: rewind to the start of the chain and execute the
	// peer's blocks over that. Both halves matter. The rewind puts the state back;
	// forgetting what this node was doing puts back the bookkeeping that is its own
	// rather than the chain's, which includes the rotation of which service gets
	// which core.
	require.NoError(t, runner.Rewind(), "adopting a chain rewinds to the start of it")
	runner.ForgetHandedOut()

	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, runner.Rebuild(s.work), "rebuild timeslot %d", slot)
		require.NoError(t, runner.Step(slot), "step timeslot %d", slot)
		assert.Equal(t, s.root, runner.Root(),
			"timeslot %d: a node that had been running rebuilt the chain to a state it was not on", slot)
	}
}

// The same again, but with a payout in the chain, which is what put it there in
// the first place.
//
// A payout is queued by a caller, on one node, and it settles in whichever
// timeslot that node is authoring. Every other node learns about it from the
// block, not from the caller: nobody asked them for it, so the only copy of it
// anywhere is in the block. A node that queued one itself and is then handed a
// chain that already settled it is holding that payout in its queue and in the
// chain, and if the queue is not emptied on the way back to the start of the
// chain it settles the same payout twice, or once before the timeslot that names
// it, and the state it arrives at is not the state the blocks describe.
//
// It stops the chain the same way every time: the node rebuilds to a root no
// block names, the check against the block fails, and the node refuses to adopt
// a chain that was perfectly good. And every node that had queued anything does
// it, so the mesh sits on the last block all of them agree on.
func TestAdoptingAChainThatSettledAPayoutThisNodeHadQueued(t *testing.T) {
	const slots = 8
	start := jamtime.Timeslot(9175000)

	author := newRuntimeWithBridge(t)
	type step struct {
		work []BlockWork
		root crypto.Hash
	}
	steps := make([]step, 0, slots)

	// The payout goes into the chain on the node that heard about it, and settles
	// in the first timeslot that runs after it was queued.
	if _, err := author.Faucet(addressOf(t, 5)); err != nil {
		t.Fatalf("queue the payout: %v", err)
	}
	for i := 0; i < slots; i++ {
		work, err := author.Run(start + jamtime.Timeslot(i))
		require.NoError(t, err, "the author must be able to run timeslot %d", i)
		steps = append(steps, step{work: work, root: author.Root()})
	}

	// The other node was asked for the same payout. It queued it too, in its own
	// queue, and it is about to be told about a chain that has already settled it.
	adopter := newRuntimeWithBridge(t)
	if _, err := adopter.Faucet(addressOf(t, 5)); err != nil {
		t.Fatalf("queue the payout on the adopting node: %v", err)
	}

	// Adopting a chain rewinds to the start of it and executes the peer's blocks
	// over that.
	require.NoError(t, adopter.Rewind(), "rewind to the start of the chain")
	adopter.ForgetHandedOut()

	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, adopter.Rebuild(s.work), "rebuild timeslot %d", slot)
		require.NoError(t, adopter.Step(slot), "step timeslot %d", slot)
		assert.Equal(t, s.root, adopter.Root(),
			"timeslot %d: the node that had queued the payout rebuilt the chain to a state it was not on", slot)
	}
}

// newRuntimeWithBridge is [newTestRuntime] with a bridge key, so that the node
// can be asked to pay a payout out of its own account the way it is in a chain
// that has one.
func newRuntimeWithBridge(t *testing.T) *Runtime {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	rt, err := New(Options{Genesis: testGenesis(t), BridgeKey: ed25519.NewKeyFromSeed(seed)})
	require.NoError(t, err)
	return rt
}

// The same payout, but the second node is not rewound: it has been following the
// chain and is asked for one more timeslot, which is what executing a peer's
// blocks on top of where this node already is actually is.
//
// This is the ordinary way a node takes on work it did not produce, and it is the
// one that does not go back to the start of the chain first, so whatever the node
// had queued of its own is still queued when the block's work arrives. The node
// was handed the same payout twice over: once by the caller that asked it for one,
// and once by the block that already settled one. Two copies of a payout that is
// once per address is one too many, and the state it settles is not the state the
// block names.
//
// The chain stops on it in the way this always stops: the timeslot after it waits
// for a block nobody writes, because the node that would write it cannot get its
// state to agree with the blocks, and the node that agrees with the blocks is not
// its author.
func TestExecutingOnTopWithoutARewindDropsNothingAndDoublesNothing(t *testing.T) {
	const slots = 6
	start := jamtime.Timeslot(9166000)

	author := newRuntimeWithBridge(t)
	type step struct {
		work []BlockWork
		root crypto.Hash
	}
	steps := make([]step, 0, slots)

	if _, err := author.Faucet(addressOf(t, 5)); err != nil {
		t.Fatalf("queue the payout: %v", err)
	}
	for i := 0; i < slots; i++ {
		work, err := author.Run(start + jamtime.Timeslot(i))
		require.NoError(t, err, "the author must be able to run timeslot %d", i)
		steps = append(steps, step{work: work, root: author.Root()})
	}

	// The other node was asked for the same payout, and queued it in its own
	// queue. It is now handed the chain that already settled one.
	other := newRuntimeWithBridge(t)
	if _, err := other.Faucet(addressOf(t, 5)); err != nil {
		t.Fatalf("queue the payout on the other node: %v", err)
	}
	// And it follows the chain up to the timeslot before the last, which is where
	// a node following the chain would be. No rewind: this is not a restart, it is
	// a node that has been running and has been given one more timeslot.
	for i := 0; i < slots-1; i++ {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, other.Rebuild(steps[i].work), "rebuild timeslot %d", slot)
		require.NoError(t, other.Step(slot), "step timeslot %d", slot)
		require.Equal(t, steps[i].root, other.Root(), "timeslot %d", slot)
	}

	// Now the timeslot that settles the payout, executed on top of where it is.
	last := start + jamtime.Timeslot(slots-1)
	require.NoError(t, other.Rebuild(steps[slots-1].work), "rebuild timeslot %d", last)
	require.NoError(t, other.Step(last), "step timeslot %d", last)
	assert.Equal(t, steps[slots-1].root, other.Root(),
		"timeslot %d: the node settled the payout from the block on top of the copy it had queued itself",
		last)
}

// A node with work of its own queued, executing a block somebody else wrote.
//
// This is what a node does whenever it is not the author of a timeslot, which is
// most of them. The block says what its timeslot ran. Anything else in the queue
// belongs to this node's own memory, was queued for a state that is not the one
// the block is about, and is not in any block.
//
// Executing the timeslot with both in the queue settled the node's own work there
// too, in a timeslot that does not name it. Its state was then the chain's state
// plus one transfer, so it wrote the next block on a root no peer could rebuild,
// every peer refused to adopt it, and the chain stopped with all of them healthy
// and all of them connected. The tell was a node reporting a balance its peers did
// not have, on a chain where the block carrying the payout had never been executed
// by anybody but the node that asked for it.
//
// The payout is queued by a caller and has not been in a block yet. Following a
// chain that has none of it is normal — the author of that chain was somebody else
// — and it has to leave the state at what the blocks say.
func TestFollowingAPeerChainDropsWorkThisNodeHadQueued(t *testing.T) {
	const slots = 6
	start := jamtime.Timeslot(9144000)

	// The chain, written by a node that was never asked to pay anything.
	author := newRuntimeWithBridge(t)
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

	// This node was asked to pay somebody, and queued it. Nobody has written it
	// into a block, because this node is not the one authoring these timeslots.
	follower := newRuntimeWithBridge(t)
	if _, err := follower.Faucet(addressOf(t, 5)); err != nil {
		t.Fatalf("queue the payout: %v", err)
	}
	require.Positive(t, pendingOf(t, follower), "the payout is queued and not yet in a block")

	// Now it follows the chain somebody else wrote.
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, follower.Rebuild(s.work), "rebuild timeslot %d", slot)
		require.NoError(t, follower.Step(slot), "step timeslot %d", slot)
		require.Equal(t, s.root, follower.Root(),
			"timeslot %d: the node settled work of its own on somebody else's chain, so its state is not the chain's state",
			slot)
	}
}

// pendingOf reports how much work a node has queued, for saying out loud that
// there was something to lose.
func pendingOf(t *testing.T, rt *Runtime) int {
	t.Helper()
	queued, _ := rt.Pending()
	return queued
}

// Work a node was asked to do is held while it follows a chain somebody else
// wrote, and settled in the next timeslot it authors.
//
// Holding it and dropping it are both defensible for the chain — the state has to
// be what the blocks say either way — and only one of them is defensible for the
// caller. A node that dropped it would lose a transfer for no reason other than
// which node the caller happened to ask, and the same request would have worked on
// the node that was about to author. So the answer has to be the same everywhere.
//
// This node follows a chain it did not write, with a payout of its own queued, and
// then it writes a timeslot itself. The payout settles there, in a block that names
// it, and every node that rebuilds that block arrives at the same balance.
func TestHeldWorkSettlesInTheNextTimeslotThisNodeAuthors(t *testing.T) {
	start := jamtime.Timeslot(9133000)

	// Somebody else's chain, with nothing in it.
	author := newRuntimeWithBridge(t)
	type step struct {
		work []BlockWork
		root crypto.Hash
	}
	var steps []step
	for i := 0; i < 4; i++ {
		work, err := author.Run(start + jamtime.Timeslot(i))
		require.NoError(t, err)
		steps = append(steps, step{work: work, root: author.Root()})
	}

	// This node is asked to pay somebody, and then told to follow that chain.
	node := newRuntimeWithBridge(t)
	if _, err := node.Faucet(addressOf(t, 5)); err != nil {
		t.Fatalf("queue the payout: %v", err)
	}
	for i, s := range steps {
		slot := start + jamtime.Timeslot(i)
		require.NoError(t, node.Rebuild(s.work), "rebuild timeslot %d", slot)
		require.NoError(t, node.Step(slot), "step timeslot %d", slot)
		require.Equal(t, s.root, node.Root(), "timeslot %d: following the chain", slot)
	}

	// Following the chain is over, and the node is a running node again. The real
	// caller of this path ends the rebuild the same way.
	node.FinishRebuild()

	// Now it writes a timeslot of its own. The payout has to go in that block,
	// because a block can only name work its author ran.
	own, err := node.Run(start + 4)
	require.NoError(t, err)
	require.Len(t, own, 1, "the timeslot this node authored carries the work that was waiting for it")

	// And a node that rebuilds that block lands where this one is.
	other := newRuntimeWithBridge(t)
	require.NoError(t, other.Rebuild(own), "rebuild the timeslot this node authored")
	require.NoError(t, other.Step(start+4), "step it")
	assert.Equal(t, node.Root(), other.Root(),
		"the timeslot this node authored is not the block the rest of the mesh rebuilds")
}
