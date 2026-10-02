package main

import (
	"context"
	"crypto/ed25519"
	"time"

	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/pkg/log"
	"github.com/eigerco/strawberry/pkg/network/node"
)

// Cuantos bloques se piden en una ida. El protocolo CE 128 admite el rango por
// bloque, y traerlos de uno en uno seria un viaje por cada bloque perdido.
const backfillBatch = 32

// Cada cuanto se mira si falta algo. Solo se hace trabajo cuando hay un hueco de
// verdad, asi que la frecuencia no cuesta nada mientras no haya ninguno.
const backfillEvery = time.Second

// backfillLoop fills in the blocks this node is missing from its peers.
//
// Announcing a block and having it are different things. A node that was off, or
// that simply fell behind, hears about a head it cannot walk: the walk to check
// that a block descends from the finalized one stops at the first parent it does
// not have. Before this existed that head was discarded as a bad header, so the
// node never caught up no matter how long it waited, and two nodes drifted
// further apart for as long as both were running. Now the head is kept as a
// handle and the blocks behind it are asked for over the protocol the node
// already speaks, in batches, until the chain is walkable again.
//
// It fills the block store and the leaf set, and nothing more. Executing a chain
// somebody else wrote is a separate matter and belongs with tip choice: until a
// node has decided which chain is canonical, replaying a foreign one would mean
// executing a branch it may then abandon.
func backfillLoop(ctx context.Context, n *node.Node, bs *chain.BlockService) {
	ticker := time.NewTicker(backfillEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		wanted := bs.BackfillHashes(backfillBatch)
		if len(wanted) == 0 {
			// Nothing missing: a header that was waiting on a gap may now be
			// placeable, which happens when a peer filled the store itself.
			if placed := bs.RetryPending(); placed > 0 {
				log.Internal.Info().Int("headers", placed).Msg("placed headers that were waiting on a gap")
			}
			continue
		}

		keys := peerKeys(n)
		if len(keys) == 0 {
			continue
		}

		fetched, placed := 0, 0
		for _, want := range wanted {
			got, err := fetchMissing(ctx, n, bs, keys, want)
			if err != nil {
				log.Internal.Debug().Err(err).Str("want", hashToHex(want)).Msg("backfill request failed")
				continue
			}
			fetched += got
			placed += bs.RetryPending()
			// A peer that did not have this block will not have the next one
			// either: it is the same gap on the same chain. Stopping here avoids
			// asking every peer the same question every pass.
			if got == 0 {
				break
			}
		}

		if fetched > 0 {
			gap := bs.Gap()
			log.Internal.Info().
				Int("blocks", fetched).
				Int("placed", placed).
				Int("stillMissing", gap.MissingBlocks).
				Int("waitingHeaders", gap.PendingHeader).
				Msg("caught up on blocks from peers")
		}
	}
}

// fetchMissing asks the peers, one after another, for the chain that descends
// from want, and stores whatever comes back.
func fetchMissing(
	ctx context.Context,
	n *node.Node,
	bs *chain.BlockService,
	keys []ed25519.PublicKey,
	want crypto.Hash,
) (int, error) {
	for _, key := range keys {
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		blocks, err := n.RequestBlocks(reqCtx, want, false, backfillBatch, key)
		cancel()
		if err != nil {
			continue
		}
		if len(blocks) == 0 {
			return 0, nil
		}

		// The request walks down from want, so the blocks come back newest
		// first and are stored oldest first. A header can only be placed once
		// its parent is there, and the parent of the first one is exactly what
		// was missing.
		stored := 0
		for i := len(blocks) - 1; i >= 0; i-- {
			if err := bs.Store.PutBlock(blocks[i]); err != nil {
				return stored, err
			}
			stored++
		}
		return stored, nil
	}
	return 0, nil
}

// peerKeys is the set of peers as keys, which is what a block request names a
// destination by.
func peerKeys(n *node.Node) []ed25519.PublicKey {
	peers := n.GetAllPeers()
	out := make([]ed25519.PublicKey, 0, len(peers))
	for _, p := range peers {
		if p.ProtoConn == nil || p.ProtoConn.TConn == nil {
			continue
		}
		if key := p.ProtoConn.TConn.PeerKey(); key != nil {
			out = append(out, key)
		}
	}
	return out
}
