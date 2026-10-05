// Package rustlib writes the embedded Rust libraries the node loads out of the
// binary and onto disk, once per machine rather than once per process.
package rustlib

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// WriteOnce writes an embedded Rust library to disk and returns the path to load
// it from, reusing the file every process on the machine asks for.
//
// It used to be a fresh temporary directory per process, and nothing ever removed
// it. The library is a couple of megabytes, so a node that was started and stopped
// a few dozen times left a few dozen megabytes behind in /tmp, and on a machine
// where /tmp is a small tmpfs that is how a net of nodes fills it and every node
// dies in the middle of a block with "no space left on device". A net spread over
// several machines pays that once per machine instead of once per node, and once
// per node per restart rather than growing without end.
//
// The name carries the hash of the bytes, so a binary with a different library in
// it gets a different file and two versions can never load each other's. It is
// written under a temporary name and renamed into place, because two nodes
// starting at the same moment must not find a half written library under the
// final name.
func WriteOnce(prefix, name string, contents []byte) (string, error) {
	sum := sha256.Sum256(contents)
	dir := filepath.Join(os.TempDir(), prefix+"-"+hex.EncodeToString(sum[:8]))
	libPath := filepath.Join(dir, name)

	// Somebody else may have just finished writing the very same file, or may be
	// writing it now. One of the right size that can be opened is the one to use.
	if st, err := os.Stat(libPath); err == nil && st.Size() == int64(len(contents)) {
		return libPath, nil
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	tmp, err := os.CreateTemp(dir, name+".*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(contents); err != nil {
		tmp.Close()        //nolint:errcheck // the write error is the one that matters
		os.Remove(tmpName) //nolint:errcheck // best effort
		return "", err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName) //nolint:errcheck // best effort
		return "", err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil { //nolint:gosec // a library that has to be loadable, not a secret
		os.Remove(tmpName) //nolint:errcheck // best effort
		return "", err
	}
	if err := os.Rename(tmpName, libPath); err != nil {
		// Losing the race against the other process is not a failure: what it just
		// put there is this same file.
		if st, statErr := os.Stat(libPath); statErr == nil && st.Size() == int64(len(contents)) {
			os.Remove(tmpName) //nolint:errcheck // best effort
			return libPath, nil
		}
		os.Remove(tmpName) //nolint:errcheck // best effort
		return "", err
	}
	return libPath, nil
}
