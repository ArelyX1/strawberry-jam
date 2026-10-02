package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/crypto/ed25519"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/pkg/devnet"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// A node that was stopped and started again has to carry on the chain it was
// running: the same tip, the same state root, and the next block built on both.
//
// This is the whole point of persisting the blocks and of rebuilding the state out
// of them, and it cannot be checked from inside the process, because the process
// that has to be restarted is this one.
func TestANodeResumesTheChainItWasRunning(t *testing.T) {
	if testing.Short() {
		t.Skip("this test starts a node and waits for its timeslots")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "strawberry")
	buildNode(t, binary)

	// The payout goes to an account of this test's own, and not to the node's
	// bridge account: the bridge account is funded when the chain opens, so a
	// balance there would say nothing about whether the payout survived.
	paid := addressFromSeed(t, 40)
	bridge := addressFromSeed(t, 1)
	require.NotEqual(t, paid, bridge, "the payout has to go to an account the node does not own")

	genesis, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)
	faucet, err := papucoin.ParseAmount(genesis.Service.FaucetAmount, genesis.Service.Decimals)
	require.NoError(t, err)
	payout := faucet.String()

	port := freePort(t)
	first := startNode(t, binary, dir, port)
	opening := waitForBlock(t, first, 2)

	empty, err := balanceOf(t, port, paid)
	require.NoError(t, err)
	require.Equal(t, "0", empty, "the account was paid before anything was claimed")

	payFaucet(t, first, paid)

	// The claim has to reach a block before the node is stopped, or the restart
	// would be tested on work that only ever lived in memory.
	settled := waitForSettledBlock(t, first, paid, opening.Number)
	require.Greater(t, settled.extrinsics, 0,
		"a block that settled a payout does not name the work it settled")

	beforeBalance, err := balanceOf(t, port, paid)
	require.NoError(t, err)
	require.Equal(t, payout, beforeBalance, "the payout did not reach the state")

	before := latestHeader(t, first)
	require.NotNil(t, before, "the first run produced blocks")

	stopNode(t, first)

	second := startNode(t, binary, dir, port)
	defer stopNode(t, second)

	// The resumed node produces the timeslots it was asleep for in a burst, so
	// the state is not stable until it has caught up with the clock.
	waitForClock(t, second)

	// The block the resumed node produces has to be a new one: the tip it resumed
	// from is already there, and waiting for a number it has already reached would
	// pass on the block the stopped node wrote.
	after := waitForBlock(t, second, before.Number+1)

	// The chain carried on rather than starting over: the new block sits on the
	// block the stopped node produced last.
	assert.Equal(t, before.Hash, after.ParentHash,
		"the resumed node built on a different block than the one its chain had reached")
	assert.Greater(t, after.TimeSlotIndex, before.TimeSlotIndex,
		"the resumed node is behind the block it resumed from")
	assert.Equal(t, after.Number, before.Number+1,
		"the resumed node lost count of where it was in the chain")

	// The state the node answers with is the state it arrived at after the newest
	// block it produced, and the next block it produces has to name that same state
	// as the state it was built on. That is the whole claim of a state root in a
	// header, checked from outside the node.
	answering := hexHashOf(rpcString(t, port, "jam_stateRoot"))
	assert.Equal(t, answering, after.ResultingStateRoot,
		"the state the node answers with is not the state its newest block produced")

	// The payout claimed before the restart is still in the state, and it is the
	// same amount rather than merely non-zero: the state was rebuilt from the
	// blocks that accepted the claim, and that claim only exists in a block.
	afterBalance, err := balanceOf(t, port, paid)
	require.NoError(t, err)
	assert.Equal(t, beforeBalance, afterBalance,
		"the payout from the first run is not in the state the second run rebuilt")
	assert.Equal(t, payout, afterBalance, "the rebuilt state paid a different amount than the first run did")

	// And the node kept producing on top of the rebuilt state rather than having
	// silently gone back to the state it started from.
	waitForClock(t, second)
	next := waitForBlock(t, second, after.Number+1)
	stillPaid, err := balanceOf(t, port, paid)
	require.NoError(t, err)
	assert.Equal(t, payout, stillPaid, "the balance changed once the chain carried on")
	assert.Equal(t, answering, next.PriorStateRoot,
		"the block after the restart was not built on the state the node is at")

	// The faucet paid a first claim before the restart, and the chain has only
	// one bridge account, which signs every payout with the next unused nonce.
	// The bridge nonce is part of the state, so a node that restarted with the
	// number it kept in memory would sign a second claim with the first nonce
	// again, and the service would refuse it: a claimed payout this run proves
	// the restart read the nonce back out of the state rather than assuming one.
	secondClaim := addressFromSeed(t, 42)
	payFaucet(t, second, secondClaim)
	settledAfterRestart := waitForSettledBlock(t, second, secondClaim, next.Number)
	require.Greater(t, settledAfterRestart.extrinsics, 0,
		"a block that settled the second payout does not name the work it settled")
	secondBalance, err := balanceOf(t, port, secondClaim)
	require.NoError(t, err)
	assert.Equal(t, payout, secondBalance,
		"the faucet refused a claim after the node restarted")

	t.Log("first run:\n" + first.output.String())
	t.Log("second run:\n" + second.output.String())
}

