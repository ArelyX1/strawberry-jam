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
	"time"

	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/devnet"
)

// defaultBaseGenesis is the economy this kit is based on. It is the shipped dev
// genesis with the founding timeslot left out on purpose: the timeslot has to
// be stamped at the moment the network is created, not when the repository was
// written.
const defaultBaseGenesis = "genesis/chain-dev.json"

// baseGenesisPath is where the economy to copy comes from: whatever --genesis
// names, so that a test or a chain of one's own is the one stamped, and the
// shipped dev genesis when the flag was left alone. Reaching for a path relative
// to the working directory alone would mean the kit only ever works when it is
// run from the repository root, which is nowhere a node actually runs.
func baseGenesisPath() string {
	if genesisPath != "" {
		return genesisPath
	}
	if _, err := os.Stat(defaultBaseGenesis); err == nil {
		return defaultBaseGenesis
	}
	// Walked up from the working directory: the binary sits in the repository
	// root's parent at most, and the genesis is a few levels down from it.
	for dir := "."; dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		candidate := filepath.Join(dir, defaultBaseGenesis)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return defaultBaseGenesis
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

	base, err := devnet.LoadGenesis(baseGenesisPath())
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
			IP:   "::",
			Port: 30334,
		})
	}
	return writeJSON(path, out)
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
