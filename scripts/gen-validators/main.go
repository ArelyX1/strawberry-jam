// Command gen-validators writes the validator file a devnet uses to decide who
// exists and where to reach them.
//
// It exists so the file can be regenerated rather than hand-edited. The public
// key has to be the one that belongs to the private key, and a public key typed
// in by hand is either right by luck or wrong in a way that only shows up as a
// handshake that never completes.
//
// Usage:
//
//	go run ./scripts/gen-validators -out test_validators.json -count 6 -port 30333
package main

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
)

type validator struct {
	Name                string `json:"name"`
	Seed                string `json:"seed"`
	Ed25519Private      string `json:"ed25519_private"`
	Ed25519Pub          string `json:"ed25519_pub"`
	BandersnatchPrivate string `json:"bandersnatch_private"`
	BandersnatchPub     string `json:"bandersnatch_pub"`
	IP                  string `json:"ip"`
	Port                int    `json:"port"`
}

var names = []string{"Alice", "Bob", "Carol", "Dave", "Erin", "Frank", "Grace", "Heidi"}

// The seed of validator i is the byte i repeated.
//
// Entries already in the file are kept exactly as they are. A validator is
// identified by its key, so regenerating a key that something already refers to
// would give that something a different identity, and the first two entries came
// with seeds that are not this pattern. Rewriting them would change who Alice
// and Bob are for no reason, so the file grows instead.
func seedFor(i int) []byte {
	seed := make([]byte, ed25519.SeedSize)
	for j := range seed {
		seed[j] = byte(i)
	}
	return seed
}

// existing reads the validators already in the file, so they survive a
// regeneration. A file that is not there yet, or cannot be read, is not an
// error: that is the first run.
func existing(path string) []validator {
	data, err := os.ReadFile(path) //nolint:gosec // a devnet fixture, not a secret
	if err != nil {
		return nil
	}
	var vs []validator
	if err := json.Unmarshal(data, &vs); err != nil {
		return nil
	}
	return vs
}

func main() {
	out := flag.String("out", "test_validators.json", "where to write the validator file")
	count := flag.Int("count", 6, "how many validators")
	port := flag.Int("port", 30333, "port of the first validator; the rest follow it")
	ip := flag.String("ip", "0000:0000:0000:0000:0000:0000:0000:0001", "address every validator listens on")
	flag.Parse()

	if *count < 1 {
		fmt.Fprintln(os.Stderr, "gen-validators: count must be at least 1")
		os.Exit(1)
	}
	if *count > len(names) {
		fmt.Fprintf(os.Stderr, "gen-validators: only %d names are known, asked for %d\n", len(names), *count)
		os.Exit(1)
	}

	// Whatever is already there comes first and is left alone.
	vs := existing(*out)
	if len(vs) > *count {
		vs = vs[:*count]
	}

	for i := len(vs); i < *count; i++ {
		seed := seedFor(i)
		priv := ed25519.NewKeyFromSeed(seed)
		pub, ok := priv.Public().(ed25519.PublicKey)
		if !ok {
			fmt.Fprintln(os.Stderr, "gen-validators: the derived key is not an ed25519 public key")
			os.Exit(1)
		}
		name := fmt.Sprintf("validator-%d", i)
		if i < len(names) {
			name = names[i]
		}
		vs = append(vs, validator{
			Name: name,
			Seed: "0x" + hex.EncodeToString(seed),
			// The private half is the seed, not the 64-byte key: the node feeds
			// this to ed25519.NewKeyFromSeed, which panics on anything else. The
			// key it is derived from is not written down, so there is nothing to
			// fall out of step with the public key beside it.
			Ed25519Private: "0x" + hex.EncodeToString(seed),
			Ed25519Pub:     "0x" + hex.EncodeToString(pub),
			IP:             *ip,
			Port:           *port + i,
		})
	}

	data, err := json.MarshalIndent(vs, "", "    ")
	if err != nil {
		fmt.Fprintf(os.Stderr, "gen-validators: %v\n", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*out, append(data, '\n'), 0o644); err != nil { //nolint:gosec // a devnet fixture, not a secret
		fmt.Fprintf(os.Stderr, "gen-validators: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("wrote %d validators to %s, ports %d-%d\n", *count, *out, *port, *port+*count-1)
}
