package main

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/eigerco/strawberry/pkg/log"
	p2pnode "github.com/eigerco/strawberry/pkg/network/node"
	"github.com/eigerco/strawberry/pkg/network/peer"
	"os"
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
	// How many validators the authorship turn rotates over. Not the chain's
	// validator count, because the number of nodes actually running a devnet is
	// a handful and the constant is not: rotating over the constant means a
	// lone node writes one timeslot in five hundred and the chain crawls.
	authorCount uint16
	parentHash  crypto.Hash
	blockNum    uint
	onBlock     func(crypto.Hash, uint, block.Header)
	// net is the network node, so a block this node writes can be told to the
	// others. nil when there is no network, which is the case in tests.
	net *p2pnode.Node
	// ctx bounds the announcement; announcing opens a stream per peer and must
	// not hold up the next timeslot if a peer is unresponsive.
	ctx context.Context
	// announceBackoff is when each peer may next be told about a block.
	//
	// Without it a peer whose announcement stream will not open is retried every
	// timeslot, forever: each attempt opens and abandons another stream, and the
	// hammering is enough for the other side to tear the connection down. Then
	// neither node can tell the other anything, which is the opposite of what
	// announcing is for.
	announceBackoff map[string]time.Time
}

// How long past the end of a timeslot a node keeps waiting for the block the
// author of that timeslot writes, and how often it looks.
const (
	foreignSlotGrace = 3 * time.Second
	foreignSlotPoll  = 200 * time.Millisecond
)

