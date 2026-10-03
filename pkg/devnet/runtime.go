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
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/constants"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/pvm"
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

	registry  *svc.Registry
	executor  svc.Runtime
	scheduler *svc.Scheduler
	// genesisServices is the state the chain started from, kept so that a switch
	// to another chain can put the runtime back at the beginning. A dev chain
	// replays from genesis in a few seconds, which is a fair price for not having
	// to keep a state per slot.
	genesisServices service.ServiceState
	// validators are the chain's validator set, kept so a rewind can restore it.
	// They come from the caller rather than from the genesis definition, which
	// describes the economy and not who is validating.
	validators []Validator
	// telemetry is the per timeslot record of what ran. It is written under mu
	// and read over the RPC, so an operator can see the work and the gas rather
	// than infer them from state roots.
	telemetry  *TelemetryStore
	papucoinID block.ServiceId
	trie       *store.Trie
	// stateDB is where the state is written after every timeslot, and read from at
	// startup. It is nil when the node was built without a store, which is the
	// case for tests that only care about a chain in memory.

	issuer     string
	bridgeKey  ed25519.PrivateKey
	bridgeAddr string
	// issued is the last number of the chain's own sequence that was handed to
	// an actor, and undecided counts the items handed out that no block has
	// ruled on yet. The chain accepts an item only when its number is the one it
	// expects next, and the state a node can read does not move until a block
	// carries it, so three transactions sent in quick succession would otherwise
	// all be handed the same number and two of them would be dropped without ever
	// being applied. Counting what this node has already handed out keeps the
	// sequence moving, and undecided is what lets a number be handed out again
	// when the item that carried it was refused.
	issued    map[string]uint64
	undecided map[string]int
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

	pvmGuest := false
	seedBalances := map[string]string{}
	var executor svc.Runtime = svc.NewExecutor(registry)
	if code := genesis.Service.Code; code != "" {
		blob, err := loadGuestBlob(code)
		if err != nil {
			return nil, fmt.Errorf("devnet: cannot load guest %q: %w", code, err)
		}
		// The guest needs its entry point and ecall indices resolved before it
		// runs, so fail here rather than at the first work item.
		if _, err := pvm.PrepareGuest(blob, ""); err != nil {
			return nil, fmt.Errorf("devnet: guest %q is not runnable: %w", code, err)
		}
		// The relay lets a wallet that only speaks Ethereum move balance: the
		// host verifies the signature and the guest applies the transfer.
		// The guest writes the genesis state itself, the way the native seed
		// does, so the two start from identical bytes. Without this the issuer
		// would hold nothing and the chain could not pay a single payout.
		seedBalances = balances
		pvmGuest = true
		executor = svc.NewPVMExecutor(blob).
			WithRelay(guestRelayAdapter{relay: relay}).
			WithSeed(svc.Seed{
				Issuer:   issuer,
				Symbol:   params.Symbol,
				Balances: seedBalances,
				ChainID:  relay.ChainID.Int64(),
			})
		log.Printf("jam running the economy as a polkavm guest: %s", code)
	}
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
	if pvmGuest {
		log.Printf("jam the guest wrote its genesis state: %d accounts in the seed, %d items in storage", len(seedBalances), seeded.Account.GetTotalNumberOfItems())
	}
	services[id] = seeded.Account

	if err := checkThreshold(services[id]); err != nil {
		return nil, fmt.Errorf("devnet: %s cannot pay for its genesis storage: %w", genesis.Service.Name, err)
	}

	validators := ValidatorState(opts.Validators)

	rt := &Runtime{
		genesis:         genesis,
		params:          params,
		state:           newStateWith(services, opts.Validators),
		registry:        registry,
		executor:        executor,
		scheduler:       scheduler,
		genesisServices: services.Clone(),
		validators:      append([]Validator(nil), opts.Validators...),
		papucoinID:      id,
		trie:            store.NewTrie(trieDB),
		issuer:          issuer,
		evmChainID:      relay.ChainID,
		weiPerRaw:       weiPerRaw(genesis.EVM.Decimals, params.Decimals),
		bridgeKey:       opts.BridgeKey,
		issued:          map[string]uint64{},
		undecided:       map[string]int{},
		telemetry:       NewTelemetryStore(),
		logf:            logf,
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

// FinishRebuild ends a rebuild, so the work the node queues from here on is work
// it was asked to do rather than work it is replaying.
//
// It deliberately does not forget the numbers this node has handed out. Those are
// its own bookkeeping, carried in memory, and a node that is running and adopts
// one block from a peer still has the right ones: they describe the chain it is
// already on. Wiping them there is what made two nodes on the same block report
// different state roots, because the nonce a following item gets is counted from
// them, so the two nodes wrote different items onto a chain they both agreed on.
// A node that has just started has nothing in memory worth keeping, and says so
// with ForgetHandedOut.
func (r *Runtime) FinishRebuild() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.replaying = false
}

