package svc_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
	svc "github.com/eigerco/strawberry/sdk"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// signedItem returns a genuine signed welcome item. When tampered is set the
// signature's first byte is flipped, which is what an attacker who cannot sign
// for the account would produce.
func signedItem(t *testing.T, tampered bool) []byte {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	address, err := papucoin.AddressFromPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := papucoin.SignItem(papucoin.Item{
		Method:       "welcome",
		Sender:       address,
		Nonce:        1,
		MustBeSigned: true,
	}, private)
	if err != nil {
		t.Fatal(err)
	}
	if tampered {
		signature := []byte(signed.Signature)
		if signature[2] == '0' {
			signature[2] = '1'
		} else {
			signature[2] = '0'
		}
		signed.Signature = string(signature)
	}
	blob, err := json.Marshal(signed)
	if err != nil {
		t.Fatal(err)
	}
	return blob
}

// TestPVMExecutorSignature pins the guest's signature check. A signed item must
// refine only when the signature is the one the sender's key made; a tampered
// one has to be declined, because a sender that cannot be proven must not move
// balances.
func TestPVMExecutorSignature(t *testing.T) {
	blob := guestBlob(t)
	for _, c := range []struct {
		item   []byte
		reject bool
		label  string
	}{
		{signedItem(t, false), false, "valid signature"},
		{signedItem(t, true), true, "tampered signature"},
	} {
		item := c.item
		exec := svc.NewPVMExecutor(blob)
		id := block.ServiceId(0)
		account := service.NewServiceAccount()
		account.Balance = 1_000_000_000

		result, err := exec.Refine(id, item, jamtime.Timeslot(0), 50_000_000_000, service.ServiceState{id: account})
		if err != nil {
			t.Fatalf("%s: refine: %v", c.label, err)
		}
		got := len(result.Report) == 0
		if got != c.reject {
			if c.reject {
				t.Errorf("%s: guest accepted it; a bad signature must be declined", c.label)
			} else {
				t.Errorf("%s: guest declined a legitimate signature", c.label)
			}
			continue
		}
		if got {
			t.Logf("%s: declined, as it should", c.label)
		} else {
			t.Logf("%s: refined into %s", c.label, result.Report)
		}
	}
}
