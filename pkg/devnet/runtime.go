package devnet

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"strconv"
	"strings"
	"sync"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state"
	"github.com/eigerco/strawberry/internal/state/merkle"
	"github.com/eigerco/strawberry/internal/store"
	"github.com/eigerco/strawberry/pkg/db"
	"github.com/eigerco/strawberry/pkg/db/pebble"
	svc "github.com/eigerco/strawberry/sdk"
	"github.com/eigerco/strawberry/sdk/papucoin"
)

// maxGas is the gas ceiling handed to every invocation. The dev chain does not
// meter gas, so the ceiling only has to be high enough that no PAPU operation is
// ever cut short, and the accumulation limit is the smallest one that satisfies
// that.
const maxGas = constants.MaxAllocatedGasAccumulation

// Options configure how a runtime is built.
type Options struct {
	// Genesis is the network definition. Required.
	Genesis *Genesis
	// BridgeKey signs the items the node mints from its own account, such as
	// faucet payouts. A node without one is still a chain: it just cannot mint.
	BridgeKey ed25519.PrivateKey
	// Logf receives the events an operator cares about. Defaults to the standard
	// logger, which is enough for a node that was started by hand.
	Logf func(format string, args ...any)
	// Validators are the validators of the chain. A dev chain needs them only so
	// that the validator state, and therefore the state root, is well formed.
	Validators []Validator
	// TrieDB keeps the trie nodes of every root the node commits, which is what
	// makes a past root readable afterwards: a node that is given a store rooted
	// in a directory can still answer what the state was at an old block after it
	// has restarted. Without one the nodes live in memory and are forgotten.
	//
	// The state of the chain is not kept here. It is rebuilt by replaying the
	// blocks, which is the only way to be sure that the state a node resumes with
	// is the state its blocks describe.
	TrieDB db.KVStore
}

// Runtime owns the JAM state of a dev node and the PAPU service running on it.
//
// It is the single source of truth for the state: the block producer asks it for
// the root of the parent state, and the RPC answers balance and trie queries
// from it, so there is never a second copy of the chain to disagree with.
type Runtime struct {
	genesis *Genesis
	params  papucoin.Params
	// evmChainID is the only chain whose Ethereum transactions this node relays,
	// and weiPerRaw is how many wei one unit of PAPU is worth, which is what makes
	// an amount that arrives in wei land on a whole number of PAPU units.
	evmChainID *big.Int
	weiPerRaw  *big.Int

	// mu serialises block execution against concurrent reads. Without it a read
	// could observe a half applied block.
	mu    sync.RWMutex
	state *state.State
	root  crypto.Hash

	registry   *svc.Registry
	executor   *svc.Executor
	scheduler  *svc.Scheduler
	papucoinID block.ServiceId
	trie       *store.Trie
	// stateDB is where the state is written after every timeslot, and read from at
	// startup. It is nil when the node was built without a store, which is the
	// case for tests that only care about a chain in memory.

	issuer      string
	bridgeKey   ed25519.PrivateKey
	bridgeAddr  string
	bridgeNonce uint64
	// bridgeFunded records that the faucet reserve has already been moved out of
	// the genesis issuer account, which may only happen once.
	bridgeFunded bool
	// replaying records that the work coming in comes from blocks this node
	// produced earlier, and not from anything that asked the node to do it now.
	// The faucet reserve is one thing that is not queued again while it is set.
	replaying bool

	logf func(format string, args ...any)
}

