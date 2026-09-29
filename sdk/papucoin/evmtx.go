package papucoin

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/eigerco/strawberry/internal/crypto"
)

// EVMTransaction is the part of an Ethereum transaction that a relayed PAPU
// transfer depends on. The signer is recovered from the signature, never taken
// from the item, so a caller cannot name an account it does not control.
type EVMTransaction struct {
	// Type is 0 for a legacy transaction, or the typed envelope byte.
	Type uint8
	// Hash is the keccak256 of the raw transaction, which is its identity.
	Hash     []byte
	From     string
	To       string
	Value    *big.Int
	Nonce    *big.Int
	GasLimit *big.Int
	Data     []byte
}

var (
	// ErrEVMEmpty is returned for a transaction with no bytes.
	ErrEVMEmpty = errors.New("papucoin: empty transaction")
	// ErrEVMBadFieldCount is returned when a transaction has the wrong shape.
	ErrEVMBadFieldCount = errors.New("papucoin: wrong number of transaction fields")
	// ErrEVMChainID is returned for a transaction signed for another chain.
	ErrEVMChainID = errors.New("papucoin: transaction is not signed for this chain")
	// ErrEVMUnsigned is returned for a legacy transaction with an empty r or s.
	ErrEVMUnsigned = errors.New("papucoin: transaction carries no signature")
	// ErrEVMContractCreation is returned for a transaction with no recipient.
	ErrEVMContractCreation = errors.New("papucoin: contract creation is not supported")
	// ErrEVMBadParity is returned for a parity bit outside {0, 1}.
	ErrEVMBadParity = errors.New("papucoin: signature parity must be 0 or 1")
	// ErrEVMBadSignature is returned when the signature cannot be recovered.
	ErrEVMBadSignature = errors.New("papucoin: cannot recover transaction signer")
	// ErrEVMBadV is returned for a legacy v that encodes no chain and no parity.
	ErrEVMBadV = errors.New("papucoin: legacy signature v is out of range")
)

// evmAddressSize is the byte length of an EVM account address.
const evmAddressSize = 20

// evmSignatureSize is the byte length of the r and s components of a signature.
const evmSignatureSize = 32

// ParseEVMTransaction decodes a signed Ethereum transaction, verifies that it
// was signed for chainID, and recovers the account that signed it. Legacy,
// EIP-2930 and EIP-1559 envelopes are supported; typed transactions must
// already carry a chain id, so they cannot be replayed onto another chain.
func ParseEVMTransaction(raw []byte, chainID *big.Int) (*EVMTransaction, error) {
	if len(raw) == 0 {
		return nil, ErrEVMEmpty
	}
	sum := keccak(raw)
	if raw[0] == 0x01 || raw[0] == 0x02 {
		return parseTypedEVMTransaction(raw[1:], raw[0], chainID, sum)
	}
	return parseLegacyEVMTransaction(raw, chainID, sum)
}

// parseTypedEVMTransaction handles the EIP-2718 envelope: a type byte followed
// by an RLP list. The signing hash covers the type byte and the nine unsigned
// fields, so replay protection has to come from the chain id field.
func parseTypedEVMTransaction(payload []byte, txType uint8, chainID *big.Int, sum []byte) (*EVMTransaction, error) {
	item, err := rlpDecode(payload)
	if err != nil {
		return nil, err
	}
	// EIP-1559 carries nine unsigned fields and EIP-2930 carries eight, because
	// only the former splits the fee into a priority and a maximum. Locating the
	// trailing fields from the end of the unsigned section handles both without
	// letting a field index drift.
	fields, err := item.rlpList(map[uint8]int{1: 11, 2: 12}[txType])
	if err != nil {
		return nil, ErrEVMBadFieldCount
	}
	unsigned, signature := fields[:len(fields)-3], fields[len(fields)-3:]
	n := len(unsigned)

	signedChainID, err := unsigned[0].rlpUint()
	if err != nil {
		return nil, err
	}
	if chainID != nil && signedChainID.Cmp(chainID) != 0 {
		return nil, ErrEVMChainID
	}
	nonce, err := unsigned[1].rlpUint()
	if err != nil {
		return nil, err
	}
	gasLimit, err := unsigned[n-5].rlpUint()
	if err != nil {
		return nil, err
	}
	to, err := evmRecipient(unsigned[n-4])
	if err != nil {
		return nil, err
	}
	value, err := unsigned[n-3].rlpUint()
	if err != nil {
		return nil, err
	}
	parity, err := evmParity(signature[0])
	if err != nil {
		return nil, err
	}

	parts := make([][]byte, n)
	for i, field := range unsigned {
		parts[i] = field.raw
	}
	signing := append([]byte{txType}, rlpConcat(parts...)...)
	from, err := recoverEVMAddress(keccak(signing), signature[1].str, signature[2].str, parity)
	if err != nil {
		return nil, err
	}

	return &EVMTransaction{
		Type:     txType,
		Hash:     sum,
		From:     from,
		To:       to,
		Value:    value,
		Nonce:    nonce,
		GasLimit: gasLimit,
		Data:     append([]byte(nil), unsigned[n-2].str...),
	}, nil
}

