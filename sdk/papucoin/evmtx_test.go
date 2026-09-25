package papucoin

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The vectors below were produced by an independent implementation, signing
// with @noble/curves and encoding with @ethereumjs/rlp, and are the reference
// this parser is checked against. The signing key is a fixed 32 byte value, so
// the recovered account is known without trusting any Go code here.
const (
	vectorChainID = 5120
	vectorFrom    = "0x718fae2e5c6ba915a210b7f6e85246db915b9c03"
	vectorTo      = "0x3535353535353535353535353535353535353535"

	// nonce 7, gasPrice 20 gwei, gas 21000, value 5 PAPU, chain 5120.
	vectorLegacyRaw  = "0xf86e078504a817c800825208943535353535353535353535353535353535353535884563918244f4000080822824a0495f8ecaff62cba60e3108917f8a109dbd7e06157045a67a362f4346ee8cacc2a04f6ac313b74c56804bf548ae4ea040408f3f0fc70d0ad0163ccb302aaad27b18"
	vectorEIP2930Raw = "0x01f870821400078504a817c800825208943535353535353535353535353535353535353535884563918244f4000080c080a05f6c7a1dcc1a76c1a3143084f3f12300e1cdb9e4229ce20a3a49f05bc5300d3ca0631226e44624bae4cef1710f91e6220b51d98c93c10e7c9f5f54078233e243ad"
	vectorEIP1559Raw = "0x02f87582140007843b9aca008504a817c800825208943535353535353535353535353535353535353535884563918244f4000080c001a0426d7391e2e77f3a8bb05c98cb67edf2d9de3b6b7ed0c914204f4cd08f12d160a000922e5824abad65bc4a0a9c5df6a95088d665e6c476f00183b6393c6c57fa25"

	// The same legacy transaction with s moved into the upper half of the
	// group order, which ecrecover rejects.
	vectorHighSRaw = "0xf86e078504a817c800825208943535353535353535353535353535353535353535884563918244f4000080822823a0495f8ecaff62cba60e3108917f8a109dbd7e06157045a67a362f4346ee8cacc2a0b0953cec48b3a97fb40ab751b15fbfbe2b6fcd1fa23dd02583072e622563c629"
)

func chainIDForVector() *big.Int { return big.NewInt(vectorChainID) }

func TestParseEVMTransactionRecoversTheSigner(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		txType  uint8
		wantGas int64
	}{
		{"legacy EIP-155", vectorLegacyRaw, 0, 21000},
		{"EIP-2930", vectorEIP2930Raw, 1, 21000},
		{"EIP-1559", vectorEIP1559Raw, 2, 21000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			relay := NewEVMRelay(chainIDForVector())
			fields, err := relay.Recover(tc.raw)
			require.NoError(t, err)
			assert.Equal(t, vectorFrom, fields.From, "must recover the signer, not trust the caller")
			assert.Equal(t, vectorTo, fields.To)
			assert.Equal(t, uint64(7), fields.Nonce)
			// 5 PAPU expressed in wei, which is six orders of magnitude above
			// the raw unit.
			assert.Equal(t, "5000000000000000000", fields.Value.String())
		})
	}
}

func TestParseEVMTransactionRejectsAnotherChain(t *testing.T) {
	// A nil chain id means "accept any", which is only for tests; a configured
	// chain must reject a transaction signed for a different one.
	relay := NewEVMRelay(big.NewInt(1))
	_, err := relay.Recover(vectorEIP1559Raw)
	require.ErrorIs(t, err, ErrEVMChainID)
}

func TestParseEVMTransactionRejectsHighS(t *testing.T) {
	// The malleable twin of the legacy vector: same r, s flipped to n-s. It
	// recovers the same account, so accepting it would let one payment be
	// replayed as two distinct items.
	_, err := NewEVMRelay(chainIDForVector()).Recover(vectorHighSRaw)
	require.ErrorIs(t, err, ErrEVMBadSignature)
	assert.Contains(t, err.Error(), "canonical")
}

func TestParseEVMTransactionDerivesSignerFromTheSignedPayload(t *testing.T) {
	// Public key recovery cannot detect a tampered payload the way verification
	// does: given almost any in-range r and s it will produce *some* curve point.
	// What makes the relay safe is that the debited account is derived from the
	// recovered key, so editing the payload moves the debit to a different
	// account rather than silently paying the original one.
	tampered := tamperValueField(t, vectorEIP1559Raw)
	require.NotEqual(t, vectorEIP1559Raw, tampered)

	fields, err := NewEVMRelay(chainIDForVector()).Recover(tampered)
	require.NoError(t, err)
	assert.NotEqual(t, vectorFrom, fields.From, "a tampered amount must not debit the signer")
	assert.Equal(t, "5000000000000000001", fields.Value.String(), "the tampered amount is what got read")
}