// A wallet that only speaks Ethereum gets everything about the chain through the
// eth_* methods, so the contract those methods present is the contract the
// chain presents. This test goes through the port the node serves and checks
// the answers are in the units and shapes an Ethereum wallet reads: hashes as
// hex strings, balances and gas in wei, and a transaction count that is the
// EVM sequence rather than the chain's own.
func TestEVMRPCAnswersLikeAnEthereumChain(t *testing.T) {
	if testing.Short() {
		t.Skip("this test starts a node and waits for its timeslots")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "strawberry")
	buildNode(t, binary)

	genesis, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)

	port := freePort(t)
	node := startNode(t, binary, dir, port)
	defer stopNode(t, node)
	opening := waitForBlock(t, node, 2)

	// The relayed transfer in the vector was signed for chain 5120 by a key that
	// exists only on the EVM side, which is what the relayer is for.
	sender := "0x718fae2e5c6ba915a210b7f6e85246db915b9c03"
	destination := "0x3535353535353535353535353535353535353535"
	relayed := "0x02f87582140080843b9aca008504a817c800825208943535353535353535353535353535353535353535884563918244f4000080c080a0102ad6d79c7e6f3dee16db944ff8a8f2c13c7777164a1d36e5b739327758a8dda06b875f8181ba400635cf14f37d1432d0d5abacfbc3d5753397171205550607fd"

	// The ids of these answers are the ones a wallet expects, and a wrong one is
	// indistinguishable from a chain the wallet has never heard of.
	assert.Equal(t, "0x1400", evm(t, node, "eth_chainId"), "this chain has to answer for chain 5120")
	// The price a wallet is shown has to be the price the chain charges. It used
	// to be zero, on the grounds that the fee is part of the transfer rather than
	// a price per unit of gas, which is true of the shape but not of the number:
	// a wallet told the price is zero shows zero, and zero is not what a transfer
	// on this chain costs.
	gasPrice := new(big.Int)
	_, ok := gasPrice.SetString(trim0x(evm(t, node, "eth_gasPrice")), 16)
	require.True(t, ok, "a gas price has to be a number a client can read")
	assert.Positive(t, gasPrice.Sign(), "a chain that charges a fee cannot quote a price of zero")

	startFeeRaw, err := papucoin.ParseAmount(genesis.Service.TransferFee, genesis.Service.Decimals)
	require.NoError(t, err)
	// The price is quoted in wei, and a raw unit of PAPU is worth however many wei
	// the two sets of decimals say, which is the same factor a balance is scaled
	// by. Scaling by the decimals alone would be out by 10^6 here.
	perGas := new(big.Int).Mul(startFeeRaw, weiPerRawOf(genesis))
	assert.Equal(t, perGas.String(), gasPrice.String(),
		"the quoted price is the fee the chain starts from, in the wei a client prices gas in")

	// Whatever number the node answers with, it has to be a number a block
	// answers for: a wallet told 0x3 asks for 0x3 and reads a real block back.
	// The chain produces as it goes, so the number is the newest block rather than
	// the one an earlier read happened to name.
	blocknumber := evm(t, node, "eth_blockNumber")
	block := evmBlock(t, node, blocknumber)
	assert.Equal(t, blocknumber, block["number"],
		"a block answers for the number it was asked by")
	got, err := strconv.ParseUint(strings.TrimPrefix(blocknumber, "0x"), 16, 64)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, got, uint64(opening.Number),
		"the block a wallet is told about has to be a block that exists")

	// Before anything was sent or received the account is empty and the next
	// transaction it signs has the nonce an empty account has.
	assert.Equal(t, "0x0", evm(t, node, "eth_getBalance", sender), "an untouched account is empty")
	assert.Equal(t, "0x0", evm(t, node, "eth_getTransactionCount", sender),
		"an account that sent nothing has the first nonce")

	// A transfer is the only thing this chain can be asked about, so the gas a
	// wallet should set is the intrinsic cost of one, and anything that is not a
	// transfer has no honest number and gets none.
	assert.Equal(t, "0x5208", evm(t, node, "eth_estimateGas", map[string]interface{}{"to": destination}),
		"the gas estimate for a transfer is not the intrinsic cost")
	if _, err := rpc(port, "eth_estimateGas", []interface{}{map[string]interface{}{}}); err == nil {
		t.Fatal("a transaction with no destination must have no gas estimate")
	}
	if _, err := rpc(port, "eth_estimateGas", []interface{}{map[string]interface{}{"to": destination, "data": "0x1234"}}); err == nil {
		t.Fatal("a transaction that carries contract data must have no gas estimate")
	}

	// The chain is read as a chain of numbers: the first block there was is the
	// genesis, and a block further back than this node keeps is a limit rather
	// than a guess.
	earliest := evmBlock(t, node, "earliest")
	assert.Equal(t, "0x0", earliest["number"], "the first block a wallet can ask for is genesis")
	first := evmBlock(t, node, "0x1")
	assert.Equal(t, "0x1", first["number"], "block 1 is the block right after genesis")
	if _, err := rpc(port, "eth_getBlockByNumber", []interface{}{"not-a-number"}); err == nil {
		t.Fatal("a block number that is not hex has to be refused")
	}

	// The EVM account can only hold PAPU if the chain's own faucet pays it, so it
	// is funded the way any other account is funded.
	_, err = rpcResult(t, port, "papucoin_faucet", sender)
	require.NoError(t, err, "the faucet refused the EVM account")
	waitForWei(t, node, sender, "0x"+new(big.Int).Mul(faucetRaw(t, genesis), weiPerRawOf(genesis)).Text(16),
		"the faucet did not reach the EVM account")

	// Relaying a signed transfer answers with the hash a wallet would look up,
	// and nothing this node made up.
	// Quote first, then send. The price is a moving one, so a client that reads
	// it after sending is reading a figure that is no longer the one it agreed
	// to, and the order here is the order a wallet has to use for the same
	// reason.
	quotedWei := new(big.Int)
	_, parsed := quotedWei.SetString(trim0x(evm(t, node, "eth_gasPrice")), 16)
	require.True(t, parsed, "a price has to be a number a client can read")
	quotedRaw := new(big.Int).Quo(quotedWei, weiPerRawOf(genesis))

	sent, err := rpc(port, "eth_sendRawTransaction", []interface{}{relayed})
	require.NoError(t, err, "a transfer signed for this chain was refused")
	hash, _ := sent["result"].(string)
	assert.Regexp(t, "^0x[0-9a-f]{64}$", hash, "a transaction hash is the hex hash of the transaction")
	assert.Equal(t, transactionHash(relayed), hash, "the hash is not the hash of the exact transaction relayed")

	// The transfer pays the destination exactly what the signature says, the
	// sender loses that plus the fee, and the EVM sequence this account is on
	// advances by one: the same numbers the chain keeps, in wei and in hex.
	sentWei := "0x" + new(big.Int).Mul(big.NewInt(5e12), weiPerRawOf(genesis)).Text(16)
	waitForWei(t, node, destination, sentWei, "the destination was not paid the signed amount")
	assert.Equal(t, "0x1", evm(t, node, "eth_getTransactionCount", sender),
		"a relayed transfer is an EVM transaction, and this is its sequence")

	senderRaw, err := papucoin.ParseAmount(genesis.Service.FaucetAmount, genesis.Service.Decimals)
	require.NoError(t, err)
	// The fee is a price, so what a client is charged is what it was quoted, and
	// a client that is quoted one figure and charged another has been told
	// something false. The quote is read back here rather than taken from the
	// genesis, because the price is not a figure fixed at genesis.
	left := new(big.Int).Sub(new(big.Int).Sub(senderRaw, big.NewInt(5e12)), quotedRaw)
	wei := new(big.Int).Mul(left, weiPerRawOf(genesis))
	assert.Equal(t, "0x"+wei.Text(16), evm(t, node, "eth_getBalance", sender),
		"the sender pays the signed amount plus the price it was quoted, counted in wei")

	// A block the chain has not produced yet is null rather than a block, and a
	// block it produced reads back with the number it was asked for.
	ahead, err := rpc(port, "eth_getBlockByNumber", []interface{}{"0xffffffff"})
	require.NoError(t, err, "a block that does not exist yet has to be a null answer, not an error")
	assert.Nil(t, ahead["result"], "a block that does not exist yet is null")

	t.Log("node:\n" + node.output.String())
}

