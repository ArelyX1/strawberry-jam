package pvm

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

// PolkavmBlob is a blob in the native polkavm container format (magic "PVM\0"),
// which is what `polkatool link` emits. It is a sequence of identified
// sections terminated by a zero id, each with a JAM compact-encoded length.
type PolkavmBlob struct {
	Sections map[byte][]byte
}

// Section ids used by the linker for the pieces the PVM needs.
const (
	SectionROData  = 2
	SectionRWData  = 3
	SectionImports = 4
	SectionExports = 5
	SectionCode    = 6
)

// readCompact decodes a JAM compact unsigned integer, returning the value and
// the number of bytes consumed.
func readCompact(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("compact: unexpected end of input")
	}
	first := b[0]
	extra := 0
	for x := first; x&0x80 != 0; x <<= 1 {
		extra++
	}
	if extra+1 > len(b) {
		return 0, 0, fmt.Errorf("compact: truncated multi-byte value")
	}
	var v uint64
	for i := 0; i < extra; i++ {
		v |= uint64(b[i+1]) << (8 * i)
	}
	v |= uint64(first&(0xff>>extra)) << (8 * extra)
	return v, extra + 1, nil
}

// ParsePolkavmBlob reads the native polkavm container into its sections.
func ParsePolkavmBlob(data []byte) (*PolkavmBlob, error) {
	if len(data) < 13 || string(data[:4]) != "PVM\x00" {
		return nil, fmt.Errorf("not a polkavm blob")
	}
	blob := &PolkavmBlob{Sections: map[byte][]byte{}}
	p := 13
	for p < len(data) {
		id := data[p]
		p++
		if id == 0 {
			return blob, nil
		}
		length, n, err := readCompact(data[p:])
		if err != nil {
			return nil, fmt.Errorf("section %d: %w", id, err)
		}
		p += n
		end := p + int(length)
		if end > len(data) {
			return nil, fmt.Errorf("section %d: length %d overruns blob", id, length)
		}
		blob.Sections[id] = data[p:end]
		p = end
	}
	return blob, fmt.Errorf("polkavm blob: missing section terminator")
}

// readVarint decodes the little-endian varint polkavm uses for export tables.
// The length is the number of leading one bits in the first byte, and the
// first byte's remaining bits carry the value's high bits.
func readVarint(b []byte) (uint64, int, error) {
	if len(b) == 0 {
		return 0, 0, fmt.Errorf("varint: empty")
	}
	first := b[0]
	length := bits.LeadingZeros8(^first)
	if length > 4 {
		return 0, 0, fmt.Errorf("varint: invalid first byte %#x", first)
	}
	if len(b) < length+1 {
		return 0, 0, fmt.Errorf("varint: truncated")
	}
	upperMask := uint8(0xff >> length)
	upperBits := uint64(upperMask&first) << (length * 8)
	tail := b[1 : length+1]

	var value uint64
	switch length {
	case 0:
		value = upperBits
	case 1:
		value = upperBits | uint64(tail[0])
	case 2:
		value = upperBits | uint64(binary.LittleEndian.Uint16(tail))
	case 3:
		value = upperBits | uint64(tail[0]) | uint64(tail[1])<<8 | uint64(tail[2])<<16
	case 4:
		value = upperBits | uint64(binary.LittleEndian.Uint32(tail))
	}

	if (length != 0 && value < uint64(1)<<(7*length)) || value > math.MaxUint32 {
		return 0, 0, fmt.Errorf("varint: non-canonical encoding %d", value)
	}
	return value, length + 1, nil
}

