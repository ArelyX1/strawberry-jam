package main

import (
	"time"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/log"
)

type blockProducer struct {
	bs          *chain.BlockService
	authorIndex uint16
	parentHash  crypto.Hash
	blockNum    uint
	onBlock     func(crypto.Hash, uint, block.Header)
}

func startBlockProducer(bs *chain.BlockService, authorIndex uint16, onBlock func(crypto.Hash, uint, block.Header)) *blockProducer {
	bp := &blockProducer{
		bs:          bs,
		authorIndex: authorIndex,
		onBlock:     onBlock,
	}
	time.Sleep(500 * time.Millisecond)

	genesisHeader, _, err := bs.Store.FindHeader(func(h block.Header) bool { return true })
	if err != nil {
		log.Internal.Warn().Msg("no genesis found, starting from scratch")
		return bp
	}
	genesisHash, err := genesisHeader.Hash()
	if err != nil {
		log.Internal.Warn().Msg("genesis hash failed")
		return bp
	}
	bp.parentHash = genesisHash
	bp.blockNum = 1

	slot := genesisHeader.TimeSlotIndex + 1
	now := jamtime.Now()
	currentSlot := now.ToTimeslot()

	if slot <= currentSlot {
		for slot <= currentSlot {
			bp.produceBlock(slot)
			slot++
			time.Sleep(10 * time.Millisecond) // slow catch-up
		}
	} else {
		nextStart := jamtime.FromTimeslot(slot).ToTime()
		delay := time.Until(nextStart)
		if delay > 0 {
			time.Sleep(delay)
		}
		bp.produceBlock(slot)
		slot++
	}

	go func() {
		ticker := time.NewTicker(jamtime.TimeslotDuration)
		defer ticker.Stop()
		for range ticker.C {
			bp.produceBlock(slot)
			slot++
		}
	}()

	return bp
}

func (bp *blockProducer) produceBlock(slot jamtime.Timeslot) {
	header := block.Header{
		ParentHash:       bp.parentHash,
		PriorStateRoot:   crypto.Hash{},
		ExtrinsicHash:    crypto.Hash{},
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

	b := block.Block{Header: header}

	if err := bp.bs.Store.PutBlock(b); err != nil {
		log.Internal.Error().Err(err).Msg("failed to store block")
		return
	}

	if err := bp.bs.HandleNewHeader(&header); err != nil {
		log.Internal.Warn().Err(err).Str("hash", hashToHex(hash)).Uint64("slot", uint64(slot)).Msg("HandleNewHeader warning")
	}

	bp.parentHash = hash
	bp.blockNum++

	num := bp.blockNum
	log.Internal.Info().
		Str("hash", hashToHex(hash)).
		Uint64("slot", uint64(slot)).
		Uint("number", num).
		Uint64("epoch", uint64(slot.ToEpoch())).
		Msg("block produced")

	if bp.onBlock != nil {
		bp.onBlock(hash, num, header)
	}
}
