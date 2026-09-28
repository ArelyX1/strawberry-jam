package papucoin

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// signatureDomain binds a signature to this chain and to this version of the
// signable payload, so a signature can never be replayed on another network.
const signatureDomain = "SDLG-PAPU::1"

// signablePayload is the exact set of fields an item signature covers. It is
// serialised as a struct on purpose: encoding/json emits struct fields in
// declaration order, which keeps the digest stable for a given item shape.
type signablePayload struct {
	Method string `json:"method"`
	Nonce  uint64 `json:"nonce"`
	// Sender is covered so a signature for one account cannot be replayed as a
	// transfer from another, and so that the only way to send is to sign.
	Sender string `json:"sender"`
	// To and Amount are covered so the signed intent is the transferred value,
	// not merely the call.
	To     string `json:"to,omitempty"`
	Amount string `json:"amount,omitempty"`
	Memo   string `json:"memo,omitempty"`
	// MustBeSigned is covered because clearing the flag after signing would
	// turn a proven item into an item nobody proved.
	MustBeSigned bool `json:"mustBeSigned"`
}

// SignablePayload returns the bytes an item signature is computed over. Callers
// that build an item by hand, as the HTTP bridge does, sign exactly this.
func SignablePayload(item Item) []byte {
	payload := signablePayload{
		Method:       item.Method,
		Nonce:        item.Nonce,
		Sender:       item.Sender,
		To:           item.To,
		Amount:       item.Amount,
		Memo:         item.Memo,
		MustBeSigned: item.MustBeSigned,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		// signablePayload holds only strings, a uint64 and a bool, so
		// marshalling it cannot fail.
		panic(fmt.Sprintf("papucoin: signable payload is not marshalable: %v", err))
	}
	return append([]byte(signatureDomain), encoded...)
}

// SignItem returns a copy of item carrying a signature made by privateKey. The
// sender is derived from the key, so signing as somebody else is impossible
// without also rewriting the item.
func SignItem(item Item, privateKey ed25519.PrivateKey) (Item, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return item, fmt.Errorf("papucoin: private key is %d bytes, want %d", len(privateKey), ed25519.PrivateKeySize)
	}

	address, err := AddressFromPublicKey(privateKey.Public().(ed25519.PublicKey))
	if err != nil {
		return item, err
	}

	signed := item
	signed.Sender = address
	// The signature covers the item as it is about to be sent, sender included,
	// so it has to be computed after the sender is filled in.
	signature := ed25519.Sign(privateKey, SignablePayload(signed))
	signed.Signature = "0x" + hex.EncodeToString(signature)
	return signed, nil
}

// VerifyItemSignature checks that item carries a signature made by the key behind
// item.Sender, and returns that key. A missing, malformed, foreign or tampered
// signature is reported as an error rather than silently accepted, because a
// sender that cannot be proven is a sender whose items must not touch balances.
func VerifyItemSignature(item Item) (ed25519.PublicKey, error) {
	publicKey, err := ValidateChainAddress(item.Sender)
	if err != nil {
		return nil, fmt.Errorf("papucoin: cannot verify item from %q: %w", item.Sender, err)
	}

	raw := strings.TrimPrefix(strings.TrimSpace(item.Signature), "0x")
	signature, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("papucoin: item from %s has a malformed signature: %w", item.Sender, err)
	}
	if !ed25519.Verify(publicKey, SignablePayload(item), signature) {
		return nil, fmt.Errorf("papucoin: item from %s is not signed by %s", item.Sender, item.Sender)
	}
	return publicKey, nil
}
