package pvm

import (
	"fmt"
	"os"
	"testing"

	"bytes"
)

// Ground truth: ParseBlob+Deblob (GP A.38) consume service blobs whose c region
// is jam-encoded. polkavm 0.37 (ReviveV1) blobs carry the same payload
// (polyvarint == jam compact natural). Transpose a 0.37 sections blob into an
// A.38 blob with the jam codec and try to execute it in the Go PVM.
func TestReviveV1Transpose(t *testing.T) {
	b, err := os.ReadFile("/tmp/opencode/quake.pol")
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
		if p+int(ln) > len(b) {
			t.Fatalf("section overruns: sid %d ln %d at %d", sid, ln, p)
		}
		sections[sid] = b[p : p+int(ln)]
		p += int(ln)
	}
	mem := sections[1]
	ro := sections[2]
	rw := sections[3]
	c := sections[6]

	var ros, rws, stack uint64
	{
		q := 0
		for _, dst := range []*uint64{&ros, &rws, &stack} {
			n, sz := jamCompact(mem[q:], dst)
			if !n {
				t.Fatal("memconfig varint")
			}
			q += sz
		}
	}
	// fall back to real section byte lengths for ro/rw (the blob carries inflated
	// legacy-era rw virtual size); keep stack from config.
	ros = uint64(len(ro))
	rws = uint64(len(rw))
	roData, rwData := ro, rw

	var jtCount, codeLen uint64
	q := 0
	if n, sz := jamCompact(c[q:], &jtCount); !n {
		t.Fatal("jt varint")
	} else {
		q += sz
	}
	entrySize := c[q]
	q++
	if n, sz := jamCompact(c[q:], &codeLen); !n {
		t.Fatal("code len varint")
	} else {
		q += sz
	}
	t.Logf("c: q=%d jt=%d es=%d cl=%d len=%d", q, jtCount, entrySize, codeLen, len(c))

	// build A.38 manually: E3(ro) E3(rw) E2(heap) E3(stack) ro rw E4(|c|) c
	var blob bytes.Buffer
	writeLE := func(v uint64, n int) {
		for i := 0; i < n; i++ {
			blob.WriteByte(byte(v >> (8 * i)))
		}
	}
	writeLE(ros, 3)
	writeLE(rws, 3)
	writeLE(0, 2)
	writeLE(stack, 3)
	blob.Write(roData)
	blob.Write(rwData)
	writeLE(uint64(len(c)), 4)
	blob.Write(c)
	bdata := blob.Bytes()
	t.Logf("manual blob len=%d head=%x", len(bdata), bdata[:20])
	if err := os.WriteFile("/tmp/opencode/quake.a38v2", bdata, 0o644); err != nil {
		t.Fatal(err)
	}

	prog, err := ParseBlob(bdata)
	if err != nil {
		t.Fatal("ParseBlob:", err)
	}
	t.Logf("sizes=%+v ro=%d rw=%d c=%d", prog.ProgramMemorySizes, len(prog.ROData), len(prog.RWData), len(prog.CodeAndJumpTable))
	if _, _, _, err := Deblob(prog.CodeAndJumpTable); err != nil {
		t.Fatal("Deblob:", err)
	}
	gas, result, _, err := InvokeWholeProgram(bdata, 0, UGas(2_000_000_000), []byte{}, hostCallNoop, AccumulateContextPair{})
	t.Logf("gasRemaining=%d result=%d err=%v", gas, len(result), err)
}

func hostCallNoop(hostCall uint64, gas Gas, regs Registers, mem Memory, ctx AccumulateContextPair) (Gas, Registers, Memory, AccumulateContextPair, error) {
	return gas, regs, mem, ctx, fmt.Errorf("ecalli %d", hostCall)
}

func jamCompact(b []byte, out *uint64) (bool, int) {
	l := 0
	for x := b[0]; x&0x80 != 0; x <<= 1 {
		l++
	}
	if l+1 > len(b) {
		return false, 0
	}
	v := uint64(0)
	for i := 0; i < l; i++ {
		v |= uint64(b[i+1]) << (8 * i)
	}
	v |= uint64(b[0]&(0xff>>l)) << (8 * l)
	*out = v
	return true, l + 1
}
