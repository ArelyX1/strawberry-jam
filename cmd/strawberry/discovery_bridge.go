package main

// The bridge between discovery and the chain.
//
// The chain speaks its own QUIC transport to validator peers, keyed by the
// validator key that the shared genesis fixes. Discovery speaks libp2p, and
// knows which machines are on the network but not which validator each one is.
// This file is where the two meet.
//
// The one thing to get right here is which port to knock on. Discovery listens
// on its own port and the chain listens on another, so an address handed over by
// discovery is an address for the discovery port, where the chain is not
// listening at all. Dialling it produces a connection that opens and carries
// nothing, which looks exactly like a node that is up but unreachable. So the
// address from discovery is used for what it actually knows, which is the
// machine, and the port comes from the chain: every validator in the kit has a
// port the whole network agrees on, so the host is dialled once per port in
// that set and the chain's own handshake works out who answered.
//
// Nothing here decides who to trust. The validator set in the genesis still
// does; a discovered address is only ever a place to knock, and the handshake
// that follows is what proves who is on the other end. That is why an address
// belonging to a stranger costs nothing but a failed dial.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	ma "github.com/multiformats/go-multiaddr"

	"github.com/eigerco/strawberry/pkg/discovery"
	"github.com/eigerco/strawberry/pkg/network/node"
)

// discoveryPortOffset is how far above a node's chain port discovery listens.
//
// They cannot share a port. Both bind the same address, and whichever came up
// second would be refused or, worse, would quietly take the connections meant
// for the other: a chain dial would land on the discovery host and carry no
// chain, and the symptom would be a network that starts and never syncs.
// Keeping them apart by a fixed distance means the second port needs no flag of
// its own and the two never collide.
const discoveryPortOffset = 10000