// The chain's memory is the state it rebuilds from its own blocks: the balances
// and the EVM nonce are parts of that state, so a node that restarted answers
// with the same numbers it answered before, and a transaction signed with the
// next number of the sequence it rebuilt is accepted.
func TestEVMBalancesAndNonceSurviveARestart(t *testing.T) {
	if testing.Short() {
		t.Skip("this test starts a node and waits for its timeslots")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "strawberry")
	buildNode(t, binary)

	genesis, err := devnet.LoadGenesis(filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"))
	require.NoError(t, err)

	sender := "0x718fae2e5c6ba915a210b7f6e85246db915b9c03"
	destination := "0x3535353535353535353535353535353535353535"
	// Two transfers signed for chain 5120 by the same key: the first is what a
	// wallet that never sent anything signs (nonce 0), the second is what it
	// signs once the chain has seen the first (nonce 1).
	first := "0x02f87582140080843b9aca008504a817c800825208943535353535353535353535353535353535353535884563918244f4000080c080a0102ad6d79c7e6f3dee16db944ff8a8f2c13c7777164a1d36e5b739327758a8dda06b875f8181ba400635cf14f37d1432d0d5abacfbc3d5753397171205550607fd"
	second := "0x02f87582140001843b9aca008504a817c800825208943535353535353535353535353535353535353535884563918244f4000080c080a04669c291d0312d7f124abbf1109566279c3dbe780ad6071edffb87fb5ab04d2ba07b46d269a5e26963c7030bd157c845f7fdd8f7da5cbd7ca75f3b04baf478608f"
	fiveWei := new(big.Int).Mul(big.NewInt(5e12), weiPerRawOf(genesis))

	port := freePort(t)
	node := startNode(t, binary, dir, port)
	waitForBlock(t, node, 2)

	_, err = rpcResult(t, port, "papucoin_faucet", sender)
	require.NoError(t, err, "the faucet refused the EVM account")
	waitForWei(t, node, sender, "0x"+new(big.Int).Mul(faucetRaw(t, genesis), weiPerRawOf(genesis)).Text(16),
		"the faucet did not reach the EVM account")

	// The first transfer settles before the node stops, so the restart has a
	// state to rebuild rather than a queue to guess from.
	// Quote, send, and keep the figure: the price moves, so the two transfers
	// below are charged what each was quoted rather than one frozen number.
	firstQuote := quoteFee(t, node, weiPerRawOf(genesis))
	sent, err := rpc(port, "eth_sendRawTransaction", []interface{}{first})
	require.NoError(t, err, "the first transfer was refused")
	assert.Regexp(t, "^0x[0-9a-f]{64}$", sent["result"], "a relayed transfer answers with its hash")
	waitForWei(t, node, destination, "0x"+fiveWei.Text(16), "the first transfer did not reach its destination")
	assert.Equal(t, "0x1", evm(t, node, "eth_getTransactionCount", sender),
		"the first transfer is the first EVM transaction of the sender")

	before := latestHeader(t, node)
	require.NotNil(t, before)

	stopNode(t, node)

	// The resumed node rebuilt both the balances and the EVM nonce out of the
	// blocks it replayed, so the account it answers with is the account the first
	// run left behind, and a transaction signed with the nonce it rebuilt is
	// accepted, which is what a wallet that signs next has to be able to count on.
	restarted := startNode(t, binary, dir, port)
	defer stopNode(t, restarted)
	waitForClock(t, restarted)
	waitForBlock(t, restarted, before.Number+1)

	assert.Equal(t, "0x"+fiveWei.Text(16), evm(t, restarted, "eth_getBalance", destination),
		"the destination of the first transfer lost its payment in the rebuild")
	assert.Equal(t, "0x1", evm(t, restarted, "eth_getTransactionCount", sender),
		"the EVM nonce was not rebuilt along with the rest of the state")

	secondQuote := quoteFee(t, node, weiPerRawOf(genesis))
	sentTwo, err := rpc(port, "eth_sendRawTransaction", []interface{}{second})
	require.NoError(t, err, "a transfer signed with the rebuilt nonce was refused")
	assert.Regexp(t, "^0x[0-9a-f]{64}$", sentTwo["result"], "the second transfer answers with its hash")

	twoWei := new(big.Int).Add(fiveWei, fiveWei)
	waitForWei(t, restarted, destination, "0x"+twoWei.Text(16), "the second transfer did not reach its destination")
	assert.Equal(t, "0x2", evm(t, restarted, "eth_getTransactionCount", sender),
		"the second transfer advanced the sequence that was rebuilt")

	senderRaw, err := papucoin.ParseAmount(genesis.Service.FaucetAmount, genesis.Service.Decimals)
	require.NoError(t, err)
	// Two signed amounts and the two prices they were quoted, which is the whole
	// of what left the account.
	feeTotal := new(big.Int).Add(firstQuote, secondQuote)
	step := new(big.Int).Add(new(big.Int).Add(big.NewInt(5e12), feeTotal), big.NewInt(5e12))
	left := new(big.Int).Sub(senderRaw, step)
	wei := new(big.Int).Mul(left, weiPerRawOf(genesis))
	assert.Equal(t, "0x"+wei.Text(16), evm(t, restarted, "eth_getBalance", sender),
		"the sender paid two signed amounts plus the price each was quoted")

	t.Log("first run:\n" + node.output.String())
	t.Log("second run:\n" + restarted.output.String())
}

// evm calls an eth_ method and requires a string answer, which is what all the
// scalar EVM answers are.
func evm(t *testing.T, node *nodeProcess, method string, params ...interface{}) string {
	t.Helper()
	raw, err := rpc(node.port, method, params)
	require.NoError(t, err, "%s refused an answer", method)
	value, ok := raw["result"].(string)
	require.True(t, ok, "%s answered %v, not a string", method, raw["result"])
	return value
}

// evmBlock calls eth_getBlockByNumber and requires the block that was asked for.
func evmBlock(t *testing.T, node *nodeProcess, number string) map[string]interface{} {
	t.Helper()
	raw, err := rpc(node.port, "eth_getBlockByNumber", []interface{}{number})
	require.NoError(t, err, "eth_getBlockByNumber refused an answer")
	block, ok := raw["result"].(map[string]interface{})
	require.True(t, ok, "eth_getBlockByNumber answered %v, not a block", raw["result"])
	return block
}

// waitForWei waits until eth_getBalance of an address answers the given wei
// amount, which is how this test waits for a payment to reach a block.
func waitForWei(t *testing.T, node *nodeProcess, address, want, message string) {
	t.Helper()
	last := ""
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		got := evm(t, node, "eth_getBalance", address)
		if got == want {
			return
		}
		last = got
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s: eth_getBalance(%s) never became %s (last %s); node:\n%s",
		message, address, want, last, node.output.String())
}

