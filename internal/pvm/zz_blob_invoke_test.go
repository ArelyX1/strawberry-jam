package pvm

import (
	"fmt"
	"bytes"
	"encoding/binary"
	"os"
	"testing"
)

// Generic .polkavm (0.37) -> A.38 transpose + invoke at chosen entry.
func TestBlobInvoke(t *testing.T) {
	src := os.Getenv("BLOB")
	if src == "" {
		src = "/tmp/opencode/quake.pol"
	}
	entry := uint64(0)
	if v := os.Getenv("ENTRY"); v != "" {
		if _, err := fmt.Sscanf(v, "%d", &entry); err != nil {
			t.Fatal(err)
		}
	}
	args := []byte{}
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[:4]) != "PVM\x00" {
		t.Fatalf("bad magic %v", b[:4])
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
		n, sz := jamCompact(b[p:], &ln)
		if !n {
			t.Fatalf("varint at %d", p)
		}
		p += sz
		sections[sid] = b[p : p+int(ln)]
		p += int(ln)
	}
	ro := sections[2]
	rw := sections[3]
	c := sections[6]

	var stack uint64
	{
		mem := sections[1]
		if mem != nil {
			q := 0
			var ros, rws uint64
			for _, dst := range []*uint64{&ros, &rws, &stack} {
				n, sz := jamCompact(mem[q:], dst)
				if !n {
					t.Fatal("memconfig varint")
				}
				q += sz
			}
		} else if v := os.Getenv("A38_STACK"); v != "" {
			if _, err := fmt.Sscanf(v, "%d", &stack); err != nil {
				t.Fatal(err)
			}
		}
	}

	var blob bytes.Buffer
	writeLE := func(v uint64, n int) {
		for i := 0; i < n; i++ {
			blob.WriteByte(byte(v >> (8 * i)))
		}
	}
	writeLE(uint64(len(ro)), 3)
	writeLE(uint64(len(rw)), 3)
	writeLE(0, 2)
	writeLE(stack, 3)
	blob.Write(ro)
	blob.Write(rw)
	_ = binary.LittleEndian
	writeLE(uint64(len(c)), 4)
	blob.Write(c)
	bdata := blob.Bytes()

	prog, err := ParseBlob(bdata)
	if err != nil {
		t.Fatal("ParseBlob:", err)
	}
	t.Logf("sizes=%+v ro=%d rw=%d c=%d", prog.ProgramMemorySizes, len(prog.ROData), len(prog.RWData), len(prog.CodeAndJumpTable))
	if _, _, _, err := Deblob(prog.CodeAndJumpTable); err != nil {
		t.Fatal("Deblob:", err)
	}
	gas, result, _, err := InvokeWholeProgram(bdata, entry, UGas(2_000_000_000), args, hostCallProbe, AccumulateContextPair{})
	t.Logf("gasRemaining=%d result(%d)=%x err=%v", gas, len(result), result, err)
}

// hostCallProbe implements the subset of GP host calls the test guests use.
func hostCallProbe(c uint64, gas Gas, regs Registers, mem Memory, x AccumulateContextPair) (Gas, Registers, Memory, AccumulateContextPair, error) {
	switch c {
	case 0: // GasID
		gas -= 10 // host_call.GasRemainingCost
		regs[R7] = uint64(gas)
		return gas, regs, mem, x, nil
	}
	return gas, regs, mem, x, fmt.Errorf("ecalli %d", c)
}