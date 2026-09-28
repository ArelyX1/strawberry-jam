package papucoin

import (
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

func TestWrongNonceSkipped(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	mustSubmitOK(t, r, Item{Method: MethodTransfer, Sender: alice, Nonce: 7, To: bob, Amount: "1"})
	assert.Equal(t, "0", FormatRaw(r.balance(t, bob), r.params.Decimals))
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
