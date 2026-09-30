package devnet

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"math/big"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
	svc "github.com/eigerco/strawberry/sdk"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// guestGenesis returns the test genesis pointed at a polkavm guest blob, so the
// economy runs through the PVM rather than the native Go handlers. The service
// account and the genesis seeding are the same either way, which is what makes
// the two comparable.
func guestGenesis(t *testing.T) *Genesis {
	t.Helper()
	blob := shippedGuest(t)
	path := t.TempDir() + "/guest.pol"
	require.NoError(t, os.WriteFile(path, blob, 0o600))

	genesis := testGenesis(t)
	genesis.Service.Code = path
	return genesis
}

// TestGuestEconomyInTheChain drives the PAPU economy as a polkavm guest through
// the devnet runtime: a welcome is refined, accumulated, and the claim, the
// balance and the supply have to move. Nothing here reaches into the guest, so
// this is the chain's own path being exercised.
func TestGuestEconomyInTheChain(t *testing.T) {
	rt, err := New(Options{Genesis: guestGenesis(t)})
	require.NoError(t, err)

	if _, ok := rt.executor.(*svc.PVMExecutor); !ok {
		t.Fatalf("expected the runtime to run the guest, got %T", rt.executor)
	}

	// Faucet is the one method a fresh key can use without an opening balance,
	// and it is signed for real, so the guest has to verify a genuine signature
	// rather than take the sender's word.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	address, err := papucoin.AddressFromPublicKey(pub)
	require.NoError(t, err)

	item, err := papucoin.SignItem(papucoin.Item{Method: "faucet", Sender: address, Nonce: 1, MustBeSigned: true}, priv)
	require.NoError(t, err)
	require.NoError(t, rt.Submit(item))

	before := rt.Root()
	require.NoError(t, rt.Step(rt.Timeslot()))
	require.NotEqual(t, before, rt.Root(), "processing an item must change the state root")

	id := rt.PapucoinID()
	account := rt.State().Services[id]

	for _, c := range []struct {
		domain byte
		scoped bool
		label  string
	}{
		{0x05, true, "faucet claim"},
		{0x02, true, "balance"},
		{0x01, false, "supply"},
	} {
		key := []byte{c.domain}
		if c.scoped {
			key = append(key, address...)
		}
		k, err := statekey.NewStorage(id, key)
		require.NoError(t, err)
		v, ok := account.GetStorage(k)
		require.True(t, ok, "%s was not written", c.label)
		require.NotEmpty(t, v, "%s is empty", c.label)
		t.Logf("%s = %x", c.label, v)
	}

	// The guest must have credited exactly what the native service credits, so
	// the balance is read back through the same view the RPC uses.
	balance, err := rt.View().Balance(address)
	require.NoError(t, err)
	want := rt.Params().FaucetAmount
	require.Equal(t, want, balance, "the guest must credit the same faucet amount as the native service")
	t.Logf("balance = %s, faucet amount = %s", balance, want)

	// The supply is the opening endowment plus the payout, since the guest
	// wrote the genesis balances itself. What has to hold is that the payout
	// moved it by exactly the faucet amount.
	supply, err := rt.View().Supply()
	require.NoError(t, err)
	require.NotZero(t, supply)
	t.Logf("supply = %s, faucet = %s", supply, want)
}

// shippedGuest loads the economy guest committed to the repo, so the chain test
// runs against the same blob that ships rather than a local build.
func shippedGuest(t *testing.T) []byte {
	t.Helper()
	if path := os.Getenv("BLOB"); path != "" {
		blob, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return blob
	}
	blob, err := os.ReadFile("../../guests/papucoin.pol")
	if err != nil {
		// The blob travels with the repository, so it being unreadable means the
		// checkout is broken or the file was dropped, not that this test has
		// nothing to say. Skipping here would let a green run hide a chain whose
		// entire economy is missing, which is the one thing these tests exist
		// to notice.
		t.Fatalf("the guest blob that ships in the repository cannot be read: %v", err)
	}
	return blob
}

func bridgeKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

// The transfer fee is state, and this chain can run the economy two ways: the
// service written in Go, or the same economy as a polkavm guest. Both read the
// same storage key with the same constants and both have to arrive at the same
// figure, or a client is quoted one price by one node and charged another by
// the next. A chain whose two implementations disagree on a number is not
// running one chain, it is running two.
func TestNativeAndGuestChargeTheSamePrice(t *testing.T) {
	// One burst past the target, run through both chains: the price has to move
	// off where it started, so the comparison is somewhere other than the
	// resting figure where both would agree for the wrong reason.
	senders := make([]ed25519.PrivateKey, papucoin.FeeBusyTarget+1)
	addresses := make([]string, len(senders))
	for i := range senders {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		addr, err := papucoin.AddressFromPublicKey(pub)
		require.NoError(t, err)
		senders[i], addresses[i] = priv, addr
	}

	run := func(t *testing.T, genesis *Genesis) *big.Int {
		t.Helper()
		rt, err := New(Options{Genesis: genesis})
		require.NoError(t, err)

		start := papucoin.CurrentFee(context.Background(), rt.View(), rt.Params())

		for i, addr := range addresses {
			fund, err := papucoin.SignItem(papucoin.Item{
				Method: "faucet", Sender: addr, Nonce: 1, MustBeSigned: true,
			}, senders[i])
			require.NoError(t, err)
			require.NoError(t, rt.Submit(fund))
			require.NoError(t, rt.Step(rt.Timeslot()))

			move, err := papucoin.SignItem(papucoin.Item{
				Method: "transfer", Sender: addr, Nonce: 2,
				To: addresses[0], Amount: "1", MustBeSigned: true,
			}, senders[i])
			require.NoError(t, err)
			require.NoError(t, rt.Submit(move))
			require.NoError(t, rt.Step(rt.Timeslot()))
		}

		// The price the workload left behind, read before anything else happens
		// to it: read afterwards it would be compared with itself.
		busy := papucoin.CurrentFee(context.Background(), rt.View(), rt.Params())

		// Then a run of blocks that carry nothing at all. An idle block is still
		// evidence that the chain is quiet, so it has to bring the price down, and
		// both implementations have to do it: this is where a service that
		// returned early on an empty block and one that did not would drift apart,
		// and the two would then quote different prices for the same chain.
		for i := 0; i < 3; i++ {
			require.NoError(t, rt.Step(rt.Timeslot()))
		}

		end := papucoin.CurrentFee(context.Background(), rt.View(), rt.Params())
		if genesis.Service.Code != "" {
			assert.NotEqual(t, start.String(), busy.String(),
				"a workload past the target has to move the price, or it is a tariff again")
			assert.Less(t, end.Cmp(busy), 0,
				"blocks that carry nothing have to bring the price back down")
		}
		return end
	}

	native := run(t, testGenesis(t))
	guest := run(t, guestGenesis(t))

	assert.Equal(t, native.String(), guest.String(),
		"the same workload has to leave both implementations at the same price")
}