// ForgetHandedOut drops the numbers this node has handed out and the count of the
// items still waiting for a block to settle them.
//
// This is for a node that has just started. Its state is read back out of the
// blocks it replays, and those numbers are part of that state, so a node that
// restarted keeping the ones it had in memory would hand out an item the chain
// has already spent a nonce on. A node that is running must not do this: its
// numbers describe the chain it is on.
func (r *Runtime) ForgetHandedOut() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issued = map[string]uint64{}
	r.undecided = map[string]int{}
	// The numbers this node handed out are not the only bookkeeping of its own
	// that a replay has to start without. Which service gets which core is decided
	// by a cursor in the scheduler that turns once per timeslot this node runs, so
	// a node that has been running replays the chain over a cursor that is nowhere
	// near where a node which had only replayed would be. The services all still
	// run and still run in the same timeslots, but in a different order among
	// themselves, and that is enough to settle to a different state: the node
	// rebuilds the chain, gets a root no block names, and refuses to build on it.
	r.scheduler.ForgetCoretime()
}

// nextNonceLocked hands out the next number of the chain's own sequence for an
// actor, and records that it did. The number the chain expects is the one it
// stored, which only moves when a block carries an item, so an account that
// sends twice before the next block would be given the same number twice and
// the chain would keep one and drop the other. Anything this node handed out
// that the state has not caught up with is counted on top, so the numbers go up
// one at a time and no item is lost.
func (r *Runtime) nextNonceLocked(actor string) (uint64, error) {
	stored, err := r.viewLocked().Nonce(actor)
	if err != nil {
		return 0, err
	}
	if stored == 0 {
		stored = r.params.FirstNonce
	}

	next := stored
	if last, ok := r.issued[actor]; ok && last >= stored {
		next = last + 1
	}
	r.issued[actor] = next
	r.undecided[actor]++
	return next, nil
}

// settleNoncesLocked runs once per timeslot, when everything queued before this
// point has been through a block and the state says which of those items the
// chain took. A number whose item was refused never got stored, so handing it
// out again is what keeps one refused transfer from pushing every later one out
// of step with the chain.
func (r *Runtime) settleNoncesLocked() {
	for actor, waiting := range r.undecided {
		if waiting == 0 {
			continue
		}
		stored, err := r.viewLocked().Nonce(actor)
		if err != nil {
			r.logf("cannot read the nonce of %s: %v", actor, err)
			continue
		}
		if stored == 0 {
			stored = r.params.FirstNonce
		}
		if last, ok := r.issued[actor]; ok && last >= stored {
			// The chain is still behind the numbers this node handed out, so the
			// items carrying them were refused. The number the chain expects is
			// free again.
			r.issued[actor] = stored - 1
		}
		r.undecided[actor] = 0
	}
}

