package discovery

// This is where a node meets the validators it is supposed to be running with,
// when neither of them is on the same network as the other.
//
// The chain fixes the validator set, and the peer id of every validator comes
// from its key, so everybody knows who to look for from the genesis alone. The
// only thing missing is where they are, and that is what the shared table is
// asked. The handshake that proves a machine is the validator it claims to be
// still happens in the chain layer; all this does is find the machine to hand
// the connection to.

import (
	"context"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
)

// rendezvousInterval is how often the table is asked for every expected peer.
// Thirty seconds is long enough for a machine to have found one route and short
// enough that a machine that appears keeps being noticed in reasonable time.
const rendezvousInterval = 30 * time.Second

// rendezvousLoop keeps asking the shared table where every expected validator
// is, and reaches each one that the table can place.
//
// It does not wait for a node to announce itself, and that is what makes a
// network of machines behind their own routers come together: each one of them
// asks for the others instead of all waiting to be asked.
func (h *Host) rendezvousLoop(ctx context.Context) {
	// The first ask comes after a short pause so the table has had a chance to
	// be bootstrapped before anything is asked of it.
	primera := time.NewTimer(5 * time.Second)
	defer primera.Stop()

	tick := time.NewTicker(rendezvousInterval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-primera.C:
		case <-tick.C:
		}

		for _, id := range h.rendezvous {
			if id == h.host.ID() {
				continue
			}
			if !h.needsFinding(ctx, id) {
				continue
			}
			h.lookFor(ctx, id)
		}
	}
}

// needsFinding reports whether the peer still has to be looked for: nothing to
// do for a peer this host is already connected to, and nothing worth doing for
// one it never had or just gave up on.
func (h *Host) needsFinding(ctx context.Context, id peer.ID) bool {
	if h.host.Network().Connectedness(id) == network.Connected {
		return false
	}
	return true
}

// lookFor asks the table where a peer is and reaches it if the table knows.
func (h *Host) lookFor(ctx context.Context, id peer.ID) {
	qctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()

	info, err := h.dht.FindPeer(qctx, id)
	if err != nil {
		h.logf("asked the shared table where %s is and it does not know yet: %v",
			id.ShortString(), err)
		return
	}
	if len(info.Addrs) == 0 {
		h.logf("the shared table knows %s but has no address for it yet", id.ShortString())
		return
	}

	// What the table reported goes into the peer store and the book, because it
	// is usually a public address and it is exactly the address the chain layer
	// will want to knock the chain port on.
	h.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerAddrTTL)
	h.book.remember(info.ID, info.Addrs)

	if err := h.host.Connect(qctx, info); err != nil {
		h.logf("the shared table knows %s but reaching it failed: %v", id.ShortString(), err)
		return
	}

	h.logf("found %s on the shared table and reached it", id.ShortString())
}