// New builds a runtime from a genesis definition: it registers PAPU, funds the
// opening balances by running the service's own initialisation, and computes the
// genesis state root.
func New(opts Options) (*Runtime, error) {
	if opts.Genesis == nil {
		return nil, fmt.Errorf("devnet: a genesis is required")
	}
	genesis := opts.Genesis

	logf := opts.Logf
	if logf == nil {
		std := log.New(os.Stderr, "jam ", log.LstdFlags)
		logf = std.Printf
	}

	params, err := paramsFrom(genesis.Service)
	if err != nil {
		return nil, err
	}
	endowment, err := strconv.ParseUint(genesis.Service.Endowment, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("devnet: service.endowment %q: %w", genesis.Service.Endowment, err)
	}

	issuer, err := genesis.Service.IssuerAddress()
	if err != nil {
		return nil, err
	}
	balances, err := genesis.Service.InitialBalances()
	if err != nil {
		return nil, err
	}

	// The EVM relay is what makes a wallet that only speaks Ethereum able to send:
	// a raw Ethereum transaction carries its own secp256k1 signature, so the service
	// can recover who signed it instead of trusting who the item claims to be. It
	// only accepts transactions for the chain the genesis names.
	relay := papucoin.NewEVMRelay(big.NewInt(genesis.EVM.ChainID))

	registry := svc.NewRegistry()
	id := block.ServiceId(genesis.Service.ID)
	if err := registry.RegisterAt(papucoin.New(params, issuer, balances, relay), id); err != nil {
		return nil, fmt.Errorf("devnet: cannot register %s as service %d: %w", genesis.Service.Name, id, err)
	}

	executor := svc.NewExecutor(registry)
	scheduler := svc.NewScheduler(registry, int(constants.MaxNumberOfItems))

	trieDB := opts.TrieDB
	if trieDB == nil {
		memory, err := pebble.NewKVStore()
		if err != nil {
			return nil, fmt.Errorf("devnet: cannot open the state store: %w", err)
		}
		trieDB = memory
	}

	// Create the account and run the service's genesis initialisation over it, so
	// the opening balances are produced by the service code rather than written
	// into the state by hand.
	account := service.NewServiceAccount()
	account.Balance = endowment
	services := service.ServiceState{id: account}

	seeded, err := executor.Initialize(id, 0, maxGas, services)
	if err != nil {
		return nil, fmt.Errorf("devnet: cannot initialise %s: %w", genesis.Service.Name, err)
	}
	services[id] = seeded.Account

	if err := checkThreshold(services[id]); err != nil {
		return nil, fmt.Errorf("devnet: %s cannot pay for its genesis storage: %w", genesis.Service.Name, err)
	}

	validators := ValidatorState(opts.Validators)

	rt := &Runtime{
		genesis:     genesis,
		params:      params,
		state:       newStateWith(services, opts.Validators),
		registry:    registry,
		executor:    executor,
		scheduler:   scheduler,
		papucoinID:  id,
		trie:        store.NewTrie(trieDB),
		issuer:      issuer,
		evmChainID:  relay.ChainID,
		weiPerRaw:   weiPerRaw(genesis.EVM.Decimals, params.Decimals),
		bridgeKey:   opts.BridgeKey,
		bridgeNonce: genesis.Service.FirstNonce,
		logf:        logf,
	}
	rt.state.ValidatorState = validators
	if rt.bridgeKey != nil {
		rt.bridgeAddr, err = papucoin.AddressFromPublicKey(rt.bridgeKey.Public().(ed25519.PublicKey))
		if err != nil {
			return nil, fmt.Errorf("devnet: bridge key does not yield an address: %w", err)
		}

	}

	rt.updateRoot()
	rt.logf("%s (%s) online: service %d, issuer %s, endowment %d, state root %x",
		genesis.Service.Name, genesis.Service.Symbol, id, issuer, endowment, rt.root)

	return rt, nil
}

// newStateWith returns a JAM state carrying the given services and nothing else.
// The Safrole side of the state stays empty on purpose: the dev chain has no
// ticket machinery, and an empty state still merklizes, which is all a state
// root needs.
func newStateWith(services service.ServiceState, validators []Validator) *state.State {
	return &state.State{
		Services:       services,
		ValidatorState: ValidatorState(validators),
		RecentHistory:  state.RecentHistory{},
	}
}

// paramsFrom turns the genesis service section into PAPU parameters.
// weiPerRaw is how many wei one unit of PAPU is worth. Ethereum counts in wei and
// this chain counts in PAPU, so the two are the same money at a different number
// of decimal places, and the ratio between those places is what makes an amount
// that arrives in wei land on a whole number of units.
func weiPerRaw(evmDecimals, rawDecimals uint8) *big.Int {
	if evmDecimals < rawDecimals {
		return new(big.Int)
	}
	return new(big.Int).Exp(big.NewInt(10), new(big.Int).SetUint64(uint64(evmDecimals-rawDecimals)), nil)
}

func paramsFrom(cfg *ServiceCfg) (papucoin.Params, error) {
	params := papucoin.DefaultParams()
	params.Name = cfg.Name
	params.Symbol = cfg.Symbol
	params.Decimals = cfg.Decimals
	params.FirstNonce = cfg.FirstNonce

	for _, field := range []struct {
		name   string
		value  string
		target **big.Int
	}{
		{"maxSupply", cfg.MaxSupply, &params.MaxSupply},
		{"transferFee", cfg.TransferFee, &params.TransferFee},
		{"faucetAmount", cfg.FaucetAmount, &params.FaucetAmount},
		{"welcomeAmount", cfg.WelcomeAmount, &params.WelcomeAmount},
	} {
		amount, err := papucoin.ParseAmount(field.value, cfg.Decimals)
		if err != nil {
			return params, fmt.Errorf("devnet: service.%s %q: %w", field.name, field.value, err)
		}
		*field.target = amount
	}
	return params, nil
}

