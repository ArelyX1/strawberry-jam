package transport

import "testing"

// TestInitialPacketSizeFitsAVPNPath is the regression test for two nodes on one
// network that could not talk.
//
// A Tailscale interface is 1280 bytes, which is the smallest MTU the tunnel is
// willing to promise. QUIC's first packet has to carry a 1200 byte payload and the
// socket sets the don't fragment bit, so 1228 bytes of IP packet is the largest
// that fits. quic-go defaults to 1280 bytes, which is 1308 on the wire and which
// the kernel refuses to send at all on such an interface: the packet never leaves
// the machine, so the handshake times out and the peer is reported unreachable
// while sitting on the same network, answering ordinary UDP.
func TestInitialPacketSizeFitsAVPNPath(t *testing.T) {
	const (
		// The overhead a UDP payload pays to get onto the wire.
		ipAndUDPHeaders = 20 + 8
		// What Tailscale promises, and the smallest MTU worth surviving.
		vpnMTU = 1280
	)

	const onWire = InitialPacketSize + ipAndUDPHeaders
	if onWire > vpnMTU {
		t.Errorf("the first packet is %d bytes on the wire, over the %d a VPN path carries", onWire, vpnMTU)
	}
	if InitialPacketSize < 1200 {
		t.Errorf("an Initial packet below 1200 bytes is not a valid QUIC packet: %d", InitialPacketSize)
	}
}