// syncBridgeNonceLocked takes the bridge account's next nonce from the state, so
func (r *Runtime) runLocked(timeslot jamtime.Timeslot) ([]BlockWork, error) {
	// Everything queued before this point has been through a block by now, so
	// this is where the numbers this node handed out get settled against what the
	// chain actually kept.
	r.settleNoncesLocked()
	r.fundBridgeLocked()

	// The timeslot is part of the state, so the root of a chain that is running
	// moves even in a timeslot that settles no work.
	r.state.TimeslotIndex = timeslot

	var work []BlockWork
	rec := TimeslotRecord{Timeslot: timeslot}

	// A timeslot hands its cores out once, and every assigned service runs
	// refine then accumulate on the state it is given.
	assignments := r.scheduler.Plan(int(constants.TotalNumberOfCores))
	rec.Assignments = len(assignments)
	for _, assignment := range assignments {
		bytes := 0
		for _, item := range assignment.Items {
			bytes += len(item.Payload)
		}
		rec.Services = append(rec.Services, Work{
			ServiceID: assignment.ServiceID,
			Items:     len(assignment.Items),
			Bytes:     bytes,
		})

		// The work a service was given is the work its block names, whether or not
		// it survives: a refused item is still an item the timeslot carried.
		for _, item := range assignment.Items {
			work = append(work, BlockWork{ServiceID: assignment.ServiceID, Payload: item.Payload})
		}

		out, err := r.runServiceLocked(assignment, timeslot)
		if err != nil {
			// A service that fails leaves its items queued rather than losing
			// them, because nothing was charged to anybody.
			r.logf("service %d did not run in timeslot %d: %v", assignment.ServiceID, timeslot, err)
			rec.Errors = append(rec.Errors, fmt.Sprintf("service %d: %v", assignment.ServiceID, err))
			rec.Failed++
		}
		rec.RefineCalls += out.RefineCalls
		rec.AccumCalls++
		rec.RefineGas += out.RefineGas
		rec.AccumulateGas += out.AccumulateGas
		if out.Refined > 0 {
			rec.Refined += out.Refined
		}
		if out.Accumulated {
			rec.Accumulated++
		}
		r.scheduler.Settle([]svc.Assignment{assignment})
	}

	// The economy's service is assigned only when it has work queued, because
	// that is what a core is for. But its transfer price is a price and a price
	// that only ever goes up is not one: after a single busy stretch the chain
	// would stay expensive forever, with nothing about the chain having changed
	// to justify it. So it is run once per timeslot regardless, with nothing to
	// do, and that empty block is the quiet block the price needs to hear about.
	if !assignedThisSlot(r.papucoinID, assignments) {
		result, err := r.executor.Accumulate(
			r.papucoinID, nil, r.scheduler.Transfers(r.papucoinID), timeslot, constants.MaxAllocatedGasAccumulation, r.state.Services,
		)
		rec.AccumCalls++
		rec.AccumulateGas += result.GasUsed
		if err != nil {
			r.logf("the economy service did not settle its price in timeslot %d: %v", timeslot, err)
			rec.Errors = append(rec.Errors, fmt.Sprintf("economy service: %v", err))
		} else if err := checkThreshold(result.Account); err != nil {
			r.logf("the economy service failed its threshold in timeslot %d: %v", timeslot, err)
			rec.Errors = append(rec.Errors, fmt.Sprintf("economy threshold: %v", err))
		} else {
			r.state.Services[r.papucoinID] = result.Account
			rec.Accumulated++
		}
	}

	r.updateRoot()
	r.telemetry.Record(rec)
	return work, nil
}

// assignedThisSlot reports whether the scheduler gave a service a core this
// timeslot, which is not the same question as whether it had anything to do.
func assignedThisSlot(id block.ServiceId, assignments []svc.Assignment) bool {
	for _, a := range assignments {
		if a.ServiceID == id {
			return true
		}
	}
	return false
}

// serviceOutcome is what one service's run cost, so the timeslot record can
// total it. It is returned even when the run failed: the gas is gone either way.
type serviceOutcome struct {
	RefineCalls   uint64
	Refined       int
	AccumulateGas uint64
	RefineGas     uint64
	Accumulated   bool
}