// checkThreshold refuses an account that could not pay for the storage it holds.
func checkThreshold(account service.ServiceAccount) error {
	threshold, err := account.ThresholdBalance()
	if err != nil {
		return err
	}
	if uint64(account.Balance) < threshold {
		return fmt.Errorf("balance %d is below the storage threshold %d", account.Balance, threshold)
	}
	return nil
}

// State exposes the live state for reads. Callers must not mutate it.
func (r *Runtime) State() *state.State {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state
}

// Root is the root of the state as it stands, which is what the next block has
// to name as its parent state root.
func (r *Runtime) Root() crypto.Hash {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.root
}

// Params returns the PAPU parameters the chain was built with.
func (r *Runtime) Params() papucoin.Params { return r.params }

// PapucoinID is the service id PAPU was registered under.
func (r *Runtime) PapucoinID() block.ServiceId { return r.papucoinID }

// AlignToGenesis places the opening state at the timeslot the chain was founded
// at, which is the timeslot the first block sits after.
//
// A genesis that was dated when the node started says when the chain begins, and
// a state that says the chain began at timeslot zero would have the first block
// name a state that is nine million timeslots older than the block itself.
func (r *Runtime) AlignToGenesis(timeslot jamtime.Timeslot) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.state.TimeslotIndex != 0 {
		return
	}
	r.state.TimeslotIndex = timeslot
	r.updateRoot()
}

// Timeslot is the timeslot the state stands at, which is the point a node
// resumes the chain from.
func (r *Runtime) Timeslot() jamtime.Timeslot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.state.TimeslotIndex
}

// Genesis returns the genesis the runtime was built from.
func (r *Runtime) Genesis() *Genesis { return r.genesis }

// BridgeAddress is the PAPU address of the node's own key, empty when the node
// has none.
func (r *Runtime) BridgeAddress() string { return r.bridgeAddr }

// updateRoot recomputes the state root. It runs with the lock held.
func (r *Runtime) updateRoot() {
	root, err := merkle.MerklizeState(*r.state, r.trie)
	if err != nil {
		// A block cannot name a parent without a root, so there is nothing to
		// carry on with.
		panic(fmt.Sprintf("devnet: cannot merklize the state: %v", err))
	}
	r.root = root
}

// BlockWork is one item of service work a timeslot carried, in the order it ran.
//
// A block says which work it carried, so a node that rebuilds the chain can hand
// the same work back and land on the same state. Without this the work a block
// settled would exist only in the memory of the node that ran it, and a restart
// would rebuild a state that never had it.
type BlockWork struct {
	ServiceID block.ServiceId
	Payload   []byte
}

// Step applies one timeslot of work and leaves the caller to find out what it
// carried. A block that has to name its work uses [Runtime.Run] instead.
func (r *Runtime) Step(timeslot jamtime.Timeslot) error {
	_, err := r.Run(timeslot)
	return err
}

// Run applies one timeslot of work: it refines the queued items, accumulates the
// ones that survived, and re-commits the state root. It reports the work it ran so
// the block naming this timeslot can commit to it.
//
// The root is updated as part of the run, so a caller that runs and then reads the
// root gets the root of the state the run produced.
func (r *Runtime) Run(timeslot jamtime.Timeslot) ([]BlockWork, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.runLocked(timeslot)
}

// Rebuild queues work that a block already carried.
//
// This is the other half of [Runtime.Run]: the payloads come out of a block, not
// from outside the node, so they do not go back through the door in [Runtime.Submit]
// and are not asked for a signature again. They were accepted once, when the block
// that named them was produced, and the header commits to them.
func (r *Runtime) Rebuild(work []BlockWork) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.replaying = true
	for _, w := range work {
		if _, err := r.scheduler.Submit(w.ServiceID, svc.WorkItem{Payload: w.Payload, Origin: "replay"}); err != nil {
			return fmt.Errorf("the queue is full: %w", err)
		}
	}
	return nil
}

// FinishRebuild ends a rebuild. The work the node queues from here on is work it
// was asked to do, and the numbers this node keeps for itself are read back out
// of the state the blocks left behind, because those numbers are part of that
// state and a node that restarted with the ones it had in memory would sign an
// item the chain has already used a nonce for.
func (r *Runtime) FinishRebuild() {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.replaying = false
	r.syncBridgeNonceLocked()
}

