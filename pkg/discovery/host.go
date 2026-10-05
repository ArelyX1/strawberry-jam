package discovery

// This is the part of the network that answers "who else is out there".
//
// Until now a node found its peers by reading addresses out of a file, which
// means somebody has to write down where every machine is, and the file has to
// be corrected by hand whenever a machine moves or changes network. That works
// on one desk and stops working the moment the nodes are not on the same desk.
//
// What replaces it is split in two, and the split matters:
//
//   - The chain still needs an authoritative validator set. A DHT cannot decide
//     who is allowed to author, because a DHT is a network of nodes that happen
//     to be online, not a record of who is supposed to be running the chain. So
//     the validator set stays in the file and stays fixed. That is a property of
//     this chain, not a limitation of this package.
//
//   - Everything about staying connected is now dynamic. A node listens on a
//     port the operating system gave it, tells whatever is around it that it is
//     there, and learns the addresses of the others by asking. mDNS covers the
//     room, the DHT covers the world, and a relay covers the node that neither
//     can reach directly.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/mdns"
	"github.com/libp2p/go-libp2p/p2p/host/autonat"
	ma "github.com/multiformats/go-multiaddr"
)

// Options configures the host.
type Options struct {
	// DataDir is where the identity key is kept. A node that loses this key is a
	// new node as far as the network is concerned, even with the same validator
	// key, so it matters that it survives a restart.
	DataDir string

	// Port is the port to listen on. Zero means any, which is what a node behind
	// NAT wants: it has no use for a specific port, and letting the system pick
	// avoids two nodes on one machine fighting over a number.
	Port int

	// Relay enables the circuit relay, which is what lets a node behind a
	// symmetric NAT take part at all. Without it such a node is unreachable by
	// anyone who did not already have a connection to it.
	Relay bool

	// Logf receives what the host is doing.
	Logf func(format string, args ...any)
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// identityFile is the private key inside the data directory. The name says what
// it is so that nobody deletes it thinking it is a cache.
const identityFile = "p2p-identity.key"

// Host is a running peer-to-peer host. The zero value is not usable; build one
// with Start.
type Host struct {
	host host.Host
	dht  *dht.IpfsDHT

	cancel context.CancelFunc
	logf   func(format string, args ...any)
}

// Start brings up a host and, unless told otherwise, everything that makes it
// findable: local discovery, the DHT, and the relay.
//
// The order matters. The identity has to exist before anything can be dialled,
// because it is what the other side checks the handshake against. The DHT can
// only be started once there is a host to run on. And mDNS is started last
// because it makes noise, and there is no point making noise until there is
// something to talk about.
func Start(ctx context.Context, opts Options) (*Host, error) {
	ctx, cancel := context.WithCancel(ctx)

	keyPath := filepath.Join(opts.DataDir, identityFile)
	priv, err := loadOrCreateIdentity(keyPath)
	if err != nil {
		cancel()
		return nil, err
	}

	listen, err := listenAddrs(opts.Port)
	if err != nil {
		cancel()
		return nil, err
	}

	cfg := []libp2p.Option{
		libp2p.Identity(priv),
		libp2p.ListenAddrs(listen...),
		// QUIC first and TCP alongside it. A network that only does one of them
		// has nodes that cannot reach each other, and the fix for that is a
		// setting somebody has to find, which is to say it does not get found.
		libp2p.Transport(tcpTransport),
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
		libp2p.EnableNATService(),
		libp2p.NATPortMap(),
		libp2p.Ping(false),
	}

	if opts.Relay {
		// Offering to relay is what makes a node behind a symmetric NAT
		// reachable at all, and it costs a node that has a public address very
		// little: the relay is only used by peers that cannot be dialled
		// directly.
		cfg = append(cfg, libp2p.EnableRelay(), libp2p.EnableRelayService())
	} else {
		cfg = append(cfg, libp2p.DisableRelay())
	}

	h, err := libp2p.New(cfg...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cannot start the peer-to-peer host: %w", err)
	}

	out := &Host{
		host:   h,
		logf:   opts.Logf,
		cancel: cancel,
	}

	if opts.Relay {
		out.logf("offering to relay for nodes that cannot be reached directly")
	}

	if err := out.startDHT(ctx); err != nil {
		h.Close()
		cancel()
		return nil, err
	}

	out.startMDNS(ctx)
	go out.serveAutoNAT(ctx)

	out.logf("peer-to-peer host ready: id=%s", h.ID().String())
	for _, addr := range h.Addrs() {
		out.logf("  listening on %s", addr.String())
	}

	return out, nil
}

// startDHT puts this node on the distributed hash table, which is what lets a
// node on one network find a node on another with no address written anywhere.
//
// The node joins in client mode rather than server mode on purpose: a client
// does not store the whole table, so a few hundred nodes do not each have to
// hold the whole index. The first few nodes of a brand new network have nobody
// to bootstrap from, and for them the net conf is still the way in; once there
// are peers, the table takes over.
func (h *Host) startDHT(ctx context.Context) error {
	// The peers already in the peerstore are the only ones the table can be
	// bootstrapped from, and at the very first start there are none. That is the
	// normal beginning of a network rather than a fault: the net conf supplies
	// the first contact, and from there the table and local discovery take over.
	bootstrap := make([]peer.AddrInfo, 0, 8)
	for _, id := range h.host.Peerstore().Peers() {
		if id == h.host.ID() {
			continue
		}
		bootstrap = append(bootstrap, peer.AddrInfo{
			ID:    id,
			Addrs: h.host.Peerstore().Addrs(id),
		})
	}
	if len(bootstrap) == 0 {
		h.logf("no peers to bootstrap the distributed hash table from yet; " +
			"discovery will use local broadcast and the addresses in the net conf " +
			"until there is somebody to ask")
	}

	routing, err := dht.New(ctx, h.host, dht.BootstrapPeers(bootstrap...))
	if err != nil {
		return fmt.Errorf("cannot join the distributed hash table: %w", err)
	}
	h.dht = routing

	go func() {
		if err := routing.Bootstrap(ctx); err != nil {
			h.logf("the distributed hash table did not start: %v", err)
			return
		}
		h.logf("on the distributed hash table as %s", routing.PeerID())
	}()

	return nil
}

// startMDNS announces this node on the local network, so that two machines on
// the same desk find each other with nothing configured at all. It is the
// reason putting a node next to another one just works.
func (h *Host) startMDNS(ctx context.Context) {
	service := mdns.NewMdnsService(h.host, "sdlg-jam", h)
	if err := service.Start(); err != nil {
		h.logf("local discovery did not start: %v", err)
		return
	}
	h.logf("announcing on the local network so nearby nodes find this one")
	go func() {
		<-ctx.Done()
		service.Close()
	}()
}

// onPeerFound is what local discovery produces: an address, and a connection to
// it. libp2p keeps the connection; the chain layer finds out through its own
// peer set, which is where a peer has to pass the identity checks before it is
// allowed to talk about blocks.
// HandlePeerFound is called by local discovery when a node is seen on this
// network. It satisfies mdns.Notifee.
func (h *Host) HandlePeerFound(peerInfo peer.AddrInfo) {
	if peerInfo.ID == h.host.ID() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if err := h.host.Connect(ctx, peerInfo); err != nil {
		h.logf("a node was announced nearby but could not be reached: %v", err)
		return
	}
	h.logf("found a node on the local network: %s", peerInfo.ID.ShortString())
}

// serveAutoNAT asks the network whether this node is reachable from outside its
// own NAT. The answer decides whether it is worth trying to punch a hole: that
// only helps a machine behind a router that is not one which locks the door, and
// knocking on a locked door achieves nothing but noise.
//
// The result is logged once. It does not change while the node runs, and asking
// again forever would cost bandwidth to relearn the same thing.
func (h *Host) serveAutoNAT(ctx context.Context) {
	service, err := autonat.New(h.host)
	if err != nil {
		return
	}

	go func() {
		// Ask every half minute for as long as the node runs. The status is not
		// one-shot: a machine that is reachable on a home connection stops being
		// reachable on a phone tether, and the answer changes under it. Logging it
		// when it changes is the useful behaviour, rather than logging it once
		// and being wrong an hour later.
		last := network.ReachabilityUnknown
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				status := service.Status()
				if status == last {
					continue
				}
				last = status
				switch status {
				case network.ReachabilityPublic:
					h.logf("this node is reachable from the open internet, " +
						"so peers should dial it directly")
				case network.ReachabilityPrivate:
					h.logf("this node sits behind a network that hides it; " +
						"peers will reach it through the relay")
				default:
					h.logf("reachability from outside is not established yet")
				}
			}
		}
	}()
}

