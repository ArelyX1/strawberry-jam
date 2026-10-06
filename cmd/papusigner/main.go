package main

// A PAPU signing helper, built against the node's own SDK so that it cannot
// drift from what the chain accepts. It prints the signed item in the base64
// form papucoin_submit takes.
//
// The first attempt at this hand-rolled the JSON, and the node refused it with
// "item is not a PAPU item", because the item shape is method/sender/nonce and
// the signature lives in the item rather than beside it. Using SignItem is what
// stops that class of mistake happening again.

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/eigerco/strawberry/sdk/papucoin"
)

func main() {
	seedHex := flag.String("seed", "", "hex Ed25519 seed of the sending account")
	method := flag.String("method", "transfer", "transfer, faucet, welcome, mint or burn")
	to := flag.String("to", "", "recipient address")
	imprimir := flag.Bool("address", false, "print the chain address of this seed and exit")
	amount := flag.String("amount", "", "amount, as a decimal string")
	nonce := flag.Uint64("nonce", 0, "nonce of this item for this sender")
	flag.Parse()

	if *seedHex == "" {
		fmt.Fprintln(os.Stderr, "signer: -seed is required")
		os.Exit(2)
	}
	seed, err := hex.DecodeString(trim0x(*seedHex))
	if err != nil {
		fail(err)
	}
	if len(seed) != ed25519.SeedSize {
		fail(fmt.Errorf("seed is %d bytes, want %d", len(seed), ed25519.SeedSize))
	}

	// Una cartera necesita saber cual es su propia direccion antes de poder
	// cobrar o gastar, y derivarla a mano es donde se cometen los errores de
	// formato. Aqui sale de la misma funcion que usa la cadena.
	if *imprimir {
		public := ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
		direccion, err := papucoin.AddressFromPublicKey(public)
		if err != nil {
			fail(err)
		}
		fmt.Println(direccion)
		return
	}

	item := papucoin.Item{
		Method: *method,
		To:     *to,
		Amount: *amount,
		Nonce:  *nonce,
		// Anything arriving from outside has to carry a proof of who sent it.
		MustBeSigned: true,
	}

	signed, err := papucoin.SignItem(item, ed25519.NewKeyFromSeed(seed))
	if err != nil {
		fail(err)
	}
	raw, err := json.Marshal(signed)
	if err != nil {
		fail(err)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(raw))
}

func trim0x(s string) string {
	if len(s) > 1 && (s[:2] == "0x" || s[:2] == "0X") {
		return s[2:]
	}
	return s
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "signer:", err)
	os.Exit(1)
}
