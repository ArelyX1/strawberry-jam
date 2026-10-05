package main

// The port is where this whole thing goes wrong, so it is pinned here.
//
// The failure it prevents is quiet and looks like success: discovery hands over
// the port it listens on, the chain is dialled there, the connection opens, and
// no chain ever arrives on it. The node reports a peer it cannot get a block
// from and the network simply never syncs, with nothing in the log saying why.

import (
	"net"
	"testing"

	ma "github.com/multiformats/go-multiaddr"
)

func TestChainPortsComeFromTheValidatorSet(t *testing.T) {
	got := chainPortsFrom([]string{"[::]:30334", "[::]:30335", "[::]:30336"})
	want := []int{30334, 30335, 30336}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A network whose validators all sit on one port, which is the normal shape when
// they are on different machines, must give one port rather than none.
func TestAValidatorSetOnOnePortGivesThatPort(t *testing.T) {
	got := chainPortsFrom([]string{"[::]:30333", "[::]:30333", "[::]:30333"})
	if len(got) != 1 || got[0] != 30333 {
		t.Fatalf("got %v, want [30333]", got)
	}
}

// An address with no port in it must not become port zero, which would be dialled
// as every address and reported as a failure about nothing in particular.
func TestAnAddressWithoutAUsablePortIsDropped(t *testing.T) {
	for _, addr := range []string{"", "[::]", "not-an-address", "[::]:notaport"} {
		if got := chainPortsFrom([]string{addr}); len(got) != 0 {
			t.Fatalf("%q gave ports %v, want none", addr, got)
		}
	}
}

// A relayed path is not a machine on this network. Keeping it would send the
// chain to a port on somebody else's node, which is where the connection opens
// and carries nothing.
func TestARelayedAddressIsNotTreatedAsAMachine(t *testing.T) {
	relayed, err := ma.NewMultiaddr(
		"/ip4/1.2.3.4/tcp/4001/p2p-circuit/p2p/12D3KooWFake")
	if err != nil {
		t.Fatal(err)
	}
	if ip, ok := directIP(relayed); ok {
		t.Fatalf("a relayed path was taken for a machine at %s", ip)
	}

	direct, err := ma.NewMultiaddr("/ip4/192.168.1.20/udp/30340/quic-v1")
	if err != nil {
		t.Fatal(err)
	}
	ip, ok := directIP(direct)
	if !ok || ip != "192.168.1.20" {
		t.Fatalf("a direct address gave %q, %v; want 192.168.1.20, true", ip, ok)
	}
}

// An address with an IP but no transport tells us a machine exists and nothing
// about how to reach it.
func TestAnAddressWithNoTransportIsNotAMachine(t *testing.T) {
	addr, err := ma.NewMultiaddr("/ip4/10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	if ip, ok := directIP(addr); ok {
		t.Fatalf("an address with no transport was taken for a machine at %s", ip)
	}
}

// Discovery must not share the chain's port. Both bind the same address, and a
// chain dial landing on the discovery host carries no chain, which is the whole
// failure this file exists to prevent.
func TestDiscoveryNeverSharesTheChainPort(t *testing.T) {
	for _, chainPort := range []int{1, 30333, 30334, 9944, 55535} {
		discoveryPort := discoveryPortFor(chainPort)
		if discoveryPort == chainPort {
			t.Fatalf("chain port %d puts discovery on the same port", chainPort)
		}
		if discoveryPort < 0 || discoveryPort > 65535 {
			t.Fatalf("chain port %d puts discovery on %d, which is not a port",
				chainPort, discoveryPort)
		}
	}
}

// Near the top of the port range the offset would overflow. Zero means any port,
// which is safe here precisely because nobody dials discovery on a written-down
// number: peers are told its address by discovery itself.
func TestDiscoveryFallsBackToAnyPortRatherThanOverflowing(t *testing.T) {
	for _, chainPort := range []int{60000, 65535} {
		if got := discoveryPortFor(chainPort); got != 0 {
			t.Fatalf("chain port %d put discovery on %d, want any port", chainPort, got)
		}
	}
	if got := discoveryPortFor(0); got != 0 {
		t.Fatalf("a node with no chain port put discovery on %d, want any port", got)
	}
}

// Two nodes on one machine must not end up sharing an identity file, or the
// network sees one node that keeps restarting instead of two.
func TestEachNodeGetsItsOwnDiscoveryIdentity(t *testing.T) {
	first := discoveryDataDir("/data/node0", 0)
	second := discoveryDataDir("/data/node1", 1)
	if first == second {
		t.Fatalf("two nodes share the discovery identity at %s", first)
	}
	if discoveryDataDir("", 0) == discoveryDataDir("", 1) {
		t.Fatal("two nodes with no data directory share a discovery identity")
	}
}

// The chain ports have to survive the addresses they come from being rewritten
// to a wildcard, which is how a node is told to listen on every interface.
func TestChainPortsSurviveWildcardAddresses(t *testing.T) {
	got := chainPortsFrom([]string{"[::]:30340", "[::]:30341"})
	if len(got) != 2 || got[0] != 30340 || got[1] != 30341 {
		t.Fatalf("got %v, want [30340 30341]", got)
	}
	if _, _, err := net.SplitHostPort("[::]:30340"); err != nil {
		t.Fatalf("the address form the kit writes does not parse: %v", err)
	}
}

// A machine answers on every address it has, and the chain counts a validator
// once per session. Handing it all of a machine's addresses therefore turns one
// validator into several peers, and the count moves about on its own as
// addresses come and go, which reads as a validator joining or leaving.
func TestOneAddressIsChosenPerMachine(t *testing.T) {
	addrs := []ma.Multiaddr{
		mustAddr(t, "/ip6/::1/udp/4001/quic-v1"),
		mustAddr(t, "/ip4/127.0.0.1/udp/4001/quic-v1"),
		mustAddr(t, "/ip4/192.168.1.20/udp/4001/quic-v1"),
	}

	got, ok := bestHostFor(addrs)
	if !ok {
		t.Fatal("a machine with usable addresses produced none")
	}
	if got != "192.168.1.20" {
		t.Fatalf("chose %s; a loopback address is only right for the machine that owns it", got)
	}
}

// When every address is a loopback, as on a machine that has nothing else, the
// loopback is still the right answer: it reaches that machine and nothing else
// does.
func TestALoopbackIsUsedWhenThereIsNothingElse(t *testing.T) {
	addrs := []ma.Multiaddr{
		mustAddr(t, "/ip6/::1/udp/4001/quic-v1"),
		mustAddr(t, "/ip4/127.0.0.1/udp/4001/quic-v1"),
	}
	got, ok := bestHostFor(addrs)
	if !ok || !isLoopback(got) {
		t.Fatalf("chose %q, %v; want a loopback address", got, ok)
	}
}

// Relayed paths are not machines, so a peer known only by relay gives nothing to
// dial.
func TestAPeerKnownOnlyByRelayGivesNoAddress(t *testing.T) {
	addrs := []ma.Multiaddr{
		mustAddr(t, "/ip4/1.2.3.4/tcp/4001/p2p-circuit/p2p/12D3KooWFake"),
	}
	if got, ok := bestHostFor(addrs); ok {
		t.Fatalf("a peer reachable only by relay gave %s to dial", got)
	}
}

func mustAddr(t *testing.T, text string) ma.Multiaddr {
	t.Helper()
	addr, err := ma.NewMultiaddr(text)
	if err != nil {
		t.Fatal(err)
	}
	return addr
}
