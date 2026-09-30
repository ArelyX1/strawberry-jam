package pvm

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// initializeTestMemory builds memory laid out like the bootstrap blob, which is
// the only program the conformance vectors run.
func initializeTestMemory(t *testing.T) (*Memory, error) {
	t.Helper()
	roData := make([]byte, 13600)
	rwData := make([]byte, 40)
	mem, err := InitializeMemory(roData, rwData, nil, 8192, 2)
	return &mem, err
}

// sbrk grows the writable segment as the guest asks for more heap. The segment
// is indexed from rw.address but sbrk names absolute pages, and getting that
// conversion wrong over-allocates by rw.address bytes while leaving rw.end
// describing the old extent, so the pages just handed out read back as
// inaccessible.
func TestSbrkExtendsTheWritableSegmentByExactlyWhatWasAskedFor(t *testing.T) {
	mem, err := initializeTestMemory(t)
	require.NoError(t, err)

	startPage := mem.rw.end / PageSize
	before := len(mem.rw.data)
	beforeEnd := mem.rw.end

	// Ask for two more pages than the segment currently covers.
	mem.allocatePages(startPage, 2)

	wantLen := before + 2*PageSize
	assert.Equal(t, wantLen, len(mem.rw.data),
		"rw.data must grow by the pages requested, no more")
	assert.Equal(t, mem.rw.address+uint32(len(mem.rw.data)), mem.rw.end,
		"rw.end must track the grown data or the pages read back inaccessible")

	// The pages sbrk just handed out have to be reachable, or the guest faults
	// on memory it was just given.
	for page := startPage; page < startPage+2; page++ {
		assert.NotEqual(t, Inaccessible, mem.GetAccess(page),
			"page %d was allocated by sbrk but is not accessible", page)
	}
	assert.Greater(t, mem.rw.end, beforeEnd)
}

// A page before rw.address is not ours to hand out; growing towards one must
// not underflow the relative offset.
func TestAllocatePagesDoesNotUnderflow(t *testing.T) {
	mem, err := initializeTestMemory(t)
	require.NoError(t, err)

	before := len(mem.rw.data)
	end := mem.rw.end

	mem.allocatePages(0, 1)

	assert.Equal(t, before, len(mem.rw.data))
	assert.Equal(t, end, mem.rw.end)
}

// Allocating a range already covered is a no-op rather than a shrink.
func TestAllocatePagesWithinExistingSegmentDoesNotShrink(t *testing.T) {
	mem, err := initializeTestMemory(t)
	require.NoError(t, err)

	before := len(mem.rw.data)
	end := mem.rw.end

	mem.allocatePages(mem.rw.address/PageSize, 1)

	assert.Equal(t, before, len(mem.rw.data))
	assert.Equal(t, end, mem.rw.end)
}
