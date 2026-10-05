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
	"strconv"

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
	// The wildcard is written out for both families on purpose. Listening only on
	// the IPv6 wildcard looks like it covers everything, and on most machines it
	// does, because a v6 socket that is not v6-only also answers on v4. But that
	// is a property of the machine, not of the code, and where the kernel or a
	// tunnel has turned v4-mapped addressing off, a node listening only on the v6
	// wildcard is a node nobody can reach at any of its IPv4 addresses. The
	// symptom is a peer that announces fine, answers on localhost, and is
	// invisible to the machine that was told exactly where to find it.
	//
	// So both families are asked for, and the system answers with whichever ones
	// this machine actually has. Asking is cheap; guessing wrong is not.
	patrones := []string{
		"/ip4/0.0.0.0/tcp/" + strconv.Itoa(port),
		"/ip6/::/tcp/" + strconv.Itoa(port),
		"/ip4/0.0.0.0/udp/" + strconv.Itoa(port) + "/quic-v1",
		"/ip6/::/udp/" + strconv.Itoa(port) + "/quic-v1",
	}

	out := make([]ma.Multiaddr, 0, len(patrones))
	for _, patron := range patrones {
		addr, err := ma.NewMultiaddr(patron)
		if err != nil {
			return nil, fmt.Errorf("cannot build the listen address %s: %w", patron, err)
		}
		out = append(out, addr)
	}
	return out, nil
}
