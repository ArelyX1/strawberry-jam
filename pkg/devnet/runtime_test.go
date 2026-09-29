package devnet

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/sdk/papucoin"
)

// repoGenesis is the genesis the tool generates from node-ts, so the tests
// check the economy that actually ships.
const repoGenesis = "../../genesis/chain-dev.json"

// testGenesis is a genesis whose funded accounts are keys the test holds, which
// is what lets the test sign real transfers instead of only reading balances.
func testGenesis(t *testing.T) *Genesis {
	t.Helper()

	genesis, err := LoadGenesis(repoGenesis)
	require.NoError(t, err)
	issuer, err := genesis.Service.IssuerAddress()
	require.NoError(t, err)
	genesis.Service.Accounts = accounts(t, map[string]string{
		issuer:          "500000",
		addressOf(t, 1): "1000",
		addressOf(t, 2): "50",
	})
	return genesis
}

func newTestRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New(Options{Genesis: testGenesis(t)})
	require.NoError(t, err)
	return rt
}

func TestShippedGenesisCarriesThePAPUEconomy(t *testing.T) {
	genesis, err := LoadGenesis(repoGenesis)
	require.NoError(t, err)
	require.NotNil(t, genesis.Service)

	assert.Equal(t, "PAPU", genesis.Service.Symbol)
	assert.Equal(t, uint8(12), genesis.Service.Decimals)
	assert.Equal(t, "1000000000", genesis.Service.MaxSupply, "the whole supply is a billion PAPU")
	assert.NotEmpty(t, genesis.Service.Issuer)
	assert.Len(t, genesis.Service.Accounts, 8)
	assert.EqualValues(t, 5120, genesis.EVM.ChainID, "the gateway presents chain 5120 to MetaMask")
}

func TestGenesisRejectsIncompleteDefinitions(t *testing.T) {
	for name, mutate := range map[string]func(*Genesis){
		"no service":         func(g *Genesis) { g.Service = nil },
		"no issuer":          func(g *Genesis) { g.Service.Issuer = "" },
		"no accounts":        func(g *Genesis) { g.Service.Accounts = nil },
		"too many decimals":  func(g *Genesis) { g.Service.Decimals = 19 },
		"bad endowment":      func(g *Genesis) { g.Service.Endowment = "much" },
		"no timeslots":       func(g *Genesis) { g.TimeslotSecs = 0 },
		"no network":         func(g *Genesis) { g.Network = "" },
		"fee with no digits": func(g *Genesis) { g.Service.TransferFee = "." },
		"trailing point":     func(g *Genesis) { g.Service.WelcomeAmount = "5." },
		"two points":         func(g *Genesis) { g.Service.FaucetAmount = "1.2.3" },
		"exponent":           func(g *Genesis) { g.Service.MaxSupply = "1e9" },
		"empty supply":       func(g *Genesis) { g.Service.MaxSupply = "" },
		"negative supply":    func(g *Genesis) { g.Service.MaxSupply = "-1" },
		"unknown field":      func(g *Genesis) { g.Service.Symbol = ""; g.Extra = 1 },
		"no evm chain id":    func(g *Genesis) { g.EVM.ChainID = 0 },
		"evm no symbol":      func(g *Genesis) { g.EVM.Symbol = "" },
		"no evm decimals":    func(g *Genesis) { g.EVM.Decimals = 0 },
		"evm below raw":      func(g *Genesis) { g.EVM.Decimals = g.Service.Decimals - 1 },
	} {
		t.Run(name, func(t *testing.T) {
			genesis := testGenesis(t)
			mutate(genesis)
			path := filepath.Join(t.TempDir(), "genesis.json")
			write(t, path, genesis)

			_, err := LoadGenesis(path)
			assert.Error(t, err, "a genesis with %s must be refused", name)
		})
	}
}

