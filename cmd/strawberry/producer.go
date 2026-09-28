package main

import (
	"fmt"
	"time"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/eigerco/strawberry/pkg/log"
)

// blockProducer builds one block per timeslot out of what the runtime did in it.
//
// The runtime is where the state of the chain lives, so a header is written after
// the timeslot it names has run, and the root it carries is the root the runtime
// arrived at. Nothing here decides what the state is.
type blockProducer struct {
	bs          *chain.BlockService
	runtime     *devnet.Runtime
	authorIndex uint16
	parentHash  crypto.Hash
	blockNum    uint
	onBlock     func(crypto.Hash, uint, block.Header)
}

// startBlockProducer starts producing a block per timeslot.
//
// A node that was stopped and started again finds the blocks it wrote before it.
// Those blocks are the history the state has to be rebuilt from, so the runtime is
// stepped over them again, quietly, and the result is checked against the root the
// last of those blocks was built on. Only then does the node carry on producing.
// onReady is called once this node has rebuilt the state its blocks describe, and
// before it starts producing. A node that cannot rebuild its own state is not a
// node that is a little late: it is a node that would write a second chain over the
// first, so it stops instead.
func startBlockProducer(bs *chain.BlockService, runtime *devnet.Runtime, authorIndex uint16, onBlock func(crypto.Hash, uint, block.Header), onReady func()) *blockProducer {
	bp := &blockProducer{
		bs:          bs,
		runtime:     runtime,
		authorIndex: authorIndex,
		onBlock:     onBlock,
	}
	time.Sleep(500 * time.Millisecond)

	genesisHeader, genesisHash, genesisFound := bs.GenesisHeader()
	if !genesisFound {
		log.Internal.Warn().Msg("no genesis found, starting from scratch")
		if onReady != nil {
			onReady()
		}
		go bp.run(0)
		return bp
	}
	bp.parentHash = genesisHash
	bp.blockNum = 1

	// The chain begins when the genesis says it does, so the opening state is
	// placed there.
	bp.runtime.AlignToGenesis(genesisHeader.TimeSlotIndex)

	// The tip is the last block this node produced, and it is the point the chain
	// is at. The blocks are indexed by timeslot on the way, because every one of
	// them carries the work its timeslot ran and a claim about the state that has
	// to be checked during the replay.
	blocks := map[jamtime.Timeslot]block.Block{}
	tip, tipHash, found := bp.findTip(blocks)

	// A chain that has not produced a block yet is at its genesis, and the genesis
	// is not a claim about any state: it is where the state starts.
	tipSlot := genesisHeader.TimeSlotIndex
	if found {
		tipSlot = tip.TimeSlotIndex

		// Every timeslot between the genesis and the tip is stepped over again to
		// rebuild the state, because the state is a function of the blocks and not
		// something a node is entitled to remember on its own. Each one is checked
		// against the block that names it, so a chain that does not replay is
		// reported at the timeslot it went wrong rather than at its end.
		replayed, err := bp.replay(genesisHeader.TimeSlotIndex+1, tipSlot, blocks)
		if err != nil {
			// Carrying on from here would produce blocks on top of a state that no
			// block ever named, which is a second chain written over the first one
			// and a balance that changes without a transaction. A node that cannot
			// rebuild its state has nothing to offer, and says so.
			log.Internal.Fatal().Err(err).
				Str("tip", hashToHex(tipHash)).
				Uint64("tipSlot", uint64(tipSlot)).
				Msg("the state of the chain could not be rebuilt from its own blocks")
			return bp
		}
		bp.parentHash = tipHash
		bp.blockNum = uint(tipSlot-genesisHeader.TimeSlotIndex) + 1
		log.Internal.Info().
			Str("hash", hashToHex(tipHash)).
			Str("stateRoot", hashToHex(runtime.Root())).
			Uint64("slot", uint64(tipSlot)).
			Uint64("replayed", replayed).
			Msg("resumed the chain from its own blocks")
	}

	// The state is this node's own, so the answers it gives from here on are
	// answers about a chain.
	if onReady != nil {
		onReady()
	}

	slot := tipSlot + 1
	if current := jamtime.Now().ToTimeslot(); slot < current {
		// The node was asleep, so the timeslots that passed while it was are
		// produced now, quickly, to catch the chain up with the clock.
		log.Internal.Info().
			Uint64("from", uint64(slot)).
			Uint64("to", uint64(current)).
			Msg("catching the chain up with the clock")
		for ; slot < current; slot++ {
			bp.produceBlock(slot)
		}
		// The burst caught up with the clock, and the node carries on producing
		// from the timeslot the clock is at.
		go bp.run(current)
	} else {
		bp.waitFor(slot)
		go bp.run(slot)
	}
	return bp
}

