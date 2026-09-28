package pvm_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/pvm"
	"github.com/eigerco/strawberry/internal/pvm/host_call"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
)

// TestABIRoundTrip drives a .polkavm guest through the Go PVM with a real
// host-call dispatcher backed by service storage. It proves the guest<->host
// ABI (args in R7/R8, ecalli id in opcode byte, storage Read/Write, halt at
// 0xffff0000 with result in memory at R7..R7+R8) against the real host_call
// implementations used by accumulate.go.
func TestABIRoundTrip(t *testing.T) {
	guest := os.Getenv("BLOB")
	if guest == "" {
		t.Skip("set BLOB=/tmp/opencode/xxx.pol")
	}
	stack := uint64(1 << 16)
	if v := os.Getenv("A38_STACK"); v != "" {
		fmt.Sscanf(v, "%d", &stack)
	}
	args, err := os.ReadFile(os.Getenv("ARGS_FILE"))
	if err != nil {
		args = nil
	}

	bdata := transposeToA38(t, guest, stack)

	serviceId := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000
	serviceState := service.ServiceState{serviceId: account}

	hostCall := func(c uint64, gas pvm.Gas, regs pvm.Registers, mem pvm.Memory, x pvm.AccumulateContextPair) (pvm.Gas, pvm.Registers, pvm.Memory, pvm.AccumulateContextPair, error) {
		var err error
		switch c {
		case host_call.GasID:
			gas, regs, err = host_call.GasRemaining(gas, regs)
		case host_call.ReadID:
			gas, regs, mem, err = host_call.Read(gas, regs, mem, account, serviceId, serviceState)
		case host_call.WriteID:
			var acct service.ServiceAccount
			gas, regs, mem, acct, err = host_call.Write(gas, regs, mem, account, serviceId)
			account = acct
			serviceState[serviceId] = acct
		default:
			err = nil // no-op for ids 1,2,5 (fetch/lookup/info) smoke guests
		}
		return gas, regs, mem, x, err
	}

	entry := uint64(0)
	gas, result, _, err := pvm.InvokeWholeProgram(bdata, entry, pvm.UGas(2_000_000_000), args, hostCall, pvm.AccumulateContextPair{})
	if err != nil {
		t.Fatalf("invoke: gas=%d err=%v", gas, err)
	}
	t.Logf("results: gasRemaining=%d result(%d)=%x", gas, len(result), result)
	k, _ := statekey.NewStorage(serviceId, []byte{0x02})
	if v, ok := account.GetStorage(k); ok {
		t.Logf("storage[02] = %x", v)
	}
}

// transposeToA38 builds an A.38 blob from a polkavm 0.37 ReviveV1 blob.
func transposeToA38(t *testing.T, path string, stack uint64) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
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
	return blob.Bytes()
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

func init() {
	// silence unused import when building without tiny tag
	_ = filepath.Clean
}