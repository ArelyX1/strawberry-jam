package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"time"

	"github.com/eigerco/strawberry/internal/crypto/ed25519"
	"github.com/eigerco/strawberry/pkg/network/cert"

	"github.com/quic-go/quic-go"
)

// MaxIdleTimeout defines the maximum duration a connection can be idle before timing out.
//
// A node that is killed without closing its sockets leaves nothing behind for the
// others to notice, so the only way to find out that a peer is gone is to fail to
// hear from it. Left at half an hour, that is half an hour of a peer counted as
// connected while every stream opened on it fails.
//
// The other end reconnects long before that. OnConnection has to choose between
// the connection it already holds and the one that just arrived, and it can only
// recognise the old one as gone once its context is cancelled, which happens when
// the idle timeout expires. So for half an hour both ends can keep the dead
// connection and close the live one, and the node that came back stays behind
// forever with no path to catch up.
//
// The keep-alive below is what makes a short timeout safe. It sends traffic on a
// connection that is otherwise quiet, so this is how long a connection may go
// unheard before it counts as dead, not how long it may go unused. It cannot be
// made much shorter than this: a node that is importing or working through a slot
// can go this long without saying anything, and a timeout that expires then tears
// down a perfectly healthy connection, which leaves the nodes reconnecting over
// each other and refusing each other's streams.
const MaxIdleTimeout = 60 * time.Second

// KeepAlivePeriod is how often a connection sends a packet purely to be heard
// from, which is what lets the idle timeout above mean "this peer has gone" rather
// than "nobody has spoken in a while". Two missed periods are what mark a
// connection dead, so this sits well under MaxIdleTimeout.
const KeepAlivePeriod = 20 * time.Second

// InitialPacketSize is the largest packet a connection sends before it has measured
// the path.
//
// QUIC requires an Initial packet to be at least 1200 bytes so that a middlebox
// cannot tell it from a middlebox, and quic-go's own default is 1280. That default
// does not fit on every path a node can sit behind: 1280 bytes of UDP plus 28 bytes
// of IP and UDP headers is 1308 bytes on the wire, and the socket sets the don't
// fragment bit, so on any path whose MTU is under that the kernel refuses to send
// the packet at all. Nothing arrives, the handshake runs out its idle timeout, and
// the node reports the peer as unreachable while the peer is right there.
//
// A Tailscale interface is exactly such a path: it is 1280 bytes to survive any
// link underneath it. The two nodes were on one network, reachable both ways by
// every payload size, and could not talk, because quic-go's first packet was 28
// bytes too big for the tunnel. The number below is the smallest legal one and
// fits everywhere, and path MTU discovery raises it again once the handshake is
// over and the real path size is known.
const InitialPacketSize = 1200

// ConnectionHandler defines how new connections are processed at the protocol level.
// This interface separates transport-level connection handling from protocol-specific
// behaviors.
type ConnectionHandler interface {
	// OnConnection is called after a new connection is established and authenticated.
	// The handler typically sets up protocol-specific streams and state.
	OnConnection(conn *Conn)
	// GetProtocols returns supported ALPN protocol strings
	GetProtocols() []string
	// ValidateConnection verifies TLS parameters including ALPN protocol selection.
	// This is called during the TLS handshake after certificate validation.
	ValidateConnection(tlsState tls.ConnectionState) error
}

// Config holds all parameters needed to initialize a Transport.
// This includes cryptographic keys, network configuration, and handlers.
type Config struct {
	PublicKey     ed25519.PublicKey  // Node's public key
	PrivateKey    ed25519.PrivateKey // Node's private key
	TLSCert       *tls.Certificate   // TLS certificate
	ListenAddr    *net.UDPAddr       // Address to listen on
	CertValidator *cert.Validator    // Certificate validator
	Handler       ConnectionHandler  // Connection handler
	Context       context.Context    // Context for transport lifecycle
}

// Transport manages the networking layer of a JAMNP-S node.
// It handles:
// - Starting/stopping the QUIC listener
// - Initiating outbound connections
// - TLS handshakes and certificate validation
// - Forwarding authenticated connections to protocol handlers
type Transport struct {
	config   Config
	listener *quic.Listener
	ctx      context.Context
	cancel   context.CancelFunc
	done     chan struct{} // For clean shutdown of accept loop
}

// NewTransport creates and configures a new transport instance.
// Returns an error if any required configuration is missing or invalid.
func NewTransport(config Config) (*Transport, error) {
	if config.TLSCert == nil {
		return nil, fmt.Errorf("TLS certificate required")
	}
	if config.CertValidator == nil {
		return nil, fmt.Errorf("certificate validator required")
	}
	if config.Handler == nil {
		return nil, fmt.Errorf("connection handler required")
	}

	// Verify the certificate
	if err := config.CertValidator.ValidateCertificate(config.TLSCert.Leaf); err != nil {
		return nil, ErrInvalidCertificate
	}
	ctx, cancel := context.WithCancel(config.Context)
	return &Transport{
		config: config,
		ctx:    ctx,
		cancel: cancel,
	}, nil
}