// announceRetryWait is how long a peer is left alone after an announcement to it
// failed. Long enough that the retries are not a stream flood, short enough that
// a peer that comes back is picked up while the chain is still moving.
const announceRetryWait = 10 * time.Second

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
func startBlockProducer(bs *chain.BlockService, runtime *devnet.Runtime, authorIndex uint16, authorCount uint16, onBlock func(crypto.Hash, uint, block.Header), onReady func(), net *p2pnode.Node, ctx context.Context) *blockProducer {
	bp := &blockProducer{
		net:             net,
		ctx:             ctx,
		announceBackoff: map[string]time.Time{},
		bs:              bs,
		runtime:         runtime,
		authorIndex:     authorIndex,
		authorCount:     authorCount,
		onBlock:         onBlock,
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
		// A node that has just started has no numbers worth keeping: its state
		// comes out of the blocks it is about to replay, and those numbers are
		// part of it.
		bp.runtime.ForgetHandedOut()

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

// run produces a block in the timeslots this validator authors, and executes the
// ones another validator authored, following the clock.
//
// A timeslot has exactly one author, this node's index modulo the number of
// validators. Without that rule every validator wrote a block in every timeslot,
// and since two blocks for one slot are a fork and not a merge, the nodes spent
// the whole run disagreeing while each of them was behaving perfectly. The
// height still matched, which is what made it look like a network that worked:
// the nodes agreed on how far the chain went and on nothing else.
func (bp *blockProducer) run(slot jamtime.Timeslot) {
	for {
		bp.waitFor(slot)
		if authorFor(slot, bp.authorIndex, bp.authorCount) {
			bp.produceBlock(slot)
		} else {
			bp.runForeignSlot(slot)
		}
		slot++
	}
}

// authorFor is the validator whose turn it is to write a given timeslot.
//
// A count of one means this node writes every timeslot, which is what a node on
// its own has to do: nobody else is going to. That holds whichever index the
// node happens to be, because modulo one is zero and comparing it against a
// non-zero index left the node writing nothing at all.
func authorFor(slot jamtime.Timeslot, validatorIndex, authorCount uint16) bool {
	switch authorCount {
	case 1:
		return true
	case 0:
		authorCount = constants.NumberOfValidators
	}
	return uint64(slot)%uint64(authorCount) == uint64(validatorIndex)
}

// runForeignSlot executes the block another validator wrote for this timeslot.
//
// The wait has to reach the end of the timeslot. The author writes its block
// while the slot is running, so a node that gave up part way through the slot it
// was waiting in always timed out, and then sat one block behind for as long as
// it ran: it was waiting for something that had not been written yet, on a
// schedule that guaranteed it would stop looking before it appeared.
func (bp *blockProducer) runForeignSlot(slot jamtime.Timeslot) {
	// The block should be there once the timeslot is over. The grace covers the
	// author writing it at the very end, plus the announcement landing.
	deadline := jamtime.FromTimeslot(slot + 1).ToTime().Add(foreignSlotGrace)
	for {
		if bp.adoptSlot(slot) {
			return
		}
		if !time.Now().Before(deadline) {
			log.Internal.Debug().Uint64("slot", uint64(slot)).
				Msg("the block for this timeslot never arrived; staying where this node is")
			return
		}
		time.Sleep(foreignSlotPoll)
	}
}

// adoptSlot executes the chain this node holds for a timeslot another validator
// authored, up to that timeslot. It reports whether it did.
func (bp *blockProducer) adoptSlot(slot jamtime.Timeslot) bool {
	_, leaf, ok := bp.bestKnownTip()
	if !ok {
		return false
	}
	header, err := bp.bs.Store.GetHeader(leaf)
	if err != nil || header.TimeSlotIndex < slot {
		return false
	}

	// The whole chain, not the last few blocks. The walk goes by parent hash from
	// the tip, so asking for a suffix gives the last n blocks and nothing before
	// them, and the replay below starts at the timeslot this node's state stands
	// at rather than at the tip. Every timeslot between the two that was not in
	// the suffix would then be stepped with no block and nothing to check it
	// against, which is how a node ends up holding a block it adopted over a
	// state that block was never built on, and refusing its own chain as
	// unrebuildable.
	chain, err := bp.bs.CanonicalChain(leaf, 0)
	if err != nil || len(chain) == 0 || chain[len(chain)-1].Header.TimeSlotIndex != slot {
		return false
	}

	// Already executed: the state stands at or past this slot and the tip is the
	// one this node is building on, so there is nothing to do.
	if bp.parentHash == leaf && bp.runtime.Timeslot() >= slot {
		return true
	}

	bySlot := make(map[jamtime.Timeslot]block.Block, len(chain))
	for _, b := range chain {
		bySlot[b.Header.TimeSlotIndex] = b
	}
	from := bp.runtime.Timeslot() + 1
	if _, err := bp.replay(from, slot, bySlot); err != nil {
		log.Internal.Debug().Err(err).Uint64("slot", uint64(slot)).
			Msg("could not execute the block this timeslot's author wrote")
		return false
	}
	bp.parentHash = leaf
	bp.blockNum = uint(slot)
	return true
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
		// STRAWBERRY_TRACE_REPLAY=1 deja una linea por timeslot con la raiz que
		// ha salido. Dos nodos que ejecutan la misma cadena tienen que dar las
		// mismas lineas, y en el mismo numero de timeslots: donde se separen, o
		// donde uno tiene mas lineas que el otro, es donde esta el fallo.
		if os.Getenv("STRAWBERRY_TRACE_REPLAY") != "" {
			fmt.Printf("REPLAY %d %s\n", uint64(slot), hashToHex(bp.runtime.Root()))
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
	// Before writing anything, make sure this node is still on the chain. If a
	// peer got ahead, following it here means the block this timeslot produces
	// builds on what everyone else is building on, instead of two nodes writing
	// one block each for the same slot and diverging.
	if moved, err := bp.followCanonical(); err != nil {
		log.Internal.Warn().Err(err).Uint64("slot", uint64(slot)).Msg("could not follow the chain a peer wrote; staying on this node's own")
	} else if moved {
		log.Internal.Info().Uint64("slot", uint64(slot)).Msg("now producing on the chain it adopted")
	}

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

	// Tell the other nodes. It goes in its own goroutine and with the node's own
	// context, not a per block one: the announcer holds a stream that is bound to
	// whatever context created it, so cancelling that context here left every
	// peer with a dead stream and every later block failed with "context
	// canceled". A peer that stops answering must also not hold up the next
	// timeslot, which is why this is not on this path.
	// A peer that has just connected is told where the chain is straight away.
	// Announcing only on new blocks was not enough: a node that had been away
	// missed the blocks that were produced while it was gone, and the peer it
	// reconnected to only ever mentioned them if it happened to produce another
	// one. Two nodes could sit on different chains for ever with no way to learn
	// that it had happened.
	if bp.net != nil {
		bp.announceTipToNewPeers()
		bp.announceWithBackoff(&header)
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

// bestKnownTip is the tip this node should be following: the block with the
// highest timeslot among all the leaves it knows about, whichever node wrote it.
//
// "Whichever node wrote it" is the whole point. Until now the producer only ever
// looked at the blocks it had written itself, so a node that fell behind kept
// producing a branch nobody else was on and the two drifted apart for good.
func (bp *blockProducer) bestKnownTip() (block.Header, crypto.Hash, bool) {
	leaves := bp.bs.Leaves()
	if len(leaves) == 0 {
		return block.Header{}, crypto.Hash{}, false
	}

	// Sorted by slot and, for two leaves on the same slot, by hash. The tie break
	// has to be the same on every node or they would pick different branches and
	// the fork would never settle.
	sort.Slice(leaves, func(i, j int) bool {
		hi, _ := leaves[i].Header.Hash()
		hj, _ := leaves[j].Header.Hash()
		if leaves[i].Slot != leaves[j].Slot {
			return leaves[i].Slot > leaves[j].Slot
		}
		return bytes.Compare(hi[:], hj[:]) < 0
	})

	best := leaves[0]
	hash, err := best.Header.Hash()
	if err != nil {
		return block.Header{}, crypto.Hash{}, false
	}
	return best.Header, hash, true
}

// followCanonical makes the node's tip and state agree with the chain everyone
// else is on, and reports whether it had to move.
//
// It only moves when the best known tip is not a block this node produced. When
// it is, there is nothing to do: the node is already on the chain and its state
// describes it.
func (bp *blockProducer) followCanonical() (bool, error) {
	tip, tipHash, ok := bp.bestKnownTip()
	if !ok {
		return false, nil
	}

	// Already following it: either it is the block we wrote last, or it is at a
	// slot we have not reached and our own tip is still ahead of everything.
	if tipHash == bp.parentHash {
		return false, nil
	}
	if tip.TimeSlotIndex <= bp.lastSlot() {
		return false, nil
	}

	log.Internal.Info().
		Uint64("tipSlot", uint64(tip.TimeSlotIndex)).
		Uint64("ourSlot", uint64(bp.lastSlot())).
		Str("tip", hashToHex(tipHash)).
		Msg("another node is ahead; following its chain")

	chain, err := bp.bs.CanonicalChain(tipHash, 0)
	if err != nil {
		return false, fmt.Errorf("walk the chain a peer wrote: %w", err)
	}

	// The state this node is holding describes the branch it is leaving, and
	// there is no un-run for the work that produced it, so it goes back to
	// genesis and the other branch is executed over that.
	if err := bp.runtime.Rewind(); err != nil {
		return false, fmt.Errorf("rewind to the start of the chain: %w", err)
	}

	// Execute it through the same path this node uses to resume its own chain
	// after a restart: one timeslot at a time, that timeslot's work before its
	// step, and the state root checked against the block it is about to rebuild.
	//
	// Handing the runtime the whole chain's work at once is what made this land
	// on the wrong state. The queue is shared, so with every timeslot's work
	// waiting before the first step, a slot could settle work that belonged to
	// the block after it. The node then held the tip of the chain it had adopted
	// and a state that chain did not describe, and every answer it gave about
	// that chain was wrong. The per-timeslot root check is what stops that going
	// unnoticed: a chain that does not rebuild is refused rather than adopted.
	genesis, _, ok := bp.bs.GenesisHeader()
	if !ok {
		return false, fmt.Errorf("the genesis header is not there, so the chain cannot be rebuilt from the start")
	}
	bySlot := make(map[jamtime.Timeslot]block.Block, len(chain))
	for _, b := range chain {
		bySlot[b.Header.TimeSlotIndex] = b
	}
	last := chain[len(chain)-1].Header
	if _, err := bp.replay(genesis.TimeSlotIndex+1, last.TimeSlotIndex, bySlot); err != nil {
		return false, fmt.Errorf("replay the chain a peer wrote: %w", err)
	}

	bp.parentHash = tipHash
	bp.blockNum = uint(last.TimeSlotIndex) + 1
	return true, nil
}

func (bp *blockProducer) lastSlot() jamtime.Timeslot {
	if bp.blockNum == 0 {
		return 0
	}
	return jamtime.Timeslot(bp.blockNum - 1)
}

// announceTipToNewPeers tells every peer that does not have an announcer yet
// where the chain is, so a node that has been away or that just started finds
// out what it missed instead of waiting for the other side to produce.
func (bp *blockProducer) announceTipToNewPeers() {
	header, hash, ok := bp.bestKnownTip()
	if !ok {
		return
	}

	now := time.Now()
	var fresh []*peer.Peer
	for _, p := range bp.net.GetAllPeers() {
		// A cached announcer whose connection died is not worth anything, and
		// leaving it set would make this peer look already-told forever.
		if p.BAnnouncer != nil {
			if p.BAnnouncer.Done() {
				p.BAnnouncer = nil
			} else {
				continue
			}
		}
		// Mismo-compensacion: un par al que no se le puede abrir el stream se
		// reintenta cada timeslot, y cada intento abre y abandona otro stream.
		if wait, ok := bp.announceBackoff[p.Address.String()]; ok && now.Before(wait) {
			continue
		}
		fresh = append(fresh, p)
	}
	if len(fresh) == 0 {
		return
	}

	for _, p := range fresh {
		ctx, cancel := context.WithTimeout(bp.ctx, 5*time.Second)
		err := bp.net.AnnounceBlock(ctx, &header, p)
		cancel()
		if err != nil {
			bp.announceBackoff[p.Address.String()] = now.Add(announceRetryWait)
			log.Internal.Debug().Err(err).Str("tip", hashToHex(hash)).Msg("could not tell a newly connected peer where the chain is; will wait before trying again")
			continue
		}
		delete(bp.announceBackoff, p.Address.String())
		log.Internal.Info().Str("tip", hashToHex(hash)).Msg("told a newly connected peer where the chain is")
	}
}

// announceWithBackoff tells the peers about a block, but only those that are not
// still inside the wait that follows a failed attempt.
func (bp *blockProducer) announceWithBackoff(header *block.Header) {
	peers := bp.net.GetAllPeers()
	if len(peers) == 0 {
		return
	}

	now := time.Now()
	var due []*peer.Peer
	for _, p := range peers {
		if wait, ok := bp.announceBackoff[p.Address.String()]; ok && now.Before(wait) {
			continue
		}
		due = append(due, p)
	}
	if len(due) == 0 {
		return
	}

	go func() {
		for _, p := range due {
			ctx, cancel := context.WithTimeout(bp.ctx, 5*time.Second)
			err := bp.net.AnnounceBlock(ctx, header, p)
			cancel()
			if err != nil {
				bp.announceBackoff[p.Address.String()] = time.Now().Add(announceRetryWait)
				log.Internal.Debug().Err(err).Str("peer", p.Address.String()).Msg("could not announce the block to a peer; will wait before trying again")
				continue
			}
			delete(bp.announceBackoff, p.Address.String())
		}
	}()
}