// parseLegacyEVMTransaction handles the unprotected EIP-155 envelope, where the
// chain id is folded into v.
func parseLegacyEVMTransaction(raw []byte, chainID *big.Int, sum []byte) (*EVMTransaction, error) {
	item, err := rlpDecode(raw)
	if err != nil {
		return nil, err
	}
	fields, err := item.rlpList(9)
	if err != nil {
		return nil, ErrEVMBadFieldCount
	}
	unsigned, signature := fields[:6], fields[6:]
	if len(signature[1].str) == 0 || len(signature[2].str) == 0 {
		return nil, ErrEVMUnsigned
	}

	to, err := evmRecipient(unsigned[3])
	if err != nil {
		return nil, err
	}
	value, err := unsigned[4].rlpUint()
	if err != nil {
		return nil, err
	}
	nonce, err := unsigned[0].rlpUint()
	if err != nil {
		return nil, err
	}
	gasLimit, err := unsigned[2].rlpUint()
	if err != nil {
		return nil, err
	}
	v, err := signature[0].rlpUint()
	if err != nil {
		return nil, err
	}

	// v is either the bare 27/28 of an unprotected transaction, or
	// 35 + 2*chainId + parity as EIP-155 defines it.
	var signing []byte
	var parity uint64
	switch {
	case v.Cmp(big.NewInt(27)) == 0 || v.Cmp(big.NewInt(28)) == 0:
		parity = new(big.Int).Sub(v, big.NewInt(27)).Uint64()
		signing = rlpConcat(
			unsigned[0].raw, unsigned[1].raw, unsigned[2].raw,
			unsigned[3].raw, unsigned[4].raw, unsigned[5].raw,
		)
	default:
		offset := new(big.Int).Sub(v, big.NewInt(35))
		if offset.Sign() < 0 {
			return nil, ErrEVMBadV
		}
		signedChainID := new(big.Int).Rsh(offset, 1)
		parity = new(big.Int).And(offset, big.NewInt(1)).Uint64()
		if chainID != nil && signedChainID.Cmp(chainID) != 0 {
			return nil, ErrEVMChainID
		}
		// EIP-155 signs the original six fields plus the chain id and two
		// empty placeholders, so the transaction cannot be moved between
		// chains.
		signing = rlpConcat(
			unsigned[0].raw, unsigned[1].raw, unsigned[2].raw,
			unsigned[3].raw, unsigned[4].raw, unsigned[5].raw,
			rlpEncodeUint(signedChainID), rlpEncodeUint(nil), rlpEncodeUint(nil),
		)
	}

	from, err := recoverEVMAddress(keccak(signing), signature[1].str, signature[2].str, parity)
	if err != nil {
		return nil, err
	}

	return &EVMTransaction{
		Type:     0,
		Hash:     sum,
		From:     from,
		To:       to,
		Value:    value,
		Nonce:    nonce,
		GasLimit: gasLimit,
		Data:     append([]byte(nil), unsigned[5].str...),
	}, nil
}

// keccak hashes with the legacy Keccak256 that Ethereum uses, and returns a
// slice rather than the fixed size array the JAM hash type carries.
func keccak(data []byte) []byte {
	sum := crypto.KeccakData(data)
	return sum[:]
}

// evmRecipient renders the recipient, rejecting the empty recipient that a
// contract creation would carry.
func evmRecipient(item rlpItem) (string, error) {
	if len(item.str) != evmAddressSize {
		return "", ErrEVMContractCreation
	}
	return "0x" + hex.EncodeToString(item.str), nil
}

