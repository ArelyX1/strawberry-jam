module github.com/eigerco/strawberry/sdk

go 1.25.5

// The SDK builds on the node's protocol primitives rather than restating them.
// Storage key derivation and the JAM hash types are consensus critical, so there
// is exactly one implementation of each and this module depends on it. The
// boundary stays explicit because the value types are distinct newtypes, so the
// compiler forces a conversion instead of letting the two drift apart.
require (
	github.com/decred/dcrd/dcrec/secp256k1/v4 v4.4.1
	github.com/eigerco/strawberry v0.7.2
	github.com/stretchr/testify v1.11.1
	golang.org/x/crypto v0.47.0
)

require (
	filippo.io/edwards25519 v1.1.1 // indirect
	github.com/davecgh/go-spew v1.1.2-0.20180830191138-d8f796af33cc // indirect
	github.com/hdevalence/ed25519consensus v0.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.1-0.20181226105442-5d4384ee4fb2 // indirect
	golang.org/x/sys v0.40.0 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

// The node has no published release that matches the Gray Paper revision this
// checkout tracks, so the sibling directory is authoritative. Go ignores a
// replace coming from a dependency, which means this only ever affects local
// builds and cannot pin a consumer to a path on this machine.
replace github.com/eigerco/strawberry => ../
