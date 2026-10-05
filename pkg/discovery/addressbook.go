package discovery

// What this file is for: remembering who was seen where.
//
// A node that has once met its peers does not need to meet them again from
// scratch every time it restarts. The addresses it learned are still, most of
// the time, the addresses those peers are on, and keeping them turns a restart
// from "I have to find the whole network again before I can do anything" into
// "I already know where everybody is".
//
// That is what makes the first contact the only hard problem. Once two nodes
// have met, neither of them needs to be told anything again, and the network
// re-forms by itself across a restart of any of its members.
//
// It is deliberately not a cache, though it looks like one, so the file is
// written next to the identity key and carries the same warning. What is in it
// is the network's address book, and a node that loses it is a node that has to
// be introduced again.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/core/peer"
	ma "github.com/multiformats/go-multiaddr"
)

// knownPeersFile is the address book inside the data directory.
const knownPeersFile = "known-peers.json"

// howLongAPeerSurvivesWithoutBeingSeenAgain.
//
// Addresses are not promises. A laptop that was on the cafe network yesterday is
// not there today, and dialling an address nobody answers wastes a handshake on
// every attempt for as long as the entry lives. Entries therefore expire, but
// slowly: an address that worked an hour ago usually still works, and dropping
// it because a node was briefly unreachable would undo the restart for no reason.
const howLongAPeerSurvives = 7 * 24 * time.Hour

type knownPeer struct {
	ID    string    `json:"id"`
	Addrs []string  `json:"addrs"`
	Seen  time.Time `json:"seen"`
}

type addressBook struct {
	path  string
	mu    sync.Mutex
	peers map[peer.ID]knownPeer
}

// loadAddressBook reads the book, treating a missing or unreadable file as an
// empty one. A node that cannot read its own address book is a node that has to
// find everybody again, which is annoying but not fatal, so it is not worth
// refusing to start over.
func loadAddressBook(dataDir string) *addressBook {
	b := &addressBook{
		path:  filepath.Join(dataDir, knownPeersFile),
		peers: make(map[peer.ID]knownPeer),
	}

	// The directory is made here and not on the first save. A node whose data
	// directory was never written to has no peer directory yet, and waiting for
	// one to appear means the book is never created: the peer it would have
	// listed is the one that answered, and by then there is nothing left to
	// announce it.
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return b
	}

	raw, err := os.ReadFile(b.path)
	if err != nil {
		return b
	}

	var guardados []knownPeer
	if err := json.Unmarshal(raw, &guardados); err != nil {
		return b
	}

	now := time.Now()
	for _, g := range guardados {
		if now.Sub(g.Seen) > howLongAPeerSurvives {
			continue
		}
		id, err := peer.Decode(g.ID)
		if err != nil {
			continue
		}
		addrs := make([]ma.Multiaddr, 0, len(g.Addrs))
		for _, a := range g.Addrs {
			addr, err := ma.NewMultiaddr(a)
			if err != nil {
				continue
			}
			addrs = append(addrs, addr)
		}
		if len(addrs) == 0 {
			continue
		}
		b.peers[id] = knownPeer{ID: g.ID, Addrs: g.Addrs, Seen: g.Seen}
	}
	return b
}

// remember records where a peer was seen, keeping the best addresses it had.
func (b *addressBook) remember(id peer.ID, addrs []ma.Multiaddr) {
	if len(addrs) == 0 {
		return
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	textos := make([]string, 0, len(addrs))
	for _, a := range addrs {
		textos = append(textos, a.String())
	}
	sort.Strings(textos)
	textos = uniq(textos)

	// The addresses already in the book are kept alongside the new ones. A peer
	// that answers on three addresses does not answer on three because three were
	// written down, and forgetting the others would mean rediscovering them.
	if previa, ok := b.peers[id]; ok {
		textos = uniq(append(textos, previa.Addrs...))
		sort.Strings(textos)
	}

	b.peers[id] = knownPeer{ID: id.String(), Addrs: textos, Seen: time.Now()}
}

// uniq removes repeats from a sorted list, keeping it sorted.
func uniq(lista []string) []string {
	if len(lista) == 0 {
		return lista
	}
	salida := lista[:1]
	for _, s := range lista[1:] {
		if s != salida[len(salida)-1] {
			salida = append(salida, s)
		}
	}
	return salida
}

// addrs returns the usable addresses known for a peer.
func (b *addressBook) addrs(id peer.ID) []ma.Multiaddr {
	b.mu.Lock()
	defer b.mu.Unlock()

	g, ok := b.peers[id]
	if !ok {
		return nil
	}
	salida := make([]ma.Multiaddr, 0, len(g.Addrs))
	for _, s := range g.Addrs {
		if addr, err := ma.NewMultiaddr(s); err == nil {
			salida = append(salida, addr)
		}
	}
	return salida
}

// all returns every peer in the book that is worth dialling, newest first, so
// that a network with a dead entry and a good one uses the good one.
func (b *addressBook) all() []peer.AddrInfo {
	b.mu.Lock()
	defer b.mu.Unlock()

	ahora := time.Now()
	ids := make([]peer.ID, 0, len(b.peers))
	for id, g := range b.peers {
		if ahora.Sub(g.Seen) > howLongAPeerSurvives {
			continue
		}
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		return b.peers[ids[i]].Seen.After(b.peers[ids[j]].Seen)
	})

	salida := make([]peer.AddrInfo, 0, len(ids))
	for _, id := range ids {
		g := b.peers[id]
		addrs := make([]ma.Multiaddr, 0, len(g.Addrs))
		for _, s := range g.Addrs {
			if addr, err := ma.NewMultiaddr(s); err == nil {
				addrs = append(addrs, addr)
			}
		}
		if len(addrs) == 0 {
			continue
		}
		salida = append(salida, peer.AddrInfo{ID: id, Addrs: addrs})
	}
	return salida
}

// save writes the book out, oldest entries first dropped.
func (b *addressBook) save() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if err := os.MkdirAll(filepath.Dir(b.path), 0o700); err != nil {
		return err
	}

	ahora := time.Now()
	guardados := make([]knownPeer, 0, len(b.peers))
	for _, g := range b.peers {
		if ahora.Sub(g.Seen) > howLongAPeerSurvives {
			continue
		}
		guardados = append(guardados, g)
	}
	sort.Slice(guardados, func(i, j int) bool { return guardados[i].Seen.After(guardados[j].Seen) })

	raw, err := json.MarshalIndent(guardados, "", "  ")
	if err != nil {
		return fmt.Errorf("no se puede codificar el libro de direcciones: %w", err)
	}
	if err := os.WriteFile(b.path, raw, 0o600); err != nil {
		return fmt.Errorf("no se puede escribir %s: %w", b.path, err)
	}
	return nil
}
