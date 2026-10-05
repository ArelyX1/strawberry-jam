//go:build linux || darwin

package rustlib

import "github.com/ebitengine/purego"

// Library is a loaded shared library on Unix, where one handle covers every
// symbol in it.
type Library struct {
	handle uintptr
}

func open(path string) (Library, error) {
	// RTLD_NOW so that a symbol missing from the library is reported now, by
	// Bind, and not on the first block that needs it. RTLD_GLOBAL so that two
	// copies of the same Rust library, bandersnatch and erasure coding, share
	// one instance of the Rust runtime between them instead of each carrying
	// its own and doubling the memory a node uses.
	handle, err := purego.Dlopen(path, purego.RTLD_NOW|purego.RTLD_GLOBAL)
	if err != nil {
		return Library{}, err
	}
	return Library{handle: handle}, nil
}

func (l Library) bind(fptr any, name string) {
	purego.RegisterLibFunc(fptr, l.handle, name)
}
