// Command walletsmoke drives the PAPU economy from two kinds of wallet against a
// running node: one that only speaks Ethereum, like MetaMask, and one that holds
// a chain key and signs items the way the SDK does. Both trust boundaries are
// exercised, and both have to end up moving real balance through the guest.
//
// It talks to a node over RPC, so it proves the same path a real user would, not
// a library call in the same process.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4"

	"github.com/eigerco/strawberry/sdk/papucoin"
)

// chainID is the chain the EVM gateway presents; a transaction signed for any
// other one must not be replayed here.
var chainID = flag.Int64("chain-id", 5120, "the chain id wallets must sign for")

var (
	rpcURL = flag.String("rpc", "http://localhost:9961", "the node's JSON-RPC endpoint")
	wait   = flag.Duration("wait", 8*time.Second, "how long to let the chain work")
)

func main() {
	flag.Parse()
	client := &http.Client{Timeout: 20 * time.Second}

	// An Ethereum key, the kind a wallet that has never heard of this chain
	// holds. Its first move has to be a payout from the node.
	key, err := secp256k1.GeneratePrivateKeyFromRand(rand.Reader)
	must(err)
	uncompressed := key.PubKey().SerializeUncompressed()
	evmAddress := "0x" + hex(papucoin.KeccakForTest(uncompressed[1:])[12:])
	fmt.Printf("wallet EVM    %s\n", evmAddress)

	_, err = rpc(client, "papucoin_faucet", evmAddress)
	must(err)
	fmt.Println("  faucet solicitado")

	time.Sleep(*wait)
	evmBalance := readBalance(client, evmAddress)
	fmt.Printf("  saldo: %s PAPU (raw %s)\n", evmBalance.Balance, evmBalance.Raw)
	if evmBalance.Raw == "0" {
		die("el faucet no movio saldo desde una wallet EVM")
	}

	// A chain key, the other trust boundary: the sender is covered by an
	// ed25519 signature over the item itself.
	chainPub, chainPriv, err := ed25519.GenerateKey(rand.Reader)
	must(err)
	chainAddress, err := papucoin.AddressFromPublicKey(chainPub)
	must(err)
	fmt.Printf("wallet cadena %s\n", chainAddress)

	signed, err := papucoin.SignItem(papucoin.Item{
		Method:       "welcome",
		Sender:       chainAddress,
		Nonce:        1,
		MustBeSigned: true,
	}, chainPriv)
	must(err)
	blob, err := json.Marshal(signed)
	must(err)

	// the endpoint takes the item as hex, the same way an RPC client sends one
	raw, err := rpc(client, "papucoin_submit", "0x"+hex(blob))
	must(err)
	var submission struct {
		Accepted bool `json:"accepted"`
	}
	must(json.Unmarshal(raw, &submission))
	if !submission.Accepted {
		die("la cadena rechazo un item correctamente firmado")
	}
	fmt.Println("  item firmado aceptado")

	time.Sleep(*wait)
	chainBalance := readBalance(client, chainAddress)
	fmt.Printf("  saldo: %s PAPU (raw %s)\n", chainBalance.Balance, chainBalance.Raw)
	if chainBalance.Raw == "0" {
		die("el welcome no movio saldo para una wallet de cadena")
	}

	// The path a wallet actually takes: a raw signed EVM transaction, verified
	// by the chain and applied by the guest. The faucet above is the node paying
	// out; this is a wallet moving its own money to another wallet. Which of the
	// three transaction shapes a wallet emits is its own choice, so all three are
	// tried: a library that only speaks one of them is still a real wallet.
	recipient := "0xbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	shapes := []struct {
		name  string
		nonce uint64
		make  func(uint64) ([]byte, error)
	}{
		{"eip1559", 0, func(nonce uint64) ([]byte, error) {
			return papucoin.SignEIP1559(
				big.NewInt(*chainID), nonce,
				1_000_000_000, 2_000_000_000, 21_000,
				mustAddr(recipient),
				big.NewInt(250*1_000_000),
				nil, key,
			)
		}},
		{"eip2930", 1, func(nonce uint64) ([]byte, error) {
			return papucoin.SignEIP2930(
				big.NewInt(*chainID), nonce,
				1_000_000_000, 60_000,
				mustAddr(recipient),
				big.NewInt(250*1_000_000),
				nil,
				[][][]byte{{
					papucoin.RlpStringForTest(make([]byte, 20)),
					papucoin.RlpStringForTest([]byte{0x01, 0x02, 0x03, 0x04}),
				}},
				key,
			)
		}},
		{"legacy", 2, func(nonce uint64) ([]byte, error) {
			return papucoin.SignEVMLegacy(
				big.NewInt(*chainID), nonce,
				1_000_000_000, 21_000,
				mustAddr(recipient),
				big.NewInt(250*1_000_000),
				nil, key,
			)
		}},
	}
	for _, shape := range shapes {
		tx, err := shape.make(shape.nonce)
		if err != nil {
			die("firmar la transferencia " + shape.name + ": " + err.Error())
		}
		if _, err := rpc(client, "eth_sendRawTransaction", "0x"+hex(tx)); err != nil {
			die("eth_sendRawTransaction " + shape.name + ": " + err.Error())
		}
		fmt.Printf("  eth_sendRawTransaction (%s) aceptada\n", shape.name)
	}

	time.Sleep(*wait)
	recipientBalance := readBalance(client, recipient)
	fmt.Printf("  receptor %s: %s PAPU\n", recipient, recipientBalance.Balance)
	if recipientBalance.Raw == "0" {
		die("una wallet EVM no pudo mover saldo a otra")
	}

	rawSupply, err := rpc(client, "papucoin_supply")
	must(err)
	var supply struct {
		Supply       string `json:"supply"`
		Raw          string `json:"raw"`
		StorageItems uint32 `json:"storageItems"`
	}
	must(json.Unmarshal(rawSupply, &supply))
	fmt.Printf("supply        %s PAPU (raw %s, %d claves de storage)\n", supply.Supply, supply.Raw, supply.StorageItems)

	fmt.Println("\nlas dos wallets movieron saldo a traves del guest")
}

type balance struct {
	Balance string `json:"balance"`
	Raw     string `json:"raw"`
}

func readBalance(client *http.Client, address string) balance {
	raw, err := rpc(client, "papucoin_balance", address)
	must(err)
	var out balance
	must(json.Unmarshal(raw, &out))
	return out
}

func rpc(client *http.Client, method string, params ...any) (json.RawMessage, error) {
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": method, "params": params,
	})
	if err != nil {
		return nil, err
	}
	resp, err := client.Post(*rpcURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("%s: %w (body %s)", method, err, raw)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %s", method, out.Error.Message)
	}
	return out.Result, nil
}

func hex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(b)*2)
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&0xf])
	}
	return string(out)
}

func mustAddr(hexAddr string) []byte {
	raw := make([]byte, 20)
	for i := 0; i < 20; i++ {
		raw[i] = byte(hexVal(hexAddr[2+i*2])<<4 | hexVal(hexAddr[3+i*2]))
	}
	return raw
}

func hexVal(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

func must(err error) {
	if err != nil {
		die(err.Error())
	}
}

func die(reason string) {
	fmt.Fprintln(os.Stderr, "FALLO:", reason)
	os.Exit(1)
}