// syncBridgeNonceLocked takes the bridge account's next nonce from the state, so
// that a payout after a restart carries a number the service will accept. The
// first nonce of the parameters is the floor, because an account the service has
// never seen has not used one yet and the number it expects is the first.
func (r *Runtime) syncBridgeNonceLocked() {
	if r.bridgeAddr == "" {
		return
	}
	nonce, err := r.viewLocked().Nonce(r.bridgeAddr)
	if err != nil {
		r.logf("cannot read the nonce of the faucet account %s: %v", r.bridgeAddr, err)
		return
	}
	if nonce > r.bridgeNonce {
		r.bridgeNonce = nonce
	}
}

func (r *Runtime) runLocked(timeslot jamtime.Timeslot) ([]BlockWork, error) {
	r.fundBridgeLocked()

	// The timeslot is part of the state, so the root of a chain that is running
	// moves even in a timeslot that settles no work.
	r.state.TimeslotIndex = timeslot

	var work []BlockWork
	// A timeslot hands its cores out once, and every assigned service runs
	// refine then accumulate on the state it is given.
	assignments := r.scheduler.Plan(int(constants.TotalNumberOfCores))
	for _, assignment := range assignments {
		// The work a service was given is the work its block names, whether or not
		// it survives: a refused item is still an item the timeslot carried.
		for _, item := range assignment.Items {
			work = append(work, BlockWork{ServiceID: assignment.ServiceID, Payload: item.Payload})
		}
		if err := r.runServiceLocked(assignment, timeslot); err != nil {
			// A service that fails leaves its items queued rather than losing
			// them, because nothing was charged to anybody.
			r.logf("service %d did not run in timeslot %d: %v", assignment.ServiceID, timeslot, err)
			r.scheduler.Settle([]svc.Assignment{assignment})
			continue
		}
		r.scheduler.Settle([]svc.Assignment{assignment})
	}

	r.updateRoot()
	return work, nil
}

// runServiceLocked refines and accumulates one service, leaving the state untouched
// if either stage fails.
func (r *Runtime) runServiceLocked(assignment svc.Assignment, timeslot jamtime.Timeslot) error {
	account, ok := r.state.Services[assignment.ServiceID]
	if !ok {
		return fmt.Errorf("the service has no account")
	}

	refined := make([]svc.RefinedItem, 0, len(assignment.Items))
	for _, item := range assignment.Items {
		result, err := r.executor.Refine(assignment.ServiceID, item.Payload, timeslot, maxGas, r.state.Services)
		if err != nil {
			// A refused item is an ordinary outcome, not a chain fault.
			r.logf("service %d refused an item from %q: %v", assignment.ServiceID, item.Origin, err)
			continue
		}
		refined = append(refined, svc.RefinedItem{Item: item.Payload, Report: result.Report})
	}

	result, err := r.executor.Accumulate(assignment.ServiceID, refined, r.scheduler.Transfers(assignment.ServiceID), timeslot, maxGas, r.state.Services)
	if err != nil {
		return fmt.Errorf("accumulate: %w", err)
	}
	if err := checkThreshold(result.Account); err != nil {
		return err
	}

	account = result.Account
	r.state.Services[assignment.ServiceID] = account
	return nil
}

// fundBridgeLocked moves the faucet reserve out of the genesis issuer account
// into the node's own account, once.
//
// The issuer of a dev genesis is a pseudo-account: its balance is in the genesis
// and no private key anywhere in the system corresponds to it, so there is
// nothing to steal by spending it. It is what the faucet pays out of.
func (r *Runtime) fundBridgeLocked() {
	if r.bridgeKey == nil || r.bridgeFunded {
		return
	}
	r.bridgeFunded = true

	// The reserve was moved once, by the first timeslot this chain ran, and that
	// timeslot has a block. Replaying that block applies the reserve again from
	// the block itself, so queueing it here as well would put the same transfer
	// into the queue twice and move the work of a later timeslot into a different
	// one than the blocks say it went into.
	if r.replaying {
		return
	}

	// A tenth of a percent of the genesis issuer balance, and never more than a
	// thousand payouts, so the reserve cannot be the bulk of the money the chain
	// was founded with.
	view := r.viewLocked()
	issuerBalance, err := view.Balance(r.issuer)
	if err != nil {
		r.logf("cannot read the issuer balance: %v", err)
		return
	}
	reserve := new(big.Int).Mul(r.params.FaucetAmount, big.NewInt(faucetReservePayouts))
	if cap := new(big.Int).Div(issuerBalance, big.NewInt(1000)); reserve.Cmp(cap) > 0 {
		reserve = cap
	}
	item := papucoin.Item{
		Method: papucoin.MethodTransfer,
		Sender: r.issuer,
		Nonce:  1,
		To:     r.bridgeAddr,
		Amount: papucoin.FormatRaw(reserve, r.params.Decimals),
	}
	if _, err := r.scheduler.Submit(r.papucoinID, svc.WorkItem{Payload: encodeItem(item), Origin: "faucet-reserve"}); err != nil {
		r.logf("could not queue the faucet reserve: %v", err)
	}
	r.logf("moving the faucet reserve into %s from the genesis issuer", r.bridgeAddr)
}

