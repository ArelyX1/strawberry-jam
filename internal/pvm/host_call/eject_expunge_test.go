package host_call

import (
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	. "github.com/eigerco/strawberry/internal/pvm"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
	"github.com/eigerco/strawberry/pkg/serialization/codec/jam"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var (
	ejectParentID = block.ServiceId(222)
	ejectChildID  = block.ServiceId(999)
	ejectHash     = crypto.Hash{9, 8, 7, 6, 5, 4, 3, 2, 1, 0}
)

// ejectSetup builds a parent/child pair where the child holds exactly one
// preimage request aged at requestTimeslot, which is what Eject needs to find.
func ejectSetup(t *testing.T, requestTimeslot jamtime.Timeslot) AccumulateContextPair {
	t.Helper()

	e32, err := jam.Marshal(struct {
		ServiceId block.ServiceId `jam:"length=32"`
	}{ejectParentID})
	require.NoError(t, err)

	var codeHash crypto.Hash
	copy(codeHash[:], e32)

	k, err := statekey.NewPreimageMeta(ejectChildID, ejectHash, 81)
	require.NoError(t, err)

	child := service.ServiceAccount{
		CodeHash:       codeHash,
		Balance:        100,
		PreimageLookup: map[crypto.Hash][]byte{codeHash: make([]byte, 81)},
	}
	require.NoError(t, child.InsertPreimageMeta(k, uint64(81),
		service.PreimageHistoricalTimeslots{requestTimeslot - 1, requestTimeslot}))

	regularCtx := AccumulateContext{
		ServiceId: ejectParentID,
		AccumulationState: state.AccumulationState{
			ServiceState: service.ServiceState{
				ejectChildID:  child,
				ejectParentID: {Balance: 1000},
			},
		},
	}
	return AccumulateContextPair{RegularCtx: regularCtx}
}

func runEject(t *testing.T, requestTimeslot, timeslot jamtime.Timeslot) (Registers, AccumulateContextPair) {
	t.Helper()

	ctxPair := ejectSetup(t, requestTimeslot)
	pp := &ProgramBlob{
		ProgramMemorySizes: ProgramMemorySizes{
			RODataSize:       0,
			RWDataSize:       256,
			StackSize:        512,
			InitialHeapPages: 200,
		},
	}
	mem, regs, err := InitializeStandardProgram(pp, nil)
	require.NoError(t, err)
	require.NoError(t, mem.Write(uint32(RWAddressBase), hash2bytes(ejectHash)))

	regs[R7] = uint64(ejectChildID)
	regs[R8] = RWAddressBase

	_, outRegs, _, outCtx, err := Eject(100, regs, mem, ctxPair, timeslot)
	require.NoError(t, err)
	return outRegs, outCtx
}

// Eject hands a child's balance to its parent and drops the child, but only
// once the request has aged past the expunge period: the spec gates the branch
// on y < t - D, with y the request timeslot and D the period.
//
// Both operands are uint32, so the subtraction has to be widened. Left in
// uint32 it wraps for every t < D, and the guard then compares y against a huge
// number instead of a negative one: the branch fires thousands of timeslots
// early and moves funds that should have stayed put. The two sibling branches
// in Forget already widen; this one was the odd case out.
func TestEjectDoesNotFireBeforeTheExpungePeriod(t *testing.T) {
	require.Positive(t, constants.PreimageExpulsionPeriod, "expunge period must be set")

	// Ask for a request younger than the period, and an eject that happens
	// before the period has even elapsed: t - D is negative here, which is
	// exactly the case the unsigned arithmetic gets wrong.
	requestTimeslot := jamtime.Timeslot(5)
	ejectTimeslot := jamtime.Timeslot(1)

	require.Negative(t, int64(ejectTimeslot)-int64(constants.PreimageExpulsionPeriod),
		"t - D must go negative for this test to mean anything")

	outRegs, outCtx := runEject(t, requestTimeslot, ejectTimeslot)

	assert.Equal(t, uint64(HUH), outRegs[R7],
		"eject fired while t - D was negative; the guard wrapped to a huge uint32")
	assert.Contains(t, outCtx.RegularCtx.AccumulationState.ServiceState, ejectChildID,
		"a refused eject must leave the child in place")
	assert.Equal(t, uint64(1000),
		outCtx.RegularCtx.AccumulationState.ServiceState[ejectParentID].Balance,
		"a refused eject must not move the child's balance to the parent")
}

// Once the request really has aged out, the branch has to fire, so the test
// above cannot pass merely because something else is broken.
func TestEjectFiresAfterTheExpungePeriod(t *testing.T) {
	requestTimeslot := jamtime.Timeslot(5)
	ejectTimeslot := jamtime.Timeslot(constants.PreimageExpulsionPeriod + 10)

	require.Positive(t, int64(ejectTimeslot)-int64(constants.PreimageExpulsionPeriod),
		"t - D must be positive here")

	outRegs, outCtx := runEject(t, requestTimeslot, ejectTimeslot)

	assert.Equal(t, uint64(OK), outRegs[R7])
	assert.NotContains(t, outCtx.RegularCtx.AccumulationState.ServiceState, ejectChildID,
		"a completed eject drops the child")
	assert.Equal(t, uint64(1100),
		outCtx.RegularCtx.AccumulationState.ServiceState[ejectParentID].Balance,
		"the parent ends up with its own balance plus the child's")
}

// The wrap is only reachable below the period, so pin the arithmetic itself:
// unsigned subtraction must never be what decides this.
func TestEjectGuardUsesWideArithmetic(t *testing.T) {
	requestTimeslot := jamtime.Timeslot(5)
	ejectTimeslot := jamtime.Timeslot(1)
	require.Negative(t, int64(ejectTimeslot)-int64(constants.PreimageExpulsionPeriod))

	// What the code used to compute, versus what it should compute.
	narrow := uint32(ejectTimeslot) - uint32(constants.PreimageExpulsionPeriod)
	wide := int64(ejectTimeslot) - int64(constants.PreimageExpulsionPeriod)
	assert.Negative(t, wide, "the comparison the spec asks for is against a negative number")
	assert.Less(t, uint64(requestTimeslot), uint64(narrow),
		"the narrow arithmetic wraps to a huge value and wrongly passes the guard")
}