// faucetRaw is the faucet amount, in the raw units the chain keeps.
func faucetRaw(t *testing.T, genesis *devnet.Genesis) *big.Int {
	t.Helper()
	amount, err := papucoin.ParseAmount(genesis.Service.FaucetAmount, genesis.Service.Decimals)
	require.NoError(t, err)
	return amount
}

// weiPerRawOf is the factor that turns one unit of the chain's money into wei:
// the two decimal places are the same money, counted differently.
func weiPerRawOf(genesis *devnet.Genesis) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), new(big.Int).SetUint64(uint64(genesis.EVM.Decimals-genesis.Service.Decimals)), nil)
}

// Every block a node produces has to be built on the state the node says it is
// at. This is the claim a state root in a header makes, checked block after block
// from outside the node: if it ever stopped holding, the roots in the chain would
// be decoration.
func TestEveryBlockIsBuiltOnTheStateTheNodeIsAt(t *testing.T) {
	if testing.Short() {
		t.Skip("this test starts a node and waits for its timeslots")
	}

	dir := t.TempDir()
	binary := filepath.Join(dir, "strawberry")
	buildNode(t, binary)

	port := freePort(t)
	node := startNode(t, binary, dir, port)
	defer stopNode(t, node)

	waitForClock(t, node)

	opening := waitForBlock(t, node, 2)
	require.NotEqual(t, crypto.Hash{}, opening.ResultingStateRoot,
		"the node does not say what state it is at")

	for round := 0; round < 3; round++ {
		// The top of the chain is what the node is at, and the claim of a root in
		// a header is that the block that names it was built on it: re-sample the
		// tip once the node is calm, then demand the block that follows names the
		// state of that tip. A burst of blocks would blur which block follows
		// which, and nothing here checks a chain that races its own clock.
		waitForClock(t, node)
		top := latestHeader(t, node)
		require.NotNil(t, top, "the node stopped answering")
		claimed := top.ResultingStateRoot
		following := waitForBlock(t, node, top.Number+1)

		assert.Equal(t, claimed, following.PriorStateRoot,
			"the block %d was not built on the state the node was at after block %d",
			following.Number, following.Number-1)
		assert.NotEqual(t, following.PriorStateRoot, following.ResultingStateRoot,
			"a timeslot did not move the state at all")
		assert.NotEqual(t, crypto.Hash{}, following.PriorStateRoot,
			"the block names a zero state root")
	}
}

