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
	"sync"
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
	// mu guards announceBackoff, which the announcement goroutines write and the
	// producer loop reads.
	mu         sync.Mutex
	parentHash crypto.Hash
	blockNum   uint
	onBlock    func(crypto.Hash, uint, block.Header)
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
	// aheadAskedAt is when this node may next ask its peers for the chain that is
	// ahead of its own tip.
	aheadAskedAt time.Time
}

// How long past the end of a timeslot a node keeps waiting for the block the
// author of that timeslot writes, and how often it looks.
const (
	foreignSlotGrace = 3 * time.Second
	foreignSlotPoll  = 200 * time.Millisecond

	// Asking for the blocks that are missing is on the path of catching up, so it
	// has to be quick: the node is waiting to take part in its own timeslot while
	// this runs. A batch of a few, one block each, and no second peer.
	recoverBatch   = 4
	recoverTimeout = 2 * time.Second

	// aheadAskInterval is how often a node behind its peers says where its own
	// chain is, which is the only way to be told about the blocks above its tip.
	//
	// It is not every pass, because saying where the chain is opens a stream per
	// peer, and a node that is not behind would be doing that forever to nobody's
	// benefit. It is short enough that a node which has fallen behind is back
	// within a couple of timeslots rather than sitting on a stale tip, and long
	// enough that the streams are not the reason the node falls further behind.
	aheadAskInterval = 2 * time.Second
	// authorRetryWait is how long an author waits before looking again for the
	// block its timeslot has to build on. Long enough that holding a timeslot is
	// not a busy loop.
	authorRetryWait = 2 * time.Second
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

	// The genesis is the head of the chain only while nothing is built on it. The
	// leaf set is in memory and is not rebuilt on a restart, so a resumed node
	// would otherwise come up with no leaves at all and tip choice would find
	// nothing to do. Adding the genesis there unconditionally was worse than
	// adding nothing: the resumed node then believed the chain was still at
	// genesis and built its next block on top of it.
	//

	// The tip is the last block this node produced, and it is the point the chain
	// is at. The blocks are indexed by timeslot on the way, because every one of
	// them carries the work its timeslot ran and a claim about the state that has
	// to be checked during the replay.
	blocks := map[jamtime.Timeslot]block.Block{}
	tip, tipHash, found := bp.findTip(blocks)
	if !found {
		// Nothing to resume, so the chain is at the block it was founded at and
		// that is its head. The leaf set is what tip choice and executing a peer's
		// block both start from, and with it empty the node finds nothing to do
		// and sits still. A resumed node gets its real head further down instead.
		bs.AddLeaf(genesisHash, genesisHeader.TimeSlotIndex)
	}

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
		// The chain this node is actually on: its tip is the head, not the
		// genesis it was founded at.
		bs.ResetLeaves()
		bs.AddLeaf(tipHash, tipSlot)
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
	if current := jamtime.Now().ToTimeslot(); slot < current && canCarryOnFromThePresent(tipSlot, current) {
		// The node was asleep, or is joining a chain that has moved on. It does not
		// write the timeslots that passed.
		//
		// Every timeslot has exactly one author, and on a network the authors of
		// those timeslots are other nodes that have already written them. Writing
		// them again is a second block for a timeslot that already has one, and that
		// is not a near miss: it is how a node joining a mesh wrote twenty-eight
		// blocks while its peers wrote three each, and how two of them ended up on
		// different chains with no way back.
		//
		// Carrying on from the present is the same path from a different timeslot,
		// not a different path, so the loop starts either way.
		log.Internal.Info().
			Uint64("from", uint64(slot)).
			Uint64("to", uint64(current)).
			Msg("the timeslots that passed are not this node's to write; carrying on from the present")
		slot = current
	}

	go bp.run(slot)

	return bp
}

// canCarryOnFromThePresent reports whether the timeslots between the chain's tip
// and the current one are all written already, which is what makes skipping them
// safe.
//
// Skipping assumes someone else wrote them. Every timeslot has one author, and on a
// mesh that author is another node, so a node joining a chain that has moved on can
// start at the present and never write a timeslot twice.
//
// A mesh that is starting has no such assumption to lean on. The chain is at the
// genesis and the clock is already past it, so the timeslots between them have no
// block and no author has written one. Skipping them leaves every node waiting for a
// parent that the other was supposed to write and never will: the author of the
// present holds its timeslot for a block from before the present, and the author
// before it was skipped by the same rule. Nothing is ever written again and the
// chain is still from the genesis, with both nodes holding a timeslot and neither
// writing it.
//
// So the chain has to reach the present before the loop may start at the present.
// Until it does, the missing timeslots are written, by whoever is their author,
// which is what a mesh coming up is for.
func canCarryOnFromThePresent(tip, current jamtime.Timeslot) bool {
	return current <= tip+1
}

