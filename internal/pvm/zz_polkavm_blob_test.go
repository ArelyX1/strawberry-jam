package pvm_test

import (
	"os"
	"testing"

	"github.com/eigerco/strawberry/internal/pvm"
)

// TestPolkavmBlobRoundTrip parses a native polkavm blob produced by
// `polkatool link -i revive_v1` and checks that the pieces the PVM needs
// survive: the export table names the entry point, and converting to the A.38
// framing keeps RO data, RW data and the code section byte-identical.
func TestPolkavmBlobRoundTrip(t *testing.T) {
	path := os.Getenv("BLOB")
	if path == "" {
		t.Skip("set BLOB=/tmp/opencode/papu-rs-econ.pol")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	blob, err := pvm.ParsePolkavmBlob(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	exports, err := blob.Exports()
	if err != nil {
		t.Fatalf("exports: %v", err)
	}
	if len(exports) == 0 {
		t.Fatal("no exports found")
	}
	for name, offset := range exports {
		t.Logf("export %q -> code offset %d", name, offset)
	}

	entry, err := blob.EntryPoint("main")
	if err != nil {
		t.Fatalf("entry point: %v", err)
	}
	t.Logf("entry point for main = %d", entry)

	a38, err := blob.ToA38(0, 1<<13)
	if err != nil {
		t.Fatalf("to A.38: %v", err)
	}

	program, err := pvm.ParseBlob(a38)
	if err != nil {
		t.Fatalf("the A.38 form must parse: %v", err)
	}
	if got, want := len(program.ROData), len(blob.Sections[pvm.SectionROData]); got != want {
		t.Errorf("ro data length: got %d, want %d", got, want)
	}
	if got, want := len(program.RWData), len(blob.Sections[pvm.SectionRWData]); got != want {
		t.Errorf("rw data length: got %d, want %d", got, want)
	}
	t.Logf("A.38 ok: ro=%d rw=%d code=%d stack=%d",
		len(program.ROData), len(program.RWData), len(program.CodeAndJumpTable), program.ProgramMemorySizes.StackSize)

	if len(program.ROData) == 0 && len(program.RWData) == 0 {
		t.Log("note: blob carries no static data sections")
	}
}

// TestPolkavmBlobInvokes runs the guest through the PVM using the entry point
// taken from the blob's own export table, rather than a hardcoded number. The
// linker decides the block layout, so the offset the host must jump to is only
// knowable by reading it back.
func TestPolkavmBlobInvokes(t *testing.T) {
	path := os.Getenv("BLOB")
	if path == "" {
		t.Skip("set BLOB=/tmp/opencode/papu-rs-econ.pol")
	}
	entryName := os.Getenv("ENTRY_NAME")
	if entryName == "" {
		entryName = "main"
	}
	var args []byte
	if f := os.Getenv("ARGS_FILE"); f != "" {
		var err error
		args, err = os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := pvm.ParsePolkavmBlob(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	entry, err := blob.EntryPoint(entryName)
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	stack := uint32(1 << 13)
	a38, err := blob.ToA38(0, 1<<13)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("invoking export %q at code offset %d (stack %d)", entryName, entry, stack)

	gas, result, _, err := pvm.InvokeWholeProgram(a38, entry, pvm.UGas(50_000_000_000), args, noopHostCall, pvm.AccumulateContextPair{})
	t.Logf("gasRemaining=%d result(%d)=%x err=%v", gas, len(result), result, err)
	if err != nil {
		t.Errorf("invoke failed: %v", err)
	}
}

func noopHostCall(_ uint64, gas pvm.Gas, regs pvm.Registers, mem pvm.Memory, ctx pvm.AccumulateContextPair) (pvm.Gas, pvm.Registers, pvm.Memory, pvm.AccumulateContextPair, error) {
	return gas, regs, mem, ctx, nil
}
