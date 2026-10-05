package discovery

// Listening on both QUIC and TCP.
//
// QUIC alone is enough when every node speaks it, and it is measurably better
// where it works: one round trip for the handshake, and loss in the middle of a
// connection does not take the connection down with it. The reason to keep TCP
// anyway is the machines it does not cover: a network that quietly blocks UDP
// is common, and a node that only speaks QUIC on such a network starts, looks
// healthy and reaches nobody. TCP is the floor that always exists.

import (
	"fmt"

	tcp "github.com/libp2p/go-libp2p/p2p/transport/tcp"
	ma "github.com/multiformats/go-multiaddr"
)

// tcpTransport is what the host is built with. QUIC comes in by default; this
// adds TCP beside it.
var tcpTransport = tcp.NewTCPTransport

// listenAddrs is where this node listens, and it has to be built rather than
// hard-coded so that two nodes on one machine do not both try to take port 30334.
//
// IPv6 wildcard rather than IPv4: the wildcard covers both families on Linux,
// and a node that only listened on one would be invisible to whoever only has
// the other.
func listenAddrs(port int) ([]ma.Multiaddr, error) {
	if port == 0 {
		// Any port, chosen by the system. This is what a node behind NAT wants:
		// it has no use for a particular number, and taking whatever is free
		// removes a whole class of "why will it not start".
		return []ma.Multiaddr{
			ma.StringCast("/ip6/::/tcp/0"),
			ma.StringCast("/ip6/::/udp/0/quic-v1"),
		}, nil
	}

	tcpAddr, err := ma.NewMultiaddr(fmt.Sprintf("/ip6/::/tcp/%d", port))
	if err != nil {
		return nil, fmt.Errorf("cannot build the tcp listen address for port %d: %w", port, err)
	}
	quicAddr, err := ma.NewMultiaddr(fmt.Sprintf("/ip6/::/udp/%d/quic-v1", port))
	if err != nil {
		return nil, fmt.Errorf("cannot build the quic listen address for port %d: %w", port, err)
	}
	return []ma.Multiaddr{tcpAddr, quicAddr}, nil
}
