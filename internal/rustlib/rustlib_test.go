package rustlib

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// Ten nodes on a machine used to mean ten copies of the same library in ten
// temporary directories that nothing ever removed, and a node that was started and
// stopped a few dozen times filled /tmp. On a machine where /tmp is a small tmpfs
// that is how a net of nodes fills it and every node dies in the middle of a block
// with "no space left on device", which is exactly what happened with eight nodes.
//
// What matters is not only that one copy is reused, but that it is reused when
// several processes ask for it at once, since that is how nodes start.
func TestOneCopyIsSharedAndNothingIsLeftBehind(t *testing.T) {
	before := libraryDirs(t, "strawberry-testlib")

	contenido := bytes.Repeat([]byte("libreria"), 512)

	primero, err := WriteOnce("strawberry-testlib", "lib.so", contenido)
	if err != nil {
		t.Fatalf("first write: %v", err)
	}

	// The same contents, asked for many times over, is one file and one directory.
	rutas := []string{primero}
	for i := 0; i < 9; i++ {
		p, err := WriteOnce("strawberry-testlib", "lib.so", contenido)
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		rutas = append(rutas, p)
	}
	for i, p := range rutas {
		if p != primero {
			t.Errorf("la llamada %d devolvio %s y la primera devolvio %s: son copias distintas", i, p, primero)
		}
	}
	if got := len(libraryDirs(t, "strawberry-testlib")) - len(before); got != 1 {
		t.Errorf("han quedado %d directorios para una sola libreria, y tiene que ser uno", got)
	}

	// And what is on disk is the library, loadable and complete.
	leido, err := os.ReadFile(primero) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(leido, contenido) {
		t.Errorf("la libreria en disco no es la que se pidio: %d bytes en vez de %d", len(leido), len(contenido))
	}

	t.Cleanup(func() { os.RemoveAll(filepath.Dir(primero)) })
}

// Several nodes starting at the same moment must not find a half written library,
// or load a library that is still being filled.
func TestConcurrentWritersDoNotLeaveAHalfWrittenLibrary(t *testing.T) {
	contenido := bytes.Repeat([]byte("x"), 1<<16)

	var wg sync.WaitGroup
	rutas := make([]string, 12)
	errs := make([]error, 12)
	for i := range rutas {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rutas[i], errs[i] = WriteOnce("strawberry-race", "lib.so", contenido)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("la escritura %d fallo: %v", i, err)
		}
		if rutas[i] != rutas[0] {
			t.Errorf("la escritura %d devolvio otra ruta: %s y %s", i, rutas[i], rutas[0])
		}
	}
	leido, err := os.ReadFile(rutas[0]) //nolint:gosec // a path this test just wrote
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !bytes.Equal(leido, contenido) {
		t.Errorf("lo que quedo en disco son %d bytes en vez de %d", len(leido), len(contenido))
	}
	t.Cleanup(func() { os.RemoveAll(filepath.Dir(rutas[0])) })
}

// A binary carrying a different library must not read the one a previous version
// left behind: the name carries the hash of the bytes.
func TestDifferentContentsDoNotShareAFile(t *testing.T) {
	a, err := WriteOnce("strawberry-diff", "lib.so", []byte("version uno"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := WriteOnce("strawberry-diff", "lib.so", []byte("version dos"))
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatalf("dos librerias distintas han acabado en el mismo sitio: %s", a)
	}
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(a)) //nolint:errcheck // best effort
		os.RemoveAll(filepath.Dir(b)) //nolint:errcheck // best effort
	})
}

func libraryDirs(t *testing.T, prefix string) []string {
	t.Helper()
	entradas, err := os.ReadDir(os.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var fuera []string
	for _, e := range entradas {
		if e.IsDir() && len(e.Name()) > len(prefix) && e.Name()[:len(prefix)] == prefix {
			fuera = append(fuera, e.Name())
		}
	}
	return fuera
}
