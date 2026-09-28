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

// TestPAPUEconomyAccumulateTranslated runs the PAPU guest in accumulate mode
// against real service storage, resolving both the entry point and the host
// call indices from the blob itself. The linker numbers ecalls by the order
// the guest first calls them, so the index a guest uses does not match the
// canonical host call id; the blob's import table is what maps between them.
func TestPAPUEconomyAccumulateTranslated(t *testing.T) {
	path := os.Getenv("BLOB")
	if path == "" {
		t.Skip("set BLOB=/tmp/opencode/papu-rs-econ.pol")
	}
	args, err := os.ReadFile(os.Getenv("ARGS_FILE"))
	if err != nil {
		t.Skip("set ARGS_FILE")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := pvm.ParsePolkavmBlob(data)
	if err != nil {
		t.Fatal(err)
	}
	entry, err := blob.EntryPoint("main")
	if err != nil {
		t.Fatal(err)
	}
	a38, err := blob.ToA38(0, 1<<13)
	if err != nil {
		t.Fatal(err)
	}

	imports, err := blob.Imports()
	if err != nil {
		t.Fatal(err)
	}
	// Map the guest's ecall index onto the host's canonical id, by name.
	translate := make(map[uint64]uint64, len(imports))
	for i, symbol := range imports {
		translate[uint64(i)] = hostCallIDByName(t, symbol)
		t.Logf("ecall %d = %q -> host id %d", i, symbol, translate[uint64(i)])
	}

	serviceId := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000
	serviceState := service.ServiceState{serviceId: account}

	hostCall := func(c uint64, gas pvm.Gas, regs pvm.Registers, mem pvm.Memory, x pvm.AccumulateContextPair) (pvm.Gas, pvm.Registers, pvm.Memory, pvm.AccumulateContextPair, error) {
		var err error
		id, ok := translate[c]
		if !ok {
			return gas, regs, mem, x, nil
		}
		switch id {
		case uint64(host_call.GasID):
			gas, regs, err = host_call.GasRemaining(gas, regs)
		case uint64(host_call.ReadID):
			gas, regs, mem, err = host_call.Read(gas, regs, mem, account, serviceId, serviceState)
		case uint64(host_call.WriteID):
			var acct service.ServiceAccount
			gas, regs, mem, acct, err = host_call.Write(gas, regs, mem, account, serviceId)
			account = acct
			serviceState[serviceId] = acct
		}
		return gas, regs, mem, x, err
	}

	gas, _, _, err := pvm.InvokeWholeProgram(a38, entry, pvm.UGas(50_000_000_000), args, hostCall, pvm.AccumulateContextPair{})
	if err != nil {
		t.Fatalf("invoke: gas=%d err=%v", gas, err)
	}
	t.Logf("accumulate ok: gasRemaining=%d", gas)

	addr := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	assertStorage(t, serviceId, account, 0x06, &addr, "welcome claim")
	assertStorage(t, serviceId, account, 0x02, &addr, "balance")
	assertStorage(t, serviceId, account, 0x01, nil, "supply")
}

// hostCallIDByName maps an import symbol such as "c3_read" to the host call it
// stands for, by looking for the canonical name inside the symbol.
func hostCallIDByName(t *testing.T, symbol string) uint64 {
	t.Helper()
	for _, candidate := range []struct {
		name string
		id   uint64
	}{
		{"read", uint64(host_call.ReadID)},
		{"write", uint64(host_call.WriteID)},
		{"gas", uint64(host_call.GasID)},
		{"fetch", uint64(host_call.FetchID)},
		{"lookup", uint64(host_call.LookupID)},
		{"info", uint64(host_call.InfoID)},
	} {
		if len(symbol) >= len(candidate.name) && contains(symbol, candidate.name) {
			return candidate.id
		}
	}
	t.Fatalf("import %q does not name a known host call", symbol)
	return 0
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

func assertStorage(t *testing.T, serviceId block.ServiceId, account service.ServiceAccount, domain byte, address *string, label string) {
	t.Helper()
	key := []byte{domain}
	if address != nil {
		key = append(key, *address...)
	}
	k, err := statekey.NewStorage(serviceId, key)
	if err != nil {
		t.Fatal(err)
	}
	v, ok := account.GetStorage(k)
	if !ok {
		t.Errorf("%s: not written", label)
		return
	}
	t.Logf("%s = %x", label, v)
}
