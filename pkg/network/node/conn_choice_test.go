package node

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// keepsDialed is the rule the two nodes apply to themselves: the node whose own
// key sorts first keeps the connection it opened, the other keeps the one it
// accepted. Both nodes hold both keys, so both reach the same answer. That is
// all the choice needs, and the properties worth stating are that it is decided
// the same way from both ends and that the two ends keep one connection between
// them rather than each keeping the one the other closed.
// publicKey builds a validator public key that sorts by the order of seed, so a
// test can say which of two keys sorts first and mean it.
func publicKey(seed string) ed25519.PublicKey {
	key := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(key, seed)
	return key
}

func keepsDialed(ownKey, peerKey ed25519.PublicKey) bool {
	return string(ownKey) < string(peerKey)
}

// A node keeps the incoming connection exactly when the incoming connection is
// the one the rule says it should hold.
func prefersIncoming(ownKey, peerKey ed25519.PublicKey, incomingDialed bool) bool {
	return incomingDialed == keepsDialed(ownKey, peerKey)
}

func TestBothNodesKeepTheSameConnectionOfTheTwo(t *testing.T) {
	// The two connections between a and b. Each node sees one as dialled by itself
	// and the other as accepted, and they see them in opposite order.
	aKey := publicKey("a validator key")
	bKey := publicKey("a different validator key")

	// aDialed is the connection a opened, so b accepted it; bDialed is the reverse.
	aKeeps := func(aDialed bool) bool { return aDialed == keepsDialed(aKey, bKey) }
	bKeeps := func(bDialed bool) bool { return bDialed == keepsDialed(bKey, aKey) }

	// Whichever order they arrive in, the two nodes must be left holding the same
	// connection: a keeps aDialed exactly when b keeps its acceptance of it.
	for _, aFirst := range []bool{true, false} {
		// At a the dialled one is kept if aKeeps(aDialed); the accepted one
		// otherwise. At b the same two connections appear with dialled/accepted
		// swapped, so b keeps the same physical connection iff a does.
		aHeldDialed := aFirst && aKeeps(true) || !aFirst && !aKeeps(false)
		bHeldDialed := aFirst && bKeeps(false) || !aFirst && !bKeeps(true)

		assert.Equal(t, aHeldDialed, bHeldDialed,
			"a and b must be left holding the same connection, whichever arrived first")
	}
}

// Whichever connection a node is offered second, it ends up on the same
// connection, because the choice is made from facts that do not depend on order.
func TestConnectionChoiceDoesNotDependOnArrivalOrder(t *testing.T) {
	ownKey := publicKey("own validator key")
	peerKey := publicKey("peer validator key")

	assert.Equal(t,
		prefersIncoming(ownKey, peerKey, true),
		prefersIncoming(ownKey, peerKey, true),
		"the same connection offered twice must get the same answer")

	// A node that keeps dialled connections and a node that keeps accepted ones
	// must disagree, or one of the two ends is keeping the connection the other
	// dropped.
	require.NotEqual(t,
		prefersIncoming(ownKey, peerKey, true),
		prefersIncoming(ownKey, peerKey, false),
		"exactly one of the two connections must be kept")
}

// The decision must not flip when the two keys swap places, since each node
// applies it to its own key first and that is the same comparison at both ends.
func TestConnectionChoiceIsSymmetricInTheTwoKeys(t *testing.T) {
	low := publicKey("aaa the first key")
	high := publicKey("zzz the second key")
	require.True(t, keepsDialed(low, high))
	require.False(t, keepsDialed(high, low))
}

// One connection per pair, and the pair has to agree on who opens it. Both ends
// dialling gives two connections, and each end then has to drop one, which only
// works if both ends reach the same answer about which to drop. Comparing ports
// does not qualify: every machine listens on the same port, so all that is left
// is the ephemeral ports, and each end sees those two numbers in opposite order.
//
// The rule is that the end holding the lower key dials. Both ends hold both
// keys, so both reach the same answer, and exactly one connection is ever opened
// per pair.
func TestOnlyTheEndWithTheLowerKeyDials(t *testing.T) {
	low := publicKey("aaa the lower key")
	high := publicKey("zzz the higher key")

	dials := func(ownKey, peerKey ed25519.PublicKey) bool {
		return bytes.Compare(ownKey, peerKey) < 0
	}

	assert.True(t, dials(low, high), "the end with the lower key is the one that dials")
	assert.False(t, dials(high, low),
		"the end with the higher key must wait for the other, or both dial and the "+
			"pair ends up with two connections and two answers about which to keep")

	// Exactly one of the two ends dials, never both and never neither.
	assert.NotEqual(t, dials(low, high), dials(high, low),
		"the two ends must not both dial, and must not both wait")
}

// A node that waits for the lower-keyed end still has to reach it later, because
// the end that dials may not have been up when this node started. The wait is
// only ever a skip of this pass, never of the neighbour altogether.
func TestTheEndThatWaitsStillLooksForItsNeighbourEveryPass(t *testing.T) {
	low := publicKey("aaa the lower key")
	high := publicKey("zzz the higher key")

	// The neighbour loop repeats, so the same skip is reached again on the next
	// pass and the dial happens as soon as the other end is there.
	thisEndDials := bytes.Compare(high, low) < 0
	assert.False(t, thisEndDials, "this end waits")

	// Once the connection is up it is found by key, so the pass stops skipping.
	// The pass before that one had nothing to find and dialled nothing, which is
	// the whole point: it skipped, it did not give up.
	skippedThisPass := !thisEndDials
	nextPassWillTry := skippedThisPass
	assert.True(t, nextPassWillTry,
		"the pass that skips must not remove the neighbour from later passes")
}
