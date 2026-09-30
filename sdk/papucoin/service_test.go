package papucoin

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
	"github.com/eigerco/strawberry/sdk"
)

const jamBalance = 1 << 40

// testAddress builds a well formed chain address from a deterministic seed, and
// testKey returns the private key behind it so that items can be signed.
func testAddress(seed byte) string {
	address, err := AddressFromPublicKey(testPublicKey(seed))
	if err != nil {
		panic(err)
	}
	return address
}

func testKey(seed byte) ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes32(seed))
}

func testPublicKey(seed byte) ed25519.PublicKey {
	publicKey, ok := testKey(seed).Public().(ed25519.PublicKey)
	if !ok {
		panic("ed25519 public key has an unexpected type")
	}
	return publicKey
}

func bytes32(b byte) []byte {
	out := make([]byte, ed25519.SeedSize)
	for i := range out {
		out[i] = b
	}
	return out
}

func evmAddress(seed byte) string {
	// An EVM address is 20 bytes, which is 40 hex characters.
	out := "0x"
	for range 40 {
		out += string("0123456789abcdef"[seed%16])
	}
	return out
}

func TestAmountRoundTrip(t *testing.T) {
	params := DefaultParams()

	cases := []struct {
		input string
		want  string
	}{
		{"1", "1"},
		{"0.5", "0.5"},
		{".5", "0.5"},
		{"0.000001", "0.000001"},
		{"123.000001", "123.000001"},
		{"1000000", "1000000"},
	}
	for _, c := range cases {
		raw, err := ParseAmount(c.input, params.Decimals)
		require.NoError(t, err, c.input)
		assert.Equal(t, c.want, FormatRaw(raw, params.Decimals), c.input)
	}
}

func TestAmountRejectsMalformed(t *testing.T) {
	params := DefaultParams()
	for _, bad := range []string{"", "abc", "-1", "1.2.3", "+1", "1.0000000000001", "1,5", "0x10"} {
		_, err := ParseAmount(bad, params.Decimals)
		assert.Error(t, err, bad)
	}
}

func TestAmountEncodingIsFixedWidth(t *testing.T) {
	params := DefaultParams()
	encoded, err := encodeAmount(params.MaxSupply)
	require.NoError(t, err)
	assert.Len(t, encoded, amountOctets)

	decoded, err := decodeAmount(encoded)
	require.NoError(t, err)
	assert.Equal(t, 0, params.MaxSupply.Cmp(decoded))

	_, err = encodeAmount(new(big.Int).Lsh(big.NewInt(1), 129))
	assert.ErrorIs(t, err, ErrAmountOverflow)
}

func TestAddressValidation(t *testing.T) {
	valid := testAddress(1)
	_, err := ValidateChainAddress(valid)
	assert.NoError(t, err)

	canonical, err := NormalizeAddress(valid)
	require.NoError(t, err)
	assert.Equal(t, valid, canonical)

	// A single character change breaks the checksum.
	broken := []byte(valid)
	if broken[len(broken)-1] == 'a' {
		broken[len(broken)-1] = 'b'
	} else {
		broken[len(broken)-1] = 'a'
	}
	_, err = ValidateChainAddress(string(broken))
	assert.Error(t, err)

	_, err = NormalizeAddress("")
	assert.ErrorIs(t, err, ErrInvalidAddress)
	_, err = NormalizeAddress("0x123")
	assert.ErrorIs(t, err, ErrInvalidAddress)
}

func TestEVMAddressNormalisation(t *testing.T) {
	upper := "0x" + strings.Repeat("AB", 20)
	assert.True(t, IsEVMAddress(upper))
	canonical, err := NormalizeAddress(upper)
	require.NoError(t, err)
	assert.Equal(t, strings.ToLower(upper), canonical)

	assert.False(t, IsEVMAddress("0x123"))
	assert.False(t, IsEVMAddress("sdlgabc"))
}

