package reedsolomon

import (
	_ "embed"
)

// On Windows the Reed-Solomon library is a DLL, and it has to be a different
// file from the one Linux uses: same code, different ABI and loader. Embedding
// it per platform is what lets one command produce a Windows binary without a
// C toolchain on the machine that builds it.
//
// The file is produced by scripts/build-release.sh, which compiles the Rust
// library for this target before go build runs and drops it here. A build
// without it fails at compile time on the missing embed, which is the right
// failure: a node running with the wrong erasure coding silently corrupts data.
const rustLibraryName = "liberasurecoding.dll"

//go:embed lib/liberasurecoding.dll
var rustLibraryBytes []byte
