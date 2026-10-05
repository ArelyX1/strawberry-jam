package main

// The network kit is the promise that standing up a node takes one step. These
// tests hold it to that: a kit written by --init-genesis has to be loadable and
// has to start a node, because every way that breaks is a way somebody on a
// remote machine finds out at the worst moment.

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/eigerco/strawberry/pkg/devnet"
)

func writeKit(t *testing.T, count int) string {
	t.Helper()
	// The kit stamps whatever economy --genesis names, so a test has to point it
	// at the shipped dev genesis rather than at the working directory.
	prev := genesisPath
	genesisPath = filepath.Join("..", "..", defaultBaseGenesis)
	t.Cleanup(func() { genesisPath = prev })

	dir := filepath.Join(t.TempDir(), "kit")
	if err := writeNetworkKit(dir, count); err != nil {
		t.Fatalf("kit: %v", err)
	}
	return dir
}

// A kit whose genesis has no founding timeslot is the single most expensive
// mistake in the whole setup, because the nodes all start, all look healthy, and
// quietly build different chains that never accept each other's blocks.
func TestKitGenesisCarriesAFoundingTimeslot(t *testing.T) {
	dir := writeKit(t, 3)

	genesis, err := devnet.LoadGenesis(filepath.Join(dir, "genesis.json"))
	if err != nil {
		t.Fatalf("the genesis the kit wrote does not load: %v", err)
	}
	if genesis.GenesisTimeslot == 0 {
		t.Fatal("the kit wrote a genesis with no genesisTimeslot, so every node that " +
			"reads it would found its own chain at its own start time and none of them " +
			"would ever agree")
	}
}

// Two kits written seconds apart have to found the chain at the same timeslot,
// or a machine given the second kit is on a different chain from the first.
func TestTwoKitsFoundTheSameChain(t *testing.T) {
	first, err := devnet.LoadGenesis(filepath.Join(writeKit(t, 2), "genesis.json"))
	if err != nil {
		t.Fatalf("first genesis: %v", err)
	}
	second, err := devnet.LoadGenesis(filepath.Join(writeKit(t, 2), "genesis.json"))
	if err != nil {
		t.Fatalf("second genesis: %v", err)
	}
	if first.GenesisTimeslot != second.GenesisTimeslot {
		t.Fatalf("two kits founded the chain at timeslot %d and %d, so they are two chains",
			first.GenesisTimeslot, second.GenesisTimeslot)
	}
}

// The genesis the kit stamps has to be the shipped dev economy, not a stub. A
// chain that comes up with no accounts has nothing to test against.
func TestKitKeepsTheDevEconomy(t *testing.T) {
	dir := writeKit(t, 3)
	genesis, err := devnet.LoadGenesis(filepath.Join(dir, "genesis.json"))
	if err != nil {
		t.Fatalf("genesis: %v", err)
	}
	if genesis.Service == nil || len(genesis.Service.Accounts) == 0 {
		t.Fatal("the kit wrote a genesis with no PAPU accounts")
	}
	if genesis.TimeslotSecs != 6 {
		t.Fatalf("timeslotSecs is %d, the dev chain is 6", genesis.TimeslotSecs)
	}
}

// Every field has to be named the way the loader reads it. A validator file with
// ed25519_public instead of ed25519_pub loads, then fails later as a bad
// certificate, which is a miserable thing to debug on someone else's machine.
func TestKitValidatorKeysMatchWhatTheNodeExpects(t *testing.T) {
	dir := writeKit(t, 4)

	raw, err := os.ReadFile(filepath.Join(dir, "validators.json"))
	if err != nil {
		t.Fatalf("validators: %v", err)
	}
	var written []FullValidatorInfo
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("the validators the kit wrote do not unmarshal: %v", err)
	}
	if len(written) != 4 {
		t.Fatalf("kit has %d validators, asked for 4", len(written))
	}

	for i, v := range written {
		seed, err := decodeHex(v.Ed25519Prv)
		if err != nil {
			t.Fatalf("validator %d: private key does not decode: %v", i, err)
		}
		if len(seed) != ed25519.SeedSize {
			t.Fatalf("validator %d: seed is %d bytes, want %d. The loader feeds this to "+
				"NewKeyFromSeed, which panics on any other length.",
				i, len(seed), ed25519.SeedSize)
		}
		pub, err := decodeHex(v.Ed25519Pub)
		if err != nil {
			t.Fatalf("validator %d: public key does not decode: %v", i, err)
		}
		if len(pub) != ed25519.PublicKeySize {
			t.Fatalf("validator %d: public key is %d bytes, want %d", i, len(pub), ed25519.PublicKeySize)
		}

		// The keys written have to be the keys the chain expects for this index,
		// or two nodes given the same kit still sign as different validators.
		_, want, err := devnet.DevValidatorKey(i)
		if err != nil {
			t.Fatalf("validator %d: devnet key: %v", i, err)
		}
		if hex.EncodeToString(pub) != hex.EncodeToString(want) {
			t.Fatalf("validator %d: kit wrote public key %s, the node derives %s",
				i, hex.EncodeToString(pub), hex.EncodeToString(want))
		}

		// A blank address leaves the validator state without a declaration and the
		// node dies at startup with "not an IPv6 address".
		if v.IP == "" {
			t.Fatalf("validator %d: the kit wrote no address, so the node cannot build "+
				"its own validator declaration and will not start", i)
		}
	}
}