// run produces a block in the timeslots this validator authors, and executes the
// ones another validator wrote, following the clock.
//
// A timeslot has exactly one author, this node's index modulo the number of
// validators. Without that rule every validator wrote a block in every timeslot,
// and since two blocks for one slot are a fork and not a merge, the nodes spent
// the whole run disagreeing while each of them was behaving perfectly.
//
// Before writing, the node waits for the block of the timeslot before its own.
// The author of a timeslot writes it while that timeslot runs, so at the start of
// a timeslot the block that timeslot is about to name does not exist yet, and a
// node that wrote anyway would build on the block it had, which is the one before
// that. Two nodes then each hold a block for the same timeslot and the chain
// splits, and no amount of syncing afterwards puts it back together. Not writing
// is much better than writing on the wrong parent.
func (bp *blockProducer) run(slot jamtime.Timeslot) {
	// Whether to say out loud that this node is waiting. It is said once per
	// spell of waiting, not once per attempt.
	aviso := true
	for {
		bp.waitFor(slot)

		// Before anything else: if the chain has fallen behind the timeslot this
		// loop is on, do not move. Wait for it to arrive.
		//
		// The timeslot on this loop only moves forward, and the chain it is
		// following moves too, but not in step. A node that was slow, or that had
		// its blocks late, ends up with the chain a few timeslots behind the
		// timeslot the loop is on. It is then waiting for the block of a timeslot
		// that its own chain has not reached, and the node whose turn that timeslot
		// is — possibly this one — is waiting too, for a block nobody has written
		// because the chain is not there to build on. Every author is waiting for a
		// block that no one is going to write, and the mesh is stopped with all of
		// them healthy and connected.
		//
		// Advancing made it worse, not better: moving past a timeslot the chain has
		// not reached is not something a node can undo, and the timeslot it moves
		// onto is one it will never be the author of. Standing still until the
		// chain arrives is what lets this node write the timeslot that is actually
		// its own, as soon as the chain gets there.
		tip, _, haveTip := bp.bestKnownTip()
		if haveTip {
			tipSlot := tip.TimeSlotIndex
			// Only when the chain has not arrived AND this node is not the author of
			// the timeslot. If it is the author, waiting helps nobody: it is the one
			// that has to move the chain forward, and standing still while the chain
			// waits for it is a deadlock with everybody in it. When this node writes
			// this timeslot, it writes it on whatever the chain has, which is how the
			// chain catches up.
			if tipSlot < slot && !authorFor(slot, bp.authorIndex, bp.authorCount) {
				// The chain has not reached a timeslot that is not this node's. Ask
				// its peers where they are, so that it arrives, and try the same
				// timeslot again.
				bp.recoverBlocksAhead()
				bp.announceTipToNewPeers()
				if aviso {
					aviso = false
					log.Internal.Debug().Uint64("slot", uint64(slot)).
						Uint64("chainTip", uint64(tipSlot)).
						Msg("the chain has not reached this timeslot yet; waiting rather than going past it")
				}
				time.Sleep(foreignSlotPoll)
				continue
			}
		}
		aviso = true
		// Catching up comes before the turn and does not replace it. It used to be
		// a case of the same switch, which meant that a node that had caught up
		// skipped the rest of the timeslot: when the timeslot it was responsible
		// for arrived while it was behind, it caught up, took that as its turn
		// being done, and never wrote the block. Nobody else writes it either,
		// because there is one author per timeslot, so the chain stopped there.
		bp.catchUpBehind()
		switch {
		case authorFor(slot, bp.authorIndex, bp.authorCount) && !bp.timeslotHasABlock(slot):
			// Not writing and moving on is what stopped the chain. The next
			// author needs the block this one did not write, waits for a block
			// nobody is going to produce, and the chain stands still with every
			// node falling further behind the clock. So an author that cannot
			// build holds the timeslot it is responsible for and keeps trying: the
			// block it is missing is on its way from a peer, and the moment it
			// lands this node writes the timeslot that was always its own.
			//
			// Not writing at all is still better than writing on the wrong
			// parent, which is a fork rather than a pause.
			if !bp.waitUntilAtTip(slot) {
				// Hold the timeslot and try again, but slowly and quietly. Retrying
				// straight away with a line each time wrote hundreds of megabytes
				// of log in a couple of minutes and filled the disk: the wait
				// inside catchUpTo had already given up, so coming straight back
				// here was a tight loop. The block being waited on is on its way
				// from a peer and takes milliseconds, not microseconds.
				if aviso {
					aviso = false
					// Which of the two things this node is short of decides what
					// has to happen next, and the log has to say which.
					//
					// It said neither. It said it was waiting for the block this
					// timeslot builds on, which is what a node short of a block
					// says, and a node whose state will not replay says it too
					// while holding a block it already has. Those are different
					// faults with different fixes, and reading the wrong one off
					// this line costs the whole diagnosis.
					tip, _, hasTip := bp.bestKnownTip()
					tipSlot := jamtime.Timeslot(0)
					if hasTip {
						tipSlot = tip.TimeSlotIndex
					}
					ev := log.Internal.Warn().Uint64("slot", uint64(slot)).
						Uint64("tipSlot", uint64(tipSlot)).
						Uint64("stateSlot", uint64(bp.runtime.Timeslot())).
						Bool("hasTip", hasTip)
					switch {
					case !hasTip || tipSlot < slot-1:
						ev.Msg("waiting for the block this timeslot has to build on; holding the timeslot")
					case bp.runtime.Timeslot() < slot-1:
						// The block is here. What is behind is the state built from
						// it, which is the other half of being at the tip and the
						// half that goes wrong on its own: a node can hold the whole
						// chain and still refuse to build on it, because replaying
						// it did not arrive at the root the blocks were built on.
						ev.Msg("holding the block this timeslot builds on, but the state has not reached it; " +
							"the chain is not short of blocks here, it is short of a replay that agrees with them")
					default:
						ev.Msg("at the tip on both counts and still not writing; holding the timeslot")
					}
				}
				// Telling the peers where this node's chain is, again, is what the
				// hold is waiting for. The block this node is missing is on its way
				// from a peer, and a peer sends it because it was told about a chain
				// it does not have. A node that announced only when it wrote said
				// nothing for as long as it held, which is exactly when its peers
				// were waiting on it: the block they were missing was the one this
				// node wrote just as they arrived, and the announcement of it went
				// out into an empty peer list.
				//
				// So two nodes sat holding a timeslot each, both waiting for a block
				// the other had, and neither of them telling the other so.
				bp.announceTipToNewPeers()
				time.Sleep(authorRetryWait)
				continue
			}
			aviso = true
			bp.produceBlock(slot)
		default:
			// Not this node's timeslot, so it has to wait for the block the author
			// of that timeslot writes. And if the chain is not there yet, this loop
			// stays where it is.
			//
			// Walking forward anyway is what stopped the chain, and it is not a
			// pause that heals. The timeslot on this loop only moves forward, and
			// the only thing that lets this node write is the chain already being at
			// the timeslot before the one it is on, so the timeslot it walks onto
			// while the chain is behind it is a timeslot it can never write. Every
			// author reaches that at once when the chain slips, because the slip is
			// in the chain and not in any one node, and from then on every node is
			// waiting for a block only another waiting node could have written.
			if !bp.runForeignSlot(slot) {
				// Saying where this node's chain is, again, is what the wait is
				// for: the block is on its way from a peer, and a peer sends it
				// because it was told about a chain it does not have. A node that
				// announced only when it wrote said nothing for as long as it
				// waited, which is exactly when its peers were waiting on it.
				bp.announceTipToNewPeers()
				continue
			}
		}
		slot++
	}
}

