package bandersnatch

import (
	_ "embed"
)

// On Windows the bandersnatch library is a DLL. See the reed-solomon Windows
// file for why each platform embeds its own.
//
// The file is produced by scripts/build-release.sh before go build runs. A build
// without it fails at compile time on the missing embed, which is the correct
// failure: a node that cannot verify validator signatures must not start and
// pretend it can.
const rustLibraryName = "libbandersnatch.dll"

//go:embed lib/libbandersnatch.dll
var rustLibraryBytes []byte
