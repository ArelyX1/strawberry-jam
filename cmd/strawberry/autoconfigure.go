package main

// autoconfigure makes a node dropped next to a network kit run with no
// arguments at all.
//
// A kit is a folder with the genesis, the validator set, an appconfig and at
// most one net conf, copied whole onto a machine. Opening the binary without
// flags looks for appconfig.json in the working directory: right when the kit
// is unpacked right here, wrong when it sits one folder down, and confusing a
// few seconds later when the node panics about a file it was never told about.
// So the fallback looks, in order, in the kit folder "config" and in the
// working directory, under the kit's own names (genesis.json rather than
// genesis/chain-dev.json, validators.json rather than test_validators.json).
//
// Nothing the command line already set is touched. The fallback answers the
// questions nobody answered: what the application config is, who the validators
// are, which economy to run, and which validator this node is. The last needs
// the node's net conf, and that is what decides whether a bare run is allowed
// to guess at all: a kit for a single machine holds exactly one
// net-conf-<index>.conf and the index is read out of the name, while a kit
// meant for many machines holds several and stays in the hands of the launch
// commands, because a bare run of one of those has no way to know which one it
// is.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const autoConfigDir = "config"

// resolveKitPath gives the fallback for one of the files the node reads, when
// its requested path is still the default and that default is not there.
func resolveKitPath(requested, dflt, kitName string) string {
	if requested != dflt {
		return requested
	}
	if fileExists(requested) {
		return requested
	}
	for _, dir := range []string{autoConfigDir, "."} {
		candidate := filepath.Join(dir, kitName)
		if fileExists(candidate) {
			return candidate
		}
	}
	return requested
}

// loneNetConf is the net conf of a kit that belongs to one machine, if there is
// one. It returns the path and the index that the name says.
func loneNetConf() (string, int, bool) {
	for _, dir := range []string{autoConfigDir, "."} {
		matches, err := filepath.Glob(filepath.Join(dir, "net-conf-*.conf"))
		if err != nil || len(matches) != 1 {
			continue
		}
		name := filepath.Base(matches[0])
		trimmed := strings.TrimSuffix(strings.TrimPrefix(name, "net-conf-"), ".conf")
		idx, err := strconv.Atoi(trimmed)
		if err != nil || idx < 0 {
			return "", 0, false
		}
		return matches[0], idx, true
	}
	return "", 0, false
}

func autoconfigure(configFile, validatorsFile, genesis *string, netConf *string, validatorIndex *int, wwwDir, dataDir *string, rpcPort *int) {
	*configFile = resolveKitPath(*configFile, "appconfig.json", "appconfig.json")
	*validatorsFile = resolveKitPath(*validatorsFile, "test_validators.json", "validators.json")
	*genesis = resolveKitPath(*genesis, "genesis/chain-dev.json", "genesis.json")

	if *netConf == "" {
		if nc, idx, ok := loneNetConf(); ok {
			*netConf = nc
			if *validatorIndex < 0 {
				*validatorIndex = idx
			}
			if *rpcPort == 9944 {
				*rpcPort = 9944 + idx
			}
		}
	}

	if *wwwDir == "" && dirExists("www") {
		*wwwDir = "www"
	}
	if *dataDir == "" && dirExists("datos") {
		*dataDir = "datos"
	}
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && !st.IsDir()
}

func dirExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