// atTipFor reports whether this node has executed the block that the block for
// this timeslot has to build on, which is the block of the timeslot before it.
//
// The comparison this used to make was between this node's own state and the best
// block it knew of, and that could not fail. A node's own newest block is by
// definition a block it knows and has executed, so the two were always equal and
// every author thought it was level with the chain before writing.
//
// An author is one timeslot behind by the time its turn comes, which is the whole
// shape of the schedule: the previous timeslot's block is written by another
// validator while that timeslot runs. So the author has to wait for that one block,
// and asking "am I at the tip I know of" never notices its absence. Two nodes then
// take turns writing on top of their own last block, each skipping the other's
// timeslots, and neither one ever builds on the other.
//
// The other half of the damage is only visible much later. The node that skipped
// ahead is now many timeslots in front, and its neighbour, which waited honestly,
// needs blocks that are already outside the window an announcement carries. There is
// nothing left for it to ask for by name, because it never learned the hashes. The
// two are then a hundred blocks apart with no way back, and the chain is still for
// ever.
//
// Demanding the parent timeslot keeps the two within one block of each other, which
// is the distance the announcement window covers.
func (bp *blockProducer) atTipFor(slot jamtime.Timeslot) bool {
	tip, _, ok := bp.bestKnownTip()
	if !ok {
		// Nothing has been produced yet. Whoever's turn it is writes the first
		// block, and there is no parent timeslot to have executed.
		return true
	}
	if tip.TimeSlotIndex >= slot {
		// This timeslot's block is already the tip, so there is nothing to wait
		// for. This is the genesis on the way up, and a block that arrived from a
		// peer before we got here.
		return true
	}
	// The clock answers half of this question and no more. Reaching the
	// timeslot before this node's own says nothing about whether the block it
	// has to build on ever arrived: a node that lost that block is looking at
	// exactly the same clock as one that has it, and both said yes.
	//
	// Asking only the clock is what let an author write its timeslot on whatever
	// block it happened to be holding, which is the one before the missing one,
	// so the block it wrote named a parent the rest of the mesh had already
	// moved past. Two nodes then hold blocks for the same stretch of chain on
	// different parents, neither is a descendant of the other, and the timeslot
	// after that waits for a block that neither of them ever writes. The chain
	// stops with every node healthy and every peer connected.
	//
	// So the chain has to have got there as well. The tip is the furthest block
	// this node knows about, and the block a timeslot has to build on is the one
	// just before it, so a tip that is still further back than that is a node
	// that has not got it however long it waits.
	if tip.TimeSlotIndex < slot-1 {
		return false
	}
	return bp.runtime.Timeslot() >= slot-1
}