// run produces a block per timeslot from here on, following the clock.
func (bp *blockProducer) run(slot jamtime.Timeslot) {
	for {
		bp.waitFor(slot)
		bp.produceBlock(slot)
		slot++
	}
}

// waitFor sleeps until the timeslot begins.
func (bp *blockProducer) waitFor(slot jamtime.Timeslot) {
	if delay := time.Until(jamtime.FromTimeslot(slot).ToTime()); delay > 0 {
		time.Sleep(delay)
	}
}

// findTip returns the block the node produced last, which is the one with the
// highest timeslot: this node writes a single chain and never forks it. The blocks
// are collected on the way, by timeslot, for the replay to read their work from and
// check their claims against.
//
// The genesis is not one of the blocks this node produced, so it is not a
// candidate for the tip.
func (bp *blockProducer) findTip(blocks map[jamtime.Timeslot]block.Block) (block.Header, crypto.Hash, bool) {
	var (
		tip     block.Header
		tipHash crypto.Hash
		found   bool
	)
	// A predicate that never matches walks every header, which is what finding the
	// last one comes to: headers are keyed by their hash, so they are not stored in
	// any order this could lean on.
	if _, _, err := bp.bs.Store.FindHeader(func(h block.Header) bool {
		if h.ParentHash == chain.GenesisParent {
			return false
		}
		hash, err := h.Hash()
		if err != nil {
			return false
		}
		if b, err := bp.bs.Store.GetBlock(hash); err == nil {
			blocks[h.TimeSlotIndex] = b
		}
		if !found || h.TimeSlotIndex > tip.TimeSlotIndex {
			tip, tipHash, found = h, hash, true
		}
		return false
	}); err != nil {
		log.Internal.Warn().Err(err).Msg("could not walk the headers of this chain")
		return block.Header{}, crypto.Hash{}, false
	}
	return tip, tipHash, found
}

// replay steps the runtime over the timeslots it already has blocks for, so the
// state the node carries on with is the state those blocks describe. It reports
// how many timeslots it stepped.
//
// Each block is asked for the work its timeslot carried and that work is queued
// again before the timeslot runs, because a timeslot that settles no work lands on
// a different state than one that settled what this node settled. A node that
// rebuilt the chain without the work would come back with a balance that never
// existed, and the state root check below is what would notice.
//
// Every step is checked against the block that names it: a header names the state
// it was built on, so a rebuilt state that does not match it means this node and
// its own blocks disagree, and the first timeslot where they do is worth far more
// than a root at the end of it.
func (bp *blockProducer) replay(from, through jamtime.Timeslot, blocks map[jamtime.Timeslot]block.Block) (uint64, error) {
	if through < from {
		return 0, nil
	}
	for slot := from; slot <= through; slot++ {
		parentRoot := bp.runtime.Root()
		if b, ok := blocks[slot]; ok && b.Header.PriorStateRoot != parentRoot {
			return uint64(slot - from), fmt.Errorf(
				"the state rebuilt for timeslot %d is %x, but the block for that timeslot was built on %x",
				slot, parentRoot, b.Header.PriorStateRoot)
		}

		work := blockWork(blocks[slot])
		if err := bp.runtime.Rebuild(work); err != nil {
			return uint64(slot - from), fmt.Errorf("timeslot %d: %w", slot, err)
		}

		if err := bp.runtime.Step(slot); err != nil {
			return uint64(slot - from), err
		}
	}
	bp.runtime.FinishRebuild()
	return uint64(through - from + 1), nil
}

