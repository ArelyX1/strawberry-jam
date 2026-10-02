package node

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"testing"

	"github.com/eigerco/strawberry/internal/validator"
)

// Every machine listens on the same port, which is the normal case once the nodes
// are on separate hosts: 30333 everywhere. When both ends of a pair dial each
// other at the same moment there are two connections per pair, and the pair has to
// agree on which one to keep. Deciding that by comparing ports works on one machine
// only by luck: the listening ports are equal, so what is left is the ephemeral
// ports, and each end sees them in a different order. Each end then keeps a
// different connection. Both stay connected, the peer list reports two neighbours,
// and nothing is ever announced.
//
// This is the rule that both ends compute from the same two keys, so exactly one of
// them dials and there is one connection per pair with nothing left to disagree
// about.
func TestBothEndsOfAPairAgreeOnWhoDials(t *testing.T) {
	seeds := []string{
		"0000000000000000000000000000000000000000000000000000000000000001",
		"0000000000000000000000000000000000000000000000000000000000000002",
		"0000000000000000000000000000000000000000000000000000000000000003",
	}

	type end struct {
		n    *Node
		key  ed25519.PublicKey
		name string
	}
	var ends []end
	for i, s := range seeds {
		seed, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub := priv.Public().(ed25519.PublicKey)
		ends = append(ends, end{
			n:    &Node{ValidatorManager: &validator.ValidatorManager{Keys: validator.ValidatorKeys{EdPub: pub}}},
			key:  pub,
			name: string(rune('A' + i)),
		})
	}

	for _, a := range ends {
		for _, b := range ends {
			if a.name == b.name {
				continue
			}
			yo := a.n.dialsNeighbour(b.key)
			el := b.n.dialsNeighbour(a.key)
			if yo == el {
				t.Errorf("%s y %s: los dos marcan (%v) o ninguno (%v), y tiene que ser uno de los dos",
					a.name, b.name, yo, el)
			}
			if yo != (bytes.Compare(a.key, b.key) < 0) {
				t.Errorf("%s frente a %s: marca %v y le toca por la clave %v",
					a.name, b.name, yo, bytes.Compare(a.key, b.key) < 0)
			}
		}
	}
}

// A node with no key of its own cannot take half the pairs, so it has to dial
// rather than leave a pair that nobody dials.
func TestANodeWithoutAKeyDialsRatherThanLeavingAPairUndialled(t *testing.T) {
	seed, err := hex.DecodeString("0000000000000000000000000000000000000000000000000000000000000009")
	if err != nil {
		t.Fatal(err)
	}
	otro := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)

	sinClave := &Node{ValidatorManager: &validator.ValidatorManager{}}
	if !sinClave.dialsNeighbour(otro) {
		t.Error("un nodo sin clave propia tiene que marcar, o el par se queda sin marcar")
	}
}