// waitUntilAtTip waits until this node has executed the block that the block for
// this timeslot builds on, and reports whether it got there.
//
// A block names as its parent the last block of the chain rather than the block of
// the timeslot immediately before, which is why this waits for state and not for a
// block by hash. What state has to be at is what the parent will be: the tip, once
// the tip is the timeslot before this one.
func (bp *blockProducer) waitUntilAtTip(slot jamtime.Timeslot) bool {
	if bp.atTipFor(slot) {
		return true
	}
	deadline := jamtime.Now().ToTime().Add(authorRetryWait * 4)
	for {
		if bp.atTipFor(slot) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		bp.catchUpBehind()
		time.Sleep(foreignSlotPoll)
	}
}

// timeslotHasABlock reports whether some block already names this timeslot.
//
// A node writes when the rotation says it is its turn. Two nodes briefly
// disagreeing about that means both write, and two blocks for one timeslot is a
// fork the chain does not come back from. One lookup turns that disagreement into
// nothing at all.
func (bp *blockProducer) timeslotHasABlock(slot jamtime.Timeslot) bool {
	for _, leaf := range bp.bs.Leaves() {
		if leaf.Slot != slot {
			continue
		}
		// A header is not a block, and a leaf whose block never arrived is a
		// timeslot that looks written and is not.
		//
		// This used to answer from the leaf alone. Leaves are added for every
		// header that arrives, so a timeslot whose header was announced before
		// its block was ever delivered counted as written, and the author of
		// that timeslot stood down. Nobody else writes it either, since there is
		// one author per timeslot, so the chain stopped on a timeslot that no
		// node had a block for and every node believed was dealt with.
		hash, err := leaf.Header.Hash()
		if err != nil {
			continue
		}
		if _, err := bp.bs.Store.GetBlock(hash); err == nil {
			return true
		}
	}
	return false
}