// rig is a PAPU service wired into the SDK, with helpers to drive it the way a
// node would.
type rig struct {
	params   Params
	issuer   string
	exec     *svc.Executor
	id       block.ServiceId
	state    service.ServiceState
	registry *svc.Registry
}

func newRig(t *testing.T, initial map[string]string, relay RelayVerifier) *rig {
	t.Helper()
	return newRigWithBalance(t, initial, relay, jamBalance)
}

func newRigWithBalance(t *testing.T, initial map[string]string, relay RelayVerifier, balance uint64) *rig {
	t.Helper()

	params := DefaultParams()
	issuer := testAddress(9)
	registry := svc.NewRegistry()
	require.NoError(t, registry.RegisterAt(New(params, issuer, initial, relay), 0))
	id := block.ServiceId(0)

	account := service.NewServiceAccount()
	account.Balance = balance
	state := service.ServiceState{id: account}

	exec := svc.NewExecutor(registry)
	seeded, err := exec.Initialize(id, 0, ^uint64(0), state)
	require.NoError(t, err)
	state[id] = seeded.Account

	return &rig{params: params, issuer: issuer, exec: exec, id: id, state: state, registry: registry}
}

func (r *rig) balance(t *testing.T, address string) *big.Int {
	t.Helper()
	return r.read(t, balanceKey(address))
}

// view is the read side a caller has: what the chain would answer right now,
// which is what a node asks before it queues anything on someone's behalf.
func (r *rig) view() *View {
	account := r.state[r.id]
	return NewView(r.id, &account)
}

func (r *rig) supply(t *testing.T) *big.Int {
	t.Helper()
	return r.read(t, []byte{keySupply})
}

// read decodes a stored amount, treating an absent entry as zero.
func (r *rig) read(t *testing.T, key []byte) *big.Int {
	t.Helper()
	stateKey, err := statekey.NewStorage(r.id, key)
	require.NoError(t, err)
	account := r.state[r.id]
	stored, ok := account.GetStorage(stateKey)
	if !ok {
		return new(big.Int)
	}
	amount, err := decodeAmount(stored)
	require.NoError(t, err)
	return amount
}

func (r *rig) nonce(t *testing.T, address string) uint64 {
	t.Helper()
	key, err := statekey.NewStorage(r.id, nonceKey(address))
	require.NoError(t, err)
	account := r.state[r.id]
	stored, ok := account.GetStorage(key)
	if !ok {
		return r.params.FirstNonce
	}
	amount, err := decodeAmount(stored)
	require.NoError(t, err)
	return amount.Uint64()
}

// submit refines an item and, if it survives refinement, accumulates it and
// commits the resulting account.
func amountFromStringForTest(t *testing.T, s string) *big.Int {
	t.Helper()
	v, err := amountFromString(s)
	require.NoError(t, err)
	return v
}

func mustSubmitOK(t *testing.T, r *rig, item Item) {
	t.Helper()
	payload, err := json.Marshal(item)
	require.NoError(t, err)
	refined, err := r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
	require.NoError(t, err)
	result, err := r.exec.Accumulate(r.id, []svc.RefinedItem{{Item: payload, Report: refined.Report}}, nil, 0, 0, r.state)
	require.NoError(t, err)
	r.state[r.id] = result.Account
}

// mustSkip asserts that an item is refused during refinement, which is where a
// service proves an item before any account is read.
func (r *rig) mustSkip(t *testing.T, item Item) {
	t.Helper()
	payload, err := json.Marshal(item)
	require.NoError(t, err)
	_, err = r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
	require.Error(t, err)
}

func funded(t *testing.T) map[string]string {
	return map[string]string{testAddress(1): "1000"}
}

func TestGenesisSeedsBalancesAndSupply(t *testing.T) {
	r := newRig(t, funded(t), nil)

	alice := testAddress(1)
	assert.Equal(t, "1000", FormatRaw(r.balance(t, alice), r.params.Decimals))
	assert.Equal(t, "1000", FormatRaw(r.supply(t), r.params.Decimals))
}