// nodeProcess is a node this test started, and knows how to ask it things.
type nodeProcess struct {
	t      *testing.T
	cmd    *exec.Cmd
	port   int
	output *bytes.Buffer
}

// header is what this test reads of a block, without depending on the block type.
type header struct {
	Hash           crypto.Hash
	ParentHash     crypto.Hash
	PriorStateRoot crypto.Hash
	// ResultingStateRoot is the state the node arrived at after this block, which
	// is the state the next block will name.
	ResultingStateRoot crypto.Hash
	TimeSlotIndex      uint32
	Number             uint
	// extrinsics is how much work this block named, and a block that names none
	// is a block a restart cannot rebuild the state from.
	extrinsics int
}

// latestHeader reads the newest block of the chain.
func latestHeader(t *testing.T, node *nodeProcess) *header {
	t.Helper()
	raw, err := rpcResult(t, node.port, "jam_getHeader")
	if err != nil {
		return nil
	}
	return &header{
		Hash:               hexHashOf(stringField(t, raw, "hash")),
		ParentHash:         hexHashOf(stringField(t, raw, "parentHash")),
		PriorStateRoot:     hexHashOf(stringField(t, raw, "stateRoot")),
		ResultingStateRoot: hexHashOf(stringField(t, raw, "resultingStateRoot")),
		TimeSlotIndex:      uint32(numberField(t, raw, "timeSlotIndex")),
		Number:             uint(numberField(t, raw, "number")),
		extrinsics:         int(numberField(t, raw, "extrinsics")),
	}
}

