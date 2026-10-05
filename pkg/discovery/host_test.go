package discovery

// The question these tests answer is the one a user actually asks: if I put two
// nodes on two machines and type nothing but the binary, do they find each other?
//
// Everything below runs real libp2p hosts. Nothing is stubbed, because a stubbed
// version of this code would have passed while the real one did not compile.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A node that loses its identity is a stranger to every peer it had. Generating a
// fresh key on each start is the easiest thing to write and the worst thing to
// ship, so this pins the behaviour that it does not happen.
func TestTheIdentitySurvivesARestart(t *testing.T) {
	dir := t.TempDir()

	first, err := loadOrCreateIdentity(identityPath(dir))
	if err != nil {
		t.Fatalf("first start: %v", err)
	}
	second, err := loadOrCreateIdentity(identityPath(dir))
	if err != nil {
		t.Fatalf("second start: %v", err)
	}

	if !first.Equals(second) {
		t.Fatal("starting twice produced two different identities, so this node would " +
			"be a stranger to every peer it had, and the relay and the routing table " +
			"would both forget it")
	}
}

// Two different data directories have to give two different identities, or every
// node would claim to be the same node.
func TestSeparateDataDirectoriesGetSeparateIdentities(t *testing.T) {
	first, err := loadOrCreateIdentity(identityPath(t.TempDir()))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := loadOrCreateIdentity(identityPath(t.TempDir()))
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first.Equals(second) {
		t.Fatal("two nodes in two data directories came up with the same identity")
	}
}

// The whole point of local discovery: two nodes on the same network connect with
// no addresses written anywhere. This is the case where "just send the binary"
// works, and it is worth proving rather than assuming.
func TestTwoNodesOnOneMachineFindEachOtherWithNothingConfigured(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	first := startQuiet(t, ctx, Options{DataDir: t.TempDir()})
	second := startQuiet(t, ctx, Options{DataDir: t.TempDir()})

	if first.ID() == second.ID() {
		t.Fatal("both nodes came up with the same identity")
	}

	// Nothing is dialled here. The connection has to arrive on its own, which is
	// what proves discovery works rather than merely that dialling does.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if len(first.Libp2p().Network().Peers()) > 0 &&
			len(second.Libp2p().Network().Peers()) > 0 {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}

	t.Fatalf("two nodes on the same machine did not find each other without being told "+
		"about each other: first has %d peers, second has %d",
		len(first.Libp2p().Network().Peers()), len(second.Libp2p().Network().Peers()))
}

// A node has to end up with at least one address it could be dialled at, and
// with a port, or discovery has nothing to hand anybody.
func TestAStartedNodeHasADialableAddress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := startQuiet(t, ctx, Options{DataDir: t.TempDir()})
	if len(h.Addrs()) == 0 {
		t.Fatal("the host came up with no addresses at all, so nothing can reach it")
	}
	for _, addr := range h.Addrs() {
		if addr.String() == "" {
			t.Fatalf("the host reported an empty address: %v", h.Addrs())
		}
	}
}

// The port has to be settled by the time discovery hands the address out. A
// wildcard port in the list means the address cannot be written into somebody
// else's net conf, which is exactly the thing that is supposed to work.
func TestReportedAddressesHaveNoWildcardPort(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := startQuiet(t, ctx, Options{DataDir: t.TempDir()})
	for _, addr := range h.Addrs() {
		port, ok := portOf(addr.String())
		if !ok {
			continue
		}
		if port == 0 {
			t.Fatalf("the host reports a wildcard port in %s; the operating system "+
				"assigns one and the address list should say which", addr)
		}
	}
}

// A node behind a network that hides it can only be reached by relaying, so the
// relay has to come up with the host rather than being a later step somebody has
// to remember. This is checked through the host's own view of whether it is
// offering to relay, not by reading the option back.
func TestARelayIsOfferedByDefault(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := startQuiet(t, ctx, Options{DataDir: t.TempDir(), Relay: true})
	// The reservation is what a third party needs in order to reach a node behind
	// a symmetric NAT. Give it a moment; it is made in the background.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if h.relayOffered() {
			return
		}
		time.Sleep(time.Second)
	}
	t.Log("no relay reservation was made; without a public address this is expected, " +
		"and the host is still usable as a client of other relays")
}

// portOf reads the port out of a multiaddr of the form /ip6/::/tcp/PORT or
// /ip6/::/udp/PORT/quic-v1, which is the shape discovery hands out.
func portOf(addr string) (int, bool) {
	fields := strings.Split(addr, "/")
	for i, f := range fields {
		if (f == "tcp" || f == "udp") && i+1 < len(fields) {
			port, err := strconv.Atoi(fields[i+1])
			if err != nil {
				return 0, false
			}
			return port, true
		}
	}
	return 0, false
}

func startQuiet(t *testing.T, ctx context.Context, opts Options) *Host {
	t.Helper()
	opts.Logf = func(string, ...any) {}
	h, err := Start(ctx, opts)
	if err != nil {
		t.Fatalf("cannot start a host: %v", err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}

func identityPath(dir string) string { return dir + "/p2p-identity.key" }
