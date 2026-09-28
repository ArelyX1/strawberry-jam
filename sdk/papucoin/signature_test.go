package papucoin

import (
	"bytes"
	"crypto/ed25519"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAddressRoundTripsThroughPublicKey(t *testing.T) {
	for seed := byte(1); seed <= 4; seed++ {
		address, err := AddressFromPublicKey(testPublicKey(seed))
		require.NoError(t, err)
		assert.Equal(t, testAddress(seed), address)

		publicKey, err := ValidateChainAddress(address)
		require.NoError(t, err)
		assert.True(t, bytes.Equal(testPublicKey(seed), publicKey))
	}
}

func TestAddressFromPublicKeyRejectsWrongLength(t *testing.T) {
	_, err := AddressFromPublicKey(make([]byte, 8))
	assert.ErrorIs(t, err, ErrInvalidAddress)
}

func TestSignedItemIsAccepted(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	item := Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "10", MustBeSigned: true}
	signed, err := SignItem(item, testKey(1))
	require.NoError(t, err)

	mustSubmitOK(t, r, signed)

	assert.Equal(t, "989.999999", FormatRaw(r.balance(t, alice), r.params.Decimals))
	assert.Equal(t, "10", FormatRaw(r.balance(t, bob), r.params.Decimals))
}

func TestSigningDerivesTheSender(t *testing.T) {
	// A caller that tries to sign as somebody else gets their own address
	// instead, so a signed item can never claim a foreign sender.
	signed, err := SignItem(Item{Method: MethodTransfer, Sender: testAddress(2), Nonce: 1}, testKey(1))
	require.NoError(t, err)
	assert.Equal(t, testAddress(1), signed.Sender)
}

func TestUnsignedPublicItemIsSkipped(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	r.mustSkip(t, Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "10", MustBeSigned: true})

	assert.Equal(t, "1000", FormatRaw(r.balance(t, alice), r.params.Decimals))
	assert.Equal(t, "0", FormatRaw(r.balance(t, bob), r.params.Decimals))
}

func TestTamperedSignedItemIsSkipped(t *testing.T) {
	for name, tamper := range map[string]func(*Item){
		"amount":     func(i *Item) { i.Amount = "999" },
		"recipient":  func(i *Item) { i.To = testAddress(3) },
		"nonce":      func(i *Item) { i.Nonce = 7 },
		"method":     func(i *Item) { i.Method = MethodMint },
		"sender":     func(i *Item) { i.Sender = testAddress(2) },
		"signature":  func(i *Item) { i.Signature = "0x" + flipped[i.Signature[2]] + i.Signature[3:] },
		"raw append": func(i *Item) { i.Memo = "hola" },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, funded(t), nil)
			alice, bob := testAddress(1), testAddress(2)

			signed, err := SignItem(Item{Method: MethodTransfer, Sender: alice, Nonce: 1, To: bob, Amount: "10", MustBeSigned: true}, testKey(1))
			require.NoError(t, err)
			tamper(&signed)

			r.mustSkip(t, signed)

			assert.Equal(t, "1000", FormatRaw(r.balance(t, alice), r.params.Decimals))
			assert.Equal(t, "0", FormatRaw(r.balance(t, bob), r.params.Decimals))
		})
	}
}

func TestItemSignedByAnotherKeyIsSkipped(t *testing.T) {
	r := newRig(t, funded(t), nil)
	alice, bob := testAddress(1), testAddress(2)

	// A valid signature, but from bob's key while claiming to be alice.
	signed, err := SignItem(Item{Method: MethodTransfer, Sender: bob, Nonce: 1, To: bob, Amount: "10", MustBeSigned: true}, testKey(2))
	require.NoError(t, err)
	signed.Sender = alice

	r.mustSkip(t, signed)

	assert.Equal(t, "0", FormatRaw(r.balance(t, bob), r.params.Decimals))
}

func TestSystemItemNeedsNoSignature(t *testing.T) {
	// The faucet is a system item: the node mints it from its own account, so
	// there is nobody to prove and nothing to steal.
	r := newRig(t, map[string]string{testAddress(9): "1000"}, nil)
	bob := testAddress(2)

	// The amount is a ceiling rather than a request: the faucet always pays
	// exactly what the genesis parameters say it pays.
	mustSubmitOK(t, r, Item{Method: MethodFaucet, Sender: r.issuer, Nonce: 1, To: bob, Amount: "250"})

	assert.Equal(t, "10000", FormatRaw(r.balance(t, bob), r.params.Decimals))
}

func TestVerifyRejectsMalformedSignature(t *testing.T) {
	item := Item{Method: MethodTransfer, Sender: testAddress(1), Nonce: 1, To: testAddress(2), Amount: "1", MustBeSigned: true, Signature: "not hex"}
	_, err := VerifyItemSignature(item)
	assert.Error(t, err)

	item.Signature = ""
	_, err = VerifyItemSignature(item)
	assert.Error(t, err)

	item.Sender = "sdlgNoSuchAddress"
	_, err = VerifyItemSignature(item)
	assert.ErrorIs(t, err, ErrInvalidAddress)
}

func TestSignItemRejectsShortKey(t *testing.T) {
	_, err := SignItem(Item{Method: MethodTransfer}, make(ed25519.PrivateKey, 10))
	assert.Error(t, err)
}

// flipped maps a hex digit to a different one, to corrupt a signature without
// changing its length.
var flipped = map[byte]string{'0': "1", '1': "2", '2': "3", '3': "4", '4': "5", '5': "6", '6': "7", '7': "8", '8': "9", '9': "a", 'a': "b", 'b': "c", 'c': "d", 'd': "e", 'e': "f", 'f': "0"}
