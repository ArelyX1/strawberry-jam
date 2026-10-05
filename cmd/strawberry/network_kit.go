package main

// The network kit is what makes standing up a node a single step. One command
// writes the four files every node needs, with the network's founding timeslot
// already filled in, so that no machine has to be told a number that the others
// would then disagree with.
//
// The founding timeslot is the whole reason this exists. Every node has to
// found the chain at the same timeslot or it builds a different chain, and the
// symptom of getting it wrong is a network that never syncs and gives no hint
// why. Writing it once, here, and sharing the file is the fix.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/devnet"
)

// defaultBaseGenesis is the economy this kit is based on. It is the shipped dev
// genesis with the founding timeslot left out on purpose: the timeslot has to
// be stamped at the moment the network is created, not when the repository was
// written.
const defaultBaseGenesis = "genesis/chain-dev.json"

// p2pPort is the port the first node listens on, and each node after it takes
// the next one. These are the ports the net conf files name, so they have to
// agree with the --port each node is launched with.
const p2pPort = 30334

// baseGenesisPath is where the economy to copy comes from: whatever --genesis
// names, so that a test or a chain of one's own is the one stamped, and the
// shipped dev genesis when the flag was left alone. Reaching for a path relative
// to the working directory alone would mean the kit only ever works when it is
// run from the repository root, which is nowhere a node actually runs.
//
// It returns an error when the genesis cannot be found, rather than a path that
// does not exist. The difference matters: a missing genesis used to leave the
// loop below spinning forever, because filepath.Dir(".") is ".", so the search
// never climbed and never ended. The command appeared to hang and wrote an empty
// kit, which reads as "the network kit does not work" instead of "you did not
// copy the genesis".
func baseGenesisPath() (string, error) {
	if genesisPath != "" {
		if _, err := os.Stat(genesisPath); err != nil {
			return "", fmt.Errorf("the genesis %s named by --genesis cannot be read: %w", genesisPath, err)
		}
		return genesisPath, nil
	}
	if _, err := os.Stat(defaultBaseGenesis); err == nil {
		return defaultBaseGenesis, nil
	}
	// Walked up from the working directory: the binary sits in the repository
	// root's parent at most, and the genesis is a few levels down from it.
	//
	// The walk starts from the absolute working directory rather than from "."
	// on purpose. filepath.Dir(".") is ".", so a walk that started at "." would
	// compare "." with itself, decide it had reached the top, and stop after
	// checking one path. Going up from an absolute path is the only way the
	// parent of a directory is ever a different string.
	dir, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("cannot work out the working directory to look for %s in: %w", defaultBaseGenesis, err)
	}
	for {
		candidate := filepath.Join(dir, defaultBaseGenesis)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("the dev genesis %s was not found from the working directory %s; "+
		"run from the repository, or name the genesis to copy with --genesis",
		defaultBaseGenesis, mustGetwd())
}

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		return "unknown"
	}
	return dir
}

func writeNetworkKit(dir string, count int) error {
	if count <= 0 {
		count = 3
	}
	if count > 32 {
		return fmt.Errorf("%d validators asked for, the dev chain holds at most 32", count)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("cannot create %s: %w", dir, err)
	}

	path, err := baseGenesisPath()
	if err != nil {
		return err
	}

	base, err := devnet.LoadGenesis(path)
	if err != nil {
		return fmt.Errorf("the base genesis is not usable: %w", err)
	}

	// The chain is founded at the timeslot that is starting right now, so
	// nobody has to replay a year of empty timeslots to catch up.
	base.GenesisTimeslot = jamtime.Timeslot(nowTimeslot(base.TimeslotSecs))

	genesisPath := filepath.Join(dir, "genesis.json")
	if err := writeJSON(genesisPath, base); err != nil {
		return err
	}

	// The validators file is keyed by the timeslot, and with all of them on
	// this one machine there is nothing to reach them at, so every address is
	// left for --net-conf or for the node to announce. Keys are the identity
	// and do not depend on where the node ends up.
	validatorsPath := filepath.Join(dir, "validators.json")
	if err := writeValidators(validatorsPath, count); err != nil {
		return err
	}

	// The node refuses to start without this, and it is trivial, but leaving it
	// out of the kit means the first thing anyone does after copying the files
	// is a panic about a file nobody was told about.
	configPath := filepath.Join(dir, "appconfig.json")
	if err := writeJSON(configPath, map[string]any{
		"loglevel":       "info",
		"validatorIndex": 0,
	}); err != nil {
		return err
	}

	// One net conf per node, because a net conf says where THIS node listens and
	// where it should look for the others. Without them every node looks for
	// everybody on the port in the validator file, nobody is listening there, and
	// the mesh is silently empty while all the nodes look perfectly healthy.
	//
	// That failure is expensive: the nodes start, produce blocks, answer the RPC
	// and say they are fine, and there is no message anywhere saying that they
	// are all alone. Writing them here is what makes the kit work on the first
	// try rather than the fourth.
	for i := 0; i < count; i++ {
		if err := writeNetConf(filepath.Join(dir, netConfName(i)), i, count); err != nil {
			return err
		}
	}

	launch := filepath.Join(dir, "LEVANTAR.txt")
	if err := writeLaunch(launch, count); err != nil {
		return err
	}

	fmt.Printf("network kit written to %s\n", dir)
	fmt.Printf("  %s: the economy, founded at timeslot %d\n", genesisPath, base.GenesisTimeslot)
	fmt.Printf("  %s: %d validator keys\n", validatorsPath, count)
	fmt.Printf("  %s: node settings\n", configPath)
	fmt.Printf("  %s: the command to run on each machine\n", launch)
	fmt.Printf("\ncopy this directory to every machine, then follow %s\n", launch)
	return nil
}