// Exports parses the export section, returning the named entry points keyed by
// name. The section is a varint count followed by that many (offset, symbol)
// pairs, where the symbol carries its own length prefix.
func (b *PolkavmBlob) Exports() (map[string]uint64, error) {
	out := map[string]uint64{}
	section, ok := b.Sections[SectionExports]
	if !ok {
		return out, nil
	}
	count, n, err := readVarint(section)
	if err != nil {
		return nil, fmt.Errorf("export count: %w", err)
	}
	p := n
	for i := uint64(0); i < count; i++ {
		offset, used, err := readVarint(section[p:])
		if err != nil {
			return nil, fmt.Errorf("export %d offset: %w", i, err)
		}
		p += used
		nameLen, used, err := readVarint(section[p:])
		if err != nil {
			return nil, fmt.Errorf("export %d name length: %w", i, err)
		}
		p += used
		if p+int(nameLen) > len(section) {
			return nil, fmt.Errorf("export %d name overruns section", i)
		}
		out[string(section[p:p+int(nameLen)])] = offset
		p += int(nameLen)
	}
	return out, nil
}

// EntryPoint returns the code offset of the named export. The chain's entry
// numbers must come from here rather than being hardcoded, because the linker
// does not promise any particular block layout: the entry the host jumps to is
// whatever offset the export table records.
func (b *PolkavmBlob) EntryPoint(name string) (uint64, error) {
	exports, err := b.Exports()
	if err != nil {
		return 0, err
	}
	offset, ok := exports[name]
	if !ok {
		return 0, fmt.Errorf("no export named %q (have %v)", name, exportNames(exports))
	}
	return offset, nil
}

func exportNames(exports map[string]uint64) []string {
	names := make([]string, 0, len(exports))
	for name := range exports {
		names = append(names, name)
	}
	return names
}

// ToA38 converts the native container into the A.38 form the PVM executes:
// E3(|o|) ⌢ E3(|w|) ⌢ E2(z) ⌢ E3(s) ⌢ o ⌢ w ⌢ E4(|c|) ⌢ c (eq. A.38 v0.7.2).
//
// The code section in the native container is the already-assembled blob body,
// so it is passed through untouched; only the framing is rebuilt.
func (b *PolkavmBlob) ToA38(initialHeapPages uint16, stackSize uint32) ([]byte, error) {
	roData := b.Sections[SectionROData]
	rwData := b.Sections[SectionRWData]
	code := b.Sections[SectionCode]
	if code == nil {
		return nil, fmt.Errorf("polkavm blob: no code section")
	}

	var out []byte
	appendUint := func(v uint64, bytes int) {
		for i := 0; i < bytes; i++ {
			out = append(out, byte(v>>(8*i)))
		}
	}
	appendUint(uint64(len(roData)), 3)
	appendUint(uint64(len(rwData)), 3)
	appendUint(uint64(initialHeapPages), 2)
	appendUint(uint64(stackSize), 3)
	out = append(out, roData...)
	out = append(out, rwData...)
	appendUint(uint64(len(code)), 4)
	out = append(out, code...)
	return out, nil
}

// Imports returns the host-call symbol for each ecall index. The linker
// assigns these indices by the order the guest first calls them, not
// alphabetically, so a guest cannot assume an index from its own naming; the
// mapping has to be read back from the blob.
func (b *PolkavmBlob) Imports() ([]string, error) {
	section, ok := b.Sections[SectionImports]
	if !ok {
		return nil, nil
	}
	count, n, err := readVarint(section)
	if err != nil {
		return nil, fmt.Errorf("import count: %w", err)
	}
	p := n
	if p+int(count)*4 > len(section) {
		return nil, fmt.Errorf("import offsets overrun section")
	}
	offsets := make([]uint32, count)
	for i := range offsets {
		offsets[i] = binary.LittleEndian.Uint32(section[p:])
		p += 4
	}
	symbols := section[p:]
	out := make([]string, count)
	for i := 0; i < int(count); i++ {
		start := int(offsets[i])
		end := len(symbols)
		if i+1 < int(count) {
			end = int(offsets[i+1])
		}
		if start > end || end > len(symbols) {
			return nil, fmt.Errorf("import %d span %d..%d out of range", i, start, end)
		}
		out[i] = string(symbols[start:end])
	}
	return out, nil
}