// catchUpBehind runs the chain this node is behind on, and reports whether there
// was anything to do.
//
// A node only executes during the timeslots it does not author, which with a mesh
// of N is one timeslot in N: a node joining a mesh of five could only execute a
// block every thirty seconds, so reaching a chain ten blocks ahead took five
// minutes and the mesh never settled. A node that is behind is not taking part in
// its own timeslot anyway, so it runs the chain it has and catches up at the speed
// of the clock. When it is level with the tip this does nothing at all.
func (bp *blockProducer) catchUpBehind() bool {
	header, leaf, ok := bp.bestKnownTip()
	if !ok {
		return false
	}
	// Asking for what is missing comes before the question of whether there is
	// anything to run, and not after it.
	//
	// This used to sit below the test that follows, which is to say it only ran
	// when the node was ahead of the clock. A node holding a timeslot is behind
	// the clock by definition: the block it is waiting for is the one the clock
	// has already gone past. So the hold was precisely the case in which the
	// node would not go and fetch the block it was holding the timeslot for,
	// and the gap it already knew the hash of sat there until something else
	// moved.
	bp.recoverMissingBlocks()

	// Asking what is missing only looks downwards, from the tips this node knows
	// about, and that is the whole of what it asked for. A node whose chain has
	// moved past it therefore had nothing to ask for, which is the wrong answer:
	// what it is missing is above its tip, and walking down from its own tip can
	// never reach it.
	//
	// It sat there saying it could not get to a timeslot whose block three of its
	// peers already had. It had the right peers connected the whole time, and it
	// had stopped asking twenty minutes earlier, and nothing in the log said so —
	// the timeslot it was waiting for looked exactly like a timeslot nobody had
	// written yet. Which is what it was, on the node: the block for it was in the
	// store of everybody else.
	//
	// So a node that is behind asks its peers where their chain is, and asks for
	// the stretch after its own tip by name. The peers answer with blocks this
	// node did not have, and the gap closes the same way a gap below the tip does.
	bp.recoverBlocksAhead()

	if header.TimeSlotIndex <= bp.runtime.Timeslot() {
		return false
	}
	// A block that never arrived cannot be executed, and waiting for it to arrive
	// on its own is waiting for nothing: announcements carry the head and the
	// stretch behind it, so a block missed early stops being inside the window long
	// before the node asks. The gap is named in the store, so it was asked for by
	// name above, from whatever peer answers, before concluding there is nothing to
	// run.
	// Only the part that is actually there, and only up to the tip.
	if ok, _ := bp.executeUpTo(header.TimeSlotIndex); !ok {
		return false
	}
	log.Internal.Info().
		Uint64("slot", uint64(bp.runtime.Timeslot())).
		Str("tip", hashToHex(leaf)).
		Msg("caught up with the chain")
	return true
}

// recoverMissingBlocks asks peers, by hash, for the blocks the store knows are
// missing between this node's state and the tip.
//
// The announcement path brings the head and a stretch behind it, which is enough
// for a node that is only a little behind and useless for one that lost a block
// early: that block is out of the window, and the node waits for a block that
// nothing is going to announce again. Asking by name is the difference between
// falling behind once and staying behind for good, and it is what makes a node
// that has been away or has just started converge on the chain it is on.
//
// It reports how many blocks it managed to get, which is zero most of the time.
// recoverBlocksAhead asks the peers where their chain is, so that the stretch above
// this node's own tip arrives.
//
// Everything else this node asks for is by name: it walks down from the tips it
// knows and asks for the hashes it finds missing on the way. That covers the whole
// of what is behind it and none of what is in front. A node that has fallen behind
// has blocks it cannot name, because a block nobody has told it about is not in
// its store to be asked for by name, and the one way to learn about it is to say
// where this node's chain stops.
//
// It is the same message a peer sends when it announces a block, and it is the
// message that carries the stretch behind the announced head. So announcing this
// node's own tip is the request: a peer that is further along answers with the
// blocks in between.
func (bp *blockProducer) recoverBlocksAhead() int {
	header, hash, ok := bp.bestKnownTip()
	if !ok {
		return 0
	}
	if bp.net == nil || bp.ctx == nil {
		return 0
	}

	// Saying where the chain is is not free and it is not the answer to a question
	// the peer asked, so it is not done on every pass. A node that is waiting for a
	// block to arrive asks for it now and then, and a node that is not behind does
	// not ask at all.
	bp.mu.Lock()
	now := time.Now()
	if now.Before(bp.aheadAskedAt) {
		bp.mu.Unlock()
		return 0
	}
	bp.aheadAskedAt = now.Add(aheadAskInterval)
	bp.mu.Unlock()

	gotten := 0
	var wg sync.WaitGroup
	for _, p := range bp.net.GetAllPeers() {
		if p == nil || p.ProtoConn == nil || p.ProtoConn.TConn == nil {
			continue
		}
		wg.Add(1)
		go func(p *peer.Peer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(bp.ctx, recoverTimeout)
			defer cancel()
			// The blocks a peer sends back are handled by the same path any other
			// block takes, so there is nothing to do with them here but count that
			// something came back.
			if err := bp.net.AnnounceBlock(ctx, &header, p); err != nil {
				log.Internal.Debug().Err(err).
					Str("tip", hashToHex(hash)).
					Msg("could not ask a peer for the chain ahead of this node")
				return
			}
			bp.mu.Lock()
			defer bp.mu.Unlock()
			gotten++
			log.Internal.Debug().Str("tip", hashToHex(hash)).
				Uint64("tipSlot", uint64(header.TimeSlotIndex)).
				Msg("asked a peer for the chain ahead of this node")
		}(p)
	}
	wg.Wait()
	return gotten
}

