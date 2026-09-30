package pvm

import (
	"bytes"
	"os"
	"testing"

	"github.com/eigerco/strawberry/pkg/serialization/codec/jam"
)

// The conformance vectors wrap a bare A.38 program in a JAM-encoded metadata
// blob, so ParseBlob only ever sees the program behind that prefix. This pins
// the framing to a real bootstrap blob: E4(|c|) is a fixed four-byte width, and
// reading it as compact silently yields a tiny bogus code length that rejects
// the program and makes the host skip the service entirely.
func TestParseBlobBootstrapFraming(t *testing.T) {
	raw, err := os.ReadFile("testdata/bootstrap_preimage.bin")
	if err != nil {
		t.Fatal(err)
	}

	buff := bytes.NewBuffer(raw)
	var metadata []byte
	if err := jam.NewDecoder(buff).Decode(&metadata); err != nil {
		t.Fatalf("decode metadata: %v", err)
	}
	if len(metadata) != 80 {
		t.Errorf("metadata length = %d, want 80", len(metadata))
	}

	program := buff.Bytes()
	if len(program) != 136975 {
		t.Fatalf("program after metadata = %d bytes, want 136975", len(program))
	}

	parsed, err := ParseBlob(program)
	if err != nil {
		t.Fatalf("ParseBlob: %v", err)
	}

	sizes := parsed.ProgramMemorySizes
	if sizes.RODataSize != 13600 {
		t.Errorf("RODataSize = %d, want 13600", sizes.RODataSize)
	}
	if sizes.RWDataSize != 40 {
		t.Errorf("RWDataSize = %d, want 40", sizes.RWDataSize)
	}
	if sizes.InitialHeapPages != 2 {
		t.Errorf("InitialHeapPages = %d, want 2", sizes.InitialHeapPages)
	}
	if sizes.StackSize != 8192 {
		t.Errorf("StackSize = %d, want 8192", sizes.StackSize)
	}
	if len(parsed.CodeAndJumpTable) != 123320 {
		t.Errorf("code length = %d, want 123320", len(parsed.CodeAndJumpTable))
	}
	if got := len(parsed.ROData); got != 13600 {
		t.Errorf("ROData length = %d, want 13600", got)
	}
	if got := len(parsed.RWData); got != 40 {
		t.Errorf("RWData length = %d, want 40", got)
	}
}
