package main

import (
	"testing"

	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/stretchr/testify/assert"
)

// The count is what a devnet sets: how many nodes are actually running. A count
// of one has to mean this node writes every timeslot, because on its own there
// is nobody else to write the rest, and a chain that only advances every other
// slot is a chain nobody can tell from a stalled one.
func TestAuthorForWithOneValidatorWritesEverySlot(t *testing.T) {
	for slot := jamtime.Timeslot(1); slot < 50; slot++ {
		for _, v := range []uint16{0, 1, 7} {
			assert.True(t, authorFor(slot, v, 1),
				"a lone node writes timeslot %d whatever index it is (%d)", slot, v)
		}
	}
}

// A timeslot has one author. If every validator wrote in every timeslot, two
// blocks would name the same slot, which is a fork rather than a merge, and the
// nodes would disagree for as long as they ran while each behaved correctly.
func TestAuthorForGivesEachTimeslotToOneValidator(t *testing.T) {
	seen := make(map[jamtime.Timeslot]int)
	for slot := jamtime.Timeslot(1); slot < 200; slot++ {
		authors := 0
		for v := 0; v < constants.NumberOfValidators; v++ {
			if authorFor(slot, uint16(v), 0) {
				authors++
			}
		}
		assert.Equal(t, 1, authors, "timeslot %d must have exactly one author", slot)
		seen[slot] = authors
	}
}

// The rule has to be the same on every node, and it has to reach every
// validator, or the chain stalls whenever the turn comes round to someone who
// never gets it.
func TestAuthorForRotatesThroughEveryValidator(t *testing.T) {
	counts := make([]int, constants.NumberOfValidators)
	for slot := jamtime.Timeslot(0); slot < jamtime.Timeslot(constants.NumberOfValidators*10); slot++ {
		for v := 0; v < constants.NumberOfValidators; v++ {
			if authorFor(slot, uint16(v), 0) {
				counts[v]++
			}
		}
	}
	assert.Len(t, counts, constants.NumberOfValidators)
	for v, c := range counts {
		assert.Equal(t, 10, c, "validator %d should author ten times in twenty timeslots", v)
	}
}

// Whatever the validator count is, one validator gets each timeslot and the rest
// do not, and a node agrees with itself. The first half is what stops the fork;
// the second is what stops a node writing two blocks for one slot.
func TestAuthorForIsOnePerTimeslotAndStable(t *testing.T) {
	for slot := jamtime.Timeslot(1); slot < 300; slot++ {
		authors := 0
		for v := 0; v < constants.NumberOfValidators; v++ {
			if authorFor(slot, uint16(v), 0) {
				authors++
			}
		}
		assert.Equal(t, 1, authors, "timeslot %d must have exactly one author", slot)
		assert.Equal(t, authorFor(slot, 0, 0), authorFor(slot, 0, 0), "a node must not change its mind")
	}
}

// The turn has to come back round, or the chain stalls for good on whichever
// validator drew the short straw and never gets to write again.
func TestAuthorForComesBackRound(t *testing.T) {
	v := uint16(3)
	assert.True(t, authorFor(jamtime.Timeslot(v), v, 0), "validator 3 authors its own slot")
	assert.True(t, authorFor(jamtime.Timeslot(v+constants.NumberOfValidators), v, 0),
		"and again after a full turn")
	assert.False(t, authorFor(jamtime.Timeslot(v+1), v, 0), "but not the next one")
}