// The node loads appconfig.json before anything else and panics without it, so a
// kit that omits it fails on the first command a newcomer types.
func TestKitShipsTheConfigTheNodeInsistsOn(t *testing.T) {
	dir := writeKit(t, 2)
	raw, err := os.ReadFile(filepath.Join(dir, "appconfig.json"))
	if err != nil {
		t.Fatalf("appconfig: %v", err)
	}
	var cfg AppConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("the appconfig the kit wrote does not unmarshal: %v", err)
	}
	if cfg.LogLevel == "" {
		t.Fatal("appconfig has no loglevel, so the node cannot parse it and panics")
	}
}

// The instructions are the whole point for somebody who did not build this, so
// they have to name the files that are actually there.
func TestKitInstructionsNameEveryFileItWrote(t *testing.T) {
	dir := writeKit(t, 3)
	raw, err := os.ReadFile(filepath.Join(dir, "LEVANTAR.txt"))
	if err != nil {
		t.Fatalf("instructions: %v", err)
	}
	text := string(raw)
	for _, want := range []string{"genesis.json", "validators.json", "appconfig.json"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the instructions never mention %s, which is a file the kit requires", want)
		}
	}
	for i := 0; i < 3; i++ {
		if !strings.Contains(text, "--validator-index "+strconv.Itoa(i)) {
			t.Fatalf("the instructions have no launch line for validator %d", i)
		}
	}
}

// Every node needs a net conf, and each one has to name a different port. If two
// nodes are told to listen on the same one, the second dies on bind; if a port
// is not written at all, the node looks for everybody on the port in the
// validator file, nobody is there, and the mesh is silently empty while every
// node reports itself healthy.
func TestKitWritesANetConfPerNodeWithDistinctPorts(t *testing.T) {
	dir := writeKit(t, 4)

	ports := map[int]string{}
	for i := 0; i < 4; i++ {
		path := filepath.Join(dir, netConfName(i))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("node %d has no net conf: %v", i, err)
		}
		conf, err := devnet.ParseNetConf(string(raw))
		if err != nil {
			t.Fatalf("node %d: the net conf the kit wrote does not parse: %v", i, err)
		}

		// It has to be loadable, not merely present.
		if conf.Listen == "" {
			t.Fatalf("node %d: net conf has no listen address", i)
		}
		if len(conf.Peers) != 4 {
			t.Fatalf("node %d: net conf knows %d peers, want 4", i, len(conf.Peers))
		}
		for j := 0; j < 4; j++ {
			if _, ok := conf.Peers[uint16(j)]; !ok {
				t.Fatalf("node %d: net conf has no address for validator %d", i, j)
			}
		}

		// The port it listens on has to be the one its peers are told to reach.
		listenPort := portOf(t, conf.Listen)
		if addr, ok := conf.Peers[uint16(i)]; ok {
			if got := portOf(t, addr); got != listenPort {
				t.Fatalf("node %d listens on %d but its own peers entry says %d, so the "+
					"others will not find it where it is", i, listenPort, got)
			}
		}
		if other, clash := ports[listenPort]; clash {
			t.Fatalf("nodes %s and %d are both told to listen on %d, and the second "+
				"one to start will die on bind", other, i, listenPort)
		}
		ports[listenPort] = fmt.Sprintf("%d", i)
	}
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("address %q does not carry a port: %v", addr, err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("port %q is not a number: %v", port, err)
	}
	return n
}