func TestGenesisSeedsEveryBalanceAndTheSupply(t *testing.T) {
	rt := newTestRuntime(t)
	view := rt.View()

	supply, err := view.Supply()
	require.NoError(t, err)
	total := new(big.Int)
	for _, amount := range rt.Genesis().Service.Accounts {
		parsed, err := papucoin.ParseAmount(amount.Amount, rt.Genesis().Service.Decimals)
		require.NoError(t, err)
		total.Add(total, parsed)
	}
	assert.Equal(t, total.String(), supply.String(), "the supply in existence is what genesis opened")

	balances, err := rt.Genesis().Service.InitialBalances()
	require.NoError(t, err)
	for address, amount := range balances {
		balance, err := view.Balance(address)
		require.NoError(t, err)
		want, err := papucoin.ParseAmount(amount, 12)
		require.NoError(t, err)
		assert.Equal(t, want.String(), balance.String(), "genesis balance of %s", address)
	}

	issuer, err := view.Issuer()
	require.NoError(t, err)
	want, err := rt.Genesis().Service.IssuerAddress()
	require.NoError(t, err)
	assert.Equal(t, want, issuer)
}

func TestStateRootIsDeterministicAndAdvances(t *testing.T) {
	first := newTestRuntime(t)
	second := newTestRuntime(t)
	assert.Equal(t, first.Root(), second.Root(), "two nodes with the same genesis have to agree on the root")

	before := first.Root()
	first.Step(1)
	assert.NotEqual(t, before, first.Root(), "a timeslot moves the state even when it settles no work")
}

func TestTransferMovesMoneyBurnsTheFeeAndAdvancesTheRoot(t *testing.T) {
	rt := newTestRuntime(t)
	alice, bob := addressOf(t, 1), addressOf(t, 2)

	before, err := rt.View().Balance(bob)
	require.NoError(t, err)
	beforeSupply, err := rt.View().Supply()
	require.NoError(t, err)
	beforeRoot := rt.Root()

	submit(t, rt, transfer(t, 1, bob, "100", 1))
	rt.Step(1)

	after, err := rt.View().Balance(bob)
	require.NoError(t, err)
	assert.Equal(t, "100", papucoin.FormatRaw(new(big.Int).Sub(after, before), 12))

	afterSupply, err := rt.View().Supply()
	require.NoError(t, err)
	assert.Equal(t, "0.000001", papucoin.FormatRaw(new(big.Int).Sub(beforeSupply, afterSupply), 12), "the fee is burned")

	nonce, err := rt.View().Nonce(alice)
	require.NoError(t, err)
	assert.EqualValues(t, 2, nonce, "the sender's nonce is consumed")

	assert.NotEqual(t, beforeRoot, rt.Root(), "moving money changes the state root")
}

func TestQueuedWorkOnlyLandsOnItsTimeslot(t *testing.T) {
	rt := newTestRuntime(t)
	bob := addressOf(t, 2)

	submit(t, rt, transfer(t, 1, bob, "42", 1))

	before, err := rt.View().Balance(bob)
	require.NoError(t, err)
	opening := papucoin.FormatRaw(before, 12)
	balance, err := rt.View().Balance(bob)
	require.NoError(t, err)
	assert.Equal(t, opening, papucoin.FormatRaw(balance, 12), "a queued item changes nothing before its timeslot")

	rt.Step(1)

	balance, err = rt.View().Balance(bob)
	require.NoError(t, err)
	assert.Equal(t, "42", papucoin.FormatRaw(new(big.Int).Sub(balance, before), 12), "the queued transfer lands in the timeslot after next")
}

// A transaction signed by a key this node has never seen is what a wallet that only
// speaks Ethereum sends. The vector is the firstSend of tools/evm-vectors, signed
// by an independent implementation for chain 5120 with a nonce of 0, which is
// what an account that has never sent anything actually has. So the amount that
// lands is decided by that signature and by nothing this node was told.
const (
	relaySender = "0x718fae2e5c6ba915a210b7f6e85246db915b9c03"
	relayTo     = "0x3535353535353535353535353535353535353535"
	relayRaw    = "0x02f87582140080843b9aca008504a817c800825208943535353535353535353535353535353535353535884563918244f4000080c080a0102ad6d79c7e6f3dee16db944ff8a8f2c13c7777164a1d36e5b739327758a8dda06b875f8181ba400635cf14f37d1432d0d5abacfbc3d5753397171205550607fd"
	// relayRawSecond is the same wallet's next transaction: nonce 1, signed for
	// chain 5120 for what a wallet that already sent one has as its next nonce.
	relayRawSecond = "0x02f87582140001843b9aca008504a817c800825208943535353535353535353535353535353535353535884563918244f4000080c080a04669c291d0312d7f124abbf1109566279c3dbe780ad6071edffb87fb5ab04d2ba07b46d269a5e26963c7030bd157c845f7fdd8f7da5cbd7ca75f3b04baf478608f"
)

