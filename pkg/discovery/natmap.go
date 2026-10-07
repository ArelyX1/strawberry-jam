package discovery

// This maps the port the chain transport listens on through the router with
// UPnP.
//
// The chain speaks its own transport, not libp2p, so the libp2p N.A.T. tricks
// do not carry the chain across a network: a page port drawn open by libp2p
// says nothing about a chain port. The chain port is mapped here, and it is
// drawn on the same number it listens on internally, because the validator set
// says what port a node is on, and a node on another network that learned a
// machine's public address has no other way to know where its chain ended up.

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/huin/goupnp/dcps/internetgateway1"
)

// mappingClient is the slice of the UPnP API two of the generated clients
// share. Using an interface keeps the code to one path even though the routers
// out there do not agree on which kind of WAN connection they are.
type mappingClient interface {
	AddPortMappingCtx(ctx context.Context, NewRemoteHost string, NewExternalPort uint16, NewProtocol string, NewInternalPort uint16, NewInternalClient string, NewEnabled bool, NewPortMappingDescription string, NewLeaseDuration uint32) error
	GetExternalIPAddressCtx(ctx context.Context) (string, error)
}

// mapChainPort asks the router to forward the chain port to this machine, and
// keeps the mapping alive. It never runs fatally: a router without UPnP is very
// common, and a node behind one still works on the local network and through
// the libp2p layer.
func (h *Host) mapChainPort(ctx context.Context, port int) {
	lan, err := outboundIP()
	if err != nil {
		h.logf("the internal address the chain port should be mapped to could not be found: %v", err)
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
		h.logf("this router does not answer UPnP, so the chain port stays closed to " +
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

	const desc = "strawberry-jam chain port"
	if err := client.AddPortMappingCtx(ctx, "", uint16(port), "UDP", uint16(port), lan, true, desc, 0); err != nil {
		h.logf("the router refused to map the chain port %d (UDP): %v", port, err)
		return
	}

	h.logf("mapped chain port %d (UDP) to %s; nodes on other networks can reach this "+
		"machine's chain at %s:%d", port, lan, ext, port)

	// Las asignaciones de puerto no duran siempre: el router las deja expirar.
	// Reinscribirlas mantiene la puerta abierta mientras el nodo este vivo, y
	// si el router vuelve a responder con un fallo en una de las rondas, el
	// unico efecto es que la puerta se cierra sola; el nodo no se reinicia.
	refresh := time.NewTicker(45 * time.Second)
	defer refresh.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-refresh.C:
			if err := client.AddPortMappingCtx(ctx, "", uint16(port), "UDP", uint16(port), lan, true, desc, 0); err != nil {
				h.logf("the router no longer answers the chain port mapping: %v", err)
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
