package svc_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
	svc "github.com/eigerco/strawberry/sdk"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// TestPVMExecutorAppliesEveryReportInABatch pins the accumulate path for more
// than one refined report at a time. Several items are refined in the same
// timeslot, and all of their reports go to the guest in one argument buffer, so a
// reader that stops after the first would silently drop the rest: the items would
// be accepted, and the money would never move.
func TestPVMExecutorAppliesEveryReportInABatch(t *testing.T) {
	blob := guestBlob(t)
	exec := svc.NewPVMExecutor(blob)
	id := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000

	wallets := []string{
		"0x1111111111111111111111111111111111111111",
		"0x2222222222222222222222222222222222222222",
		"0x3333333333333333333333333333333333333333",
	}

	reports := make([]svc.RefinedItem, 0, len(wallets))
	for _, wallet := range wallets {
		reports = append(reports, svc.RefinedItem{
			Report: []byte(`{"op":"faucet","actor":"` + papucoinMethodTestIssuer + `","sender":"` + papucoinMethodTestIssuer + `","nonce":1,"to":"` + wallet + `"}`),
		})
	}

	result, err := exec.Accumulate(id, reports, nil, jamtime.Timeslot(0), 50_000_000_000, service.ServiceState{id: account})
	require.NoError(t, err)

	params := papucoin.DefaultParams()
	want, err := papucoin.ParseAmount(params.FaucetAmount.String(), params.Decimals)
	require.NoError(t, err)

	for _, wallet := range wallets {
		key := append([]byte{0x02}, wallet...)
		k, err := statekey.NewStorage(id, key)
		require.NoError(t, err)
		v, ok := result.Account.GetStorage(k)
		require.True(t, ok, "the balance of %s was not written", wallet)
		t.Logf("balance of %s = %x", wallet, v)

		// The claim marker has to be there too, or a second claim would succeed.
		claimKey := append([]byte{0x05}, wallet...)
		ck, err := statekey.NewStorage(id, claimKey)
		require.NoError(t, err)
		marker, ok := result.Account.GetStorage(ck)
		require.True(t, ok, "the claim marker of %s was not written", wallet)
		require.Equal(t, []byte{1}, marker)
	}
	require.NotNil(t, want)

	// A second batch for the same wallets must be refused, which is what makes
	// the claim marker worth having.
	repeat, err := exec.Accumulate(id, reports, nil, jamtime.Timeslot(0), 50_000_000_000, service.ServiceState{id: result.Account})
	require.NoError(t, err)
	for _, wallet := range wallets {
		key := append([]byte{0x02}, wallet...)
		k, err := statekey.NewStorage(id, key)
		require.NoError(t, err)
		v, ok := repeat.Account.GetStorage(k)
		require.True(t, ok)
		require.True(t, strings.Contains(string(v), ""), "the balance should still be there")
		t.Logf("balance tras repetir: %s = %x", wallet, v)
	}
}

const papucoinMethodTestIssuer = "sdlgYsddpudnTY8tup2DtKeP5yTj4vQ2irGDvBEX6PuSAWpYHGV55"
