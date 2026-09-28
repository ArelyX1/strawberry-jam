package papucoin

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// SDLGPrefix is the human readable prefix of a chain-native address.
const SDLGPrefix = "sdlg"

// decodedAddressLen is the size of a decoded chain address: a 32 byte public
// key followed by a 4 byte checksum.
const decodedAddressLen = ed25519.PublicKeySize + 4

var (
	// ErrInvalidAddress is returned for an address that is neither a valid
	// chain address nor a valid EVM address.
	ErrInvalidAddress = errors.New("papucoin: invalid address")
	// ErrChecksum is returned when a chain address has a bad checksum, which
	// usually means a typo rather than an unsupported address.
	ErrChecksum = errors.New("papucoin: address checksum mismatch")
)

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// base58Decode decodes a base58 string into bytes. It rejects characters
// outside the alphabet so that a mistyped address fails rather than decoding to
// a different key.
func base58Decode(input string) ([]byte, error) {
	if input == "" {
		return nil, fmt.Errorf("%w: empty", ErrInvalidAddress)
	}

	number := new(big.Int)
	radix := big.NewInt(58)

	for _, r := range input {
		index := strings.IndexRune(base58Alphabet, r)
		if index < 0 {
			return nil, fmt.Errorf("%w: %q is not base58", ErrInvalidAddress, input)
		}
		number.Mul(number, radix)
		number.Add(number, big.NewInt(int64(index)))
	}

	decoded := number.Bytes()

	// Every leading '1' is a leading zero byte.
	leading := 0
	for leading < len(input) && input[leading] == base58Alphabet[0] {
		leading++
	}
	out := make([]byte, leading+len(decoded))
	copy(out[leading:], decoded)
	return out, nil
}

// NormalizeAddress validates an address and returns its canonical form. EVM
// addresses are lowercased; chain addresses keep their case because base58 is
// case sensitive. An empty result means the address was unusable.
func NormalizeAddress(input string) (string, error) {
	v := strings.TrimSpace(input)
	if v == "" {
		return "", fmt.Errorf("%w: empty", ErrInvalidAddress)
	}
	if IsEVMAddress(v) {
		return NormalizeEVMAddress(v), nil
	}
	if _, err := ValidateChainAddress(v); err != nil {
		return "", err
	}
	return v, nil
}

// ValidateChainAddress checks that input is a well formed chain address with a
// matching checksum, and returns its public key.
func ValidateChainAddress(input string) (ed25519.PublicKey, error) {
	if !strings.HasPrefix(input, SDLGPrefix) {
		return nil, fmt.Errorf("%w: missing %q prefix", ErrInvalidAddress, SDLGPrefix)
	}

	decoded, err := base58Decode(strings.TrimPrefix(input, SDLGPrefix))
	if err != nil {
		return nil, err
	}
	if len(decoded) != decodedAddressLen {
		return nil, fmt.Errorf("%w: decoded to %d bytes, want %d", ErrInvalidAddress, len(decoded), decodedAddressLen)
	}

	publicKey := ed25519.PublicKey(decoded[:ed25519.PublicKeySize])
	if !ValidChecksum(publicKey) {
		return nil, fmt.Errorf("%w: %s", ErrChecksum, input)
	}
	return publicKey, nil
}

// ValidChecksum reports whether a public key carries the correct address
// checksum, which is the first 4 bytes of its blake2b digest.
func ValidChecksum(publicKey []byte) bool {
	return bytes.Equal(AddressChecksum(publicKey), publicKey[ed25519.PublicKeySize:ed25519.PublicKeySize+4])
}

// AddressFromPublicKey renders a public key as a chain address. It is the
// inverse of [ValidateChainAddress], and the only way to learn which address a
// private key controls.
func AddressFromPublicKey(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: public key is %d bytes, want %d", ErrInvalidAddress, len(publicKey), ed25519.PublicKeySize)
	}
	decoded := make([]byte, 0, decodedAddressLen)
	decoded = append(decoded, publicKey...)
	decoded = append(decoded, AddressChecksum(publicKey)...)
	return SDLGPrefix + base58Encode(decoded), nil
}

// base58Encode renders bytes in base58 with the same alphabet [base58Decode]
// reads, including one leading '1' per leading zero byte.
func base58Encode(input []byte) string {
	if len(input) == 0 {
		return ""
	}

	leading := 0
	for leading < len(input) && input[leading] == 0 {
		leading++
	}

	number := new(big.Int).SetBytes(input)
	radix := big.NewInt(58)
	mod := new(big.Int)

	var encoded []byte
	for number.Sign() > 0 {
		number.DivMod(number, radix, mod)
		encoded = append(encoded, base58Alphabet[mod.Int64()])
	}
	for range leading {
		encoded = append(encoded, base58Alphabet[0])
	}
	for i, j := 0, len(encoded)-1; i < j; i, j = i+1, j-1 {
		encoded[i], encoded[j] = encoded[j], encoded[i]
	}
	return string(encoded)
}

// AddressChecksum returns the 4 byte checksum suffix for a public key.
func AddressChecksum(publicKey []byte) []byte {
	sum := blake2b.Sum256(publicKey)
	return sum[:4]
}

// IsEVMAddress reports whether input looks like a 20 byte hex address.
func IsEVMAddress(input string) bool {
	return strings.HasPrefix(input, "0x") && len(input) == 42 && isHex(input[2:])
}

// NormalizeEVMAddress lowercases an EVM address for use as a storage key.
func NormalizeEVMAddress(input string) string {
	return strings.ToLower(input)
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}