// faucetReservePayouts is how many payouts the reserve is meant to cover before
// an operator has to fund the bridge account by hand.
const faucetReservePayouts = 1_000

// Submit hands an item to the scheduler. Everything arriving from outside the
// node goes through here, so this is where an item is forced to be signed before
// anything can be refined.
func (r *Runtime) Submit(item papucoin.Item) error {
	if item.Sender == "" {
		return fmt.Errorf("item has no sender")
	}
	if _, err := papucoin.NormalizeAddress(item.Sender); err != nil {
		return fmt.Errorf("item sender: %w", err)
	}
	if item.Nonce == 0 {
		return fmt.Errorf("item nonce is 0, but the first nonce is %d", r.params.FirstNonce)
	}
	if item.Amount != "" {
		if _, err := papucoin.ParseAmount(item.Amount, r.params.Decimals); err != nil {
			return fmt.Errorf("item amount: %w", err)
		}
	}
	if item.Method != papucoin.MethodTransfer && item.Method != papucoin.MethodFaucet && item.Method != papucoin.MethodMint {
		return fmt.Errorf("unknown method %q", item.Method)
	}

	// An item that arrives from outside has to be signed, and it has to say so:
	// the flag is covered by the signature, so an item cannot arrive claiming to
	// need no proof and then skip the proof. This is the trust boundary, and it
	// is the only reason the flag can be trusted at all.
	if !item.MustBeSigned {
		return fmt.Errorf("item from %s declares itself unsigned, and an item that arrives from outside has to declare that it is signed by %s", item.Sender, item.Sender)
	}
	if _, err := papucoin.VerifyItemSignature(item); err != nil {
		return fmt.Errorf("item signature: %w", err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.enqueue(item, item.Sender)
}

// SubmitRelayed queues a transfer that arrives as a raw Ethereum transaction.
//
// This is the second trust boundary, and a different one. An item that arrives
// through Submit has to carry an Ed25519 signature over the item, which means the
// wallet has to hold a key of this chain. A wallet that only speaks Ethereum, like
// MetaMask, has nothing but a secp256k1 key, and the transaction it signs already
// proves who it is: the service recovers the sender from the signature and takes
// the sender, the destination, the amount and the nonce from the signed bytes
// rather than from anything this item claims. So the item built here says nothing
// the service will believe, and nothing here can be forged into a transfer from an
// account the sender does not control.
//
// The transaction is decoded here too, so a transaction that could never be
// refunded is rejected at the door instead of sitting in the queue failing there.
func (r *Runtime) SubmitRelayed(rawTx string) (papucoin.Item, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(rawTx, "0x"))
	if err != nil {
		return papucoin.Item{}, fmt.Errorf("raw transaction: %w", err)
	}
	tx, err := papucoin.ParseEVMTransaction(raw, r.evmChainID)
	if err != nil {
		return papucoin.Item{}, err
	}
	if tx.Value.Sign() <= 0 {
		return papucoin.Item{}, errors.New("papucoin: a relayed transaction has to move a positive amount")
	}
	if new(big.Int).Mod(tx.Value, r.weiPerRaw).Sign() != 0 {
		return papucoin.Item{}, errors.New("papucoin: a relayed amount has to be a whole number of micro-PAPU")
	}

	from, err := papucoin.NormalizeAddress(tx.From)
	if err != nil {
		return papucoin.Item{}, fmt.Errorf("relayed sender: %w", err)
	}
	to, err := papucoin.NormalizeAddress(tx.To)
	if err != nil {
		return papucoin.Item{}, fmt.Errorf("relayed destination: %w", err)
	}
	// An account has two sequences of numbers and this node is the one that keeps
	// the chain's: the wallet keeps the Ethereum nonce, and the chain keeps its own
	// so that a replayed transaction cannot be a second transfer. The transaction's
	// nonce is checked by the wallet and by the service's EVM nonce record, so what
	// this item has to carry is the next number of the chain's sequence, which is
	// the number the service stored the last time the account was used. An account
	// the chain has never used has not stored one, and the number it expects then
	// is the first nonce of the parameters.
	nonce, err := r.View().Nonce(from)
	if err != nil {
		return papucoin.Item{}, err
	}
	if nonce == 0 {
		nonce = r.params.FirstNonce
	}

	item := papucoin.Item{
		Method: papucoin.MethodTransfer,
		Sender: from,
		To:     to,
		// An item's amount is written the way a person writes money, in PAPU and
		// not in the wei the transaction happens to carry.
		Amount: papucoin.FormatRaw(new(big.Int).Quo(tx.Value, r.weiPerRaw), r.params.Decimals),
		Nonce:  nonce,
		Raw:    rawTx,
		// The proof of this item is the signature inside Raw, which the service
		// checks before it reads the account. The flag stays false because an
		// Ed25519 signature is exactly what this kind of item cannot have.
		MustBeSigned: false,
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.enqueue(item, from); err != nil {
		return papucoin.Item{}, err
	}
	return item, nil
}

// enqueue puts a checked item on the scheduler. The caller holds the lock.
func (r *Runtime) enqueue(item papucoin.Item, origin string) error {
	if _, err := r.scheduler.Submit(r.papucoinID, svc.WorkItem{Payload: encodeItem(item), Origin: origin}); err != nil {
		return fmt.Errorf("the queue is full: %w", err)
	}
	return nil
}

// Faucet queues a claim on the chain's faucet from the node's own account.
//
// The amount is not the caller's to choose: the chain pays what its genesis says a
// faucet pays, and only once per address, so the item carries no amount of its own
// and the service decides. A node that accepted an amount here and quietly paid
// another would be lying to whoever asked for it.
func (r *Runtime) Faucet(to string) (papucoin.Item, error) {
	if r.bridgeKey == nil {
		return papucoin.Item{}, fmt.Errorf("the node has no bridge key, so it cannot pay for a payout")
	}
	normalized, err := papucoin.NormalizeAddress(to)
	if err != nil {
		return papucoin.Item{}, err
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	item, err := papucoin.SignItem(papucoin.Item{
		Method:       papucoin.MethodFaucet,
		Nonce:        r.bridgeNonce,
		To:           normalized,
		MustBeSigned: true,
	}, r.bridgeKey)
	if err != nil {
		return papucoin.Item{}, err
	}
	r.bridgeNonce++

	if _, err := r.scheduler.Submit(r.papucoinID, svc.WorkItem{Payload: encodeItem(item), Origin: "faucet"}); err != nil {
		return papucoin.Item{}, fmt.Errorf("the queue is full: %w", err)
	}
	return item, nil
}

// Pending reports how many items are queued and how many fit in a timeslot, so
// an operator can see the node falling behind.
func (r *Runtime) Pending() (queued, slots int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scheduler.Pending(), int(constants.MaxNumberOfItems)
}

// viewLocked returns a read view over the PAPU account. The caller holds at least
// the read lock.
func (r *Runtime) viewLocked() *papucoin.View {
	account, ok := r.state.Services[r.papucoinID]
	if !ok {
		account = service.NewServiceAccount()
	}
	return papucoin.NewView(r.papucoinID, &account)
}

// WeiPerRaw is how many wei one unit of PAPU is worth, which is what lets a client
// that counts in wei read a balance this chain counts in PAPU.
func (r *Runtime) WeiPerRaw() *big.Int { return r.weiPerRaw }

// Account returns a copy of a service account, for reads.
func (r *Runtime) Account(id block.ServiceId) (service.ServiceAccount, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	account, ok := r.state.Services[id]
	if !ok {
		return service.ServiceAccount{}, false
	}
	return account.Clone(), true
}

// View returns a read view over the PAPU account.
func (r *Runtime) View() *papucoin.View {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.viewLocked()
}

// encodeItem is the payload a PAPU item takes into refine.
func encodeItem(item papucoin.Item) []byte {
	payload, err := json.Marshal(item)
	if err != nil {
		// An Item is a struct of strings, a uint64 and two bools.
		panic(fmt.Sprintf("devnet: item is not marshalable: %v", err))
	}
	return payload
}
