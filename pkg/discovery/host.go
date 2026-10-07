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
	"github.com/libp2p/go-libp2p/core/crypto"
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

	// Identity overrides the key derived from the validator key.
	//
	// A node whose identity is derived rather than random can be worked out from
	// the genesis by every other node, which is what lets two machines find each
	// other without either having met a third. Leaving this nil keeps the stored
	// random key, which still works between nodes that have already met.
	Identity crypto.PrivKey

	// Bootstrap seeds the distributed hash table with peers to ask.
	//
	// Any one of them is enough, and there does not have to be any: the address
	// book from previous runs is used too. That is deliberate. A network whose
	// first contact depends on one particular machine being up is a network with
	// a single point of failure wearing a disguise, and it fails exactly when
	// somebody is already trying to fix something else.
	Bootstrap []peer.AddrInfo

	// PublicRendezvous joins the public distributed hash table as well as the
	// local one. Two nodes on different networks have no way to meet on the
	// local table, because there is no local anything in common: the shared
	// table is what they have, and a node that stays on its own table can only
	// ever be found by a machine on the same network as it. This is what makes
	// a node behind its own router findable from a phone tether with nothing
	// written down.
	PublicRendezvous bool

	// RendezvousPeers are the peers this node expects to exist, found by
	// identifier rather than by address.
	//
	// The chain fixes the validator set, and the peer id of every validator can
	// be worked out from its key, so a node can be told who its partners are
	// without being told where they are. Once the node knows who to look for, it
	// asks the table where each of them is instead of waiting to be found.
	RendezvousPeers []peer.ID

	// ChainPort is the port the chain transport listens on. When the node sits
	// behind a router that answers UPnP, this attempts to map that exact port to
	// this machine, so a node on another network can reach the chain at the
	// port the genesis says, with no forwarding set up by hand. Zero skips it.
	ChainPort int

	// Logf receives what the host is doing.
	Logf func(format string, args ...any)
}

func (o *Options) logf(format string, args ...any) {
	if o.Logf != nil {
		o.Logf(format, args...)
	}
}

// peerAddrTTL is how long an address learned about a peer is trusted without
// being confirmed. Long enough to cover a restart, short enough that a machine
// that moved is eventually dialled at its new address and not only at the old
// one.
const peerAddrTTL = 24 * time.Hour

// identityFile is the private key inside the data directory. The name says what
// it is so that nobody deletes it thinking it is a cache.
const identityFile = "p2p-identity.key"

