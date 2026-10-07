package discovery

// This is how a node joins the mesh without being told who or where any other
// member is. It does not look for validators by name, and it does not wait to
// be introduced: it signs in under the key of its own chain and answers the
// mesh's roll call.
//
// The key comes from an identity every machine of the same chain computes the
// same way, so a node that joined once and left comes back to the same place,
// a second node that has never spoken to the first finds it there, and a node
// of some other chain never even sees it. Under that key every node keeps a
// provider record that says "here is where I am", and asks who else has
// provided the same key. Provider records are served by the shared table for
// as long as they are renewed, which is what makes two machines that nothing
// knows about find each other: a plain lookup by peer id only works when
// somebody happens to be holding that id in its own address book.
//
// The handshake that proves a machine belongs where it says it does still
// happens in the chain layer. This only finds the machine.

import (
	"context"
	"time"

	"github.com/ipfs/go-cid"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	mh "github.com/multiformats/go-multihash"
)

// rendezvousInterval is how often this node renews its membership and reads the
// roll call. Thirty seconds keeps a provider record alive and notices a machine
// that appeared or left in the same window.
const rendezvousInterval = 30 * time.Second

// rendezvousNamespace separates these records from anything else that could
// share the table by coincidence, and lets the provider and the asker of a key
// meet even though neither computes the other's peer id.
const rendezvousNamespace = "sdlg-jam/network/v1/"

// networkCID is the key every node of a chain provides and reads. It depends
// only on the chain's identity, never on any address or peer id, so a node
// that has just started can work it out from the genesis alone.
func networkCID(network string) cid.Cid {
	h1, _ := mh.Sum([]byte(rendezvousNamespace+network), mh.SHA2_256, -1)
	return cid.NewCidV1(cid.Raw, h1)
}

// rendezvousLoop keeps this node on the roll call of its chain and the other
// members in reach, for as long as the node runs.
func (h *Host) rendezvousLoop(ctx context.Context) {
	// The first round comes after a short pause so the table has had a chance
	// to be bootstrapped before anything is asked of it.
	primera := time.NewTimer(5 * time.Second)
	defer primera.Stop()

	tick := time.NewTicker(rendezvousInterval)
	defer tick.Stop()

	anunciado := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-primera.C:
		case <-tick.C:
		}

		h.announce(ctx, &anunciado)
		h.rollCall(ctx)
	}
}

// announce registers this node on the mesh: it becomes one of the answers a
// member of the same chain gets when it asks who is there. The record has to
// be renewed or the table forgets it, which is why it happens every round.
func (h *Host) announce(ctx context.Context, ya *bool) {
	qctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := h.dht.Provide(qctx, networkCID(h.network), true); err != nil {
		h.logf("this node could not sign in on the shared table: %v", err)
		return
	}
	if !*ya {
		h.logf("signed in on the shared table, so a node of the same chain can find it")
		*ya = true
	}
}

// rollCall reads who else of the same chain is here and reaches them. Whoever
// is listed has signed in under the same key, which is all the introduction the
// node needs: the rest — is the machine really one of the validators? — is
// answered later, by the chain handshake, not here.
func (h *Host) rollCall(ctx context.Context) {
	qctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	provedores, err := h.dht.FindProviders(qctx, networkCID(h.network))
	if err != nil {
		h.logf("asked the shared table who is here and it does not answer: %v", err)
		return
	}
	if len(provedores) == 0 {
		h.logf("asked the shared table who is here and it has no answers yet")
		return
	}

	for _, info := range provedores {
		if info.ID == h.host.ID() {
			continue
		}
		if h.host.Network().Connectedness(info.ID) == network.Connected {
			continue
		}
		h.meet(qctx, info)
	}
}

// meet reaches a machine the roll call listed, and writes its addresses down
// where the chain layer can knock the chain port on them.
func (h *Host) meet(ctx context.Context, info peer.AddrInfo) {
	h.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerAddrTTL)
	h.book.remember(info.ID, info.Addrs)

	if err := h.host.Connect(ctx, info); err != nil {
		h.logf("the shared table lists %s but reaching it failed: %v", info.ID.ShortString(), err)
		return
	}

	h.logf("found %s on the shared table and reached it", info.ID.ShortString())
}