// ID is this node's peer identifier, which is what everything else addresses it
// by. It is derived from the identity key and does not change between restarts.
func (h *Host) ID() peer.ID { return h.host.ID() }

// Host exposes the underlying libp2p host.
func (h *Host) Libp2p() host.Host { return h.host }

// Addrs are the addresses other nodes can reach this one at right now. They
// change when the network does, which is the whole reason this package exists.
func (h *Host) Addrs() []ma.Multiaddr { return h.host.Addrs() }

// Connect dials a peer directly. Used for the addresses that are known rather
// than discovered, which is still how the first nodes of a network meet.
// relayOffered reports whether this host can be reached through somebody else's
// relay, which is what makes a node behind a symmetric NAT take part at all.
//
// It is read from the addresses the host publishes rather than from the option
// that was asked for, because asking for the relay and actually having one are
// not the same thing: a node on a home connection cannot reserve one, and a node
// that believes it is reachable when it is not would give up on dialling
// directly and wait forever.
func (h *Host) relayOffered() bool {
	for _, addr := range h.Addrs() {
		if _, err := addr.ValueForProtocol(ma.P_CIRCUIT); err == nil {
			return true
		}
	}
	return false
}

func (h *Host) Connect(ctx context.Context, id peer.ID, addr ma.Multiaddr) error {
	return h.host.Connect(ctx, peer.AddrInfo{ID: id, Addrs: []ma.Multiaddr{addr}})
}

// Close stops the host and everything hanging off it.
func (h *Host) Close() error {
	h.cancel()
	var firstErr error
	if h.dht != nil {
		if err := h.dht.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := h.host.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// loadOrCreateIdentity reads the persistent key, or makes one the first time.
//
// Generating a new key on every start would be the easiest thing to write and
// the worst thing to ship: every restart would make the node a stranger to every
// peer it had, and the relay and the table would both forget it.
func loadOrCreateIdentity(path string) (privKey, error) {
	if raw, err := os.ReadFile(path); err == nil {
		key, err := unmarshalIdentity(raw)
		if err != nil {
			return nil, fmt.Errorf("the peer-to-peer identity at %s is not readable: %w", path, err)
		}
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("cannot read the peer-to-peer identity: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the data directory: %w", err)
	}

	key, raw, err := generateIdentity()
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return nil, fmt.Errorf("cannot write the peer-to-peer identity: %w", err)
	}
	return key, nil
}
