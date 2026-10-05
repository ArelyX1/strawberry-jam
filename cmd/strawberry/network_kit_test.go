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
	"time"

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

// A missing genesis used to leave the search for it spinning forever, because
// filepath.Dir(".") is "." and the walk up the tree never moved. The symptom was
// a command that appeared to hang and wrote an empty kit, which reads as "the
// kit does not work" rather than "the genesis is not here". This pins that it
// answers instead.
func TestAMissingGenesisIsReportedInsteadOfHungOn(t *testing.T) {
	dir := t.TempDir()
	chdir(t, dir)

	old := genesisPath
	genesisPath = ""
	t.Cleanup(func() { genesisPath = old })

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := baseGenesisPath()
		if err == nil {
			t.Error("a genesis that is not there was reported as found")
		}
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("looking for a genesis that is not there did not finish; this is the " +
			"loop that never left the working directory")
	}
}

// The same walk has to actually find the genesis when it is in a parent of the
// working directory, which is the case where the binary is run from a data
// directory rather than from the repository.
func TestAGenesisInAParentDirectoryIsFound(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "genesis"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, defaultBaseGenesis), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	chdir(t, deep)

	old := genesisPath
	genesisPath = ""
	t.Cleanup(func() { genesisPath = old })

	got, err := baseGenesisPath()
	if err != nil {
		t.Fatalf("the genesis two levels up was not found: %v", err)
	}
	want := filepath.Join(root, defaultBaseGenesis)
	if filepath.Clean(got) != filepath.Clean(want) {
		t.Fatalf("found %s, want %s", got, want)
	}
}

func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

// Every validator has to have its own port, and that number has to be the same
// one the net conf and the launch command use.
//
// This failed before with a fixed port in the validator set while the other two
// files counted from p2pPort. Nothing caught it, because the launch command
// passes --port explicitly and overwrote the wrong value on its way in. The
// damage only appeared for a node started by hand, or by anything that does not
// copy the launch command verbatim: it died with "address already in use" and
// the network was quietly one validator short.
func TestEveryValidatorGetsItsOwnPort(t *testing.T) {
	dir := t.TempDir()
	if err := writeNetworkKit(dir, 4); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "validators.json"))
	if err != nil {
		t.Fatal(err)
	}
	var validators []struct {
		Index int    `json:"index"`
		IP    string `json:"ip"`
		Port  int    `json:"port"`
	}
	if err := json.Unmarshal(raw, &validators); err != nil {
		t.Fatal(err)
	}
	if len(validators) != 4 {
		t.Fatalf("the kit has %d validators, want 4", len(validators))
	}

	ports := make(map[int]int)
	for _, v := range validators {
		if want := p2pPort + v.Index; v.Port != want {
			t.Errorf("validator %d has port %d, want %d", v.Index, v.Port, want)
		}
		if other, taken := ports[v.Port]; taken {
			t.Errorf("validators %d and %d both have port %d, so one of them cannot start",
				other, v.Index, v.Port)
		}
		ports[v.Port] = v.Index
		if v.IP == "" {
			t.Errorf("validator %d has no address, and a node with none dies at startup", v.Index)
		}
	}
}

// The launch command has to name the same port the validator set does, or the
// override it passes will fight with the file the node reads.
func TestTheLaunchCommandAgreesWithTheValidatorSet(t *testing.T) {
	dir := t.TempDir()
	if err := writeNetworkKit(dir, 3); err != nil {
		t.Fatal(err)
	}
	launch, err := os.ReadFile(filepath.Join(dir, "LEVANTAR.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		want := fmt.Sprintf("--port %d", p2pPort+i)
		if !strings.Contains(string(launch), want) {
			t.Errorf("the launch command does not say %q, so this node would listen "+
				"somewhere other than where the validator set says it does", want)
		}
	}
}
