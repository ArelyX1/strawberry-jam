package handlers

import (
	"encoding/binary"
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/chain"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	"github.com/eigerco/strawberry/pkg/serialization/codec/jam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The header carries an epoch marker with one key pair per validator, so it is as
// big as the validator set, and the marker only rides along on the first timeslot
// of an epoch. That makes the header two different sizes in normal operation, and
// the announcement has to carry both without either being mistaken for the other.
//
// This is the round trip that used to fail with "unmarshal header: decoding struct
// field 'EpochMarker': unexpected EOF": the header was written into a buffer sized
// for a header from a much smaller validator set and the marker fell off the end.
func TestAnnouncementCarriesAHeaderOfEitherSize(t *testing.T) {
	finalized := chain.LatestFinalized{TimeSlotIndex: 7}
	for i := range finalized.Hash {
		finalized.Hash[i] = byte(i)
	}

	headerWithMarker := block.Header{
		ParentHash:    crypto.Hash{1},
		TimeSlotIndex: 200,
		EpochMarker:   &block.EpochMarker{},
	}
	headerWithoutMarker := block.Header{
		ParentHash:    crypto.Hash{1},
		TimeSlotIndex: 100,
	}

	headers := map[string]block.Header{
		"sin marcador de epoca": headerWithoutMarker,
		"con marcador de epoca": headerWithMarker,
	}

	sizes := map[int]bool{}
	for name, header := range headers {
		hb, err := header.Bytes()
		require.NoError(t, err, name)

		content := serializeAnnouncement(hb, finalized)
		require.Greater(t, len(content), len(hb), name)

		// Read it back the way the receiver does.
		headerLen := int(binary.LittleEndian.Uint32(content))
		require.Equal(t, len(hb), headerLen, "%s: la longitud del mensaje no es la de la cabecera", name)
		require.Equal(t, len(hb)+4+36, len(content), name)

		var got block.Header
		require.NoError(t, jam.Unmarshal(content[4:4+headerLen], &got), name)

		gotHash, err := got.Hash()
		require.NoError(t, err, name)
		wantHash, err := header.Hash()
		require.NoError(t, err, name)
		assert.Equal(t, wantHash, gotHash, "%s: la cabecera no llega entera", name)
		assert.Equal(t, header.TimeSlotIndex, got.TimeSlotIndex, name)

		finalAt := 4 + headerLen
		assert.Equal(t, finalized.Hash, crypto.Hash(content[finalAt:finalAt+32]), name)
		assert.Equal(t, uint32(finalized.TimeSlotIndex), binary.LittleEndian.Uint32(content[finalAt+32:]), name)
		sizes[headerLen] = true
	}

	// And the point of the exercise: the two are genuinely different sizes, so a
	// fixed offset could not have read both.
	assert.Len(t, sizes, 2, "las dos cabeceras deberian ocupar distinto sitio en el mensaje")
}

// A message whose stated header does not fit is refused with a number to look at,
// rather than being read into past the end of the buffer.
func TestAnnouncementRejectsAHeaderThatDoesNotFit(t *testing.T) {
	content := serializeAnnouncement(make([]byte, 400), chain.LatestFinalized{})

	// Claim a header far longer than the message carries.
	binary.LittleEndian.PutUint32(content, 9000)

	ba := &BlockAnnouncer{}
	err := ba.processAnnouncement(content)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "9000")
}

// An announcement too short to hold even the length and the finalized block is
// refused before anything is read out of it.
func TestAnnouncementRejectsATruncatedMessage(t *testing.T) {
	ba := &BlockAnnouncer{}
	require.Error(t, ba.processAnnouncement(make([]byte, 8)))
}

// Deciding whether a block may be announced means walking back to the finalized
// block, and the walk needs the ancestors. A node that has fallen behind does
// not have them, and that is not a block from another chain: it is a block whose
// ancestors are exactly the ones it has not been given yet.
//
// Refusing to announce it is what stopped a chain between two nodes holding one
// timeslot each. Both had the block only the other could announce, each was
// waiting on an announcement from the other, and neither could send one. The
// request that follows an announcement is what brings the ancestors in, so a
// block nobody can prove is on the right side of the finalized block has to be
// announced anyway.
func TestABlockWhoseAncestorsAreMissingIsStillAnnounced(t *testing.T) {
	finalized := chain.LatestFinalized{TimeSlotIndex: 7}
	for i := range finalized.Hash {
		finalized.Hash[i] = byte(i)
	}

	bs := newTestChainService(t)
	bs.UpdateLatestFinalized(finalized.Hash, finalized.TimeSlotIndex)
	ba := &BlockAnnouncer{
		BlockService: bs,
		announced:    map[crypto.Hash]*block.Header{},
	}

	// The parent of this header is in no store anywhere, so the walk cannot
	// reach the finalized block.
	orphan := block.Header{
		ParentHash:    crypto.Hash{0xaa},
		TimeSlotIndex: 300,
	}

	shouldAnnounce, err := ba.shouldAnnounce(&orphan)
	require.NoError(t, err,
		"a header whose ancestors are missing is not an error to announce")
	assert.True(t, shouldAnnounce,
		"a block whose ancestors this node lacks has to be announced, or it is "+
			"only ever learned about by a peer that is equally behind")

	// The negative case still holds: a block that is on the chain is announced
	// once and then left alone, and one already announced is not sent twice.
	// The finalized block itself, so a walk that reaches its height finds it and
	// the answer can be yes. Without it in the store the walk cannot finish and
	// every block here would look like an ancestor this node lacks.
	finalizedHeader := block.Header{
		ParentHash:    crypto.Hash{0xff},
		TimeSlotIndex: finalized.TimeSlotIndex,
	}
	require.NoError(t, bs.Store.PutHeader(finalizedHeader))
	finalizedHash, err := finalizedHeader.Hash()
	require.NoError(t, err)
	bs.UpdateLatestFinalized(finalizedHash, finalized.TimeSlotIndex)

	// A block whose parent is the finalized block, and that parent is in the
	// store, so the walk reaches the finalized block and the answer is yes.
	complete := &block.Header{
		ParentHash:    finalizedHash,
		TimeSlotIndex: finalized.TimeSlotIndex + 1,
	}
	require.NoError(t, bs.Store.PutHeader(*complete))
	first, err := ba.shouldAnnounce(complete)
	require.NoError(t, err)
	require.True(t, first, "the first announcement of a known block must be sent")

	hash, err := complete.Hash()
	require.NoError(t, err)
	_ = hash
	ba.announced[hash] = complete

	again, err := ba.shouldAnnounce(complete)
	require.NoError(t, err)
	assert.False(t, again,
		"a block already announced to this peer must not be announced again")
}

// newTestChainService builds a chain service over a store of its own, with the
// genesis at the origin so a test can lay blocks on it.
func newTestChainService(t *testing.T) *chain.BlockService {
	t.Helper()
	db, err := pebble.NewKVStore()
	require.NoError(t, err)
	bs, err := chain.NewBlockService(db, 0)
	require.NoError(t, err)
	return bs
}
