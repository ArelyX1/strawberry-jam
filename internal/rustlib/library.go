// Package rustlib writes the embedded Rust libraries the node loads out of the
// binary and onto disk, once per machine rather than once per process, and binds
// their exported functions to Go variables.
package rustlib

// A Library is a Rust shared library loaded into the process, and Bind attaches
// one of its exported functions to a Go variable.
//
// The indirection exists because the platforms do not agree on how a shared
// library is loaded. Unix has dlopen, with flags, and a single handle from which
// every symbol is resolved by name. Windows has LoadLibrary followed by
// GetProcAddress for each name, and purego does not implement dlopen there at
// all. Nothing else about the two differs, so nothing else is duplicated: the
// calling packages declare their function signatures in plain Go and never
// mention which platform they are on.
//
// Both implementations force the load in Open rather than deferring it to the
// first call. A library whose own dependency is missing then fails here, naming
// that dependency, instead of failing much later inside a signature
// verification with nothing to explain it.

// Open loads the Rust shared library at path.
func Open(path string) (Library, error) { return open(path) }

// Bind attaches the exported function called name to the Go variable fptr,
// which must be a pointer to a variable of a func type matching the Rust
// signature exactly.
//
// It stops the process if the function is not exported. That means the library
// on disk is not the one this binary was built against, and the alternative is
// calling an unregistered variable, which is a jump into nothing rather than an
// error anybody could report.
func (l Library) Bind(fptr any, name string) { l.bind(fptr, name) }