func evmParity(item rlpItem) (uint64, error) {
	v, err := item.rlpUint()
	if err != nil {
		return 0, err
	}
	if v.Cmp(big.NewInt(1)) > 0 {
		return 0, ErrEVMBadParity
	}
	return v.Uint64(), nil
}

// recoverEVMAddress reproduces the ecrecover precompile. The account is the last
// 20 bytes of the keccak256 of the uncompressed public key without its 0x04
// tag, which is the same derivation the gateway uses in TypeScript.
func recoverEVMAddress(sigHash []byte, r, s []byte, parity uint64) (string, error) {
	if parity > 1 {
		return "", ErrEVMBadParity
	}
	rBytes, err := evmSignatureComponent(r)
	if err != nil {
		return "", err
	}
	sBytes, err := evmSignatureComponent(s)
	if err != nil {
		return "", err
	}

	// ecrecover rejects a signature whose s is in the upper half of the order,
	// because flipping s to n-s would otherwise produce a second valid
	// transaction spending the same balance.
	var sScalar secp256k1.ModNScalar
	if overflow := sScalar.SetByteSlice(sBytes); overflow {
		return "", ErrEVMBadSignature
	}
	if sScalar.IsOverHalfOrder() {
		return "", fmt.Errorf("%w: signature s is not canonical", ErrEVMBadSignature)
	}

	// secp256k1 expects a recovery byte before r and s, and that byte is 27
	// plus the parity, which is exactly how Ethereum encodes v.
	var compact [1 + 2*evmSignatureSize]byte
	compact[0] = byte(27 + parity)
	copy(compact[1:], rBytes)
	copy(compact[1+evmSignatureSize:], sBytes)

	publicKey, _, err := ecdsa.RecoverCompact(compact[:], sigHash)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrEVMBadSignature, err)
	}
	uncompressed := publicKey.SerializeUncompressed()
	return "0x" + hex.EncodeToString(keccak(uncompressed[1:])[HashAddressOffset:]), nil
}

// evmSignatureComponent left pads a signature component to 32 bytes. RLP drops
// leading zeros, so a small r or s arrives short.
func evmSignatureComponent(b []byte) ([]byte, error) {
	if len(b) > evmSignatureSize {
		return nil, fmt.Errorf("%w: signature component is %d bytes", ErrEVMBadSignature, len(b))
	}
	out := make([]byte, evmSignatureSize)
	copy(out[evmSignatureSize-len(b):], b)
	return out, nil
}

// HashAddressOffset is where a 32 byte hash starts its 20 byte address.
const HashAddressOffset = 32 - evmAddressSize

// EVMRelay is the production RelayVerifier. It decodes a raw Ethereum
// transaction and reports the account that actually signed it, so the service
// debits a key the caller controls instead of a key the caller claims.
type EVMRelay struct {
	// ChainID is the only chain whose transactions are accepted. A nil
	// ChainID skips the check, which is only useful in tests.
	ChainID *big.Int
}

// NewEVMRelay returns a verifier for the given chain.
func NewEVMRelay(chainID *big.Int) *EVMRelay {
	return &EVMRelay{ChainID: chainID}
}

// Recover implements RelayVerifier.
func (e *EVMRelay) Recover(rawHex string) (EVMFields, error) {
	raw, err := decodeEVMRaw(rawHex)
	if err != nil {
		return EVMFields{}, err
	}
	tx, err := ParseEVMTransaction(raw, e.ChainID)
	if err != nil {
		return EVMFields{}, err
	}
	if !tx.Nonce.IsUint64() {
		return EVMFields{}, fmt.Errorf("papucoin: transaction nonce %s does not fit in 64 bits", tx.Nonce)
	}
	if tx.Value.Sign() <= 0 {
		return EVMFields{}, ErrEmptyValue
	}
	return EVMFields{
		From:  strings.ToLower(tx.From),
		To:    strings.ToLower(tx.To),
		Nonce: tx.Nonce.Uint64(),
		Value: tx.Value,
	}, nil
}

func decodeEVMRaw(rawHex string) ([]byte, error) {
	trimmed := strings.TrimPrefix(strings.TrimSpace(rawHex), "0x")
	if trimmed == "" {
		return nil, ErrEVMEmpty
	}
	raw, err := hex.DecodeString(trimmed)
	if err != nil {
		return nil, fmt.Errorf("papucoin: transaction is not hex: %w", err)
	}
	return raw, nil
}

