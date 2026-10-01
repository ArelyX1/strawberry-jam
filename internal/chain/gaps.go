package chain

import (
	"bytes"
	"errors"
	"fmt"
	"sort"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/store"
)

// ErrMissingAncestors means the header may well be good but this node does not
// have the blocks it builds on, so it cannot judge it yet.
//
// It is a distinct error because the two reasons a header cannot be checked
// look identical from the outside and call for opposite responses. A header
// that is not a descendant of anything this node finalized is bogus and has to
// be dropped. A header whose ancestors are simply absent is good news that
// arrived early: the node was behind, and the answer is to fetch what is
// missing. As one generic failure both were the same, so the second was
// silently discarded and the node stayed behind for good.
var ErrMissingAncestors = errors.New("block ancestors are missing")

// IsMissingHeader reports whether an error is a missing ancestor rather than a
// header that genuinely does not belong to this node's chain.
func IsMissingHeader(err error) bool {
	return err != nil && (errors.Is(err, store.ErrHeaderNotFound) || errors.Is(err, ErrMissingAncestors))
}

// trackPending remembers headers that arrived before the blocks they build on.
//
// A node that is behind hears about a head it cannot walk, and that head is the
// most useful thing it has been told: it is the handle to ask for everything it
// missed. Discarding it throws that away, so it is kept and retried once the gap
// is filled.
func (bs *BlockService) trackPending(header block.Header, hash crypto.Hash) {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	if bs.pending == nil {
		bs.pending = make(map[crypto.Hash]block.Header)
	}
	bs.pending[hash] = header
}

// PendingGaps is how many headers are waiting on a gap, for the log and for
// deciding whether a fetch is worth doing.
func (bs *BlockService) PendingGaps() int {
	bs.mu.RLock()
	defer bs.mu.RUnlock()
	return len(bs.pending)
}

// takePending removes and returns everything waiting on a gap, so it can be
// retried after a fill. What still cannot be placed goes back to waiting.
func (bs *BlockService) takePending() []block.Header {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	out := make([]block.Header, 0, len(bs.pending))
	for _, h := range bs.pending {
		out = append(out, h)
	}
	bs.pending = make(map[crypto.Hash]block.Header)
	return out
}

// gapHashes walks back from every leaf and returns the block hashes this node
// does not have, nearest first and without repeats.
//
// It stops at the first hash it does have: the node only needs the part that is
// actually missing, not the whole history. The walk is bounded because a peer
// far ahead would otherwise have it walk the entire chain, and a bound turns
// that from a hang into a number.
func (bs *BlockService) gapHashes(limit int) []crypto.Hash {
	if limit <= 0 {
		return nil
	}

	bs.mu.RLock()
	leafHashes := make([]crypto.Hash, 0, len(bs.KnownLeaves))
	for hash := range bs.KnownLeaves {
		leafHashes = append(leafHashes, hash)
	}
	// Copia de las pendientes: se necesitan con el bs.mu tomado, y el recorrido
	// que sigue llama a GetHeader, que no usa ese cerrojo pero si toca disco.
	pending := make([]block.Header, 0, len(bs.pending))
	for _, h := range bs.pending {
		pending = append(pending, h)
	}
	bs.mu.RUnlock()

	seen := make(map[crypto.Hash]bool)
	missing := make([]crypto.Hash, 0, limit)
	add := func(h crypto.Hash) bool {
		if seen[h] {
			return true
		}
		seen[h] = true
		missing = append(missing, h)
		return len(missing) < limit
	}

	// Las cabeceras que llegaron sobre un hueco van primero. Son las unicas que
	// saben que falta algo: una cabecera que no se pudo colocar nunca se ajoute
	// como hoja, de modo que mirando solo las hojas no se encontraba ningun
	// hueco y el relleno no tenia por donde empezar. De cada una se quiere el
	// bloque entero, porque solo tenemos la cabecera, y ademas su padre, que es
	// el hole en si.
	sort.Slice(pending, func(i, j int) bool {
		return bytes.Compare(pending[i].ParentHash[:], pending[j].ParentHash[:]) < 0
	})
	for _, h := range pending {
		hash, err := h.Hash()
		if err != nil {
			continue
		}
		if !add(hash) {
			return missing
		}
		if !add(h.ParentHash) {
			return missing
		}
	}

	// Sort the leaves so the request asks for the same blocks in the same order
	// every pass, instead of reshuffling between passes.
	sort.Slice(leafHashes, func(i, j int) bool {
		return bytes.Compare(leafHashes[i][:], leafHashes[j][:]) < 0
	})

	for _, leaf := range leafHashes {
		header, err := bs.Store.GetHeader(leaf)
		if err != nil {
			continue
		}
		// The leaf is only wanted when its block is missing. A leaf this node
		// wrote itself is complete, and asking for it again was how a whole
		// chain came back reporting one block still missing.
		if _, err := bs.Store.GetBlock(leaf); err != nil {
			if !add(leaf) {
				return missing
			}
		}
		// The walk stops at the first header this node has: from there on the
		// chain is unbroken and there is nothing to go and get.
		if _, err := bs.Store.GetHeader(header.ParentHash); err == nil {
			continue
		}
		if !add(header.ParentHash) {
			return missing
		}
	}

	return missing
}

