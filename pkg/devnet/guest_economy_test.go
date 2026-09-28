package devnet

import (
	"crypto/ed25519"
	"crypto/rand"
	"os"
	"testing"

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

	supply, err := rt.View().Supply()
	require.NoError(t, err)
	require.Equal(t, want, supply)
	t.Logf("supply = %s", supply)
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
		t.Skip("no guest blob; build it or set BLOB")
	}
	return blob
}