func (bp *blockProducer) recoverMissingBlocks() int {
	hashes := bp.bs.BackfillHashes(recoverBatch)
	if len(hashes) == 0 {
		return 0
	}
	// Nowhere to ask. This is now reached on every pass rather than only when the
	// node is ahead of the clock, so a producer that has no network yet has to
	// answer for itself instead of dereferencing nothing.
	if bp.net == nil || bp.ctx == nil {
		return 0
	}
	var peers []*peer.Peer
	for _, p := range bp.net.GetAllPeers() {
		if p == nil || p.ProtoConn == nil || p.ProtoConn.TConn == nil {
			continue
		}
		peers = append(peers, p)
	}
	if len(peers) == 0 {
		return 0
	}

	gotten := 0
	for _, p := range peers {
		for _, h := range hashes {
			ctx, cancel := context.WithTimeout(bp.ctx, recoverTimeout)
			blocks, err := bp.net.RequestBlocks(ctx, h, false, 1, p.ProtoConn.TConn.PeerKey())
			cancel()
			if err != nil {
				continue
			}
			for _, b := range blocks {
				if err := bp.bs.StoreImportedBlock(b); err != nil {
					continue
				}
				gotten++
			}
		}
		if gotten > 0 {
			break
		}
	}
	if gotten > 0 {
		log.Internal.Info().Int("blocks", gotten).
			Msg("asked for the blocks that were missing and got them")
	}
	return gotten
}