func TestGenesisRejectsOversupply(t *testing.T) {
	params := DefaultParams()
	registry := svc.NewRegistry()
	// One whole unit above the maximum supply must be refused, while exactly
	// the maximum supply is still allowed.
	huge := FormatRaw(new(big.Int).Add(params.MaxSupply, big.NewInt(1)), params.Decimals)
	require.NoError(t, registry.RegisterAt(New(params, testAddress(9), map[string]string{testAddress(1): huge}, nil), 0))
	id := block.ServiceId(0)

	account := service.NewServiceAccount()
	account.Balance = jamBalance

	_, err := svc.NewExecutor(registry).Initialize(id, 0, ^uint64(0), service.ServiceState{id: account})
	assert.Error(t, err)
}

func TestTransferMovesBalanceAndBurnsFee(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	before := r.balance(t, alice)
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "10"})

	assert.Equal(t, "989.999999", FormatRaw(r.balance(t, alice), r.params.Decimals))
	assert.Equal(t, "10", FormatRaw(r.balance(t, bob), r.params.Decimals))
	// The 1 micro fee is burned, so supply drops even though value moved.
	assert.Equal(t, "999.999999", FormatRaw(r.supply(t), r.params.Decimals))
	assert.True(t, r.balance(t, alice).Cmp(before) < 0, "sender balance fell")
}

func TestTransferInsufficientBalanceIsSkipped(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "100000"})

	assert.Equal(t, "1000", FormatRaw(r.balance(t, alice), r.params.Decimals))
	assert.Equal(t, "0", FormatRaw(r.balance(t, bob), r.params.Decimals))
	// A skipped operation must not consume the nonce.
	assert.Equal(t, uint64(1), r.nonce(t, alice))
}

func TestNonceReplayRejected(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "1"})
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "1"})

	assert.Equal(t, "1", FormatRaw(r.balance(t, bob), r.params.Decimals), "replay must not move value twice")
	assert.Equal(t, uint64(2), r.nonce(t, alice))
}

func TestStaleNonceSkipped(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	// The account moves on, and the number it used is behind it now. An item
	// carrying that number is the same one submitted twice, so it moves nothing.
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "1"})
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "1"})

	assert.Equal(t, "1", FormatRaw(r.balance(t, bob), r.params.Decimals), "a number already gone by moves nothing")
}

func TestConsecutiveNoncesInOneBlockAllApply(t *testing.T) {
	// An account that sends twice before the next block has both items numbered
	// from the state it can see, which is the same for both. Only one of them can
	// carry the exact number the chain is on, so an exact rule loses the other
	// without a word. Nothing may be lost.
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "1"})
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 2, To: bob, Amount: "1"})
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 3, To: bob, Amount: "1"})

	assert.Equal(t, "3", FormatRaw(r.balance(t, bob), r.params.Decimals),
		"a run of numbers is money that is already committed and must not vanish")
	assert.Equal(t, uint64(4), r.nonce(t, alice))
}

// A number ahead of the account's is taken, not skipped. That is the other half
// of not losing transfers: an item can only be ahead because this chain numbered
// it, or because the account it moves money for signed it, and a run of numbers
// in a block is money that is already committed. What still cannot happen is the
// same item being taken twice, which is what a number that has gone by means, and
// no third party can produce a number at all without a signature the service
// checks.
func TestNumberAheadIsTakenAndReplayIsNot(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 7, To: bob, Amount: "1"})
	assert.Equal(t, "1", FormatRaw(r.balance(t, bob), r.params.Decimals))
	assert.Equal(t, uint64(8), r.nonce(t, alice), "the account moves to one past the number it used")

	// The very same item, submitted again, is behind the account now.
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 7, To: bob, Amount: "1"})
	assert.Equal(t, "1", FormatRaw(r.balance(t, bob), r.params.Decimals), "the same number twice moves the money once")
}