func TestRelayedTransferPaysWhatTheSignatureSays(t *testing.T) {
	// The signer of the transaction is an account that only exists on the EVM side,
	// so the only way it can hold PAPU is the bridge: that is what a wallet holding
	// nothing but a secp256k1 key depends on.
	rt, err := New(Options{Genesis: testGenesis(t), BridgeKey: keyOf(t, 9)})
	require.NoError(t, err)
	_, err = rt.Faucet(relaySender)
	require.NoError(t, err)
	rt.Step(1)

	funded, err := rt.View().Balance(relaySender)
	require.NoError(t, err)
	require.NotZero(t, funded, "the bridge should have paid the EVM account")

	item, err := rt.SubmitRelayed(relayRaw)
	require.NoError(t, err)
	assert.Equal(t, relaySender, item.Sender, "the sender is the one the signature names")
	assert.Equal(t, relayTo, item.To)
	assert.Equal(t, "5", item.Amount, "5 PAPU out of the 5e18 wei the transaction carries")
	assert.False(t, item.MustBeSigned, "a relayed item is proved by its transaction, not by an Ed25519 signature")

	before, err := rt.View().Balance(relaySender)
	require.NoError(t, err)
	rt.Step(rt.Timeslot() + 1)

	paid, err := rt.View().Balance(relayTo)
	require.NoError(t, err)
	assert.Equal(t, "5", papucoin.FormatRaw(paid, 12), "the destination is paid the signed amount")

	after, err := rt.View().Balance(relaySender)
	require.NoError(t, err)
	fee, err := papucoin.ParseAmount(testGenesis(t).Service.TransferFee, 12)
	require.NoError(t, err)
	sent, err := papucoin.ParseAmount("5", 12)
	require.NoError(t, err)
	assert.Equal(t, new(big.Int).Add(sent, fee), new(big.Int).Sub(before, after),
		"the sender pays the signed amount plus the fee, and nothing else")

	// The wallet's next transaction is accepted with the next number of the
	// chain's own sequence, which the first transfer advanced: signing the next
	// number a wallet would read is not what this chain asks of a relayed
	// transaction, and an item that guessed it would be refused.
	second, err := rt.SubmitRelayed(relayRawSecond)
	require.NoError(t, err)
	assert.Equal(t, "5", second.Amount, "the second transaction moves the same signed amount")
	rt.Step(rt.Timeslot() + 2)

	paid, err = rt.View().Balance(relayTo)
	require.NoError(t, err)
	assert.Equal(t, "10", papucoin.FormatRaw(paid, 12), "the second transfer paid the destination too")
}

func TestRelayedTransferRefusesWhatCouldNeverPayOut(t *testing.T) {
	rt := newTestRuntime(t)

	// Signed for chain 1, not for the chain this node runs.
	_, err := rt.SubmitRelayed("0xf86e078504a817c800825208943535353535353535353535353535353535353535353535353535353535353535353535884563918244f4000080822824a0495f8ecaff62cba60e3108917f8a109dbd7e06157045a67a362f4346ee8cacc2a04f6ac313b74c56804bf548ae4ea040408f3f0fc70d0ad0163ccb302aaad27b18")
	require.Error(t, err, "a transaction for another chain has to be refused at the door")

	_, err = rt.SubmitRelayed("not hex at all")
	require.Error(t, err)

	queued, _ := rt.Pending()
	assert.Zero(t, queued, "nothing that failed to decode may wait in the queue")
}

