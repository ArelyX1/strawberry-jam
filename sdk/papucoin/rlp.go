package papucoin

import (
	"errors"
	"math/big"
)

// This file implements only the slice of RLP that a relayed transaction needs.
// The important property is that a decoded item keeps the exact bytes it arrived
// as, so re-encoding a list is a concatenation of those bytes. A signing hash is
// therefore always computed over precisely what the signer signed, and a
// non-canonical encoding can never be quietly normalised into a different
// transaction that verifies against the same signature.

var (
	errRLPTruncated = errors.New("papucoin: truncated rlp")
	errRLPTrailing  = errors.New("papucoin: trailing bytes after rlp item")
	errRLPCanon     = errors.New("papucoin: non-canonical rlp encoding")
	errRLPNotList   = errors.New("papucoin: expected an rlp list")
	errRLPNotString = errors.New("papucoin: expected an rlp byte string")
)

// rlpItem is one decoded RLP item: either a byte string or a list.
type rlpItem struct {
	// raw is the item exactly as it appeared on the wire. Re-encoding uses it
	// verbatim rather than reconstructing it.
	raw    []byte
	isList bool
	str    []byte
	list   []rlpItem
}

// rlpUint decodes a byte string that holds a canonical big-endian integer.
func (i rlpItem) rlpUint() (*big.Int, error) {
	if i.isList {
		return nil, errRLPNotString
	}
	if len(i.str) > 0 && i.str[0] == 0 {
		// A leading zero would make the same number encodable two ways, which
		// is exactly the ambiguity a signing hash must not have.
		return nil, errRLPCanon
	}
	return new(big.Int).SetBytes(i.str), nil
}

// rlpList decodes a list of exactly n items.
func (i rlpItem) rlpList(n int) ([]rlpItem, error) {
	if !i.isList {
		return nil, errRLPNotList
	}
	if len(i.list) != n {
		return nil, errors.New("papucoin: wrong number of fields in rlp list")
	}
	return i.list, nil
}

// rlpDecode decodes a single item and rejects trailing bytes.
func rlpDecode(b []byte) (rlpItem, error) {
	item, rest, err := rlpDecodeItem(b)
	if err != nil {
		return rlpItem{}, err
	}
	if len(rest) != 0 {
		return rlpItem{}, errRLPTrailing
	}
	return item, nil
}

func rlpDecodeItem(b []byte) (rlpItem, []byte, error) {
	if len(b) == 0 {
		return rlpItem{}, nil, errRLPTruncated
	}
	prefix := b[0]
	switch {
	case prefix <= 0x7f:
		return rlpItem{raw: b[:1], str: b[:1]}, b[1:], nil

	case prefix <= 0xb7:
		size := int(prefix - 0x80)
		if len(b) < 1+size {
			return rlpItem{}, nil, errRLPTruncated
		}
		// A single byte below 0x80 has a shorter form, so the long form would be
		// a second spelling of the same string. A string that means an integer
		// must additionally have no leading zeros, but that is a property of the
		// field rather than of the encoding: a 32 byte signature component keeps
		// its width even when the top byte is zero.
		if size == 1 && b[1] <= 0x7f {
			return rlpItem{}, nil, errRLPCanon
		}
		return rlpItem{raw: b[:1+size], str: b[1 : 1+size]}, b[1+size:], nil

	case prefix <= 0xbf:
		size, rest, err := rlpReadSize(b, 1, int(prefix-0xb7))
		if err != nil {
			return rlpItem{}, nil, err
		}
		// The long form exists for payloads of 56 bytes or more, so reaching for
		// it below that threshold is a redundant encoding.
		if size < 56 {
			return rlpItem{}, nil, errRLPCanon
		}
		if len(rest) < size {
			return rlpItem{}, nil, errRLPTruncated
		}
		return rlpItem{raw: b[:len(b)-len(rest)+size], str: rest[:size]}, rest[size:], nil

	case prefix <= 0xf7:
		size := int(prefix - 0xc0)
		if len(b) < 1+size {
			return rlpItem{}, nil, errRLPTruncated
		}
		list, err := rlpDecodeList(b[1 : 1+size])
		if err != nil {
			return rlpItem{}, nil, err
		}
		return rlpItem{raw: b[:1+size], isList: true, list: list}, b[1+size:], nil

	default:
		size, rest, err := rlpReadSize(b, 1, int(prefix-0xf7))
		if err != nil {
			return rlpItem{}, nil, err
		}
		if size < 56 {
			return rlpItem{}, nil, errRLPCanon
		}
		if len(rest) < size {
			return rlpItem{}, nil, errRLPTruncated
		}
		list, err := rlpDecodeList(rest[:size])
		if err != nil {
			return rlpItem{}, nil, err
		}
		return rlpItem{raw: b[:len(b)-len(rest)+size], isList: true, list: list}, rest[size:], nil
	}
}

// rlpReadSize reads a big-endian length of lenOfLen bytes starting at b[from],
// and returns the payload size along with the bytes that follow the length.
func rlpReadSize(b []byte, from, lenOfLen int) (int, []byte, error) {
	if len(b) < from+lenOfLen {
		return 0, nil, errRLPTruncated
	}
	if b[from] == 0 {
		// A length must not be padded, or the same payload has several
		// encodings.
		return 0, nil, errRLPCanon
	}
	size := 0
	for _, c := range b[from : from+lenOfLen] {
		size = size<<8 | int(c)
	}
	return size, b[from+lenOfLen:], nil
}

func rlpDecodeList(payload []byte) ([]rlpItem, error) {
	var out []rlpItem
	for len(payload) > 0 {
		item, rest, err := rlpDecodeItem(payload)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		payload = rest
	}
	return out, nil
}

// rlpConcat returns the encoding of a list, reusing the exact encoding of each
// member. EIP-155 defines the signing payload as a list that is the original
// field list with three scalars appended, so the members must not be rebuilt.
func rlpConcat(items ...[]byte) []byte {
	size := 0
	for _, item := range items {
		size += len(item)
	}
	out := make([]byte, 0, rlpListHeaderSize(size)+size)
	out = append(out, rlpListHeader(size)...)
	for _, item := range items {
		out = append(out, item...)
	}
	return out
}

// rlpEncodeUint encodes an integer the way EIP-155 requires its three appended
// scalars to be encoded: minimal big-endian, with zero as the empty string. A
// nil value is the empty placeholder EIP-155 appends, so it encodes as zero.
func rlpEncodeUint(v *big.Int) []byte {
	if v == nil || v.Sign() == 0 {
		return []byte{0x80}
	}
	be := v.Bytes()
	return append([]byte{0x80 + byte(len(be))}, be...)
}

func rlpListHeaderSize(payload int) int {
	if payload <= 55 {
		return 1
	}
	return 1 + (payload+255)/256
}

func rlpListHeader(payload int) []byte {
	if payload <= 55 {
		return []byte{0xc0 + byte(payload)}
	}
	n := rlpListHeaderSize(payload) - 1
	header := make([]byte, 0, n+1)
	header = append(header, 0xf7+byte(n))
	for shift := (n - 1) * 8; shift >= 0; shift -= 8 {
		header = append(header, byte(payload>>shift))
	}
	return header
}