func TestMintOnlyByIssuer(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	mustSubmitOK(t, r, Item{Method: MethodMint, Sender: alice, Nonce: 1, To: bob, Amount: "5"})
	assert.Equal(t, "0", FormatRaw(r.balance(t, bob), r.params.Decimals))

	mustSubmitOK(t, r, Item{Method: MethodMint, Sender: r.issuer, Nonce: 1, To: bob, Amount: "5"})
	assert.Equal(t, "5", FormatRaw(r.balance(t, bob), r.params.Decimals))
	assert.Equal(t, "1005", FormatRaw(r.supply(t), r.params.Decimals))
}

func TestMintRespectsMaxSupply(t *testing.T) {
	r := newRig(t, funded(t), nil)
	bob := testAddress(2)

	huge := FormatRaw(r.params.MaxSupply, r.params.Decimals)
	mustSubmitOK(t, r, Item{Method: MethodMint, Sender: r.issuer, Nonce: 1, To: bob, Amount: huge})

	assert.Equal(t, "0", FormatRaw(r.balance(t, bob), r.params.Decimals))
	assert.Equal(t, "1000", FormatRaw(r.supply(t), r.params.Decimals))
}

func TestBurnReducesBalanceAndSupply(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice := testAddress(1)

	mustSubmitOK(t, r, Item{Method: MethodBurn, Sender: alice, Nonce: 1, Amount: "100"})

	assert.Equal(t, "900", FormatRaw(r.balance(t, alice), r.params.Decimals))
	assert.Equal(t, "900", FormatRaw(r.supply(t), r.params.Decimals))
}

func TestFaucetClaimIsVisibleBeforeItIsPaid(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice := testAddress(1)

	claimed, err := r.view().FaucetClaimed(alice)
	assert.NoError(t, err)
	assert.False(t, claimed, "an address that never asked holds no claim")

	mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: alice, Nonce: 1})

	// A caller that funds accounts asks this before queueing anything. Once the
	// claim is visible the caller knows a second item would be refused, and a
	// refused item still costs the sender a nonce.
	claimed, err = r.view().FaucetClaimed(alice)
	assert.NoError(t, err)
	assert.True(t, claimed)
}

func TestClaimSurvivesForOtherAddress(t *testing.T) {
	r := newRig(t, funded(t), nil)
	mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: r.issuer, Nonce: 1, To: testAddress(1)})

	// The claim belongs to the address that was paid, not to the sender, so one
	// account taking its payout leaves every other account free to be funded.
	claimed, err := r.view().FaucetClaimed(testAddress(1))
	assert.NoError(t, err)
	assert.True(t, claimed)

	claimed, err = r.view().FaucetClaimed(testAddress(2))
	assert.NoError(t, err)
	assert.False(t, claimed)
}

func TestFaucetOnlyOnce(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice := testAddress(1)

	mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: alice, Nonce: 1})
	assert.Equal(t, "11000", FormatRaw(r.balance(t, alice), r.params.Decimals))

	mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: alice, Nonce: 2})
	assert.Equal(t, "11000", FormatRaw(r.balance(t, alice), r.params.Decimals), "faucet pays out once only")
}

func TestWelcomeOnlyOnce(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice := testAddress(1)

	mustSubmitOK(t, r, Item{Method: MethodWelcome, Sender: alice, Nonce: 1})
	assert.Equal(t, "1005", FormatRaw(r.balance(t, alice), r.params.Decimals))

	mustSubmitOK(t, r, Item{Method: MethodWelcome, Sender: alice, Nonce: 2})
	assert.Equal(t, "1005", FormatRaw(r.balance(t, alice), r.params.Decimals))
}

func TestFaucetForAnotherAddress(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: alice, Nonce: 1, To: bob})
	assert.Equal(t, "10000", FormatRaw(r.balance(t, bob), r.params.Decimals))
	assert.Equal(t, "1000", FormatRaw(r.balance(t, alice), r.params.Decimals))
}

