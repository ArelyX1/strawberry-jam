package pebble

import (
	"errors"
	"fmt"
	"github.com/cockroachdb/pebble"
	"github.com/cockroachdb/pebble/vfs"
	"sync/atomic"
)

type KVStore struct {
	db     *pebble.DB
	closed atomic.Bool
}

// NewKVStore initializes a new in-memory key-value store using Pebble.
//
// Nothing written to it survives the process, which is what the conformance
// tests and the throwaway dev node want. A node that has to keep its blocks
// and its state root across restarts has to use [NewKVStoreAt] instead.
func NewKVStore() (*KVStore, error) {
	return openStore("", vfs.NewMem())
}

// NewKVStoreAt initializes a key-value store backed by a directory on disk, so
// blocks, headers and the state trie outlive the process. An empty path falls
// back to the in-memory store, which keeps every existing caller working.
func NewKVStoreAt(path string) (*KVStore, error) {
	if path == "" {
		return NewKVStore()
	}
	return openStore(path, vfs.Default)
}

func openStore(path string, fs vfs.FS) (*KVStore, error) {
	opts := &pebble.Options{
		FS:                          fs,
		Cache:                       pebble.NewCache(64 * 1024 * 1024),
		MemTableSize:                32 * 1024 * 1024,
		MemTableStopWritesThreshold: 4,
	}

	db, err := pebble.Open(path, opts)
	if err != nil {
		return nil, err
	}

	return &KVStore{db: db}, nil
}

func (p *KVStore) Get(key []byte) ([]byte, error) {
	if p.closed.Load() {
		return nil, ErrClosed
	}

	value, closer, err := p.db.Get(key)
	if err != nil {
		if errors.Is(err, pebble.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("pebble get failed: %w", err)
	}

	// Make a copy of the value since it's only valid while closer is open
	result := make([]byte, len(value))
	copy(result, value)

	if err := closer.Close(); err != nil {
		return result, nil
	}

	return result, nil
}

func (p *KVStore) Put(key, value []byte) error {
	if p.closed.Load() {
		return ErrClosed
	}
	return p.db.Set(key, value, pebble.Sync)
}

func (p *KVStore) Delete(key []byte) error {
	if p.closed.Load() {
		return ErrClosed
	}
	return p.db.Delete(key, pebble.Sync)
}

func (p *KVStore) Close() error {
	if !p.closed.CompareAndSwap(false, true) {
		return nil
	}
	return p.db.Close()
}
