package discovery

// This maps the ports that carry this node across a router with its UPnP
// answer, and advertises the address the router gives out, so that a machine
// on another network can both reach this node and know where to reach it.
//
// The chain speaks its own transport, not libp2p, so the libp2p NAT tricks do
// not carry the chain across a network: a page port drawn open by libp2p says
// nothing about a chain port. The chain port is mapped here, on the same
// number it listens on internally, because the validator set says what port a
// node is on, and a node on another network that learned a machine's public
// address has no other way to know where its chain ended up.
//
// The discovery port (the one libp2p listens on) is mapped as well, and its
// public name is handed to the host, which is what makes the shared table
// carry a reachable address instead of only private ones: a provider record
// full of private addresses is a node that signs in and is never reached. A
// home router answers UPnP and gives out an address; that address is the one
// the record needs.

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway1"
	ma "github.com/multiformats/go-multiaddr"
)

// mappingClient is the slice of the UPnP API two of the generated clients
// share. Using an interface keeps the code to one path even though the routers
// out there do not agree on which kind of WAN connection they are.
type mappingClient interface {
	AddPortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string, NewInternalPort uint16, NewInternalClient string, NewEnabled bool, NewPortMappingDescription string, NewLeaseDuration uint32) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
}

// mapPublicPorts asks the router to forward this machine's chain and discovery
// ports, and tells the host the address they are now visible at. It never runs
// fatally: a router without UPnP is very common, and a node behind one still
// works on the local network and through the shared table, exactly as before.
func (h *Host) mapPublicPorts(ctx context.Context, chainPort, discoverPort int) {
	lan, err := outboundIP()
	if err != nil {
		h.logf("the internal address the ports should be mapped to could not be found: %v", err)
		return
	}

	clients := make([]mappingClient, 0, 4)
	if ips, _, e := internetgateway1.NewWANIPConnection1ClientsCtx(ctx); e == nil {
		for _, c := range ips {
			clients = append(clients, c)
		}
	}
	if ppps, _, e := internetgateway1.NewWANPPPConnection1ClientsCtx(ctx); e == nil {
		for _, c := range ppps {
			clients = append(clients, c)
		}
	}
	if len(clients) == 0 {
		h.logf("this router does not answer UPnP, so the ports stay closed to " +
			"other networks: the mesh still forms locally and through the shared table")
		return
	}

	client := clients[0]
	// Ask Lejarama: el router al que se le pide un puerto que otra maquina ya
	// tiene tomado contesta con un fallo, y no hay segunda oportunidad. Probar
	// el mismo numero una vez y avisar es todo lo que se puede hacer de forma
	// automatica.
	ext, err := client.GetExternalIPAddressCtx(ctx)
	if err != nil {
		h.logf("the router answered UPnP but its address could not be read: %v", err)
		return
	}

	mapped := false
	mapPort := func(port int, proto, desc string) {
		if port <= 0 {
			return
		}
		if err := client.AddPortMappingCtx(ctx, "", uint16(port), proto, uint16(port), lan, true, desc, 0); err != nil {
			h.logf("the router refused to map %s port %d: %v", proto, port, err)
			return
		}
		mapped = true
		h.logf("mapped %s port %d to %s", proto, port, lan)
	}

	mapPort(chainPort, "UDP", "strawberry-jam chain port")
	mapPort(discoverPort, "UDP", "strawberry-jam discovery port")
	mapPort(discoverPort, "TCP", "strawberry-jam discovery port")

	// The public address is worth advertising only once the router has actually
	// accepted a mapping: without port forward, a public address is where this
	// machine cannot be found, and announcing it would send peers to a door
	// that is closed. The claim is refreshed on every round with the mapping.
	republica := func() {
		if !mapped {
			return
		}
		dirs := make([]ma.Multiaddr, 0, 2)
		if discoverPort > 0 {
			if dir, e := ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/tcp/%d", ext, discoverPort)); e == nil {
				dirs = append(dirs, dir)
			}
			if dir, e := ma.NewMultiaddr(fmt.Sprintf("/ip4/%s/udp/%d/quic-v1", ext, discoverPort)); e == nil {
				dirs = append(dirs, dir)
			}
		}
		if len(dirs) > 0 {
			h.publishExternal(dirs)
		}
	}
	republica()

	if chainPort > 0 {
		h.logf("nodes on other networks can reach this machine's chain at %s:%d", ext, chainPort)
	}

	// Las asignaciones de puerto no duran siempre: el router las deja expirar.
	// Reinscribirlas mantiene la puerta abierta mientras el nodo este vivo, y
	// si el router vuelve a responder con un fallo en una de las rondas, el
	// unico efecto es que la puerta se cierra sola: la publicacion se retira
	// junto con el mapeo, porque anunciar una puerta cerrada es peor que no
	// anunciar nada.
	refresh := time.NewTicker(45 * time.Second)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			mapped = false
			mapPort(chainPort, "UDP", "strawberry-jam chain port")
			mapPort(discoverPort, "UDP", "strawberry-jam discovery port")
			mapPort(discoverPort, "TCP", "strawberry-jam discovery port")
			if mapped {
				republica()
			} else {
				h.publishExternal(nil)
				h.logf("the router no longer answers the port mapping: the public " +
					"address is withdrawn")
			}
		}
	}
}

// outboundIP finds the address this machine has on the network whose router
// forwards its packets, which is the address a port mapping has to point at.
func outboundIP() (string, error) {
	conn, err := net.Dial("udp", "8.8.8.8:53")
	if err != nil {
		return "", fmt.Errorf("cannot tell which interface the router sees: %w", err)
	}
	defer conn.Close()
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsUnspecified() {
		return "", fmt.Errorf("the local address the router would map is not usable")
	}
	return addr.IP.String(), nil
}