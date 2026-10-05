package devnet

import (
	"crypto/ed25519"
	"fmt"
	"net"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/safrole"
	"github.com/eigerco/strawberry/internal/validator"
)

// Validator is one validator of a dev chain: an Ed25519 key and the address other
// validators reach it at.
type Validator struct {
	Ed25519 ed25519.PublicKey
	// Metadata is the validator declaration, an 18 byte IP and port followed by
	// padding, exactly as the JAM protocol defines it.
	Metadata crypto.MetadataKey
}

// DevValidatorKey returns the deterministic key pair of validator number index,
// both halves, so that something writing a shared validator file has to derive
// exactly the key the node will later run with. Exposing the private half is
// what DevValidatorKeys, which only publishes public keys, cannot do.
func DevValidatorKey(index int) (ed25519.PrivateKey, ed25519.PublicKey, error) {
	seed := make([]byte, ed25519.SeedSize)
	// Every byte is the index, which keeps the keys obviously synthetic and
	// trivially reproducible. DevValidatorKeys derives them the same way, and
	// the two have to agree or a written file would not match the node reading it.
	for i := range seed {
		seed[i] = byte(index)
	}
	key := ed25519.NewKeyFromSeed(seed)
	publicKey, ok := key.Public().(ed25519.PublicKey)
	if !ok {
		return nil, nil, fmt.Errorf("devnet: ed25519 public key has an unexpected type")
	}
	return key, publicKey, nil
}

// ValidatorMetadata builds the declaration of a validator listening on addr.
func ValidatorMetadata(addr *net.UDPAddr) crypto.MetadataKey {
	var metadata crypto.MetadataKey
	// The declaration is an IPv6 address and a port, and the dev chain only ever
	// listens on IPv6 wildcard addresses.
	copy(metadata[:], addr.IP.To16())
	// Ports are written little endian, the same way the protocol reads them.
	metadata[16] = byte(addr.Port)
	metadata[17] = byte(addr.Port >> 8)
	return metadata
}

// ParseValidatorAddr turns a "ip:port" string into a declaration.
func ParseValidatorAddr(listen string) (crypto.MetadataKey, error) {
	addr, err := net.ResolveUDPAddr("udp", listen)
	if err != nil {
		return crypto.MetadataKey{}, fmt.Errorf("devnet: %q is not an address: %w", listen, err)
	}
	return ValidatorMetadata(addr), nil
}

// ValidatorState builds the validator state of a dev chain.
//
// The dev chain has no Safrole, but the state still has to serialize, because a
// state root is computed over the whole state. That means the sealing key series
// cannot be left empty: it is a union, and an empty union is not a value the
// codec can encode. It is filled with the validators' bandersnatch keys, which
// is what the series would hold if the chain ran tickets.
func ValidatorState(validators []Validator) validator.ValidatorState {
	data := safrole.ValidatorsData{}
	for index := range data {
		if index < len(validators) {
			data[index].Ed25519 = validators[index].Ed25519
			data[index].Metadata = validators[index].Metadata
		}
	}

	series := crypto.EpochKeys{}
	for slot := range series {
		if len(validators) == 0 {
			continue
		}
		series[slot] = data[slot%len(validators)].Bandersnatch
	}

	sealingKeys := safrole.SealingKeys{}
	sealingKeys.Set(series)

	return validator.ValidatorState{
		CurrentValidators:  data,
		ArchivedValidators: data,
		QueuedValidators:   data,
		SafroleState: safrole.State{
			NextValidators:    data,
			SealingKeySeries:  sealingKeys,
			TicketAccumulator: []block.Ticket{},
		},
	}
}

// defaultP2PPort is the first port of the JAM peer to peer range, used only to
// fill in the declaration of a validator that was not given an address.
const defaultP2PPort = 30333

// DevValidatorKeys returns the deterministic validator keys a dev chain runs
// with, so two nodes started with the same validator index agree on the state.
func DevValidatorKeys(count int, listenAddrs []string) ([]Validator, error) {
	validators := make([]Validator, count)
	for index := range validators {
		_, publicKey, err := DevValidatorKey(index)
		if err != nil {
			return nil, err
		}

		metadata := crypto.MetadataKey{}
		if index < len(listenAddrs) {
			parsed, err := ParseValidatorAddr(listenAddrs[index])
			if err != nil {
				return nil, err
			}
			metadata = parsed
		} else {
			// The default is the port a dev validator listens on, so the state
			// still names somewhere reachable.
			metadata = ValidatorMetadata(&net.UDPAddr{IP: net.IPv6loopback, Port: defaultP2PPort + index})
		}

		validators[index] = Validator{Ed25519: publicKey, Metadata: metadata}
	}
	return validators, nil
}