// Host is a running peer-to-peer host. The zero value is not usable; build one
// with Start.
type Host struct {
	host      host.Host
	dht       *dht.IpfsDHT
	book      *addressBook
	bootstrap []peer.AddrInfo
	// publicBootstrap is the slice of published bootstrap peers the public
	// table was seeded with, kept for the dialer that keeps reaching out.
	publicBootstrap []peer.AddrInfo
	// rendezvous is who this node keeps asking the table after. The ids come
	// from the validator set, so they are known before any of them is met.
	rendezvous []peer.ID
	// publicRendezvous remembers that the shared table is a subject too.
	publicRendezvous bool

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
	var (
		priv crypto.PrivKey
		err  error
	)
	priv = opts.Identity
	if priv == nil {
		priv, err = loadOrCreateIdentity(keyPath)
		if err != nil {
			cancel()
			return nil, err
		}
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
		cfg = append(cfg, libp2p.EnableRelay(), libp2p.EnableRelayService(),
			// Hole punching is what lets two nodes behind their own routers
			// reach each other once each knows a public address of the other.
			// Without it, a relay stays necessary forever and joins every
			// conversation the two nodes have.
			libp2p.EnableHolePunching())
	} else {
		cfg = append(cfg, libp2p.DisableRelay())
	}

	h, err := libp2p.New(cfg...)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("cannot start the peer-to-peer host: %w", err)
	}

	out := &Host{
		host:             h,
		book:             loadAddressBook(opts.DataDir),
		bootstrap:        opts.Bootstrap,
		rendezvous:       opts.RendezvousPeers,
		publicRendezvous: opts.PublicRendezvous,
		logf:             opts.Logf,
		cancel:           cancel,
	}

	// Everything the address book holds goes back into the peer store before the
	// table is started, because the table can only be bootstrapped from peers and
	// this is where the peers this node already met come from. It is what makes a
	// restart find the network again with nothing else available.
	out.recordKnownPeers()

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

	if opts.ChainPort > 0 {
		go out.mapChainPort(ctx, opts.ChainPort)
	}
	if len(opts.RendezvousPeers) > 0 {
		go out.rendezvousLoop(ctx)
	}

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
	// Se junta lo que se sabe de cuatro maneras independientes, y no hace falta
	// que ninguna funcione. Lo que está en el peerstore yaincludes lo que la
	// tabla sabe; lo que da el bootstrap son las semillas; lo del libro son los
	// pares de corridas anteriores.
	//
	// Reunirlas es lo que quita el punto unico de fallo. Con una sola fuente, un
	// nodo apaga el DHT de su vecino hasta que ese vecino vuelve, y la red se
	// parte sin que ninguna de las dos mitades parezca equivocada.
	conjunto := make(map[peer.ID]peer.AddrInfo)

	for _, id := range h.host.Peerstore().Peers() {
		if id == h.host.ID() {
			continue
		}
		conjunto[id] = peer.AddrInfo{ID: id, Addrs: h.host.Peerstore().Addrs(id)}
	}

	for _, info := range h.bootstrap {
		if info.ID == h.host.ID() {
			continue
		}
		conjunto[info.ID] = info
		h.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerAddrTTL)
	}

	for _, info := range h.book.all() {
		if info.ID == h.host.ID() {
			continue
		}
		if previa, ok := conjunto[info.ID]; ok {
			conjunto[info.ID] = peer.AddrInfo{
				ID:    info.ID,
				Addrs: append(append([]ma.Multiaddr{}, previa.Addrs...), info.Addrs...),
			}
			continue
		}
		conjunto[info.ID] = info
		h.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerAddrTTL)
	}

	bootstrap := make([]peer.AddrInfo, 0, len(conjunto))
	for _, info := range conjunto {
		if len(info.Addrs) == 0 {
			continue
		}
		bootstrap = append(bootstrap, info)
	}

	// Joining the public table too is what lets two nodes on two different
	// networks find each other. The local seeds are reachable only from this
	// network; the published ones are the addresses of the shared table that
	// nodes from everywhere ask, and they are reachable from anywhere. Both are
	// kept: the local seeds keep a private network private, and the public ones
	// do not exist to be asked for the chain, only to find the machines.
	if h.publicRendezvous {
		h.publicBootstrap = dht.GetDefaultBootstrapPeerAddrInfos()
		vistos := make(map[peer.ID]bool, len(bootstrap))
		for _, info := range bootstrap {
			vistos[info.ID] = true
		}
		for _, info := range h.publicBootstrap {
			if visto, ok := vistos[info.ID]; ok && visto {
				continue
			}
			vistos[info.ID] = true
			bootstrap = append(bootstrap, info)
		}
	}

	if len(bootstrap) == 0 {
		// Esto no es un fallo y no se va a decir como si lo fuera. Una red nueva
		// empieza sin nadie, y el nodo tiene que arrancar igualmente para poder ser
		// encontrado por los demas: si se negara a arrancar, nadie lo encontraria
		// nunca y la red no empezaria.
		h.logf("nobody to ask yet, so the table starts empty and this node waits to be " +
			"found: it announces itself on the local network and answers whoever asks")
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
		h.logf("on the distributed hash table as %s, seeded with %d peers", routing.PeerID(), len(bootstrap))
	}()

	h.dialSeeds(ctx)
	h.watchPeers(ctx)

	return nil
}