// SignEIP1559 builds and signs a typed EIP-1559 transaction, returning its raw
// encoding. It is the counterpart of [ParseEVMTransaction]: a test, a wallet
// simulator or a tool needs to produce the bytes a chain would accept, and
// hand-rolling the envelope in every caller is how the two sides drift apart.
// halfOrder is (n-1)/2, the boundary above which ecrecover rejects s. Signatures
// are flipped to sit below it, so the same signed digest has one canonical
// encoding.
var halfOrder = func() secp256k1.ModNScalar {
	// (n-1)/2 for secp256k1, taken from the curve order.
	order, _ := new(big.Int).SetString(
		"fffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141", 16)
	half := new(big.Int).Rsh(order, 1)
	var s secp256k1.ModNScalar
	if overflow := s.SetByteSlice(half.Bytes()); overflow {
		panic("secp256k1 half order does not fit the scalar")
	}
	return s
}()

func SignEIP1559(
	chainID *big.Int,
	nonce uint64,
	gasTipCap, gasFeeCap, gasLimit uint64,
	to []byte,
	value *big.Int,
	data []byte,
	key *secp256k1.PrivateKey,
) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("papucoin: signing needs a private key")
	}
	if len(to) != 20 {
		return nil, fmt.Errorf("papucoin: destination is %d bytes, want 20", len(to))
	}

	fields := [][]byte{
		rlpEncodeUint(chainID),
		rlpEncodeUint(new(big.Int).SetUint64(nonce)),
		rlpEncodeUint(new(big.Int).SetUint64(gasTipCap)),
		rlpEncodeUint(new(big.Int).SetUint64(gasFeeCap)),
		rlpEncodeUint(new(big.Int).SetUint64(gasLimit)),
		rlpEncodeString(to),
		rlpEncodeUint(value),
		{0x80}, // empty data
		{0xc0}, // empty access list
	}

	// The parser reconstructs the signing hash by concatenating the raw
	// encodings of the unsigned fields, so signing has to do the same rather
	// than re-encode them into a list.
	signing := append([]byte{0x02}, rlpConcat(fields...)...)
	digest := keccak(signing)

	// SignCompact returns a magic code: 27 plus the recovery id, with bit 3 set
	// when the public key was requested compressed. The recovery id is what
	// ecrecover needs, so the offset and the compression bit come off.
	compact := ecdsa.SignCompact(key, digest, true)
	recovery := (compact[0] - 27) & 3
	yParity := recovery & 1
	signature := append([]byte{recovery}, compact[1:]...)
	r := signature[1:33]
	s := signature[33:65]
	// signRFC6979 already produces a low-s signature, so there is nothing to
	// flip here. A recovery id above one would mean otherwise, and it is worth
	// refusing rather than silently emitting something the chain rejects.
	if recovery > 1 {
		return nil, fmt.Errorf("%w: signing produced a high s", ErrEVMBadSignature)
	}
	// A yParity of 1 is the bare byte 0x01. A yParity of 0 has to be the empty
	// string: a single 0x00 byte would be an integer with a leading zero, which
	// is the non-canonical spelling of zero.
	yParityField := rlpEncodeUint(big.NewInt(int64(yParity)))

	signed := make([][]byte, 0, len(fields)+3)
	signed = append(signed, fields...)
	signed = append(signed, yParityField)
	signed = append(signed, rlpEncodeString(r))
	signed = append(signed, rlpEncodeString(s))
	return append([]byte{0x02}, rlpEncodeList(signed)...), nil
}

// KeccakForTest exposes the chain's keccak so that a test can compute an address
// the same way a wallet would. It is only for tests and fixtures; nothing in the
// service should need it.
func KeccakForTest(data []byte) []byte {
	return keccak(data)
}