func TestRefineRejectsBadInput(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice := testAddress(1)

	for _, item := range []Item{
		{Method: "nope", Sender: alice, Nonce: 1},
		{Method: MethodTransfer, Sender: alice, Nonce: 1, To: "garbage", Amount: "1"},
		{Method: MethodTransfer, Sender: alice, Nonce: 1, To: testAddress(2), Amount: "0"},
		{Method: MethodTransfer, Sender: alice, Nonce: 1, To: testAddress(2), Amount: "-1"},
		{Method: MethodMint, Sender: alice, Nonce: 1, To: "0x123", Amount: "1"},
		{Method: MethodTransfer, Sender: "not-an-address", Nonce: 1, To: testAddress(2), Amount: "1"},
	} {
		payload, err := json.Marshal(item)
		require.NoError(t, err)
		_, err = r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
		assert.Error(t, err, item.Method+" "+item.To)
	}
}

func TestRefineRejectsMalformedPayload(t *testing.T) {
	r := newRig(t, funded(t), nil)
	_, err := r.exec.Refine(r.id, []byte("not json"), 0, ^uint64(0), r.state)
	assert.Error(t, err)
}

func TestRefuseRelayedTransferWithoutVerifier(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), evmAddress(3)

	payload, err := json.Marshal(Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Raw: "0xdeadbeef"})
	require.NoError(t, err)
	_, err = r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
	assert.ErrorIs(t, err, ErrRelayUnconfigured)
}

type stubRelay struct {
	fields EVMFields
	err    error
}

func (s stubRelay) Recover(string) (EVMFields, error) { return s.fields, s.err }

func TestRelayedTransferUsesRecoveredSigner(t *testing.T) {
	// An EVM account funded on the chain relays a transfer signed with ECDSA.
	sender := evmAddress(3)
	relay := stubRelay{fields: EVMFields{
		From:  sender,
		To:    evmAddress(4),
		Nonce: 0,
		Value: WeiFromRaw(big.NewInt(5e12)), // 5 PAPU
	}}
	r := newRig(t, map[string]string{sender: "100"}, relay)

	payload, err := json.Marshal(Item{Method: MethodTransfer, Sender: testAddress(1), Nonce: 1, To: evmAddress(4), Raw: "0xdeadbeef"})
	require.NoError(t, err)

	refined, err := r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
	require.NoError(t, err)

	var op Op
	require.NoError(t, json.Unmarshal(refined.Report, &op))
	// The recovered signer replaces the item sender, and the wei value is
	// scaled down to raw units.
	assert.Equal(t, sender, op.Sender)
	// Amounts in a refined op are raw units, not display amounts.
	assert.Equal(t, "5000000000000", op.Amount)
	assert.Equal(t, "5", FormatRaw(amountFromStringForTest(t, op.Amount), r.params.Decimals))
	require.NotNil(t, op.EVMNonce)
	assert.Equal(t, uint64(0), *op.EVMNonce)
	// The item nonce belongs to the relaying wallet, not to the EVM signer.
	assert.Equal(t, testAddress(1), op.Actor)

	result, err := r.exec.Accumulate(r.id, []svc.RefinedItem{{Item: payload, Report: refined.Report}}, nil, 0, 0, r.state)
	require.NoError(t, err)
	r.state[r.id] = result.Account

	assert.Equal(t, "94.999999", FormatRaw(r.balance(t, sender), r.params.Decimals))
	assert.Equal(t, "5", FormatRaw(r.balance(t, evmAddress(4)), r.params.Decimals))
	assert.Equal(t, "99.999999", FormatRaw(r.supply(t), r.params.Decimals))
}

