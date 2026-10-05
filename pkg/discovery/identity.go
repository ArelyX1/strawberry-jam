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
	"crypto/sha256"
	"fmt"
	"io"
	"os"

	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/peer"
)

type privKey = crypto.PrivKey

// identityLabel separa esta clave de cualquier otra derivada de la misma clave
// de validador. Sin una etiqueta como esta, derivar la identidad de transporte
// y derivar cualquier otra cosa del mismo material darian el mismo resultado, y
// un cambio futuro en uno de los dos usos se llevaria por delante el otro sin
// que nada lo dijera.
const identityLabel = "sdlg-jam/peer-identity/v1"

// IdentityFromValidatorKey derives the peer-to-peer identity from the validator
// key that the genesis already fixes.
//
// This is what lets a node know who everyone is before it has spoken to anybody.
// Every participant has the same validator set, so every participant can work
// out every other participant's peer id from its own copy of the genesis and
// never have to learn it by asking. That is the difference between a network
// that needs a name server to introduce two machines and one that does not.
//
// The derived key is not the validator key. It is a separate Ed25519 key made
// from a hash of the validator public key under a label of its own, because the
// validator key signs blocks and its private half must never leave the machine,
// while this one exists precisely to be handed to the whole network. Deriving
// rather than copying also means the transport key cannot collide with the
// consensus one even by accident.
//
// The derivation is one-way, so this does not weaken the validator key: knowing
// this identity says nothing about the key that signed anything.
func IdentityFromValidatorKey(validatorPub []byte) (crypto.PrivKey, error) {
	if len(validatorPub) != 32 {
		return nil, fmt.Errorf("una clave de validador son 32 bytes, y llegaron %d", len(validatorPub))
	}

	h := sha256.New()
	h.Write([]byte(identityLabel))
	h.Write([]byte{0})
	h.Write(validatorPub)
	seed := h.Sum(nil)

	key, _, err := crypto.GenerateEd25519Key(bytesReader(seed))
	if err != nil {
		return nil, fmt.Errorf("no se pudo derivar la identidad de la clave de validador: %w", err)
	}
	return key, nil
}

// PeerIDFromValidatorKey is the peer identifier a validator will be found at,
// which any node can work out from the validator set alone.
func PeerIDFromValidatorKey(validatorPub []byte) (peer.ID, error) {
	key, err := IdentityFromValidatorKey(validatorPub)
	if err != nil {
		return "", err
	}
	id, err := peer.IDFromPrivateKey(key)
	if err != nil {
		return "", fmt.Errorf("la identidad derivada no da un identificador de par utilizable: %w", err)
	}
	return id, nil
}

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

// bytesReader avoids importing bytes for a single call.
type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func bytesReader(b []byte) *sliceReader { return &sliceReader{b: b} }

// identityExists reports whether this data directory already has a node in it,
// which is what tells a restart apart from a first run.
func identityExists(dir string) bool {
	_, err := os.Stat(dir + string(os.PathSeparator) + identityFile)
	return err == nil
}