func stringField(t *testing.T, head map[string]interface{}, name string) string {
	t.Helper()
	value, _ := head[name].(string)
	return value
}

func buildNode(t *testing.T, path string) {
	t.Helper()
	// The node is built from the package this test lives in, which is the node
	// itself, so the test exercises the binary it is testing rather than a copy.
	build := exec.Command("go", "build", "-tags", "dev", "-o", path, ".")
	out, err := build.CombinedOutput()
	require.NoError(t, err, "cannot build the node: %s", out)
}

// moduleRoot is the root of the node's own module, which is the directory the node
// expects to be started in.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	require.NoError(t, err)
	return root
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

func startNode(t *testing.T, binary, dir string, rpcPort int) *nodeProcess {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}

	cmd := exec.Command(binary,
		"-rpc-port", strconv.Itoa(rpcPort),
		"-port", strconv.Itoa(rpcPort+1000),
		"-data-dir", filepath.Join(dir, "chain"),
		"-genesis", filepath.Join(moduleRoot(t), "genesis", "chain-dev.json"),
		"-bridge-wallet", hex.EncodeToString(seed),
		// One node on its own, so it writes every timeslot. Left to rotate over
		// the chain's validator count it would write one timeslot in a thousand
		// and the chain it is asked to resume would have holes in it.
		"-author-count", "1",
	)
	// The node is started in the module, because that is where the genesis it is
	// given and the validator file it reads live.
	cmd.Dir = moduleRoot(t)
	output := &bytes.Buffer{}
	cmd.Stdout = output
	cmd.Stderr = output
	require.NoError(t, cmd.Start())

	node := &nodeProcess{t: t, cmd: cmd, port: rpcPort, output: output}
	waitForRPC(t, node)
	return node
}