// SignEVMLegacy builds and signs a pre-EIP-2718 transaction, returning its raw
// encoding. The chain id is folded into v the way EIP-155 prescribes, so a legacy
// transaction is as chain-bound as a typed one.
func SignEVMLegacy(
	chainID *big.Int,
	nonce uint64,
	gasPrice uint64,
	gasLimit uint64,
	to []byte,
	value *big.Int,
	data []byte,
	key *secp256k1.PrivateKey,
) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("papucoin: signing needs a private key")
	}
	if len(to) != 20 {
		return nil, fmt.Errorf("papucoin: destination is %d bytes, want 20", len(to))
	}
	if data == nil {
		data = []byte{}
	}

	unsigned := [][]byte{
		rlpEncodeUint(new(big.Int).SetUint64(nonce)),
		rlpEncodeUint(new(big.Int).SetUint64(gasPrice)),
		rlpEncodeUint(new(big.Int).SetUint64(gasLimit)),
		rlpEncodeString(to),
		rlpEncodeUint(value),
		rlpEncodeString(data),
	}

	// EIP-155: sign over the six fields, then the chain id and two empty
	// placeholders, and report it in v as chainId*2 + 35 + parity. The
	// placeholders have to be encoded exactly as the parser decodes them, so
	// rlpEncodeUint(nil) is used rather than a literal byte.
	signingChain := rlpConcat(append(append([][]byte{}, unsigned...),
		rlpEncodeUint(chainID), rlpEncodeUint(nil), rlpEncodeUint(nil))...)

	digest := keccak(signingChain)
	compact := ecdsa.SignCompact(key, digest, true)
	recovery := (compact[0] - 27) & 3
	r := compact[1:33]
	s := compact[33:65]
	if recovery > 1 {
		return nil, fmt.Errorf("%w: signing produced a high s", ErrEVMBadSignature)
	}

	v := new(big.Int).Mul(chainID, big.NewInt(2))
	v.Add(v, big.NewInt(int64(35+recovery)))

	signed := make([][]byte, 0, 9)
	signed = append(signed, unsigned...)
	signed = append(signed, rlpEncodeUint(v), rlpEncodeString(r), rlpEncodeString(s))
	return rlpEncodeList(signed), nil
}

// SignEIP2930 builds and signs an EIP-2930 transaction, the type that exists
// only to carry an access list. The chain accepts it, so a wallet that emits it
// has to be able to move balance.
func SignEIP2930(
	chainID *big.Int,
	nonce uint64,
	gasPrice uint64,
	gasLimit uint64,
	to []byte,
	value *big.Int,
	data []byte,
	accessList [][][]byte,
	key *secp256k1.PrivateKey,
) ([]byte, error) {
	if key == nil {
		return nil, fmt.Errorf("papucoin: signing needs a private key")
	}
	if len(to) != 20 {
		return nil, fmt.Errorf("papucoin: destination is %d bytes, want 20", len(to))
	}
	if data == nil {
		data = []byte{}
	}

	fields := [][]byte{
		rlpEncodeUint(chainID),
		rlpEncodeUint(new(big.Int).SetUint64(nonce)),
		rlpEncodeUint(new(big.Int).SetUint64(gasPrice)),
		rlpEncodeUint(new(big.Int).SetUint64(gasLimit)),
		rlpEncodeString(to),
		rlpEncodeUint(value),
		rlpEncodeString(data),
		rlpEncodeListOfLists(accessList),
	}

	digest := keccak(append([]byte{0x01}, rlpEncodeList(fields)...))
	compact := ecdsa.SignCompact(key, digest, true)
	recovery := (compact[0] - 27) & 3
	if recovery > 1 {
		return nil, fmt.Errorf("%w: signing produced a high s", ErrEVMBadSignature)
	}
	r := compact[1:33]
	s := compact[33:65]

	signed := make([][]byte, 0, len(fields)+3)
	signed = append(signed, fields...)
	signed = append(signed, rlpEncodeUint(big.NewInt(int64(recovery))))
	signed = append(signed, rlpEncodeString(r), rlpEncodeString(s))
	return append([]byte{0x01}, rlpEncodeList(signed)...), nil
}

// rlpEncodeListOfLists encodes a list whose items are themselves lists, which is
// the shape an EIP-2930 access list takes.
func rlpEncodeListOfLists(items [][][]byte) []byte {
	encoded := make([][]byte, 0, len(items))
	for _, item := range items {
		encoded = append(encoded, rlpEncodeList(item))
	}
	return rlpEncodeList(encoded)
}

// RlpStringForTest exposes the string encoding so a caller building a
// transaction field, such as a test assembling an access list, encodes it the
// same way the signers do rather than by hand.
func RlpStringForTest(b []byte) []byte {
	return rlpEncodeString(b)
}
