package pvm_test

import (
	"os"
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/pvm"
	"github.com/eigerco/strawberry/internal/pvm/host_call"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
)

// TestPAPUEconomyAccumulate runs the PAPU guest in accumulate mode against real
// service storage, using the entry point read from the blob's export table.
// Accumulate is distinguished from refine by the trailing NUL byte in the
// arguments, so the same export serves both.
func TestPAPUEconomyAccumulate(t *testing.T) {
	path := os.Getenv("BLOB")
	if path == "" {
		t.Skip("set BLOB=/tmp/opencode/papu-rs-econ.pol")
	}
	args, err := os.ReadFile(os.Getenv("ARGS_FILE"))
	if err != nil {
		t.Skip("set ARGS_FILE to the concatenated reports plus a trailing NUL")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := pvm.ParsePolkavmBlob(data)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	entry, err := blob.EntryPoint("main")
	if err != nil {
		t.Fatalf("entry: %v", err)
	}
	a38, err := blob.ToA38(0, 1<<13)
	if err != nil {
		t.Fatal(err)
	}

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
		}
		return gas, regs, mem, x, err
	}

	gas, result, _, err := pvm.InvokeWholeProgram(a38, entry, pvm.UGas(50_000_000_000), args, hostCall, pvm.AccumulateContextPair{})
	if err != nil {
		t.Fatalf("invoke at entry %d: gas=%d err=%v", entry, gas, err)
	}
	t.Logf("accumulate ok: gasRemaining=%d result(%d)=%x", gas, len(result), result)

	// The PAPU economy stores per-key values as 16-byte big-endian integers.
	// Claim markers live at 0x05+address (faucet) and 0x06+address (welcome).
	addr := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	claimKey := append([]byte{0x06}, addr...)
	k, err := statekey.NewStorage(serviceId, claimKey)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := account.GetStorage(k); ok {
		t.Logf("welcome claim marker = %x", v)
	} else {
		t.Errorf("welcome claim marker was not written")
	}

	balanceKey := append([]byte{0x02}, addr...)
	bk, err := statekey.NewStorage(serviceId, balanceKey)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := account.GetStorage(bk); ok {
		t.Logf("balance = %x", v)
	} else {
		t.Errorf("balance was not written")
	}

	supplyKey, err := statekey.NewStorage(serviceId, []byte{0x01})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := account.GetStorage(supplyKey); ok {
		t.Logf("supply = %x", v)
	} else {
		t.Errorf("supply was not written")
	}
}
