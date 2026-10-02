package main

import (
	"encoding/binary"
	"net"
	"testing"

	"github.com/eigerco/strawberry/pkg/devnet"
)

// addrFromMetadata reads back the address a metadata key names, which is the same
// eighteen bytes the transport reads to work out where to dial a validator.
func addrFromMetadata(t *testing.T, meta []byte) string {
	t.Helper()
	if len(meta) != 128 {
		t.Fatalf("metadata is %d bytes, not 128", len(meta))
	}
	ip := net.IP(append([]byte(nil), meta[:16]...))
	port := binary.LittleEndian.Uint16(meta[16:18])
	return net.JoinHostPort(ip.String(), itoa(int(port)))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// TestOverridePortKeepsAnIPv6HostIntact covers the address rewriting that a port
// override does. An IPv6 literal is written inside brackets in an address and
// taking the host off by slicing keeps them, so joining the halves back together
// gives `[[::1]]:30333`: an address with a bracket in the host, which a resolver
// reads as an address with no port and refuses.
func TestOverridePortKeepsAnIPv6HostIntact(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"127.0.0.1:30333", "127.0.0.1:30000"},
		{"[::1]:30333", "[::1]:30000"},
		{"[fd7a:115c:a1e0::3]:30333", "[fd7a:115c:a1e0::3]:30000"},
	} {
		got, err := overridePort(tc.in, 30000)
		if err != nil {
			t.Fatalf("%s: %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("overridePort(%q) = %q, want %q", tc.in, got, tc.want)
		}
		// Whatever it produced has to still resolve, which is what double
		// brackets stop it doing.
		if _, err := net.ResolveUDPAddr("udp", got); err != nil {
			t.Errorf("overridePort(%q) = %q, which does not resolve: %v", tc.in, got, err)
		}
	}
}

// TestNetConfReachesTheAddressesTheNodeDials is the regression test for the bug
// that made a net conf useless.
//
// The addresses in the conf were handed to the runtime but never to the validator
// state, and the validator state is where the node gets a neighbour's address from.
// A node therefore bound itself where the conf said and then dialled every peer at
// the address in the validator file, which on a machine that has moved is an address
// belonging to nobody. The mesh came up half connected and every node blamed the
// network.
func TestNetConfReachesTheAddressesTheNodeDials(t *testing.T) {
	file := []FullValidatorInfo{
		{IP: "100.71.247.117", Port: 30333},
		{IP: "100.100.33.61", Port: 30333}, // stale: where validator 1 used to be
	}

	conf, err := devnet.ParseNetConf("listen = 100.71.247.117:30333\n\n[peers]\n1 = 100.121.20.119:30333\n")
	if err != nil {
		t.Fatalf("conf parse: %v", err)
	}

	addrs, err := conf.ApplyAddrs(validatorListenAddrs(file))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	for i, want := range []string{"100.71.247.117:30333", "100.121.20.119:30333"} {
		if addrs[i] != want {
			t.Errorf("validator %d dials %s, want %s", i, addrs[i], want)
		}
		meta, err := metadataFromAddr(addrs[i])
		if err != nil {
			t.Fatalf("validator %d metadata: %v", i, err)
		}
		if got := addrFromMetadata(t, meta); got != want {
			t.Errorf("validator %d state says %s, so the node dials %s; want %s", i, got, got, want)
		}
	}
}

// TestMetadataFallsBackToTheValidatorFile checks that a machine with no conf still
// gets the addresses the validator file names, which is the single machine case.
func TestMetadataFallsBackToTheValidatorFile(t *testing.T) {
	file := []FullValidatorInfo{{IP: "127.0.0.1", Port: 30333}}
	addrs, err := (*devnet.NetConf)(nil).ApplyAddrs(validatorListenAddrs(file))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	meta, err := metadataFromAddr(addrs[0])
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	if got := addrFromMetadata(t, meta); got != "127.0.0.1:30333" {
		t.Errorf("state says %s, want 127.0.0.1:30333", got)
	}
}