// blockWork is the work a block names, in the order the block lists it.
func blockWork(b block.Block) []devnet.BlockWork {
	work := make([]devnet.BlockWork, 0, len(b.Extrinsic.EP))
	for _, preimage := range b.Extrinsic.EP {
		work = append(work, devnet.BlockWork{ServiceID: preimage.ServiceIndex, Payload: preimage.Data})
	}
	return work
}

// blockExtrinsic puts a timeslot's work in the block that names it. A dev block
// carries its work as preimages, which is the slot the protocol reserves for
// external data, so the header's ExtrinsicHash commits to the work this block
// settled rather than to the root of an empty list.
func blockExtrinsic(work []devnet.BlockWork) block.Extrinsic {
	preimages := make(block.PreimageExtrinsic, 0, len(work))
	for _, w := range work {
		preimages = append(preimages, block.Preimage{ServiceIndex: w.ServiceID, Data: w.Payload})
	}
	return block.Extrinsic{EP: preimages}
}

// produceBlock runs one timeslot and writes the block that names it.
//
// The root of the state as it stands is the parent state of this block, so it is
// read before the timeslot's work runs.
func (bp *blockProducer) produceBlock(slot jamtime.Timeslot) {
	parentRoot := bp.runtime.Root()

	work, err := bp.runtime.Run(slot)
	if err != nil {
		log.Internal.Error().Err(err).Uint64("slot", uint64(slot)).Msg("the runtime could not advance the timeslot")
		return
	}

	// The block carries the work its timeslot settled, so the state a restart
	// rebuilds is the state this block describes and not the state this node
	// happened to be holding when it was stopped.
	extrinsic := blockExtrinsic(work)
	extrinsicHash, err := extrinsic.Hash()
	if err != nil {
		log.Internal.Error().Err(err).Msg("failed to hash the block extrinsic")
		return
	}

	header := block.Header{
		ParentHash:       bp.parentHash,
		PriorStateRoot:   parentRoot,
		ExtrinsicHash:    extrinsicHash,
		TimeSlotIndex:    slot,
		BlockAuthorIndex: bp.authorIndex,
	}

	if slot.IsFirstTimeslotInEpoch() {
		header.EpochMarker = &block.EpochMarker{
			Entropy:        crypto.Hash{},
			TicketsEntropy: crypto.Hash{},
		}
	}

	hash, err := header.Hash()
	if err != nil {
		log.Internal.Error().Err(err).Msg("failed to hash block header")
		return
	}

	if err := bp.bs.Store.PutBlock(block.Block{Header: header, Extrinsic: extrinsic}); err != nil {
		log.Internal.Error().Err(err).Msg("failed to store block")
		return
	}
	if err := bp.bs.HandleNewHeader(&header); err != nil {
		log.Internal.Warn().Err(err).Str("hash", hashToHex(hash)).Uint64("slot", uint64(slot)).Msg("HandleNewHeader warning")
	}

	bp.parentHash = hash
	bp.blockNum++

	log.Internal.Info().
		Str("hash", hashToHex(hash)).
		Str("parentStateRoot", hashToHex(parentRoot)).
		Str("stateRoot", hashToHex(bp.runtime.Root())).
		Uint64("slot", uint64(slot)).
		Uint("number", bp.blockNum).
		Uint64("epoch", uint64(slot.ToEpoch())).
		Int("work", len(work)).
		Msg("block produced")

	if bp.onBlock != nil {
		bp.onBlock(hash, bp.blockNum, header)
	}
}
