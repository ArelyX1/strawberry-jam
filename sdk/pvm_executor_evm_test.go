package svc_test

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// testRelay recovers senders with the same implementation the chain uses, which
// is the point: the guest is not being handed an identity, it is being handed
// the answer to a question the host answered.
type testRelay struct{ chainID int64 }

func (r testRelay) Recover(rawHex string) (string, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(rawHex, "0x"))
	if err != nil {
		return "", err
	}
	tx, err := papucoin.ParseEVMTransaction(raw, big.NewInt(r.chainID))
	if err != nil {
		return "", err
	}
	return tx.From, nil
}

// relayedFixture is a signed EIP-1559 transaction built the way a wallet builds
// it, so the guest is checked against a real signature rather than a stub.
type relayedFixture struct {
	key       *secp256k1.PrivateKey
	from      string
	to        string
	evmNonce  uint64
	raw       string
	weiAmount *big.Int
}

func newRelayedFixture(t *testing.T, evmNonce uint64, microPAPU int64) relayedFixture {
	t.Helper()
	key, err := secp256k1.GeneratePrivateKeyFromRand(rand.Reader)
	require.NoError(t, err)

	// the EVM address is the keccak of the uncompressed key, not the key itself
	from := "0x" + hexOf(papucoin.KeccakForTest(key.PubKey().SerializeUncompressed()[1:])[12:])
	to := "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	wei := new(big.Int).Mul(big.NewInt(microPAPU), big.NewInt(1_000_000))

	raw, err := papucoin.SignEIP1559(
		big.NewInt(1),
		evmNonce,
		1_000_000_000,
		2_000_000_000,
		21_000,
		mustAddr(t, to),
		wei,
		nil,
		key,
	)
	require.NoError(t, err)

	return relayedFixture{
		key:       key,
		from:      from,
		to:        to,
		evmNonce:  evmNonce,
		raw:       "0x" + hexOf(raw),
		weiAmount: wei,
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

func mustAddr(t *testing.T, hex string) []byte {
	t.Helper()
	raw := make([]byte, 20)
	for i := 0; i < 20; i++ {
		raw[i] = decodeHexByte(hex[2+i*2], hex[3+i*2])
	}
	return raw
}

func decodeHexByte(hi, lo byte) byte {
	return hexVal(hi)<<4 | hexVal(lo)
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	blob, err := json.Marshal(v)
	require.NoError(t, err)
	return blob
}

func unmarshalJSON(blob []byte, v any) error {
	return json.Unmarshal(blob, v)
}

// TestPVMExecutorRelayedEVM drives a raw EIP-1559 transaction through the guest.
// A wallet that only speaks Ethereum holds a secp256k1 key and nothing else, so
// the only way it can move balance is if the guest recovers the sender from the
// signature rather than believing the item.
func TestPVMExecutorRelayedEVM(t *testing.T) {
	fixture := newRelayedFixture(t, 3, 250)

	item := papucoin.Item{
		Method: papucoin.MethodTransfer,
		Sender: fixture.from,
		To:     fixture.to,
		Nonce:  1,
		Amount: "250",
		Raw:    fixture.raw,
	}

	exec, all := seededExecutor(t, 1, testRelay{chainID: 1})
	id := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000

	result, err := exec.Refine(id, mustJSON(t, item), jamtime.Timeslot(0), 50_000_000_000, all)
	require.NoError(t, err)
	require.NotEmpty(t, result.Report, "the guest refused a valid relayed transaction")
	t.Logf("report: %s", result.Report)

	var report struct {
		Op       string `json:"op"`
		Sender   string `json:"sender"`
		To       string `json:"to"`
		Amount   string `json:"amount"`
		Fee      string `json:"fee"`
		EVMNonce uint64 `json:"evmNonce"`
	}
	require.NoError(t, json.Unmarshal(result.Report, &report))
	require.Equal(t, papucoin.MethodTransfer, report.Op)
	require.Equal(t, fixture.from, report.Sender, "the sender must be recovered from the signature")
	require.Equal(t, fixture.to, report.To)
	require.Equal(t, "250", report.Amount, "250 micro-PAPU out of the wei value")
	require.NotZero(t, report.EVMNonce, "the report must carry the nonce the host read")
}

// TestPVMExecutorRelayedRejectsTampered checks that a transaction whose
// destination was swapped after signing is refused. The signature covers the
// destination, so comparing it against the claimed one has to catch it.
func TestPVMExecutorRelayedRejectsTampered(t *testing.T) {
	fixture := newRelayedFixture(t, 1, 250)

	item := papucoin.Item{
		Method: papucoin.MethodTransfer,
		Sender: fixture.from,
		To:     "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Nonce:  1,
		Amount: "250",
		Raw:    fixture.raw,
	}

	exec, all := seededExecutor(t, 1, testRelay{chainID: 1})
	id := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000

	result, err := exec.Refine(id, mustJSON(t, item), jamtime.Timeslot(0), 50_000_000_000, all)
	require.NoError(t, err)
	// The signature is valid, so the host recovers the real sender; the guest is
	// the one that refuses, because the claimed destination is not the signed one.
	require.Contains(t, string(result.Report), "relayError",
		"a destination that disagrees with the signature must be refused")
}

// TestPVMExecutorRelayedRejectsForeignChain makes sure a transaction signed for
// another chain is not replayed here, which is what folding the chain id into
// the signing hash is for.
func TestPVMExecutorRelayedRejectsForeignChain(t *testing.T) {
	key, err := secp256k1.GeneratePrivateKeyFromRand(rand.Reader)
	require.NoError(t, err)
	raw, err := papucoin.SignEIP1559(
		big.NewInt(137), // a different chain
		1,
		1_000_000_000,
		2_000_000_000,
		21_000,
		mustAddr(t, "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		big.NewInt(250*1_000_000),
		nil,
		key,
	)
	require.NoError(t, err)

	// the EVM address is the keccak of the uncompressed key, not the key itself
	from := "0x" + hexOf(papucoin.KeccakForTest(key.PubKey().SerializeUncompressed()[1:])[12:])
	item := papucoin.Item{
		Method: papucoin.MethodTransfer,
		Sender: from,
		To:     "0xaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Nonce:  1,
		Amount: "250",
		Raw:    "0x" + hexOf(raw),
	}

	exec, all := seededExecutor(t, 1, testRelay{chainID: 1})
	id := block.ServiceId(0)
	account := service.NewServiceAccount()
	account.Balance = 1_000_000_000

	result, err := exec.Refine(id, mustJSON(t, item), jamtime.Timeslot(0), 50_000_000_000, all)
	require.Error(t, err, "a transaction for another chain must not be replayed here")
	require.Empty(t, result.Report)
	_ = ecdsa.NewSignature
}
