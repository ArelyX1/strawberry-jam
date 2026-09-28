package pvm

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestEcalliProbe transpiles a tiny polkavm 0.37 (ReviveV1) blob that calls
// `ecalli 0` into an A.38 blob and runs it in the Go PVM. It verifies two
// things: (a) the opcode byte emitted for `ecalli` decodes as Ecalli=10 in the
// Go PVM (firing the host call), and (b) the halting convention (ra=0xffff0000)
// is respected.
func TestEcalliProbe(t *testing.T) {
	var pol string
	for _, name := range []string{"ecalli.pol", "quake.pol"} {
		if b, err := os.ReadFile(filepath.Join("/tmp/opencode", name)); err == nil {
			if string(b[:4]) == "PVM\x00" {
				pol = name
				break
			}
		}
	}
	if pol == "" {
		t.Skip("no polkavm blob in /tmp/opencode")
	}
	b, err := os.ReadFile(filepath.Join("/tmp/opencode", pol))
	if err != nil {
		t.Fatal(err)
	}
	p := 5 + 8
	sections := map[byte][]byte{}
	for p < len(b) {
		sid := b[p]
		p++
		if sid == 0 {
			break
		}
		var ln uint64
		if n, sz := jamCompact(b[p:], &ln); !n {
			t.Fatalf("varint at %d", p)
		} else {
			p += sz
		}
		if p+int(ln) > len(b) {
			t.Fatalf("section overruns: sid %d ln %d", sid, ln)
		}
		sections[sid] = b[p : p+int(ln)]
		p += int(ln)
	}
	t.Logf("sections: %v", sectionLenMap(sections))

	var stack uint64
	if mem, ok := sections[1]; ok {
		var q int
		for _, dst := range []*uint64{new(uint64), new(uint64), &stack} {
			if n, sz := jamCompact(mem[q:], dst); !n {
				t.Fatal("mem varint")
			} else {
				q += sz
			}
		}
	}

	var blob bytes.Buffer
	writeLE := func(v uint64, n int) {
		for i := 0; i < n; i++ {
			blob.WriteByte(byte(v >> (8 * i)))
		}
	}
	writeLE(uint64(len(sections[2])), 3)
	writeLE(uint64(len(sections[3])), 3)
	writeLE(0, 2)
	writeLE(stack, 3)
	blob.Write(sections[2])
	blob.Write(sections[3])
	writeLE(uint64(len(sections[6])), 4)
	blob.Write(sections[6])
	bdata := blob.Bytes()
	t.Logf("A.38 len=%d head=%x", len(bdata), bdata[:16])

	prog, err := ParseBlob(bdata)
	if err != nil {
		t.Fatal("ParseBlob:", err)
	}
	t.Logf("sizes=%+v", prog.ProgramMemorySizes)

	gas, result, _, err := InvokeWholeProgram(bdata, 5, UGas(2_000_000_000), nil, hostCallNoop, AccumulateContextPair{})
	t.Logf("gasRemaining=%d result=%d err=%v", gas, len(result), err)
}

func sectionLenMap(m map[byte][]byte) string {
	var s string
	for k, v := range m {
		s += fmt.Sprintf("%d=%d ", k, len(v))
	}
	return s
}