func TestUnsignedAndForgedSubmissionsNeverReachTheQueue(t *testing.T) {
	rt := newTestRuntime(t)
	issuer := issuerOf(rt)
	bob := addressOf(t, 200)

	unsigned := papucoin.Item{Method: papucoin.MethodTransfer, Sender: issuer, Nonce: 1, To: bob, Amount: "1"}
	assert.Error(t, rt.Submit(unsigned), "an item that declares itself unsigned must be refused")

	// Nor may an item that is signed but hides behind the flag.
	declared := transfer(t, 1, bob, "1", 1)
	declared.MustBeSigned = false
	assert.Error(t, rt.Submit(declared), "a signed item that declares itself unsigned must be refused")

	// A signature that does cover the item, but from somebody else's key.
	forged, err := papucoin.SignItem(papucoin.Item{Method: papucoin.MethodTransfer, Nonce: 1, To: bob, Amount: "1"}, keyOf(t, 2))
	require.NoError(t, err)
	forged.Sender = issuer
	assert.Error(t, rt.Submit(forged), "a foreign signature must be refused")

	// A signature that is valid for a different amount.
	tampered := transfer(t, 1, bob, "1", 1)
	tampered.Amount = "1000"
	assert.Error(t, rt.Submit(tampered), "an item whose contents moved after signing must be refused")

	// And a claim of the sign flag must not buy a way round the check.
	cleared := transfer(t, 1, bob, "1", 1)
	cleared.MustBeSigned = false
	cleared.Signature = ""
	assert.Error(t, rt.Submit(cleared), "clearing the flag must not skip the signature")

	queued, _ := rt.Pending()
	assert.Equal(t, 0, queued, "nothing refused may occupy a slot")
	rt.Step(1)

	balance, err := rt.View().Balance(bob)
	require.NoError(t, err)
	assert.Equal(t, "0", balance.String())
}

func TestOnlyTheIssuerCanMint(t *testing.T) {
	rt := newTestRuntime(t)
	claimant := addressOf(t, 202)

	before, err := rt.View().Supply()
	require.NoError(t, err)

	// A mint is signed by the claimant, and the service only honours a mint that
	// comes from the issuer, so this settles nothing at all. The node refuses it
	// at the door rather than queueing something the service will drop: an item
	// nobody may send should not take up a slot in a block.
	item := mint(t, 1, claimant, "7", 1)
	require.Error(t, rt.Submit(item), "a mint from anybody but the issuer must be refused")
	rt.Step(1)

	balance, err := rt.View().Balance(claimant)
	require.NoError(t, err)
	assert.Equal(t, "0", papucoin.FormatRaw(balance, 12), "a mint from anybody but the issuer must settle nothing")

	after, err := rt.View().Supply()
	require.NoError(t, err)
	assert.Equal(t, before.String(), after.String(), "and it must not change the supply")
}

func TestFaucetPaysOutOfTheBridgeAccount(t *testing.T) {
	rt, err := New(Options{Genesis: testGenesis(t), BridgeKey: keyOf(t, 7)})
	require.NoError(t, err)
	require.NotEmpty(t, rt.BridgeAddress())

	// The first timeslot moves the reserve out of the genesis issuer account.
	rt.Step(1)
	rt.Step(2)

	reserve, err := rt.View().Balance(rt.BridgeAddress())
	require.NoError(t, err)
	assert.Positive(t, reserve.Sign(), "the bridge account has to be funded before it can pay out")

	paying := addressOf(t, 201)
	item, err := rt.Faucet(paying)
	require.NoError(t, err)
	assert.Equal(t, papucoin.MethodFaucet, item.Method)
	assert.NotEmpty(t, item.Signature, "the node signs what it pays for")

	rt.Step(3)

	paid, err := rt.View().Balance(paying)
	require.NoError(t, err)
	assert.Equal(t, papucoin.FormatRaw(rt.Params().FaucetAmount, 12), papucoin.FormatRaw(paid, 12), "the payout is the faucet amount, whatever was asked for")
}

func TestFaucetNeedsABridgeKey(t *testing.T) {
	rt := newTestRuntime(t)
	_, err := rt.Faucet(addressOf(t, 2))
	assert.Error(t, err, "a node with no key cannot pay for a payout")
}

