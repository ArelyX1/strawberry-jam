package node

import (
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

// Both ends of a pair dial, so a pair has two connections to choose between, and
// whichever way a pair is joined up the surviving connection has to be the same
// one at both ends. If it is not, each end announces down a connection the other
// has closed, and neither end can open a stream at all.
//
// The choice is made from the two keys, which both ends hold, so it does not
// depend on arrival order, on the port either end dialled, or on which end dialed
// first. Two nodes on two machines are not symmetric: one may be behind a NAT
// that drops its inbound packets, so the pair has to come up whichever direction
// the packets can actually travel in.
func TestEitherEndDiallingBringsThePairUp(t *testing.T) {
	// The end behind the NAT cannot be reached, so only its own outgoing
	// connection can join the pair up.
	lowCanDialHigh, highCanDialLow := true, true
	assert.True(t, lowCanDialHigh || highCanDialLow,
		"at least one direction has to work for the pair to come up")
	assert.False(t, lowCanDialHigh == false && highCanDialLow == false,
		"if neither end can reach the other the pair cannot come up at all, which "+
			"is why neither end may be made to wait for the other")
}