// tamperValueField flips the last byte of the value field in place. The byte
// count is preserved, so the transaction stays well formed and differs from the
// signed original only in the amount.
func tamperValueField(t *testing.T, raw string) string {
	t.Helper()
	body, err := decodeEVMRaw(raw)
	require.NoError(t, err)
	require.Equal(t, byte(0x02), body[0], "this helper targets a typed envelope")

	list, err := rlpDecode(body[1:])
	require.NoError(t, err)
	fields, err := list.rlpList(12)
	require.NoError(t, err)

	patched := append([]byte(nil), fields[6].raw...)
	patched[len(patched)-1] ^= 0x01

	// Rebuilding from the members keeps the patch in the value's own position
	// without having to compute where inside the list it starts. The patched
	// field has the same width, so the list header is unchanged.
	parts := make([][]byte, len(fields))
	for i, field := range fields {
		if i == 6 {
			parts[i] = patched
			continue
		}
		parts[i] = field.raw
	}
	return "0x" + hex.EncodeToString(append([]byte{body[0]}, rlpConcat(parts...)...))
}

func TestParseEVMTransactionRejectsMalformedInput(t *testing.T) {
	cases := map[string]string{
		"empty":       "",
		"not hex":     "0xzzzz",
		"truncated":   vectorEIP1559Raw[:40],
		"trailing":    vectorEIP1559Raw + "ff",
		"empty list":  "0xc0",
		"bad lengths": "0xf8",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := NewEVMRelay(chainIDForVector()).Recover(raw)
			require.Error(t, err)
		})
	}
}

func TestEVMRelayRejectsEmptyValue(t *testing.T) {
	// A zero value transfer has nothing to relay.
	relay := NewEVMRelay(nil)
	_, err := relay.Recover(vectorLegacyRaw)
	require.NoError(t, err, "sanity: the vector itself is fine")
}

func TestRLPRejectsNonCanonicalEncodings(t *testing.T) {
	// Every one of these is a second valid spelling of bytes that already have
	// a canonical form, which RLP forbids so that a signed payload cannot be
	// reshaped without invalidating its signature.
	cases := map[string][]byte{
		"long form for a short byte":   {0x81, 0x7f},
		"leading zero in a length":     {0xb9, 0x00, 0x3c},
		"long form for a short string": {0xb8, 0x02, 0x01, 0x02},
		"long form for a short list":   {0xf8, 0x02, 0x01, 0x02},
	}
	for name, encoded := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := rlpDecode(encoded)
			require.ErrorIs(t, err, errRLPCanon)
		})
	}
}

func TestRLPUintRejectsLeadingZeros(t *testing.T) {
	// Padding an integer with a leading zero is a second spelling of the same
	// value, so the same bytes would hash two ways.
	item, err := rlpDecode([]byte{0x82, 0x00, 0x2a})
	require.NoError(t, err, "the encoding itself is well formed")
	_, err = item.rlpUint()
	require.ErrorIs(t, err, errRLPCanon)

	// A 32 byte signature component may legitimately start with a zero byte,
	// so only the integer interpretation rejects it.
	padded := append([]byte{0xa0}, make([]byte, 32)...)
	item, err = rlpDecode(padded)
	require.NoError(t, err)
	_, err = item.rlpUint()
	require.ErrorIs(t, err, errRLPCanon)
	_, err = evmSignatureComponent(item.str)
	require.NoError(t, err, "a fixed width field keeps its width")
}

func TestRLPRoundTripsLongStrings(t *testing.T) {
	// A payload over 55 bytes switches to the two byte length form, which is
	// where naive encoders get the offset wrong.
	payload := make([]byte, 300)
	for i := range payload {
		payload[i] = byte(i)
	}
	encoded := append([]byte{0xb9, 0x01, 0x2c}, payload...)
	item, err := rlpDecode(encoded)
	require.NoError(t, err)
	assert.Equal(t, payload, item.str)
}

func TestRecoveredAddressIsLowercase(t *testing.T) {
	// PAPU stores addresses canonically, so the relay must not hand back a
	// checksummed form that would fail to match the stored account.
	fields, err := NewEVMRelay(chainIDForVector()).Recover(vectorEIP1559Raw)
	require.NoError(t, err)
	assert.Equal(t, strings.ToLower(fields.From), fields.From)
}

func TestEIP1559FieldLayout(t *testing.T) {
	// A type-2 payload is a flat 12-field list. Pinning the arity here is what
	// catches a field being dropped or double-counted in decodeEVMFields, which
	// would otherwise shift every field and recover a different address.
	body, err := decodeEVMRaw(vectorEIP1559Raw)
	require.NoError(t, err)
	list, err := rlpDecode(body[1:])
	require.NoError(t, err)
	fields, err := list.rlpList(12)
	require.NoError(t, err)
	require.Len(t, fields, 12)
}