// nowTimeslot is the timeslot the chain begins at. It is derived from the same
// epoch the rest of the devnet tooling uses so that a kit written here lines up
// with a kit written by the shell scripts.
func nowTimeslot(slotSecs int) int64 {
	epoch := time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)
	elapsed := time.Since(epoch)
	return int64(elapsed.Seconds()) / int64(slotSecs)
}

func writeValidators(path string, count int) error {
	// The field names and shapes here are the ones the loader actually reads.
	// The public key is ed25519_pub and not ed25519_public, and the private half
	// is written under both seed and ed25519_private so that either spelling
	// works: getting either wrong does not fail to load, it fails later and
	// cryptographically, which is a far worse way to find out.
	type entry struct {
		Name       string `json:"name"`
		Index      int    `json:"index"`
		Seed       string `json:"seed"`
		Ed25519Prv string `json:"ed25519_private"`
		Ed25519Pub string `json:"ed25519_pub"`
		IP         string `json:"ip"`
		Port       int    `json:"port"`
	}
	out := make([]entry, 0, count)
	for i := 0; i < count; i++ {
		priv, pub, err := devnet.DevValidatorKey(i)
		if err != nil {
			return fmt.Errorf("validator %d key: %w", i, err)
		}
		seedHex := "0x" + hex.EncodeToString(priv.Seed())
		out = append(out, entry{
			Name:       fmt.Sprintf("validator-%d", i),
			Index:      i,
			Seed:       seedHex,
			Ed25519Prv: seedHex,
			Ed25519Pub: "0x" + hex.EncodeToString(pub),
			// The IPv6 wildcard, not a blank address. A blank one leaves the
			// validator state without a usable declaration and the node dies at
			// startup with "not an IPv6 address", which is a poor first thing to
			// hand someone setting up a network. Every machine can listen on
			// ::, and a machine behind NAT gets its reachable address from what
			// its peers announce rather than from a value baked in here.
			IP: "::",
			// The port is derived from the index, and it has to be. A single
			// port written here looks right until two nodes are launched without
			// --port, at which point the second one dies with "address already in
			// use" and the network silently ends up one node short. The net conf
			// and the launch command both counted from p2pPort already; this was
			// the one place that did not, so the validator set disagreed with
			// every other file about where anybody listens.
			Port: p2pPort + i,
		})
	}
	return writeJSON(path, out)
}

// netConfName is the file a node's own view of the network lives in. The index
// is in the name because a machine holding several nodes needs one per node and
// they cannot share: each says which of them listens where.
func netConfName(index int) string {
	return fmt.Sprintf("net-conf-%d.conf", index)
}

// The addresses are the IPv6 wildcard and a port derived from the index. On one
// machine that is exactly right. Across machines it is a starting point rather
// than an answer, and that is honest: where a node is reachable depends on the
// NAT in front of it, which is not knowable when the kit is written. Discovery
// is what fills this in, and until it does, a machine behind NAT is set by hand.
func writeNetConf(path string, index, count int) error {
	port := p2pPort + index
	var b strings.Builder
	fmt.Fprintf(&b, "# Where node %d listens, and where it looks for the others.\n", index)
	fmt.Fprintf(&b, "# The port has to match the --port this node is given.\n")
	fmt.Fprintf(&b, "listen = [::]:%d\n\n", port)
	b.WriteString("[peers]\n")
	for i := 0; i < count; i++ {
		fmt.Fprintf(&b, "%d = [::]:%d\n", i, p2pPort+i)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func writeLaunch(path string, count int) error {
	var b []byte
	add := func(format string, args ...any) {
		b = append(b, fmt.Sprintf(format, args...)...)
	}

	add("How to stand up this network\n")
	add("==============================\n\n")
	add("Every machine runs the same binary against the same three files from this\n")
	add("directory. Nothing else has to match, and nothing has to be edited.\n\n")

	add("1. On the first machine, run a node:\n\n")
	add("   ./strawberry --init-genesis ./red   (you already did this)\n\n")
	add("2. Copy this directory to the other machines.\n\n")
	add("3. On each machine, start one node per validator you want:\n\n")

	for i := 0; i < count; i++ {
		add("   ./strawberry \\\n")
		add("     --config %s/appconfig.json \\\n", "/ruta/a/esta/carpeta")
		add("     --validators-file %s/validators.json \\\n", "/ruta/a/esta/carpeta")
		add("     --genesis %s/genesis.json \\\n", "/ruta/a/esta/carpeta")
		add("     --validator-index %d \\\n", i)
		add("     --net-conf %s/%s \\\n", "/ruta/a/esta/carpeta", netConfName(i))
		add("     --port %d \\\n", p2pPort+i)
		add("     --rpc-port %d \\\n", 9944+i)
		add("     --data-dir ./datos%d\n\n", i)
	}

	add("On a machine holding more than one node, give each its own index and its\n")
	add("own data directory, and start them at the same time.\n\n")
	add("The nodes find each other on the local network by themselves, and reach\n")
	add("the ones on other networks through the relay. There are no addresses to\n")
	add("write down anywhere.\n\n")
	add("Check that they agree:\n\n")
	add("   curl -s -X POST -H 'content-type: application/json' \\\n")
	add("     -d '{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"jam_getHeader\",\"params\":[null]}' \\\n")
	add("     http://localhost:9944 | grep -o '\"timeSlotIndex\":[0-9]*'\n\n")
	add("Every node has to print the same number and keep climbing. The state root\n")
	add("comes out of the same call under resultingStateRoot and has to match too.\n")

	return os.WriteFile(path, b, 0o644)
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode %s: %w", path, err)
	}
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("cannot write %s: %w", path, err)
	}
	return nil
}