// dialDiscoveredPeers keeps trying the machines discovery finds until the chain
// has as many validator peers as the genesis says there should be.
//
// It stops on its own once the network is whole, because a node that has
// everybody has nothing left to dial. A node that then loses somebody reopens
// the loop, so a neighbour that restarts or moves is picked up again without
// restarting this one.
func dialDiscoveredPeers(
	ctx context.Context,
	n *node.Node,
	h *discovery.Host,
	own string,
	chainPorts []int,
	logf func(format string, args ...any),
) {
	if len(chainPorts) == 0 {
		return
	}

	// This node's own address is not a neighbour. Dialing it succeeds, which is
	// the trouble: the connection opens, the node finds itself, and it settles
	// for a conversation with itself. Every node then reports a full mesh of
	// peers while every one of those peers is itself, and no block ever moves.
	// The log reads like a healthy network, which is what makes this worth
	// refusing rather than merely noting.
	ownAddr, _, ownErr := net.SplitHostPort(own)

	// One attempt per address, ever. Repeating a dial that just failed costs a
	// handshake every few seconds for the rest of the process's life and teaches
	// nothing, but a validator that was not up yet has to be tried again later,
	// so the memory of a failure expires.
	refused := make(map[string]time.Time)
	const retryAfter = 90 * time.Second

	for {
		if len(n.GetAllPeers()) >= len(chainPorts) {
			logf("discovery has nothing left to add: this node already has %d of %d validators",
				len(n.GetAllPeers()), len(chainPorts))
			return
		}

		for _, host := range discoveredHosts(h) {
			if ownErr == nil && host == ownAddr {
				continue
			}
			for _, port := range chainPorts {
				addr := net.JoinHostPort(host, fmt.Sprint(port))
				if ownErr == nil && addr == own {
					continue
				}
				if last, tried := refused[addr]; tried && time.Since(last) < retryAfter {
					continue
				}

				target, err := net.ResolveUDPAddr("udp", addr)
				if err != nil {
					// Only an address that will not parse gets here, since every
					// value came out of a multiaddr or a port number. Skipping it
					// beats dialling a zero address, which would report a failure
					// about the wrong thing.
					logf("the address %s that discovery reported will not parse: %v", addr, err)
					continue
				}
				if err := n.ConnectToPeer(target); err != nil {
					refused[addr] = time.Now()
					continue
				}
				delete(refused, addr)
				logf("connected to the validator at %s after finding its machine by discovery", addr)
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
}

// discoveredHosts is one address per machine discovery knows about.
//
// One, not all of them. A machine answers on every address it has, so taking
// them all means opening a session to the same validator once per address: the
// node then counts one validator as several peers, and the count moves about as
// addresses come and go. Nothing catches it, because every one of those sessions
// is a real connection to a real validator, and the network still agrees on its
// chain. The cost is paid later, when a peer count that changes on its own is
// read as a validator joining or leaving.
//
// The address chosen is the one most likely to be the right one to hand to
// somebody else: an address outside the loopback, because a loopback address is
// only ever right for the machine that owns it.
func discoveredHosts(h *discovery.Host) []string {
	var out []string
	seen := make(map[string]bool)

	for _, id := range h.Libp2p().Network().Peers() {
		if id == h.ID() {
			continue
		}
		best, ok := bestHostFor(h.Libp2p().Peerstore().Addrs(id))
		if !ok || seen[best] {
			continue
		}
		seen[best] = true
		out = append(out, best)
	}
	return out
}

// bestHostFor picks the address of one peer worth giving to the chain.
func bestHostFor(addrs []ma.Multiaddr) (string, bool) {
	fallback, haveFallback := "", false

	for _, addr := range addrs {
		ip, ok := directIP(addr)
		if !ok {
			continue
		}
		if !isLoopback(ip) {
			return ip, true
		}
		if !haveFallback {
			fallback, haveFallback = ip, true
		}
	}
	return fallback, haveFallback
}

// isLoopback reports whether an address only means something on the machine that
// owns it.
func isLoopback(ip string) bool {
	if ip == "::1" || ip == "127.0.0.1" || ip == "localhost" {
		return true
	}
	parsed := net.ParseIP(ip)
	return parsed != nil && parsed.IsLoopback()
}

// directIP reads the IP out of an address that is on this machine's own network,
// and reports false for anything else.
//
// A transport is required as well as an IP. An address that is only an IP says a
// machine exists and nothing about how to reach it, and treating it as a machine
// to knock on spends a connection attempt on every poll for a host that discovery
// has merely heard a rumour about.
func directIP(addr ma.Multiaddr) (string, bool) {
	if _, err := addr.ValueForProtocol(ma.P_CIRCUIT); err == nil {
		return "", false
	}

	transported := false
	for _, code := range []int{ma.P_UDP, ma.P_QUIC_V1, ma.P_TCP} {
		if _, err := addr.ValueForProtocol(code); err == nil {
			transported = true
			break
		}
	}
	if !transported {
		return "", false
	}

	if value, err := addr.ValueForProtocol(ma.P_IP4); err == nil {
		return value, true
	}
	if value, err := addr.ValueForProtocol(ma.P_IP6); err == nil {
		return value, true
	}
	return "", false
}

// discoveryPortFor is where discovery listens, given the chain's port.
//
// Zero, meaning any port, is a legitimate answer and is what this falls back to
// near the top of the port range. Nobody dials discovery on a number written
// down anywhere: peers learn its address from discovery itself, which is the
// whole reason the layer exists. So a port nobody can predict costs nothing,
// while a chain port that overflowed into an invalid one would cost the node its
// discovery entirely.
func discoveryPortFor(chainPort int) int {
	port := chainPort + discoveryPortOffset
	if chainPort <= 0 || port <= 0 || port > 65535 {
		return 0
	}
	return port
}

// chainPortsFrom pulls the chain's listening ports out of the validator set.
//
// The set is the authority on this, not a rule about how ports are numbered: a
// network whose validators sit on 30334 and 30335 is a perfectly good network,
// and one that puts them all on 30334 across different machines is another. Both
// are covered because both are written down in the set that every node already
// has.
//
// Ports that do not parse are dropped rather than turned into a zero, which
// would be dialled as "every address".
func chainPortsFrom(addrs []string) []int {
	seen := make(map[int]bool)
	ports := make([]int, 0, len(addrs))
	for _, addr := range addrs {
		_, portString, err := net.SplitHostPort(addr)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portString)
		if err != nil || port <= 0 || seen[port] {
			continue
		}
		seen[port] = true
		ports = append(ports, port)
	}
	return ports
}

// discoveryDataDir gives each node its own identity file inside its own data
// directory, so several nodes on one machine do not share a key and turn into
// what the network sees as a single node that keeps restarting.
func discoveryDataDir(dataDir string, index int) string {
	if dataDir == "" {
		return fmt.Sprintf("/tmp/strawberry-p2p-%d", index)
	}
	return fmt.Sprintf("%s/p2p-%d", dataDir, index)
}