func TestViewReadsRawStorageKeys(t *testing.T) {
	view := newTestRuntime(t).View()

	supply, ok := view.Raw([]byte{0x01})
	require.True(t, ok, "the supply lives under key 0x01")
	assert.Len(t, supply, 16, "an amount is stored in 16 bytes")

	_, ok = view.Raw([]byte{0xff})
	assert.False(t, ok, "an unused key reads as absent")
}

func TestUnknownAccountsReadAsEmpty(t *testing.T) {
	view := newTestRuntime(t).View()
	stranger := addressOf(t, 200)

	nonce, err := view.Nonce(stranger)
	require.NoError(t, err)
	assert.EqualValues(t, 0, nonce, "an account that never sent anything has used no nonce")

	balance, err := view.Balance(stranger)
	require.NoError(t, err)
	assert.Equal(t, "0", balance.String())

	_, err = view.Balance("sdlgNotAnAddress")
	assert.Error(t, err, "a malformed address is an error, not an empty balance")
}

func TestQueueReportsHowMuchWorkIsWaiting(t *testing.T) {
	rt := newTestRuntime(t)
	bob := addressOf(t, 2)

	queued, slots := rt.Pending()
	assert.Equal(t, 0, queued)
	assert.Positive(t, slots)

	submit(t, rt, transfer(t, 1, bob, "1", 1))
	submit(t, rt, transfer(t, 1, bob, "1", 2))

	queued, _ = rt.Pending()
	assert.Equal(t, 2, queued)

	rt.Step(1)
	queued, _ = rt.Pending()
	assert.Equal(t, 0, queued, "a timeslot drains the queue")
}

// transfer builds a signed transfer from the account keyOf owns.
func transfer(t *testing.T, owner byte, to, amount string, nonce uint64) papucoin.Item {
	t.Helper()
	return signed(t, papucoin.Item{Method: papucoin.MethodTransfer, Nonce: nonce, To: to, Amount: amount}, owner)
}

func mint(t *testing.T, owner byte, to, amount string, nonce uint64) papucoin.Item {
	t.Helper()
	return signed(t, papucoin.Item{Method: papucoin.MethodMint, Nonce: nonce, To: to, Amount: amount}, owner)
}

func signed(t *testing.T, item papucoin.Item, owner byte) papucoin.Item {
	t.Helper()
	// An item that leaves the node has to declare that it is signed, and the
	// flag is part of what the signature covers.
	item.MustBeSigned = true
	result, err := papucoin.SignItem(item, keyOf(t, owner))
	require.NoError(t, err)
	return result
}

func submit(t *testing.T, rt *Runtime, item papucoin.Item) {
	t.Helper()
	require.NoError(t, rt.Submit(item))
}

func keyOf(t *testing.T, seed byte) ed25519.PrivateKey {
	t.Helper()
	seed32 := make([]byte, ed25519.SeedSize)
	for i := range seed32 {
		seed32[i] = seed
	}
	return ed25519.NewKeyFromSeed(seed32)
}

func addressOf(t *testing.T, seed byte) string {
	t.Helper()
	publicKey, ok := keyOf(t, seed).Public().(ed25519.PublicKey)
	require.True(t, ok)
	address, err := papucoin.AddressFromPublicKey(publicKey)
	require.NoError(t, err)
	return address
}

func issuerOf(rt *Runtime) string {
	issuer, err := rt.Genesis().Service.IssuerAddress()
	if err != nil {
		return ""
	}
	return issuer
}

func ValidateChainAddress(address string) (ed25519.PublicKey, error) {
	return papucoin.ValidateChainAddress(address)
}

// accounts turns a map of address to amount into the public key form a genesis
// carries, so a test can fund accounts whose keys it holds.
func accounts(t *testing.T, byAddress map[string]string) []GenesisAccount {
	t.Helper()
	out := make([]GenesisAccount, 0, len(byAddress))
	for address, amount := range byAddress {
		publicKey, err := ValidateChainAddress(address)
		require.NoError(t, err)
		out = append(out, GenesisAccount{PublicKey: hex.EncodeToString(publicKey), Amount: amount})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PublicKey < out[j].PublicKey })
	return out
}

func write(t *testing.T, path string, genesis *Genesis) {
	t.Helper()
	raw, err := json.Marshal(genesis)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw, 0o600))
}