// runServiceLocked refines and accumulates one service, leaving the state untouched
// if either stage fails.
func (r *Runtime) runServiceLocked(assignment svc.Assignment, timeslot jamtime.Timeslot) (serviceOutcome, error) {
	var out serviceOutcome
	account, ok := r.state.Services[assignment.ServiceID]
	if !ok {
		return out, fmt.Errorf("the service has no account")
	}

	refined := make([]svc.RefinedItem, 0, len(assignment.Items))
	for _, item := range assignment.Items {
		result, err := r.executor.Refine(assignment.ServiceID, item.Payload, timeslot, maxGas, r.state.Services)
		out.RefineCalls++
		out.RefineGas += result.GasUsed
		if err != nil {
			// A refused item is an ordinary outcome, not a chain fault.
			r.logf("service %d refused an item from %q: %v", assignment.ServiceID, item.Origin, err)
			continue
		}
		out.Refined++
		// A guest that emits nothing, or emits a refusal of its own, is saying
		// why in the report; without this the operator only sees a balance that
		// did not move.
		if len(result.Report) == 0 {
			r.logf("service %d declined an item from %q without giving a reason", assignment.ServiceID, item.Origin)
		} else {
			r.logf("service %d refined an item from %q into %s", assignment.ServiceID, item.Origin, result.Report)
		}
		refined = append(refined, svc.RefinedItem{Item: item.Payload, Report: result.Report})
	}

	result, err := r.executor.Accumulate(assignment.ServiceID, refined, r.scheduler.Transfers(assignment.ServiceID), timeslot, maxGas, r.state.Services)
	out.AccumulateGas += result.GasUsed
	if err != nil {
		return out, fmt.Errorf("accumulate: %w", err)
	}
	if err := checkThreshold(result.Account); err != nil {
		return out, err
	}
	out.Accumulated = true

	account = result.Account
	r.state.Services[assignment.ServiceID] = account

	// The bridge signs payouts with a nonce of its own choosing, and the service
	// only accepts the one it expects next. Numbers this node handed out are
	// settled here too, because this is the point where what the chain kept is
	// known: a number whose item was refused is free to be handed out again.
	r.settleNoncesLocked()
	return out, nil
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
	// Welcome and burn are claims a holder can make about itself, so they belong
	// here as much as a transfer does. Mint stays out: only the issuer may mint,
	// and the issuer is the chain itself, not whoever submits an item.
	switch item.Method {
	case papucoin.MethodTransfer, papucoin.MethodFaucet, papucoin.MethodWelcome, papucoin.MethodBurn:
	default:
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
	// The number this item carries comes out of a counter this node keeps, so the
	// lock is taken before it is read and held until the item is queued.
	r.mu.Lock()
	defer r.mu.Unlock()

	// An account has two sequences of numbers and this node is the one that keeps
	// the chain's: the wallet keeps the Ethereum nonce, and the chain keeps its own
	// so that a replayed transaction cannot be a second transfer. The transaction's
	// nonce is checked by the wallet and by the service's EVM nonce record, so what
	// this item has to carry is the next number of the chain's sequence, which is
	// the number the service stored the last time the account was used, plus the
	// numbers this node has already handed out and no block has ruled on yet. An
	// account the chain has never used has not stored one, and the number it
	// expects then is the first nonce of the parameters.
	nonce, err := r.nextNonceLocked(from)
	if err != nil {
		return papucoin.Item{}, err
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

	// A payout is once per address, and the service refuses the second one. The
	// service also only advances a sender's nonce for the items it accepts, so
	// queueing an item it is going to refuse would burn a nonce here that it
	// never claims: the next payout would be numbered one too high, the service
	// would drop that too, and from then on no address could be funded at all.
	// Asking first also turns a silent no-op into an answer the caller can use.
	claimed, err := r.viewLocked().FaucetClaimed(normalized)
	if err != nil {
		return papucoin.Item{}, err
	}
	if claimed {
		return papucoin.Item{}, fmt.Errorf("%s has already taken its payout from the faucet", normalized)
	}

	// The payout is an item like any other, so it takes its number from the same
	// place every item does. That is what lets a payout that the chain refused
	// give its number back instead of pushing every later one out of step.
	nonce, err := r.nextNonceLocked(r.bridgeAddr)
	if err != nil {
		return papucoin.Item{}, err
	}

	item, err := papucoin.SignItem(papucoin.Item{
		Method:       papucoin.MethodFaucet,
		Nonce:        nonce,
		To:           normalized,
		MustBeSigned: true,
	}, r.bridgeKey)
	if err != nil {
		return papucoin.Item{}, err
	}
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

// loadGuestBlob reads a guest blob named by the genesis. A path is taken as a
// path; anything else is resolved against the guest directory, so a genesis can
// ship a bare file name.
func loadGuestBlob(name string) ([]byte, error) {
	candidates := []string{name}
	if !filepath.IsAbs(name) {
		candidates = append(candidates, filepath.Join(guestDir, name))
	}
	for _, candidate := range candidates {
		if blob, err := os.ReadFile(candidate); err == nil {
			return blob, nil
		}
	}
	return nil, fmt.Errorf("no blob at %v", candidates)
}

var guestDir = "guests"

// guestRelayAdapter narrows the service's relay to what a guest needs: just the
// address. The guest reads the rest out of the transaction itself, so passing a
// whole struct across would be redundant. It lives here rather than in the SDK
// because papucoin already imports the SDK, and importing it back would be a
// cycle.
type guestRelayAdapter struct {
	relay *papucoin.EVMRelay
}

func (a guestRelayAdapter) Recover(rawHex string) (string, error) {
	fields, err := a.relay.Recover(rawHex)
	if err != nil {
		return "", err
	}
	return fields.From, nil
}

// ServiceAccount returns a copy of a service's account, and whether it exists.
func (r *Runtime) ServiceAccount(id block.ServiceId) (service.ServiceAccount, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	acc, ok := r.state.Services[id]
	return acc, ok
}

// QueueDepth reports how much work is waiting to be refined, and how many cores
// the timeslot will hand out.
func (r *Runtime) QueueDepth() (int, int) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.scheduler.Pending(), int(constants.TotalNumberOfCores)
}

// IsScheduled reports whether the scheduler is currently holding work for a
// service, which is what stops a service that is already running from being told
// it may join again.
func (r *Runtime) IsScheduled(id block.ServiceId) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, rec := range r.telemetry.History(1) {
		for _, w := range rec.Services {
			if w.ServiceID == id {
				return true
			}
		}
	}
	return false
}

// Rewind puts the runtime back at the state the chain started from.
//
// This is what a switch between chains needs. When a node adopts a chain written
// by somebody else, the state it is holding describes a branch that is about to
// be abandoned, and there is no way forward from there: the work that produced
// it cannot be un-run. So the runtime goes back to genesis and the canonical
// chain is replayed over it, which is exactly what a node does at startup, and
// gives the same answer for the same reason.
//
// The cost is a full replay, a few seconds on the dev chain. The alternative
// would be a state per slot, which is a lot of memory to save a replay that
// happens when a chain actually changes, and a chain does not change often.
//
// Anything the node handed out and the abandoned chain never settled is
// forgotten, because the numbers that mattered were the ones the chain it
// followed accepted. Keeping them would make the node sign an item the chain has
// already spent a nonce on.
func (r *Runtime) Rewind() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.state = newStateWith(r.genesisServices.Clone(), r.validators)
	r.state.ValidatorState = ValidatorState(r.validators)
	// The scheduler is rebuilt rather than emptied: what it holds is work queued
	// for the branch being abandoned, and carrying that over would execute a
	// service's work against a state it was never queued for.
	r.scheduler = svc.NewScheduler(r.registry, int(constants.MaxNumberOfItems))
	r.issued = map[string]uint64{}
	r.undecided = map[string]int{}
	r.updateRoot()
	return nil
}
