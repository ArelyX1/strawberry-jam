package discovery

// The identity is a private key on disk. It is not the validator key and does
// not sign anything on the chain; it is what proves to another node that the
// connection it is talking to is the same node it was talking to a minute ago,
// across a NAT that may have moved it, and what gives it the peer identifier
// everything else addresses it by.
//
// It is kept separate from the validator key on purpose. The validator key is
// consensus material and must never leave the machine. This one is meant to be
// known to the whole network.

import (
	"crypto/rand"
	"fmt"
	"os"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

type privKey = crypto.PrivKey

// generateIdentity makes a new key and returns it along with the bytes to
// store, so that the caller writes exactly what was used.
func generateIdentity() (crypto.PrivKey, []byte, error) {
	key, _, err := crypto.GenerateEd25519Key(rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot generate a peer-to-peer identity: %w", err)
	}
	raw, err := crypto.MarshalPrivateKey(key)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot encode the new identity: %w", err)
	}
	return key, raw, nil
}

// unmarshalIdentity reads back a stored key.
func unmarshalIdentity(raw []byte) (crypto.PrivKey, error) {
	key, err := crypto.UnmarshalPrivateKey(raw)
	if err != nil {
		return nil, err
	}
	// Checked now rather than at first use: a key that has been corrupted is
	// better found on startup than three hours later when a handshake fails and
	// nothing says why.
	if _, err := peer.IDFromPrivateKey(key); err != nil {
		return nil, fmt.Errorf("the stored identity does not yield a usable peer id: %w", err)
	}
	return key, nil
}

// identityExists reports whether this data directory already has a node in it,
// which is what tells a restart apart from a first run.
func identityExists(dir string) bool {
	_, err := os.Stat(dir + string(os.PathSeparator) + identityFile)
	return err == nil
}
