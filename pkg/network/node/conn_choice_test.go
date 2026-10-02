package node

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Two nodes that both dial each other end up with two connections to the same
// peer, and the one they drop has to be the same one at both ends. Comparing by
// arrival order cannot do that, because the two connections turn up in opposite
// order at the two nodes: each keeps the one it dialled itself, having just
// closed the one the other is holding. The ports are the only thing both nodes
// hold the same numbers for.
func TestConnectionChoiceIsTheSameWhicheverOrderTheyArriveIn(t *testing.T) {
	// Each node sees a connection it dialled with its own ephemeral port first,
	// and the same connection at the other end with those two ports swapped. That
	// swap is the whole difficulty: compared as they come, the two nodes are
	// comparing different numbers about the same two connections.
	//
	// A dialled B, so A sees (51000, 30333) and B sees (30333, 51000).
	aDialAtA := [2]int{51000, 30333}
	aDialAtB := [2]int{30333, 51000}
	// B dialled A, so B sees (52000, 30333) and A sees (30333, 52000).
	bDialAtB := [2]int{52000, 30333}
	bDialAtA := [2]int{30333, 52000}

	// A holds aDialAtA and is offered bDialAtA. B holds bDialAtB and is offered
	// aDialAtB. Same two connections, and each node sees them in its own order.
	aPrefersIncoming := preferPorts(bDialAtA[0], bDialAtA[1], aDialAtA[0], aDialAtA[1])
	bPrefersIncoming := preferPorts(aDialAtB[0], aDialAtB[1], bDialAtB[0], bDialAtB[1])

	assert.NotEqual(t, aPrefersIncoming, bPrefersIncoming,
		"the two nodes must not disagree about which connection to drop, or each "+
			"ends up holding the one the other just closed")
}

// Both nodes have to end up on the same one, whichever they saw first, so the
// winner has to be the same connection when the question is asked from either end.
func TestConnectionChoiceNamesTheSameWinnerFromBothEnds(t *testing.T) {
	// The same two connections as the other test, seen from each end.
	firstAtA := [2]int{51000, 30333}
	firstAtB := [2]int{30333, 51000}
	secondAtA := [2]int{30333, 52000}
	secondAtB := [2]int{52000, 30333}

	// preferPorts answers "should the incoming one replace the one held", so
	// asking it from each end has to name the same connection as the winner.
	firstWinsFromThisEnd := preferPorts(firstAtA[0], firstAtA[1], secondAtA[0], secondAtA[1])
	firstWinsFromTheOtherEnd := !preferPorts(secondAtB[0], secondAtB[1], firstAtB[0], firstAtB[1])
	assert.True(t, firstWinsFromThisEnd, "the smaller pair is the one to keep")
	assert.True(t, firstWinsFromTheOtherEnd,
		"the two ends have to name the same connection as the one to keep")
}

// The answer cannot depend on which order the two connections were seen in.
func TestConnectionChoiceIsSymmetricInItsArguments(t *testing.T) {
	small := [2]int{51000, 30333}
	large := [2]int{52000, 30334}

	assert.True(t, preferPorts(small[0], small[1], large[0], large[1]),
		"the smaller port wins")
	assert.False(t, preferPorts(large[0], large[1], small[0], small[1]),
		"and asking the other way round gives the other answer, as it must")

	// The same two connections with the ports the other end sees them with: the
	// answer has to be the same connection either way.
	assert.True(t, preferPorts(small[1], small[0], large[1], large[0]),
		"swapping which port is local must not change which connection wins")
	assert.False(t, preferPorts(large[1], large[0], small[1], small[0]))
}

// The remote port only matters when the local ones are equal, which two live
// connections to the same peer cannot be. It still has to be a decision.
func TestConnectionChoiceFallsBackToTheRemotePort(t *testing.T) {
	assert.True(t, preferPorts(51000, 30333, 51000, 30334))
	assert.False(t, preferPorts(51000, 30334, 51000, 30333))
	// And the same two, seen from the other end.
	assert.True(t, preferPorts(30333, 51000, 30334, 51000))
	assert.False(t, preferPorts(51000, 30333, 51000, 30333),
		"the same connection against itself is not a reason to replace it")
}
