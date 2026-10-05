//go:build windows

package rustlib

import (
	"fmt"
	"syscall"

	"github.com/ebitengine/purego"
)

// Library is a loaded DLL on Windows, where the handle comes from LoadLibrary
// and each symbol is resolved separately by name.
type Library struct {
	dll *syscall.LazyDLL
}

func open(path string) (Library, error) {
	dll := syscall.NewLazyDLL(path)

	// LazyDLL does not actually load anything until something is called through
	// it, and a DLL whose own dependencies are missing is a common failure on
	// Windows. Forcing the load here means that failure arrives as an error
	// naming the missing dependency, at start up, instead of much later as a
	// signature that does not verify for no visible reason.
	if err := dll.Load(); err != nil {
		return Library{}, fmt.Errorf("no se pudo cargar %s: %w", path, err)
	}
	return Library{dll: dll}, nil
}

func (l Library) bind(fptr any, name string) {
	proc := l.dll.NewProc(name)
	if err := proc.Find(); err != nil {
		panic(fmt.Errorf("la libreria de Rust no exporta %s: %w", name, err))
	}
	purego.RegisterFunc(fptr, proc.Addr())
}
