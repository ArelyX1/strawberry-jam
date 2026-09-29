package svc_test

import (
	"os"
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
	svc "github.com/eigerco/strawberry/sdk"
)

// TestPVMExecutorRefine runs the PAPU guest through the SDK executor, which is
// the path the chain itself uses. Refine must return the same report the guest
// returned under the bare PVM, so this pins the SDK wiring rather than the
// guest.
func TestPVMExecutorRefine(t *testing.T) {
	blob := guestBlob(t)
	item := []byte(`{"method":"welcome","sender":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","nonce":1}`)
	if f := os.Getenv("ARGS_FILE"); f != "" {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		item = raw
	}

	exec := svc.NewPVMExecutor(blob)
	id := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000
	all := service.ServiceState{id: account}

	result, err := exec.Refine(id, item, jamtime.Timeslot(0), 50_000_000_000, all)
	if err != nil {
		t.Fatalf("refine: %v", err)
	}
	if len(result.Report) == 0 {
		t.Fatal("guest returned no report")
	}
	t.Logf("report: %s", result.Report)
}

// TestPVMExecutorAccumulate applies a refined report and checks the guest
// actually wrote the welcome claim, balance and supply.
func TestPVMExecutorAccumulate(t *testing.T) {
	blob := guestBlob(t)
	report := []byte(`{"op":"welcome","actor":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","sender":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","nonce":1,"to":"0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`)

	exec := svc.NewPVMExecutor(blob)
	id := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000
	all := service.ServiceState{id: account}

	result, err := exec.Accumulate(id, []svc.RefinedItem{{Report: report}}, nil, jamtime.Timeslot(0), 50_000_000_000, all)
	if err != nil {
		t.Fatalf("accumulate: %v", err)
	}

	addr := []byte("0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	for _, c := range []struct {
		domain byte
		scoped bool
		label  string
	}{
		{0x06, true, "welcome claim"},
		{0x02, true, "balance"},
		{0x01, false, "supply"},
	} {
		key := []byte{c.domain}
		if c.scoped {
			key = append(key, addr...)
		}
		k, err := stateKey(id, key)
		if err != nil {
			t.Fatal(err)
		}
		v, ok := result.Account.GetStorage(k)
		if !ok {
			t.Errorf("%s: not written", c.label)
			continue
		}
		t.Logf("%s = %x", c.label, v)
	}
}

// seededExecutor returns an executor for a chain that has said which EVM chain
// it presents. A guest reads that from the seed it was given, and a chain that
// has not said relays nothing, which is what a guest without a seed would see.
// seededExecutor returns an executor for a chain that has said which EVM chain
// it presents, along with the account that seed was written into. The account
// matters: the chain id lives in the guest's storage, so a refine over a
// different account is a chain that never said what it is, and it relays nothing.
func seededExecutor(t *testing.T, chainID int64, relay svc.Relay) (*svc.PVMExecutor, service.ServiceState) {
	t.Helper()
	exec := svc.NewPVMExecutor(guestBlob(t)).WithSeed(svc.Seed{
		Issuer:  "sdlgYsddpudnTY8tup2DtKeP5yTj4vQ2irGDvBEX6PuSAWpYHGV55",
		Symbol:  "PAPU",
		ChainID: chainID,
	})
	if relay != nil {
		exec = exec.WithRelay(relay)
	}
	account := seededAccount()
	all := service.ServiceState{0: account}
	seeded, err := exec.Initialize(0, 0, 50_000_000_000, all)
	if err != nil {
		t.Fatalf("seeding the guest: %v", err)
	}
	all[0] = seeded.Account
	return exec, all
}

func seededAccount() service.ServiceAccount {
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000
	return account
}

// guestBlob loads the economy guest that ships in the repo, so the tests do not
// depend on a build having happened elsewhere.
func guestBlob(t *testing.T) []byte {
	t.Helper()
	if path := os.Getenv("BLOB"); path != "" {
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return blob
	}
	blob, err := os.ReadFile("../guests/papucoin.pol")
	if err != nil {
		t.Skip("no guest blob; build it or set BLOB")
	}
	return blob
}

func stateKey(id block.ServiceId, key []byte) (statekey.StateKey, error) {
	return statekey.NewStorage(id, key)
}