// BackfillHashes is what the sync loop asks for: the blocks to go and get, so
// that this node can walk its own chain again.
func (bs *BlockService) BackfillHashes(limit int) []crypto.Hash {
	return bs.gapHashes(limit)
}

// RetryPending reconsiders every header that was waiting on a gap, and is what
// turns a filled gap back into a usable chain.
func (bs *BlockService) RetryPending() int {
	waiting := bs.takePending()
	placed := 0
	for i := range waiting {
		header := waiting[i]
		if err := bs.HandleNewHeader(&header); err == nil {
			placed++
		} else if IsMissingHeader(err) {
			hash, herr := header.Hash()
			if herr == nil {
				bs.trackPending(header, hash)
			}
		}
		// Any other failure means the header does not belong here at all, and
		// letting it go is the correct outcome.
	}
	return placed
}

// GapReport is what the node can say about how far behind it is, for the log and
// for the operator.
type GapReport struct {
	MissingBlocks int
	PendingHeader int
	OldestMissing jamtime.Timeslot
}

// Gap summarises the holes.
func (bs *BlockService) Gap() GapReport {
	missing := bs.gapHashes(1024)
	report := GapReport{MissingBlocks: len(missing), PendingHeader: bs.PendingGaps()}
	for _, h := range missing {
		header, err := bs.Store.GetHeader(h)
		if err == nil {
			report.OldestMissing = header.TimeSlotIndex
			break
		}
		if header, err := bs.Store.GetBlock(h); err == nil {
			report.OldestMissing = header.Header.TimeSlotIndex
			break
		}
	}
	return report
}

// CanonicalChain walks back from a tip to the point this node's chain starts and
// returns the blocks in forward order, so a chain can be executed the way it
// was written.
//
// Walking by parent hash and not by timeslot is the point. Timeslots do not
// identify a block in a chain that forked: two blocks can share a timeslot, and
// a map keyed by timeslot keeps whichever arrived last, which is how a node ends
// up replaying a mixture of two branches and landing on a state that no branch
// ever described.
func (bs *BlockService) CanonicalChain(tip crypto.Hash, limit int) ([]block.Block, error) {
	if limit <= 0 {
		limit = 4096
	}

	backwards := make([]block.Block, 0, limit)
	seen := make(map[crypto.Hash]bool)
	current := tip

	for len(backwards) < limit {
		if seen[current] {
			return nil, fmt.Errorf("the chain loops at %x", current)
		}
		seen[current] = true

		b, err := bs.Store.GetBlock(current)
		if err != nil {
			// A header without its block is a hole, and walking into it would
			// mean guessing what came before. The backfill has to run first.
			return nil, fmt.Errorf("no block for %x: %w", current, err)
		}
		backwards = append(backwards, b)
		current = b.Header.ParentHash

		// The chain ends where the parent stops having a block, which is the
		// genesis: there is no block for it because nothing produced it.
		if _, err := bs.Store.GetBlock(current); err != nil {
			break
		}
	}

	for i, j := 0, len(backwards)-1; i < j; i, j = i+1, j-1 {
		backwards[i], backwards[j] = backwards[j], backwards[i]
	}
	return backwards, nil
}

// LeafBlock is a tip with its header, so that choosing between tips does not
// need a store read for each one.
type LeafBlock struct {
	Header block.Header
	Slot   jamtime.Timeslot
}

// Leaves gives the block service's view of the tips, as blocks rather than
// hashes so a caller does not have to fetch each one.
func (bs *BlockService) Leaves() []LeafBlock {
	bs.mu.RLock()
	hashes := make([]crypto.Hash, 0, len(bs.KnownLeaves))
	for hash := range bs.KnownLeaves {
		hashes = append(hashes, hash)
	}
	bs.mu.RUnlock()

	out := make([]LeafBlock, 0, len(hashes))
	for _, hash := range hashes {
		header, err := bs.Store.GetHeader(hash)
		if err != nil {
			continue
		}
		out = append(out, LeafBlock{Header: header, Slot: header.TimeSlotIndex})
	}
	return out
}