// dialSeeds keeps trying to reach the peers it was told about, for as long as
// the node runs.
//
// One attempt at startup is not enough, and the reason is not that dialing is
// flaky. It is that the other machine is very likely not up yet: two people
// start two machines at the same time, or one starts after the other has given
// up, and a node that asked once and then forgot is a node that stays alone
// until somebody happens to restart both. So a seed is retried until it answers,
// with the wait growing each time so that a machine that is genuinely gone costs
// progressively less.
func (h *Host) dialSeeds(ctx context.Context) {
	pendientes := h.book.all()
	for _, info := range h.bootstrap {
		pendientes = append(pendientes, info)
	}
	for _, info := range h.publicBootstrap {
		pendientes = append(pendientes, info)
	}
	if len(pendientes) == 0 {
		return
	}

	go func() {
		vistos := make(map[peer.ID]bool, len(pendientes))
		// muertos son los pares cuyas direcciones del libro acaban de fallar. Se
		// dejan de marcar una vez que el par vuelve a responder: o por la tabla,
		// o porque un marcado de la red lo trajo de nuevo, y entonces volver a
		// pedirle por las demas direcciones tiene sentido.
		muertos := make(map[peer.ID]bool)
		espera := 2 * time.Second

		for {
			quedan := 0
			for _, info := range pendientes {
				if info.ID == h.host.ID() || len(info.Addrs) == 0 {
					continue
				}
				if muertos[info.ID] {
					continue
				}
				if h.host.Network().Connectedness(info.ID) == network.Connected {
					vistos[info.ID] = true
					continue
				}
				quedan++
				if vistos[info.ID] {
					// It was reached before and is not any more. Somebody
					// went away, and the one thing worth doing is looking
					// for it again rather than waiting to be asked.
					h.logf("lost contact with %s, looking for it again", info.ID)
					vistos[info.ID] = false
				}

				select {
				case <-ctx.Done():
					return
				default:
				}

				if err := h.host.Connect(ctx, info); err != nil {
					// Una direccion muerta no se olvida porque el par este muerto: se
					// olvida porque es la direccion la que no responde, y guardarla
					// solo hace que el nodo siga gritando "unreachable" durante dias
					// despues de que la maquina se haya movido. Si la maquina vuelve
					// a esa direccion, mDNS, la tabla y el cuaderno la aprenden de
					// nuevo; el libro no es la unica memoria que queda.
					delLibro := false
					for _, r := range h.book.all() {
						if r.ID == info.ID {
							delLibro = true
							break
						}
					}
					if delLibro {
						h.book.forget(info.ID)
						muertos[info.ID] = true
					}
					h.logf("could not reach %s to ask it for the others: %v", info.ID, err)
					continue
				}
				h.logf("reached %s to ask it for the others", info.ID)
				vistos[info.ID] = true
				muertos[info.ID] = false
				quedan--
			}

			if quedan == 0 {
				espera = 2 * time.Second
			} else if espera < time.Minute {
				espera *= 2
			}
			if espera > time.Minute {
				espera = time.Minute
			}

			t := time.NewTimer(espera)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
		}
	}()
}

// watchPeers keeps the address book up to date, which is the whole point of it:
// a peer seen now is a peer that can be found after a restart.
func (h *Host) watchPeers(ctx context.Context) {
	guardar := func() {
		for _, id := range h.host.Peerstore().Peers() {
			if id == h.host.ID() {
				continue
			}
			h.book.remember(id, h.host.Peerstore().Addrs(id))
		}
		if err := h.book.save(); err != nil {
			h.logf("the address book could not be written: %v", err)
		}
	}

	// La version de Notify que hay aqui no devuelve un tirador para cancelar, asi
	// que la suscripcion se queda hasta que el contexto se cierra. Es lo bastante:
	// el aviso de conexion solo sirve para escribir en el libro, y un libro al
	// que se deje de escribir tras cancelar el host ya no sirve para nada.
	h.host.Network().Notify(&network.NotifyBundle{
		ConnectedF: func(_ network.Network, c network.Conn) {
			h.book.remember(c.RemotePeer(), []ma.Multiaddr{c.RemoteMultiaddr()})
		},
		DisconnectedF: func(_ network.Network, c network.Conn) {
			h.book.remember(c.RemotePeer(), []ma.Multiaddr{c.RemoteMultiaddr()})
		},
	})

	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				guardar()
				return
			case <-t.C:
				guardar()
			}
		}
	}()
}

// recordKnownPeers puts the address book back into the peer store at startup.
func (h *Host) recordKnownPeers() {
	for _, info := range h.book.all() {
		if info.ID == h.host.ID() {
			continue
		}
		h.host.Peerstore().AddAddrs(info.ID, info.Addrs, peerAddrTTL)
	}
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
