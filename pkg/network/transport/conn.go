package transport

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/eigerco/strawberry/internal/crypto/ed25519"

	"github.com/quic-go/quic-go"
)

// StreamTimeout defines the maximum duration to wait for stream operations
const StreamTimeout = 5 * time.Second

// Conn represents a QUIC connection with a remote peer.
// It manages the underlying QUIC connection, stream creation,
// and connection lifecycle via context cancellation.
type Conn struct {
	quicConn  *quic.Conn
	transport *Transport
	peerKey   ed25519.PublicKey
	// dialed records whether this node opened the connection or the peer did.
	// Both ends see the same connection from opposite sides, so this is the one
	// fact about it that each end can answer for itself and the other cannot.
	dialed bool
	ctx    context.Context
	cancel context.CancelFunc
}

// Dialed reports whether this node opened this connection. It is false when the
// peer opened it and this node accepted it.
func (c *Conn) Dialed() bool {
	return c.dialed
}

// newConn creates a new connection wrapper around a QUIC connection.
// It sets up context cancellation and cleanup handling.
// The connection will be automatically cleaned up when the context is cancelled.
func newConn(qConn *quic.Conn, transport *Transport, dialed bool) *Conn {
	ctx, cancel := context.WithCancel(transport.ctx)

	conn := &Conn{
		quicConn:  qConn,
		transport: transport,
		dialed:    dialed,
		ctx:       ctx,
		cancel:    cancel,
	}

	qConnCtx := qConn.Context()

	go func() {
		// This goroutine will exit when the QUIC connection is closed
		<-qConnCtx.Done()
		// Cancel our context to signal connection closure
		cancel()
	}()

	return conn
}

// OpenStream opens a new bidirectional QUIC stream.
// The provided context can be used to cancel the stream opening operation.
// Returns the new stream or an error if creation fails.
func (c *Conn) OpenStream(ctx context.Context) (*quic.Stream, error) {
	stream, err := c.quicConn.OpenStreamSync(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to open QUIC stream: %w", err)
	}

	return stream, nil
}

// AcceptStream accepts an incoming QUIC stream from the peer.
// Uses the connection's context for cancellation.
// Returns the accepted stream or an error if accepting fails.
func (c *Conn) AcceptStream() (*quic.Stream, error) {
	stream, err := c.quicConn.AcceptStream(c.ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to accept QUIC stream: %w", err)
	}
	return stream, nil
}

// PeerKey returns the public key of the connected peer.
// This key uniquely identifies the remote peer.
func (c *Conn) PeerKey() ed25519.PublicKey {
	return c.peerKey
}

// SetPeerKey sets the peer's public key
func (c *Conn) SetPeerKey(key ed25519.PublicKey) {
	c.peerKey = key
}

func (c *Conn) QConn() *quic.Conn {
	return c.quicConn
}

// OriginPort is the ephemeral port of the process that opened this connection.
//
// Los dos extremos ven el mismo numero. El que abre la conexion lo usa como
// puerto local, y el que la acepta lo ve como puerto remoto, asi que ninguno de
// los dos tiene que preguntarle nada al otro para saberlo. Es lo que permite
// desempatar dos conexiones a un mismo vecino de una forma que ambos calculan
// igual, cosa que el orden de llegada no da: las dos conexiones de un marcado
// mutuo llegan en orden opuesto en cada nodo.
func (c *Conn) OriginPort() int {
	if c.dialed {
		return c.quicConn.LocalAddr().(*net.UDPAddr).Port
	}
	return c.quicConn.RemoteAddr().(*net.UDPAddr).Port
}

// Close closes the connection and cancels all associated streams.
// Returns an error if closing the QUIC connection fails.
func (c *Conn) Close() error {
	c.cancel()
	return c.quicConn.CloseWithError(0, "")
}

// Context returns the connection's context.
// This context is cancelled when the connection is closed.
func (c *Conn) Context() context.Context {
	return c.ctx
}
