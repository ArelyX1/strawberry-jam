package peer_test

import (
	"sync"
	"testing"

	"github.com/eigerco/strawberry/internal/crypto/ed25519"

	"github.com/eigerco/strawberry/pkg/network/peer"

	"github.com/stretchr/testify/require"
)

// TestPeerSetUnderConcurrentAccess is the regression test for the peer set being
// read and written from several goroutines at once.
//
// Two nodes that start together dial each other, so one connection arrives from
// the network while the one this node dialled comes back through the same door,
// and the loop that keeps retrying unreachable neighbours is reading the set the
// whole time. Before the set had a lock of its own those three touched the same
// map at once, which is a data race and, when it loses, a map read while another
// goroutine is writing it takes the process down.
//
// Run with -race to see anything: the fix is worth nothing without it.
func TestPeerSetUnderConcurrentAccess(t *testing.T) {
	set := peer.NewPeerSet()

	const (
		workers  = 8
		rounds   = 200
		keyCount = 6
	)

	keys := make([]ed25519.PublicKey, keyCount)
	for i := range keys {
		k, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		keys[i] = k
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				key := keys[(w+i)%keyCount]
				idx := uint16((w + i) % keyCount)
				p := &peer.Peer{Ed25519Key: key, ValidatorIndex: &idx}

				set.AddPeer(p)
				_ = set.GetByEd25519Key(key)
				_ = set.GetByValidatorIndex(idx)
				_ = set.GetAllPeers()
				set.RemovePeer(p)
			}
		}(w)
	}
	wg.Wait()

	require.Empty(t, set.GetAllPeers(), "every peer was removed, so none should be left")
}

// TestPeerSetKeepsEveryPeerWhileTheyStay checks the set still holds what it is
// told to hold, which is the thing the locking must not have broken.
func TestPeerSetKeepsEveryPeerWhileTheyStay(t *testing.T) {
	set := peer.NewPeerSet()

	peers := make([]*peer.Peer, 4)
	for i := range peers {
		k, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)
		idx := uint16(i)
		peers[i] = &peer.Peer{Ed25519Key: k, ValidatorIndex: &idx}
		set.AddPeer(peers[i])
	}

	require.Len(t, set.GetAllPeers(), 4)
	for i, p := range peers {
		require.Equal(t, p, set.GetByEd25519Key(p.Ed25519Key), "peer %d", i)
		require.Equal(t, p, set.GetByValidatorIndex(uint16(i)), "peer %d", i)
	}

	set.RemovePeer(peers[1])
	require.Len(t, set.GetAllPeers(), 3)
	require.Nil(t, set.GetByEd25519Key(peers[1].Ed25519Key))
}