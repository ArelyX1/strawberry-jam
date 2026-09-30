package papucoin

import (
	"fmt"
	"math/big"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
)

// View reads PAPU state out of a service account without running the service.
// It is how a node answers a balance query and how an explorer inspects state
// without a second implementation of the storage layout.
//
// A View is a value over a snapshot of the account: it reads no further than it
// was handed, so a caller that wants a consistent picture must take the account
// under a lock, as the runtime does.
type View struct {
	id      block.ServiceId
	account *service.ServiceAccount
}

// NewView returns a view over the PAPU storage of an account. id is the service
// the account belongs to, because service storage keys are namespaced by it.
func NewView(id block.ServiceId, account *service.ServiceAccount) *View {
	return &View{id: id, account: account}
}

// ServiceID is the service the view reads from.
func (v *View) ServiceID() block.ServiceId { return v.id }

// Raw returns the stored value for a raw service storage key, which is what a
// trie lookup needs.
func (v *View) Raw(key []byte) ([]byte, bool) {
	if v.account == nil {
		return nil, false
	}
	stateKey, err := statekey.NewStorage(v.id, key)
	if err != nil {
		return nil, false
	}
	return v.account.GetStorage(stateKey)
}

// Balance returns what an address holds, in raw units. An address the service
// has never seen holds nothing rather than being an error, because that is the
// state a wallet expects to read before it has ever been funded.
func (v *View) Balance(address string) (*big.Int, error) {
	normalized, err := NormalizeAddress(address)
	if err != nil {
		return nil, err
	}

	stored, ok := v.Raw(balanceKey(normalized))
	if !ok {
		return new(big.Int), nil
	}
	return decodeAmount(stored)
}

// Nonce returns the next nonce an address is expected to use. An address the
// service has never seen has not used any nonce yet, which the caller reads as
// zero and turns into the first nonce of the parameters.
func (v *View) Nonce(address string) (uint64, error) {
	normalized, err := NormalizeAddress(address)
	if err != nil {
		return 0, err
	}

	stored, ok := v.Raw(nonceKey(normalized))
	if !ok {
		return 0, nil
	}
	amount, err := decodeAmount(stored)
	if err != nil {
		return 0, err
	}
	if !amount.IsUint64() {
		return 0, fmt.Errorf("papucoin: nonce of %s does not fit in 64 bits", normalized)
	}
	return amount.Uint64(), nil
}

// FaucetClaimed reports whether an address has already taken its payout, which
// is a one-off per address. A caller that funds accounts uses this to answer
// before it queues anything: a second payout is refused by the service, and
// queueing it anyway would spend a nonce that the service is not going to
// advance, leaving the sender's next item out of step with the chain forever.
func (v *View) FaucetClaimed(address string) (bool, error) {
	normalized, err := NormalizeAddress(address)
	if err != nil {
		return false, err
	}
	stored, ok := v.Raw(claimKey(keyFaucet, normalized))
	if !ok {
		return false, nil
	}
	return len(stored) > 0, nil
}

// EVMNonce is the number of Ethereum transactions this chain has already
// accepted from an address, which is the number a wallet has to sign the next
// one with. It is not the PAPU nonce: the chain keeps that sequence of its own
// and assigns it, so a client that only speaks Ethereum never sees it.
func (v *View) EVMNonce(address string) (uint64, error) {
	normalized, err := NormalizeAddress(address)
	if err != nil {
		return 0, err
	}

	stored, ok := v.Raw(evmNonceKey(normalized))
	if !ok {
		return 0, nil
	}
	amount, err := decodeAmount(stored)
	if err != nil {
		return 0, err
	}
	if !amount.IsUint64() {
		return 0, fmt.Errorf("papucoin: EVM nonce of %s does not fit in 64 bits", normalized)
	}
	return amount.Uint64(), nil
}

// Supply is the amount in existence, which is the genesis supply plus whatever
// has been minted, less what the fees have burned.
func (v *View) Supply() (*big.Int, error) {
	stored, ok := v.Raw([]byte{keySupply})
	if !ok {
		return new(big.Int), nil
	}
	return decodeAmount(stored)
}

// Issuer is the only address the service lets mint.
func (v *View) Issuer() (string, error) {
	stored, ok := v.Raw([]byte{keyIssuer})
	if !ok {
		return "", ErrNoConfig
	}
	return string(stored), nil
}

// StorageFootprint is what the service's storage costs the account: the items
// and octets the protocol charges for. It is what an account balance has to
// stay above, so an operator can tell how much of a balance is rent.
func (v *View) StorageFootprint() (items uint32, octets uint64) {
	if v.account == nil {
		return 0, 0
	}
	return v.account.GetTotalNumberOfItems(), v.account.GetTotalNumberOfOctets()
}
