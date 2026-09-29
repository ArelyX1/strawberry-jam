package svc_test

import (
	"crypto/rand"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// TestPVMExecutorRelayedTransactionShapes checks that a relayed transaction is
// accepted in every shape the host's parser accepts. A wallet's library decides
// the shape, not the chain, so a guest that only understood one of them would
// turn a real wallet's transfer into a refusal.
func TestPVMExecutorRelayedTransactionShapes(t *testing.T) {
	chain := big.NewInt(5120)
	recipient := "0xcccccccccccccccccccccccccccccccccccccccc"
	amount := big.NewInt(250 * 1_000_000)

	key, err := secp256k1.GeneratePrivateKeyFromRand(rand.Reader)
	require.NoError(t, err)
	from := "0x" + hexOf(papucoin.KeccakForTest(key.PubKey().SerializeUncompressed()[1:])[12:])
	destination := make([]byte, 20)
	for i := range destination {
		destination[i] = 0xcc
	}

	legacy, err := papucoin.SignEVMLegacy(chain, 1, 1_000_000_000, 21_000, destination, amount, nil, key)
	require.NoError(t, err)

	accessList := [][][]byte{{
		papucoin.RlpStringForTest(make([]byte, 20)),
		papucoin.RlpStringForTest([]byte{0x01, 0x02, 0x03, 0x04}),
	}}
	eip2930, err := papucoin.SignEIP2930(chain, 1, 1_000_000_000, 60_000, destination, amount, nil, accessList, key)
	require.NoError(t, err)

	eip1559, err := papucoin.SignEIP1559(chain, 1, 1_000_000_000, 2_000_000_000, 21_000, destination, amount, nil, key)
	require.NoError(t, err)

	// All three shapes the host's parser accepts, because which one a wallet
	// emits is the wallet's choice and not something the chain gets to reject.
	shapes := []struct {
		name string
		raw  []byte
	}{
		{"legacy", legacy},
		{"eip2930", eip2930},
		{"eip1559", eip1559},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			// The host has to accept it, or the guest is not the only thing to fix.
			fields, err := papucoin.NewEVMRelay(chain).Recover("0x" + hexOf(shape.raw))
			require.NoError(t, err, "the host must accept a %s transaction", shape.name)
			require.Equal(t, from, fields.From)
			require.Equal(t, amount.String(), fields.Value.String())

			// And the guest has to produce a report out of it.
			exec, all := seededExecutor(t, chain.Int64(), testRelay{chainID: chain.Int64()})
			id := block.ServiceId(0)
			account := service.NewServiceAccount()
			account.Balance = 1_000_000_000
			item := papucoin.Item{
				Method: papucoin.MethodTransfer,
				Sender: from,
				To:     recipient,
				Nonce:  1,
				Amount: "250",
				Raw:    "0x" + hexOf(shape.raw),
			}
			result, err := exec.Refine(id, mustJSON(t, item), jamtime.Timeslot(0), 50_000_000_000, all)
			require.NoError(t, err)
			require.NotContains(t, string(result.Report), "relayError",
				"the guest refused a %s transaction the host accepted: %s", shape.name, result.Report)
			t.Logf("%s: %s", shape.name, result.Report)
		})
	}
}