func TestRelayedDestinationMustMatchSignature(t *testing.T) {
	relay := stubRelay{fields: EVMFields{From: evmAddress(3), To: evmAddress(4), Nonce: 0, Value: weiGranularity}}
	r := newRig(t, map[string]string{evmAddress(3): "100"}, relay)

	// The item claims a different destination than the signed transaction.
	payload, err := json.Marshal(Item{Method: MethodTransfer, Sender: testAddress(1), Nonce: 1, To: evmAddress(5), Raw: "0xdeadbeef"})
	require.NoError(t, err)
	_, err = r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
	assert.ErrorIs(t, err, ErrRelayMismatch)
}

func TestRelayedValueMustBeWholeMicroUnits(t *testing.T) {
	relay := stubRelay{fields: EVMFields{From: evmAddress(3), To: evmAddress(4), Value: big.NewInt(1)}}
	r := newRig(t, map[string]string{evmAddress(3): "100"}, relay)

	payload, err := json.Marshal(Item{Method: MethodTransfer, Sender: testAddress(1), Nonce: 1, To: evmAddress(4), Raw: "0xdeadbeef"})
	require.NoError(t, err)
	_, err = r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
	assert.ErrorIs(t, err, ErrRelayMismatch)
}

func TestStorageIsRejectedWhenBalanceCannotPayForIt(t *testing.T) {
	// Barely enough JAM balance to cover the basic minimum, so seeding a
	// balance entry must be refused rather than silently truncating state.
	params := DefaultParams()
	registry := svc.NewRegistry()
	require.NoError(t, registry.RegisterAt(New(params, testAddress(9), funded(t), nil), 0))

	account := service.NewServiceAccount()
	account.Balance = service.BasicMinimumBalance

	_, err := svc.NewExecutor(registry).Initialize(0, 0, ^uint64(0), service.ServiceState{0: account})
	assert.ErrorIs(t, err, svc.ErrStorageFull)
}

// sequenceRelay hands out one EVM transaction per call, in order, so a test can
// sign several as a wallet would and submit them before any of them is applied.
type sequenceRelay struct {
	from, to string
	nonces   []uint64
	calls    int
}

func (s *sequenceRelay) Recover(string) (EVMFields, error) {
	n := s.nonces[s.calls]
	s.calls++
	return EVMFields{From: s.from, To: s.to, Nonce: n, Value: WeiFromRaw(big.NewInt(1e12))}, nil
}

// A wallet sends its transactions one after another, so the second one is
// signed before the first is applied. The EVM nonce is what tells them apart,
// and both have to land.
func TestSequentialEVMNoncesBothApply(t *testing.T) {
	sender, recipient := evmAddress(3), evmAddress(4)
	relay := &sequenceRelay{from: sender, to: recipient, nonces: []uint64{0, 1}}
	r := newRig(t, map[string]string{sender: "100"}, relay)

	items := make([]svc.RefinedItem, 0, 2)
	for i, raw := range []string{"0x01", "0x02"} {
		payload, err := json.Marshal(Item{
			Method: MethodTransfer, Sender: testAddress(1),
			Nonce: uint64(i) + 1, To: recipient, Raw: raw,
		})
		require.NoError(t, err)

		refined, err := r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
		require.NoError(t, err)
		items = append(items, svc.RefinedItem{Item: payload, Report: refined.Report})
	}

	result, err := r.exec.Accumulate(r.id, items, nil, 0, 0, r.state)
	require.NoError(t, err)
	r.state[r.id] = result.Account

	assert.Equal(t, "2", FormatRaw(r.balance(t, recipient), r.params.Decimals),
		"both transactions land, the second one after the first raised the nonce")
}

