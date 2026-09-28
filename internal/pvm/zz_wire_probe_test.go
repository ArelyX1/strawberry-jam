package pvm

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"

	"github.com/eigerco/strawberry/pkg/serialization/codec/jam"
)

func TestVectorWireTruth(t *testing.T) {
	vec, err := os.ReadFile("/tmp/opencode/vector_blob.hex")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := hex.DecodeString(string(bytes.TrimSpace(vec))[2:])
	if err != nil {
		t.Fatal(err)
	}

	var metaLen uint64
	buff := bytes.NewBuffer(raw)
	if err := jam.NewDecoder(buff).Decode(&metaLen); err != nil {
		t.Fatal("metalength", err)
	}
	var meta []byte
	if err := jam.NewDecoder(buff).Decode(&meta); err != nil {
		t.Fatal("meta", err)
	}
	t.Logf("metaLen=%d len(meta)=%d", metaLen, len(meta))
	rest := make([]byte, buff.Len())
	copy(rest, buff.Bytes())

	p, err := ParseBlob(rest)
	if err != nil {
		t.Fatal("ParseBlob:", err, "len rest", len(rest))
	}
	t.Logf("ProgramMemorySizes=%+v", p.ProgramMemorySizes)
	t.Logf("ro=%d rw=%d codeLen=%d", len(p.ROData), len(p.RWData), len(p.CodeAndJumpTable))
	code, bitmask, jt, err := Deblob(p.CodeAndJumpTable)
	if err != nil {
		t.Fatal("Deblob:", err)
	}
	t.Logf("deblob ok code=%d bitmask=%d jt=%d", len(code), len(bitmask), len(jt))
	t.Logf("c-header hex: %s", hex.EncodeToString(p.CodeAndJumpTable[:16]))
}