// catchUpTo executes the chain up to and including the given timeslot, waiting up
// to the end of the timeslot after that one plus a grace period. It reports
// whether the state got there.
func (bp *blockProducer) catchUpTo(target jamtime.Timeslot, what string) bool {
	if bp.runtime.Timeslot() >= target {
		return true
	}
	// The block for the timeslot being waited on is written while that timeslot
	// runs, so the wait has to reach the end of the timeslot after it. Stopping
	// earlier is what left nodes permanently a block behind: they were waiting
	// for something that had not been written yet, on a schedule that guaranteed
	// they would stop looking before it appeared.
	deadline := jamtime.FromTimeslot(target + 2).ToTime().Add(foreignSlotGrace)
	said := ""
	for {
		ok, why := bp.executeUpTo(target)
		if ok {
			return true
		}
		if said == "" {
			said = why
			log.Internal.Debug().Uint64("slot", uint64(target)).
				Msg("cannot get to this timeslot yet: " + why)
		}
		if !time.Now().Before(deadline) {
			log.Internal.Debug().Uint64("slot", uint64(target)).
				Msg("gave up waiting for " + what)
			return false
		}
		time.Sleep(foreignSlotPoll)
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
// It reports whether the chain got there. The answer is what the loop that calls
// it runs on, and throwing it away is what stopped the chain.
//
// Advancing past a timeslot the chain has not reached is not something a node can
// undo. The timeslot this loop is on only ever moves forward, and the one thing
// that decides whether this node writes is whether the chain is already at the
// timeslot before the one it is on. So a node that walked its timeslot forward
// past a stretch the chain had not caught up to reached a timeslot it could never
// write: its chain was behind it and getting further behind, every author was
// waiting for a block that only the node ahead of it could write, and all of them
// were waiting. Nothing recovers from that, because the thing that would recover
// it is the one thing that cannot go back.
//
// Any work in the chain is enough to cause it. A timeslot that settles work takes
// the author longer than a timeslot that settles none, so the chain falls a slot
// or two behind, and the node whose timeslot it is walks forward into the gap it
// just made and stands still there for good.
func (bp *blockProducer) runForeignSlot(slot jamtime.Timeslot) bool {
	return bp.catchUpTo(slot, "the block this timeslot's author wrote")
}

// executeUpTo runs as much of the chain as this node can actually walk, up to and
// including the given timeslot, and reports whether it got there along with why
// not when it did not.
//
// It takes the unbroken run of blocks that ends at the tip rather than the whole
// chain. A node that has only just joined is missing something below almost every
// tip, and asking it to hold every block back to genesis before it executes
// anything means it executes nothing at all until the backfill has filled the lot:
// its state stands still while the chain grows past it, and the tip it publishes
// keeps naming a block its state has never reached. Executing the part that is
// there is what lets it catch up while the rest arrives.
func (bp *blockProducer) executeUpTo(slot jamtime.Timeslot) (bool, string) {
	_, leaf, ok := bp.bestKnownTip()
	if !ok {
		return false, "no tip known yet"
	}
	tipHeader, err := bp.bs.Store.GetHeader(leaf)
	if err != nil {
		return false, "the tip header is not in the store"
	}
	if tipHeader.TimeSlotIndex < slot {
		return false, fmt.Sprintf("the tip is at slot %d and this timeslot is %d", tipHeader.TimeSlotIndex, slot)
	}

	available := bp.bs.AvailableChain(leaf, 0)
	if len(available) == 0 {
		return false, "not one block of the chain is in the store yet"
	}
	last := available[len(available)-1].Header
	if last.TimeSlotIndex < slot {
		return false, fmt.Sprintf("the blocks run out at slot %d and this timeslot is %d", last.TimeSlotIndex, slot)
	}

	if bp.runtime.Timeslot() >= slot {
		bp.parentHash = leaf
		return true, ""
	}

	bySlot := make(map[jamtime.Timeslot]block.Block, len(available))
	through := bp.runtime.Timeslot()
	for _, b := range available {
		if b.Header.TimeSlotIndex <= bp.runtime.Timeslot() {
			continue
		}
		if b.Header.TimeSlotIndex > slot {
			break
		}
		bySlot[b.Header.TimeSlotIndex] = b
		if b.Header.TimeSlotIndex > through {
			through = b.Header.TimeSlotIndex
		}
	}
	if len(bySlot) == 0 {
		return false, fmt.Sprintf("the run of blocks starts at slot %d and the state stands at %d",
			available[0].Header.TimeSlotIndex, bp.runtime.Timeslot())
	}

	hechos, err := bp.replay(bp.runtime.Timeslot()+1, through, bySlot)
	if err != nil {
		return false, "replaying it did not rebuild the state: " + err.Error()
	}
	// The replay stops at a timeslot it has no block for, so it may not have got
	// as far as the timeslot asked for. Building on the block it never ran would
	// name a parent whose state is not the one this node is holding.
	if uint64(through-bp.runtime.Timeslot()) != hechos {
		bp.parentHash = leaf
		return true, ""
	}

	// The parent is the last block that was run, not the tip. The tip can be
	// several blocks ahead of the state, and building on it would name a parent
	// from the future: the block would not sit on the chain the node has, and the
	// next node to look at it would find a parent whose state does not match the
	// one the block claims. It is also what made two nodes that had reached the
	// same state still be on different blocks.
	lastRun, herr := bySlot[through].Header.Hash()
	if herr != nil {
		return false, "the block it just ran does not hash"
	}
	bp.parentHash = lastRun
	bp.blockNum = uint(through)
	return true, ""
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
	// Whatever happens in here, this node stops being a replaying node on the way
	// out, including when it gives up.
	//
	// It did not, and a node that had given up stayed in that state for good. Two
	// things follow from being one. The faucet reserve is not queued again while it
	// is set, so the node stopped funding itself; and work this node was asked to do
	// is only put back into the queue when it is not replaying, so it stopped
	// settling anything at all. A node that could not rebuild a chain once could
	// then never produce again, however many times it tried, and it said nothing
	// about why: it looked exactly like a node with nothing to do.
	defer bp.runtime.FinishRebuild()
	for slot := from; slot <= through; slot++ {
		b, ok := blocks[slot]
		if !ok {
			// A timeslot with no block here is a timeslot nobody has described.
			//
			// Running it anyway is what put two nodes on the same block with two
			// different states. A node that is behind executes everything that has
			// arrived in one go, and this loop used to walk every timeslot up to the
			// tip, stepping the ones with no block as if they had settled no work at
			// all. A node that had all the blocks ran those timeslots for real, so
			// the two arrived at different states and then published the same block
			// over each of them: same block, two roots, and nothing in the log to
			// say why.
			//
			// What happened in a timeslot with no block cannot be guessed, so the
			// replay stops at the hole and the node executes what it can actually
			// prove. The rest arrives later, and the next pass picks it up from
			// here. Stopping is also what lets it be at the right state at all:
			// inventing a timeslot is not a slower way of catching up, it is a
			// different state.
			return uint64(slot - from), nil
		}

		parentRoot := bp.runtime.Root()
		if b.Header.PriorStateRoot != parentRoot {
			return uint64(slot - from), fmt.Errorf(
				"the state rebuilt for timeslot %d is %x, but the block for that timeslot was built on %x",
				slot, parentRoot, b.Header.PriorStateRoot)
		}

		work := blockWork(b)
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

	// Only a tip that is behind where this node is gives it nothing to do. A tip
	// at the same slot is not behind, it is a rival block for the same timeslot,
	// and this is where the mesh used to come apart.
	//
	// Two nodes can both decide a timeslot is unwritten and both write it. Each
	// one is behaving correctly and each has no way to know about the other: the
	// block that settles the race is the one a peer happens to deliver first. That
	// is not supposed to matter, because bestKnownTip breaks the tie between two
	// blocks for one timeslot the same way on every node — lower hash wins — so
	// every node in the mesh agrees on which of the two is the chain.
	//
	// Agreeing on which one is the chain is not the same as being on it. This
	// refused to move unless the other tip was strictly further ahead, so a node
	// whose own block lost the tie sat on the branch that had just lost, wrote the
	// next timeslot on top of it, and lost the next tie as well, because its peers
	// were now a slot ahead and no longer tied with it. Four nodes, four blocks for
	// the timeslot, four chains, each one slot behind the last, and none of them
	// willing to step onto a chain of the same height because the rule only spoke
	// about chains that were further ahead.
	//
	// It does not need to know how many peers there are. Every node works out the
	// same answer from the same blocks, so however many devices the mesh runs on,
	// the ones holding the losing block step onto the winning one and the mesh is
	// one chain again.
	if tip.TimeSlotIndex < bp.lastSlot() {
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
	if bp.net == nil {
		return
	}
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

	var wg sync.WaitGroup
	for _, p := range fresh {
		wg.Add(1)
		go func(p *peer.Peer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(bp.ctx, 5*time.Second)
			err := bp.net.AnnounceBlock(ctx, &header, p)
			cancel()
			bp.mu.Lock()
			defer bp.mu.Unlock()
			if err != nil {
				bp.announceBackoff[p.Address.String()] = now.Add(announceRetryWait)
				log.Internal.Debug().Err(err).Str("tip", hashToHex(hash)).
					Msg("could not tell a newly connected peer where the chain is; will wait before trying again")
				return
			}
			delete(bp.announceBackoff, p.Address.String())
			log.Internal.Info().Str("tip", hashToHex(hash)).Msg("told a newly connected peer where the chain is")
		}(p)
	}
	go wg.Wait()
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

	// One at a time is not good enough once there is more than one peer. Each
	// announcement to a peer that has no announcer yet has to open a stream and
	// wait for its handshake, and that is allowed five seconds. With two peers
	// that is ten seconds of a six second timeslot, so the second peer was never
	// told about anything: it sat connected, receiving nothing, and could not
	// catch up with a chain it was never being told about. They go in parallel.
	var wg sync.WaitGroup
	for _, p := range due {
		wg.Add(1)
		go func(p *peer.Peer) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(bp.ctx, 5*time.Second)
			err := bp.net.AnnounceBlock(ctx, header, p)
			cancel()
			bp.mu.Lock()
			defer bp.mu.Unlock()
			if err != nil {
				bp.announceBackoff[p.Address.String()] = time.Now().Add(announceRetryWait)
				log.Internal.Debug().Err(err).Str("peer", p.Address.String()).
					Msg("could not announce the block to a peer; will wait before trying again")
				return
			}
			delete(bp.announceBackoff, p.Address.String())
		}(p)
	}
	go wg.Wait()
}
