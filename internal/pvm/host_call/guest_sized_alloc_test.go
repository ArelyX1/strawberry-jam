package host_call

import (
	"math"
	"testing"

	. "github.com/eigerco/strawberry/internal/pvm"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every host call that reads a guest-supplied range sizes its buffer from a
// register the guest controls. Sizing first and validating afterwards is not a
// correctness bug, it is a fatal one: make([]byte, r) for a large enough r is a
// runtime throw rather than a panic, so no recover can catch it and the node
// process goes down. Verified empirically in isolation: make([]byte, 1<<40)
// prints "fatal error: runtime: out of memory" and the process exits.
//
// Worse, the calls run from InvokeHostCall, which is outside the recover in
// InvokeBasic: that defer has already returned by the time a host call body
// runs. These tests assert the calls answer with a panic instead, which is both
// the spec's answer for an inaccessible range and the one the process survives.
const oneTebibyte = uint64(1) << 40

func allocTestMemory(t *testing.T) Memory {
	t.Helper()
	pp := &ProgramBlob{
		ProgramMemorySizes: ProgramMemorySizes{
			RODataSize: 0, RWDataSize: 256, StackSize: 512, InitialHeapPages: 200,
		},
	}
	mem, _, err := InitializeStandardProgram(pp, nil)
	require.NoError(t, err)
	return mem
}

// Read must refuse an absurd key length rather than try to allocate it.
func TestReadRefusesGuestSizedAllocation(t *testing.T) {
	mem := allocTestMemory(t)

	// R7 = MaxUint64 means "my own account", otherwise Read answers NONE before
	// it ever looks at the key range.
	regs := Registers{R7: math.MaxUint64, R8: RWAddressBase, R9: oneTebibyte}

	_, _, _, err := Read(100, regs, mem, service.ServiceAccount{}, 0, service.ServiceState{})
	require.Error(t, err, "an absurd key length must be refused, not allocated")
	assert.Contains(t, err.Error(), "inaccessible memory")
}

// Write takes two ranges off the guest, so both have to be checked.
func TestWriteRefusesGuestSizedAllocations(t *testing.T) {
	t.Run("key", func(t *testing.T) {
		mem := allocTestMemory(t)
		regs := Registers{R7: 42, R8: RWAddressBase, R9: oneTebibyte}
		_, _, _, _, err := Write(100, regs, mem, service.ServiceAccount{}, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inaccessible memory")
	})

	t.Run("value", func(t *testing.T) {
		mem := allocTestMemory(t)
		// R9 is a sane key length, so only the value range is impossible.
		regs := Registers{R7: 42, R8: RWAddressBase, R9: 8, R10: RWAddressBase, R11: oneTebibyte}
		_, _, _, _, err := Write(100, regs, mem, service.ServiceAccount{}, 0)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "inaccessible memory")
	})
}

// Log used to log the read failure and carry on, answering success on memory
// the guest was never allowed to touch.
func TestLogRefusesGuestSizedAllocations(t *testing.T) {
	t.Run("message", func(t *testing.T) {
		mem := allocTestMemory(t)
		// R8 = 0 skips the target and goes straight to the message.
		regs := Registers{R8: 0, R10: RWAddressBase, R11: oneTebibyte}
		_, _, _, err := Log(100, regs, mem, nil, nil)
		require.Error(t, err)
	})

	t.Run("target", func(t *testing.T) {
		mem := allocTestMemory(t)
		// R8 = target address, R9 = target length; R10/R11 = the message.
		regs := Registers{R8: RWAddressBase, R9: oneTebibyte, R10: RWAddressBase, R11: 8}
		_, _, _, err := Log(100, regs, mem, nil, nil)
		require.Error(t, err, "an unreadable target must panic, not log and continue")
	})
}

// HasAccess is the guard these calls rely on, so pin its edges directly.
func TestHasAccessRejectsWhatItCannotAllocate(t *testing.T) {
	mem := allocTestMemory(t)

	assert.False(t, mem.HasAccess(RWAddressBase, oneTebibyte, ReadOnly),
		"a length beyond the address space is never reachable")
	assert.False(t, mem.HasAccess(math.MaxUint64, 1, ReadOnly),
		"an address beyond the address space is never reachable")
	assert.False(t, mem.HasAccess(0, 8, ReadOnly),
		"the first 64K are forbidden")
	assert.False(t, mem.HasAccess(math.MaxUint32-3, 8, ReadOnly),
		"a range that overflows uint32 is never reachable")
	assert.True(t, mem.HasAccess(RWAddressBase, 8, ReadOnly),
		"a range inside the writable segment is reachable")
	assert.True(t, mem.HasAccess(RWAddressBase, 0, ReadWrite),
		"an empty range is trivially reachable, which is what the spec means by it")
}