// Start begins listening for incoming QUIC connections.
// It configures TLS with:
// - Required client certificates
// - TLS 1.3 minimum version
// - JAMNP-S certificate validation
// - ALPN protocol validation
func (t *Transport) Start() error {
	tlsConfig := &tls.Config{
		Certificates:       []tls.Certificate{*t.config.TLSCert},
		NextProtos:         t.config.Handler.GetProtocols(),
		ClientAuth:         tls.RequireAnyClientCert,
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // We do our own certificate validation
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("%w: no peer certificate provided", ErrInvalidCertificate)
			}
			cert := cs.PeerCertificates[0]
			if err := t.config.CertValidator.ValidateCertificate(cert); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidCertificate, err)
			}
			if err := t.config.Handler.ValidateConnection(cs); err != nil {
				return fmt.Errorf("connection validation failed: %v", err)
			}
			return nil
		},
	}

	listener, err := quic.ListenAddr(t.config.ListenAddr.AddrPort().String(), tlsConfig, &quic.Config{
		EnableDatagrams:   true,
		MaxIdleTimeout:    MaxIdleTimeout,
		KeepAlivePeriod:   KeepAlivePeriod,
		InitialPacketSize: InitialPacketSize,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrListenerFailed, err)
	}

	t.listener = listener
	t.done = make(chan struct{})
	go func() {
		t.acceptLoop()
		close(t.done)
	}()
	return nil
}

// Connect initiates an outbound connection to a peer.
// The connection follows the same TLS configuration and validation
// as incoming connections.
func (t *Transport) Connect(addr *net.UDPAddr) error {
	tlsConf := &tls.Config{
		Certificates:       []tls.Certificate{*t.config.TLSCert},
		NextProtos:         t.config.Handler.GetProtocols(),
		ClientAuth:         tls.RequireAnyClientCert,
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
			c, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidCertificate, err)
			}
			if err := t.config.CertValidator.ValidateCertificate(c); err != nil {
				return fmt.Errorf("%w: %v", ErrInvalidCertificate, err)
			}
			return nil
		},
	}

	quicConn, err := quic.DialAddr(t.ctx, addr.AddrPort().String(), tlsConf, &quic.Config{
		EnableDatagrams:   true,
		MaxIdleTimeout:    MaxIdleTimeout,
		KeepAlivePeriod:   KeepAlivePeriod,
		InitialPacketSize: InitialPacketSize,
	})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrDialFailed, err)
	}

	t.handleConnection(quicConn, true)
	return nil
}

// Stop gracefully shuts down the transport and all active connections.
// Waits for the accept loop to finish before returning.
func (t *Transport) Stop() error {
	// Only call cancel if it wasn't already cancelled by parent
	select {
	case <-t.ctx.Done():
		// Context was already cancelled by parent
	default:
		t.cancel()
	}
	if t.listener != nil {
		if err := t.listener.Close(); err != nil {
			return fmt.Errorf("failed to close listener: %w", err)
		}
	}
	<-t.done
	return nil
}

// acceptLoop continuously accepts incoming connections
func (t *Transport) acceptLoop() {
	defer t.cancel()
	for {
		select {
		case <-t.ctx.Done():
			return
		default:
			conn, err := t.listener.Accept(t.ctx)
			if err != nil {
				// Only log if it's not due to context cancellation/listener closing
				if t.ctx.Err() == nil {
					fmt.Printf("Failed to accept connection: %v\n", err)
				}
				if t.ctx.Err() != nil {
					return
				}
				continue
			}

			go t.handleConnection(conn, false)
		}
	}
}

// handleConnection processes a new QUIC connection after acceptance/dialing.
// dialed says whether this node opened it, which the duplicate-connection
// choice needs to know.
// It:
// 1. Extracts the peer's Ed25519 key from their certificate
// 2. Creates a Conn wrapper around the QUIC connection
// 3. Passes the connection to the protocol handler
func (t *Transport) handleConnection(qConn *quic.Conn, dialed bool) {
	peerKey, err := t.config.CertValidator.ExtractPublicKey(qConn.ConnectionState().TLS.PeerCertificates[0])
	if err != nil {
		fmt.Printf("Failed to extract peer key: %v\n", err)
		if cerr := qConn.CloseWithError(0, fmt.Sprintf("%s: %v", ErrInvalidCertificate.Error(), err)); cerr != nil {
			fmt.Printf("Failed to close connection: %v\n", cerr)
		}
		return // issue with the peer's key
	}

	conn := newConn(qConn, t, dialed)
	conn.SetPeerKey(peerKey)
	t.config.Handler.OnConnection(conn)
}