func (n *nodeProcess) stop() {
	if n.cmd.Process == nil {
		return
	}
	_ = n.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		_, _ = n.cmd.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = n.cmd.Process.Kill()
	}
}

func stopNode(t *testing.T, node *nodeProcess) {
	t.Helper()
	node.stop()
}

// waitForRPC waits for the node to answer the chain it is part of.
func waitForRPC(t *testing.T, node *nodeProcess) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := rpc(node.port, "papucoin_chainParams", nil); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("the node never answered: %s", node.output.String())
}

// waitForBlock waits until the chain has at least the given number of blocks.
// waitForClock waits until the node has caught up with the wall clock, which is
// when the burst of blocks a node produces for the timeslots it slept through has
// ended. A state sampled while that burst is running is not a state the node is
// at for long, so the checks that follow a read can race the next block.
func waitForClock(t *testing.T, node *nodeProcess) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if latest := latestHeader(t, node); latest != nil &&
			latest.TimeSlotIndex >= uint32(jamtime.Now().ToTimeslot()) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the node never caught up with the clock: %s", node.output.String())
}

func waitForBlock(t *testing.T, node *nodeProcess, blocks uint) header {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if latest := latestHeader(t, node); latest != nil && latest.Number >= blocks {
			return *latest
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the chain did not reach block %d: %s", blocks, node.output.String())
	return header{}
}

func numberField(t *testing.T, head map[string]interface{}, name string) uint64 {
	t.Helper()
	value, ok := head[name].(float64)
	if !ok {
		return 0
	}
	return uint64(value)
}

// addressFromSeed is the PAPU address of a key this test makes up, so a test can
// hold an account whose balance depends on what the chain did with it.
func addressFromSeed(t *testing.T, from byte) string {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = from + byte(i)
	}
	address, err := papucoin.AddressFromPublicKey(ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey))
	require.NoError(t, err)
	return address
}

