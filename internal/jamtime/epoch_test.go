package jamtime

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/eigerco/strawberry/internal/constants"
)

// epochSeconds is how long an epoch lasts in the chain this binary was built for.
//
// It is read from the chain spec rather than written down because the spec decides
// it: mainnet runs 600 timeslots to the epoch and the development spec runs 12. A
// test that says 3600 is not testing the protocol, it is testing one chain spec,
// and it fails the moment the node is built for another.
func epochSeconds(t *testing.T) uint64 {
	t.Helper()
	return uint64(constants.TimeslotsPerEpoch) * uint64(TimeslotDuration.Seconds())
}

func TestEpoch_FromEpoch(t *testing.T) {
	t.Run("first epoch", func(t *testing.T) {
		jt := FromEpoch(0)
		assert.Equal(t, uint64(0), jt.Seconds)
	})

	t.Run("arbitrary epoch", func(t *testing.T) {
		jt := FromEpoch(100)
		assert.Equal(t, 100*epochSeconds(t), jt.Seconds)
	})

	t.Run("last possible epoch", func(t *testing.T) {
		jt := FromEpoch(7158278)
		assert.Equal(t, 7158278*epochSeconds(t), jt.Seconds)
	})
}

func TestEpoch_ToEpoch(t *testing.T) {
	t.Run("start of first epoch", func(t *testing.T) {
		jt := JamTime{Seconds: 0}
		epoch := jt.ToEpoch()
		assert.Equal(t, Epoch(0), epoch)
	})

	t.Run("middle of arbitrary epoch", func(t *testing.T) {
		jt := JamTime{Seconds: epochSeconds(t) + 1} // 1 second into the second epoch
		epoch := jt.ToEpoch()
		assert.Equal(t, Epoch(1), epoch)
	})

	t.Run("end of last possible epoch", func(t *testing.T) {
		last, err := FromEpoch(MaxEpoch).Add(EpochDuration - time.Second)
		assert.NoError(t, err)
		assert.Equal(t, MaxEpoch, last.ToEpoch())
	})
}

func TestEpoch_CurrentEpoch(t *testing.T) {
	currentEpoch := CurrentEpoch()
	now := time.Now().UTC()
	assert.Equal(t, Epoch((now.Unix()-JamEpoch.Unix())/int64(epochSeconds(t))), currentEpoch)
}

func TestEpoch_EpochStart(t *testing.T) {
	t.Run("first epoch", func(t *testing.T) {
		start := Epoch(0).EpochStart()
		expected := uint64(0)
		assert.Equal(t, expected, start.Seconds)
	})

	t.Run("arbitrary epoch", func(t *testing.T) {
		start := Epoch(100).EpochStart()
		assert.Equal(t, 100*epochSeconds(t), start.Seconds)
	})
}

func TestEpoch_EpochEnd(t *testing.T) {
	t.Run("first epoch", func(t *testing.T) {
		end, err := Epoch(0).EpochEnd()
		assert.Nil(t, err)
		assert.Equal(t, epochSeconds(t)-1, end.Seconds)
	})

	t.Run("arbitrary epoch", func(t *testing.T) {
		end, err := Epoch(100).EpochEnd()
		assert.Nil(t, err)
		assert.Equal(t, 101*epochSeconds(t)-1, end.Seconds)
	})

	t.Run("max epoch returns jam time for last timeslot", func(t *testing.T) {
		jamTime, err := MaxEpoch.EpochEnd()
		assert.Nil(t, err)
		expected := FromTimeslot(MaxTimeslot)
		assert.Equal(t, expected.Seconds, jamTime.Seconds)
	})
}

func TestEpoch_NextEpoch(t *testing.T) {
	t.Run("from first epoch", func(t *testing.T) {
		next, err := Epoch(0).NextEpoch()
		assert.NoError(t, err)
		assert.Equal(t, Epoch(1), next)
	})

	t.Run("call to NextEpoch at MaxEpoch causes error", func(t *testing.T) {
		_, err := MaxEpoch.NextEpoch()
		assert.ErrorIs(t, err, ErrMaxEpochReached)
	})
}

func TestEpoch_PreviousEpoch(t *testing.T) {
	t.Run("from second epoch", func(t *testing.T) {
		prev, err := Epoch(1).PreviousEpoch()
		assert.NoError(t, err)
		assert.Equal(t, Epoch(0), prev)
	})

	t.Run("call to PreviousEpoch at MinEpoch causes error", func(t *testing.T) {
		_, err := MinEpoch.PreviousEpoch()
		assert.ErrorIs(t, err, ErrMinEpochReached)
	})
}

func TestEpoch_ValidateEpoch(t *testing.T) {
	t.Run("valid epoch", func(t *testing.T) {
		err := ValidateEpoch(1000)
		assert.NoError(t, err)
	})

	t.Run("min valid epoch", func(t *testing.T) {
		err := ValidateEpoch(MinEpoch)
		assert.NoError(t, err)
	})

	t.Run("max valid epoch", func(t *testing.T) {
		err := ValidateEpoch(MaxEpoch)
		assert.NoError(t, err)
	})

	t.Run("Epoch too large", func(t *testing.T) {
		err := ValidateEpoch(MaxEpoch + 1)
		assert.ErrorIs(t, err, ErrEpochExceedsMaxJamTime)
	})
}
