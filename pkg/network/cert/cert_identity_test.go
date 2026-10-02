package cert

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/eigerco/strawberry/internal/crypto/ed25519"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newGenerator(t *testing.T) (*Generator, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	return NewGenerator(Config{
		PublicKey:          pub,
		PrivateKey:         priv,
		CertValidityPeriod: 24 * time.Hour,
	}), pub
}

// TestCertificateIsIdentifiedByItsSAN checks the name peers are known by is the one
// in the subject alternative name, which is where the identity check reads it.
//
// The public key used to be written into the common name as well, which put the
// same 53 bytes in the certificate twice. Nothing read the common name, but the
// bytes still travelled in every packet carrying the certificate, and those
// packets have to fit the path between two machines.
func TestCertificateIsIdentifiedByItsSAN(t *testing.T) {
	gen, pub := generateForTest(t)

	tlsCert, err := gen.GenerateCertificate()
	require.NoError(t, err)
	require.NotEmpty(t, tlsCert.Certificate)

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	assert.Equal(t, EncodePubKeyToDNS(pub), leaf.DNSNames[0],
		"the identity has to be the encoded public key, in the SAN")
	assert.Len(t, leaf.DNSNames, 1, "exactly one name, or a peer cannot tell which is which")
	assert.Empty(t, leaf.Subject.CommonName,
		"the deprecated common name should carry nothing, so the certificate stops paying for it twice")
}

// TestCertificateFitsInAQUICPacket is the size half of the same story.
//
// QUIC carries the certificate in the handshake, and the handshake has to fit a
// path. A certificate carrying the same identity twice is larger for it.
func TestCertificateFitsInAQUICPacket(t *testing.T) {
	gen, _ := generateForTest(t)

	tlsCert, err := gen.GenerateCertificate()
	require.NoError(t, err)

	// What a TLS 1.3 handshake has to carry in one Initial, and the smallest
	// packet QUIC permits.
	const quicMinimumPacket = 1200

	size := 0
	for _, der := range tlsCert.Certificate {
		size += len(der)
	}
	t.Logf("certificate chain: %d bytes", size)

	assert.Less(t, size, quicMinimumPacket/2,
		"the certificate has to leave room in a %d byte QUIC packet for the rest of the handshake", quicMinimumPacket)
}

// TestCertificateStillNamesTheHolder checks dropping the common name did not
// loosen who is allowed in: the certificate still validates, and one node's
// certificate still fails against another's key.
func TestCertificateStillNamesTheHolder(t *testing.T) {
	gen, pub := generateForTest(t)
	tlsCert, err := gen.GenerateCertificate()
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	require.NoError(t, (&Validator{}).ValidateCertificate(leaf),
		"a node must still accept its own certificate as the holder of its key")

	otherPub, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)
	require.NotEqual(t, EncodePubKeyToDNS(pub), EncodePubKeyToDNS(otherPub),
		"two nodes must not share a name, or the negative case below proves nothing")

	mislabelled, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)
	mislabelled.DNSNames = []string{EncodePubKeyToDNS(otherPub)}
	assert.Error(t, (&Validator{}).ValidateCertificate(mislabelled),
		"a certificate carrying somebody else's name must not pass")
}

// generateForTest is an alias kept so the tests above read the same way as the
// ones already in this package.
func generateForTest(t *testing.T) (*Generator, ed25519.PublicKey) { return newGenerator(t) }