// The fee is a price, so it has to move with what the chain is being asked to
// do. A fee frozen at genesis is a number that is wrong the moment the chain is
// busy, and wrong in the direction that lets anybody flood it for free.
func TestFeeRisesWhenTheBlockIsBusy(t *testing.T) {
	r := newRig(t, funded(t), nil)

	// One block doing more transfers than the target is what "busy" means, so
	// the price has to come out the other side higher than it went in.
	bob := testAddress(2)
	for i := 0; i <= FeeBusyTarget; i++ {
		mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: testAddress(byte(20 + i)), Nonce: 1})
	}
	// The price the block after the funding starts from.
	before := currentFeeForTest(t, r)

	mustAccumulateBatch(t, r, func(i int) Item {
		return Item{Method: MethodTransfer, Sender: testAddress(byte(20 + i)), Nonce: 2, To: bob, Amount: "1"}
	}, FeeBusyTarget+1)
	after := currentFeeForTest(t, r)

	want := new(big.Int).Mul(before, big.NewInt(FeeUpNumerator))
	want.Quo(want, big.NewInt(FeeUpDenominator))
	assert.Equal(t, want.String(), after.String(), "a busy block leaves the next one charging more")
}

// A block inside the band leaves the price alone: a chain that moved the price
// on every single transfer would be quoting a different figure every block, and
// no client could ever predict what it would be charged.
func TestFeeStandsStillInsideTheBand(t *testing.T) {
	r := newRig(t, funded(t), nil)
	mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: testAddress(20), Nonce: 1})

	before := currentFeeForTest(t, r)
	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: testAddress(20), Nonce: 2, To: testAddress(2), Amount: "1"})
	assert.Equal(t, before.String(), currentFeeForTest(t, r).String(), "one transfer is inside the band")
}

func TestFeeFallsWhenTheBlockIsIdle(t *testing.T) {
	r := newRig(t, funded(t), nil)

	// Push the price up first by loading a block, then let an idle block bring
	// it back down. The point is not the exact figure, it is that it moves down
	// rather than staying wherever the last burst left it.
	for i := 0; i < FeeBusyTarget+2; i++ {
		alice := testAddress(byte(10 + i))
		mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: alice, Nonce: 1})
	}
	busy := currentFeeForTest(t, r)
	mustSubmitOK(t, r, Item{Method: MethodMint, Sender: r.issuer, Nonce: 1, To: testAddress(3), Amount: "1"})

	assert.LessOrEqual(t, currentFeeForTest(t, r).Int64(), busy.Int64(),
		"an idle block never leaves the price above where the burst put it")
}

// A price is what a client is quoted, so it has to be readable, and it has to be
// denominated in a fraction of a PAPU: at one unit a transfer of a thousandth
// would be charged a thousand times what it moved.
func TestFeeIsAFractionOfOneAndIsReadable(t *testing.T) {
	r := newRig(t, funded(t), nil)
	fee := currentFeeForTest(t, r)

	assert.Greater(t, fee.Sign(), 0, "a transfer has to cost something")
	assert.Less(t, fee.Cmp(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(r.params.Decimals)), nil)), 0,
		"the fee has to be a fraction of one PAPU, not a whole unit")
	assert.GreaterOrEqual(t, fee.Int64(), FeeFloor, "the floor holds")
	assert.LessOrEqual(t, fee.Int64(), FeeCeiling, "the ceiling holds")
}

// mustAccumulateBatch submits several items and accumulates them together,
// which is what a block with more work in it than a quiet one looks like. Sending
// them one at a time would be a block each, and a block of one transfer is
// inside the band where the price stands still.
func mustAccumulateBatch(t *testing.T, r *rig, item func(int) Item, n int) {
	t.Helper()
	items := make([]svc.RefinedItem, 0, n)
	for i := 0; i < n; i++ {
		payload, err := json.Marshal(item(i))
		require.NoError(t, err)
		refined, err := r.exec.Refine(r.id, payload, 0, ^uint64(0), r.state)
		require.NoError(t, err)
		items = append(items, svc.RefinedItem{Item: payload, Report: refined.Report})
	}
	result, err := r.exec.Accumulate(r.id, items, nil, 0, 0, r.state)
	require.NoError(t, err)
	r.state[r.id] = result.Account
}

func currentFeeForTest(t *testing.T, r *rig) *big.Int {
	t.Helper()
	account := r.state[r.id]
	return CurrentFee(context.Background(), NewView(r.id, &account), r.params)
}
