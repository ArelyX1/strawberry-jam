package pvm

import (
	"fmt"
)

// GuestProgram is a service blob prepared for execution: the A.38 framing the
// PVM runs, plus the two things that cannot be hardcoded because the linker
// owns them, namely where the entry point sits and which host call each ecall
// index stands for.
type GuestProgram struct {
	Blob        *PolkavmBlob
	Entry       uint64
	EcallToHost map[uint64]uint64
	StackSize   uint32
}

// defaultGuestStackSize is only a floor: the linker's own figure is used when
// the blob records one, since a guest doing curve arithmetic needs far more
// stack than the linker can infer.
const defaultGuestStackSize = 1 << 16

// ResolveHostCall is set by the host_call package at init, since it owns the
// canonical host call ids and importing it from here would be a cycle. It maps
// a guest import symbol to a host call id.
var ResolveHostCall func(symbol string) (uint64, bool)

// PrepareGuest parses a service blob, resolving its entry point and ecall
// mapping. entryName selects the export to start at; the default matches the
// single-export guest this chain runs.
func PrepareGuest(blob []byte, entryName string) (*GuestProgram, error) {
	parsed, err := ParsePolkavmBlob(blob)
	if err != nil {
		return nil, err
	}
	if entryName == "" {
		entryName = "main"
	}
	entry, err := parsed.EntryPoint(entryName)
	if err != nil {
		return nil, err
	}
	if ResolveHostCall == nil {
		return nil, fmt.Errorf("host call resolver not installed")
	}
	imports, err := parsed.Imports()
	if err != nil {
		return nil, err
	}
	mapping := make(map[uint64]uint64, len(imports))
	for i, symbol := range imports {
		id, ok := ResolveHostCall(symbol)
		if !ok {
			return nil, fmt.Errorf("import %q (%d) does not name a known host call", symbol, i)
		}
		mapping[uint64(i)] = id
	}
	stack := uint32(defaultGuestStackSize)
	if cfg, err := parsed.MemoryConfig(); err == nil && cfg != nil && cfg.StackSize > stack {
		stack = cfg.StackSize
	}
	return &GuestProgram{
		Blob:        parsed,
		Entry:       entry,
		EcallToHost: mapping,
		StackSize:   stack,
	}, nil
}

// Code returns the A.38 framing to hand to the PVM.
func (g *GuestProgram) Code() ([]byte, error) {
	return g.Blob.ToA38(0, g.StackSize)
}

// HostCallID translates an ecall index into the canonical host call id, so the
// chain's dispatcher can switch on the ids it already knows.
func (g *GuestProgram) HostCallID(ecall uint64) (uint64, bool) {
	id, ok := g.EcallToHost[ecall]
	return id, ok
}
