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
	// Framed records that the blob was a bare A.38 program with no import
	// table, so that an ecall index is the host call id rather than a position
	// in a list this program does not have.
	Framed bool
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
//
// A blob arrives in one of two shapes. The one the chain's own linker emits is
// the native polkavm container, which names its exports and its imports, so the
// entry point and the ecall table can be read out of it. The other is the plain
// A.38 framing, which names nothing: the entry sits at the start and the ecall
// index is the host call id. The conformance vectors use that shape, and a blob
// that is a perfectly good program was being refused because it was not wearing
// a container.
func PrepareGuest(blob []byte, entryName string) (*GuestProgram, error) {
	if parsed, err := ParsePolkavmBlob(blob); err == nil {
		return prepareFromContainer(parsed, entryName)
	}
	// Not a container, so it has to be the framing itself. ParseBlob is the one
	// that reads it, and its error is the one worth reporting when neither
	// shape fits, because "not a container" says nothing about a program.
	framed, err := ParseBlob(blob)
	if err != nil {
		return nil, fmt.Errorf("service code is neither a polkavm container nor A.38 framed: %w", err)
	}
	return prepareFromFraming(framed), nil
}

// prepareFromFraming prepares a program that carries no export table and no
// import table. There is nothing to look anything up in, so the entry point is
// the start of the code and an ecall index is taken to be the host call it
// names.
func prepareFromFraming(framed *ProgramBlob) *GuestProgram {
	stack := uint32(defaultGuestStackSize)
	if framed.ProgramMemorySizes.StackSize > stack {
		stack = framed.ProgramMemorySizes.StackSize
	}
	return &GuestProgram{
		Blob:      framingToBlob(framed),
		Entry:     0,
		Framed:    true,
		StackSize: stack,
	}
}

// framingToBlob lifts a parsed A.38 program into the shape ToA38 reads, so both
// container blobs and bare framings take the same path from there on.
func framingToBlob(framed *ProgramBlob) *PolkavmBlob {
	return &PolkavmBlob{Sections: map[byte][]byte{
		SectionROData: framed.ROData,
		SectionRWData: framed.RWData,
		SectionCode:   framed.CodeAndJumpTable,
	}}
}

func prepareFromContainer(parsed *PolkavmBlob, entryName string) (*GuestProgram, error) {
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
// chain's dispatcher can switch on the ids it already knows. A program with no
// import table has nothing to translate: the index is the id.
func (g *GuestProgram) HostCallID(ecall uint64) (uint64, bool) {
	if g.Framed {
		return ecall, true
	}
	id, ok := g.EcallToHost[ecall]
	return id, ok
}