// balanceOf is what the chain says an address holds, in the units the chain uses.
func balanceOf(t *testing.T, port int, address string) (string, error) {
	t.Helper()
	balance, err := rpcResult(t, port, "papucoin_balance", address)
	if err != nil {
		return "", err
	}
	raw, _ := balance["raw"].(string)
	return raw, nil
}

// waitForSettledBlock waits for a block that named the payout in the work it
// settled, and reports that block.
//
// Waiting for the balance to change is not the same thing: a balance can change
// and a block can still not name the work that changed it, and a block that names
// no work is a block a restart cannot rebuild from.
func waitForSettledBlock(t *testing.T, node *nodeProcess, address string, from uint) header {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		if latest := latestHeader(t, node); latest != nil && latest.Number > from && latest.extrinsics > 0 {
			return *latest
		}
		time.Sleep(500 * time.Millisecond)
	}
	require.FailNow(t, "no block named the work it settled",
		"the payout to %s reached the state but no block named the work that paid it: %s",
		address, node.output.String())
	return header{}
}

// payFaucet has the node pay the chain's faucet to an address.
func payFaucet(t *testing.T, node *nodeProcess, address string) {
	t.Helper()
	_, err := rpcResult(t, node.port, "papucoin_faucet", address)
	require.NoError(t, err, "the faucet claim was refused")
}

// rpcResult calls a method and returns its result.
func rpcResult(t *testing.T, port int, method string, params ...interface{}) (map[string]interface{}, error) {
	t.Helper()
	raw, err := rpc(port, method, params)
	if err != nil {
		return nil, err
	}
	result, ok := raw["result"].(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("%s answered %v", method, raw)
	}
	return result, nil
}

func rpcString(t *testing.T, port int, method string, params ...interface{}) string {
	t.Helper()
	raw, err := rpc(port, method, params)
	require.NoError(t, err)
	value, _ := raw["result"].(string)
	return value
}

func rpc(port int, method string, params []interface{}) (map[string]interface{}, error) {
	body := map[string]interface{}{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		body["params"] = params
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	response, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d", port), "application/json", bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	var answer map[string]interface{}
	if err := json.NewDecoder(response.Body).Decode(&answer); err != nil {
		return nil, err
	}
	if failure, ok := answer["error"]; ok {
		return nil, fmt.Errorf("%s: %v", method, failure)
	}
	return answer, nil
}

func hexHash(hash crypto.Hash) string {
	return "0x" + hex.EncodeToString(hash[:])
}

func hexHashOf(value string) crypto.Hash {
	decoded, err := hex.DecodeString(trimHex(value))
	if err != nil {
		return crypto.Hash{}
	}
	hash := crypto.Hash{}
	copy(hash[:], decoded)
	return hash
}

func trimHex(value string) string {
	return strings.TrimPrefix(value, "0x")
}

func trim0x(s string) string { return strings.TrimPrefix(s, "0x") }

// quoteFee reads the price a transfer would be charged right now, in raw units.
// A client has to read this before it sends, because the price moves: a client
// that reads it afterwards is reading a figure the chain is no longer charging.
func quoteFee(t *testing.T, proc *nodeProcess, weiPerRaw *big.Int) *big.Int {
	t.Helper()
	quotedWei := new(big.Int)
	_, parsed := quotedWei.SetString(trim0x(evm(t, proc, "eth_gasPrice")), 16)
	require.True(t, parsed, "a price has to be a number a client can read")
	return new(big.Int).Quo(quotedWei, weiPerRaw)
}
