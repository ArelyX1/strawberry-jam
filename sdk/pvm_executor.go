package svc

import (
	"encoding/json"
	"fmt"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/pvm"
	"github.com/eigerco/strawberry/internal/pvm/host_call"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/work"
)

// Relay is what a guest needs from the host to accept a relayed EVM transfer:
// given the raw transaction, the recovered sender. The guest does not verify
// the signature itself, because curve arithmetic in a program without a
// standard library is a trap with no upside here. The host is the chain, and
// this program is code the chain chose to run, so the chain vouching for the
// sender is the whole trust model rather than a hole in it.
type Relay interface {
	Recover(rawHex string) (from string, err error)
}

// PVMExecutor runs a service whose logic lives in a polkavm guest instead of a
// Go handler. The guest is a regular JAM service code blob: it is refined and
// accumulated through the PVM like any other, with the entry point and the ecall
// indices taken from the blob because the linker assigns both.
//
// Storage is the same service account the native handlers use, so a guest and a
// native service see identical state and can be swapped without migrating it.
type PVMExecutor struct {
	blob  []byte
	relay Relay
	seed  *Seed
}

// NewPVMExecutor returns an executor for a guest blob. Without a relay a
// relayed transfer is refused, which is the safe default: an item that claims
// to come from an Ethereum wallet must not be taken on trust.
func NewPVMExecutor(blob []byte) *PVMExecutor {
	return &PVMExecutor{blob: blob}
}

// WithRelay returns the executor with a relay attached, so a guest can accept
// transactions from wallets that only speak Ethereum.
func (e *PVMExecutor) WithRelay(relay Relay) *PVMExecutor {
	e.relay = relay
	return e
}

// attachRelay verifies a relayed item and states the recovered sender in it,
// under a field the guest reads. The claimed sender is left alone: the guest
// ignores it and uses this one, because only the host's answer means anything.
func (e *PVMExecutor) attachRelay(item []byte) ([]byte, error) {
	var envelope struct {
		Raw string `json:"raw"`
	}
	if err := json.Unmarshal(item, &envelope); err != nil {
		return item, nil
	}
	if envelope.Raw == "" {
		return item, nil
	}
	if e.relay == nil {
		return nil, fmt.Errorf("relayed transfers need a relay verifier")
	}
	from, err := e.relay.Recover(envelope.Raw)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(item, &fields); err != nil {
		return nil, err
	}
	recovered, err := json.Marshal(from)
	if err != nil {
		return nil, err
	}
	fields["evmSender"] = recovered
	return json.Marshal(fields)
}

// prepare validates the blob and resolves what the linker decided.
func (e *PVMExecutor) prepare() (*pvm.GuestProgram, []byte, error) {
	guest, err := pvm.PrepareGuest(e.blob, "")
	if err != nil {
		return nil, nil, fmt.Errorf("preparing guest: %w", err)
	}
	code, err := guest.Code()
	if err != nil {
		return nil, nil, fmt.Errorf("framing guest: %w", err)
	}
	return guest, code, nil
}

// Refine runs the guest over one work item. The guest returns its report as the
// PVM halt result, which is exactly what a refine handler emits.
func (e *PVMExecutor) Refine(id block.ServiceId, item []byte, _ jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error) {
	guest, code, err := e.prepare()
	if err != nil {
		return Result{}, err
	}
	prepared, err := e.attachRelay(item)
	if err != nil {
		return Result{}, err
	}
	item = prepared
	original, ok := all[id]
	if !ok {
		return Result{}, fmt.Errorf("%w: %d", ErrUnknownService, id)
	}
	working := original.Clone()
	serviceState := all.Clone()
	serviceState[id] = working

	call := func(ecall uint64, gas pvm.Gas, regs pvm.Registers, mem pvm.Memory, x pvm.RefineContextPair) (pvm.Gas, pvm.Registers, pvm.Memory, pvm.RefineContextPair, error) {
		hostCall, ok := guest.HostCallID(ecall)
		if !ok {
			return gas, regs, mem, x, pvm.ErrPanicf("unknown ecall %d", ecall)
		}
		var err error
		switch hostCall {
		case uint64(host_call.GasID):
			gas, regs, err = host_call.GasRemaining(gas, regs)
		case uint64(host_call.ReadID):
			gas, regs, mem, err = host_call.Read(gas, regs, mem, working, id, serviceState)
		case uint64(host_call.WriteID):
			var acct service.ServiceAccount
			gas, regs, mem, acct, err = host_call.Write(gas, regs, mem, working, id)
			working = acct
			serviceState[id] = acct
		}
		return gas, regs, mem, x, err
	}

	_, report, _, err := pvm.InvokeWholeProgram(code, guest.Entry, pvm.UGas(gasLimit), item, call, pvm.RefineContextPair{
		IntegratedPVMMap: make(map[uint64]pvm.IntegratedPVM),
		Segments:         []work.Segment{},
	})
	if err != nil {
		return Result{}, fmt.Errorf("refine guest: %w", err)
	}
	if len(report) == 0 {
		// An empty report means the guest declined the item, which is a valid
		// outcome and not a failure.
		return Result{Account: working}, nil
	}
	return Result{Report: report, Account: working}, nil
}

// Accumulate applies the refined reports. They are concatenated into one
// argument buffer terminated by a NUL, which is how one export serves both
// phases: the guest reads the trailing zero as "this is accumulate".
func (e *PVMExecutor) Accumulate(id block.ServiceId, items []RefinedItem, _ []service.DeferredTransfer, _ jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error) {
	guest, code, err := e.prepare()
	if err != nil {
		return Result{}, err
	}
	original, ok := all[id]
	if !ok {
		return Result{}, fmt.Errorf("%w: %d", ErrUnknownService, id)
	}
	working := original.Clone()
	serviceState := all.Clone()
	serviceState[id] = working

	var args []byte
	for _, item := range items {
		if len(item.Report) == 0 {
			continue
		}
		args = append(args, item.Report...)
	}
	args = append(args, 0)

	call := func(ecall uint64, gas pvm.Gas, regs pvm.Registers, mem pvm.Memory, x pvm.AccumulateContextPair) (pvm.Gas, pvm.Registers, pvm.Memory, pvm.AccumulateContextPair, error) {
		hostCall, ok := guest.HostCallID(ecall)
		if !ok {
			return gas, regs, mem, x, pvm.ErrPanicf("unknown ecall %d", ecall)
		}
		var err error
		switch hostCall {
		case uint64(host_call.GasID):
			gas, regs, err = host_call.GasRemaining(gas, regs)
		case uint64(host_call.ReadID):
			gas, regs, mem, err = host_call.Read(gas, regs, mem, working, id, serviceState)
		case uint64(host_call.WriteID):
			var acct service.ServiceAccount
			gas, regs, mem, acct, err = host_call.Write(gas, regs, mem, working, id)
			working = acct
			serviceState[id] = acct
		}
		return gas, regs, mem, x, err
	}

	if _, _, _, err := pvm.InvokeWholeProgram(code, guest.Entry, pvm.UGas(gasLimit), args, call, pvm.AccumulateContextPair{}); err != nil {
		return Result{}, fmt.Errorf("accumulate guest: %w", err)
	}
	return Result{Account: working}, nil
}

// Seed is what a guest needs in order to write the genesis state of the economy
// into its own storage: who may mint, what the asset is called, and the opening
// balances. The native service gets this by running its seed handler; a guest
// has no way to receive it otherwise, and a chain whose issuer starts at zero
// cannot pay its first faucet.
type Seed struct {
	Issuer   string
	Symbol   string
	Balances map[string]string
	// ChainID is the chain this node presents to Ethereum wallets. A transaction
	// signed for any other chain must not be replayed here, and the guest has to
	// know which one this is to tell.
	ChainID int64
}

// Initialize hands the seed to the guest, which writes it exactly as the native
// seed writes it, so the two start from byte-identical state.
func (e *PVMExecutor) Initialize(id block.ServiceId, _ jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error) {
	account, ok := all[id]
	if !ok {
		return Result{}, fmt.Errorf("%w: %d", ErrUnknownService, id)
	}
	if e.seed == nil {
		// Without a seed there is nothing to write. Returning the account
		// untouched would leave the chain with an issuer that holds nothing,
		// which is a chain that cannot pay a single payout, so this is an error
		// the operator needs to see rather than an empty balance discovered
		// later.
		return Result{}, fmt.Errorf("a guest economy needs a seed to write the genesis state")
	}

	guest, code, err := e.prepare()
	if err != nil {
		return Result{}, err
	}
	payload, err := json.Marshal(e.seed)
	if err != nil {
		return Result{}, err
	}

	working := account.Clone()
	serviceState := all.Clone()
	serviceState[id] = working

	call := func(ecall uint64, gas pvm.Gas, regs pvm.Registers, mem pvm.Memory, x pvm.AccumulateContextPair) (pvm.Gas, pvm.Registers, pvm.Memory, pvm.AccumulateContextPair, error) {
		hostCall, ok := guest.HostCallID(ecall)
		if !ok {
			return gas, regs, mem, x, pvm.ErrPanicf("unknown ecall %d", ecall)
		}
		var err error
		switch hostCall {
		case uint64(host_call.GasID):
			gas, regs, err = host_call.GasRemaining(gas, regs)
		case uint64(host_call.ReadID):
			gas, regs, mem, err = host_call.Read(gas, regs, mem, working, id, serviceState)
		case uint64(host_call.WriteID):
			var acct service.ServiceAccount
			gas, regs, mem, acct, err = host_call.Write(gas, regs, mem, working, id)
			working = acct
			serviceState[id] = acct
		}
		return gas, regs, mem, x, err
	}

	// The trailing NUL is the accumulate marker, so a seed is written through
	// the same entry the reports go through.
	args := append(payload, 0)
	if _, _, _, err := pvm.InvokeWholeProgram(code, guest.Entry, pvm.UGas(gasLimit), args, call, pvm.AccumulateContextPair{}); err != nil {
		return Result{}, fmt.Errorf("seeding guest: %w", err)
	}
	return Result{Account: working}, nil
}

// WithSeed returns the executor with the genesis state it should write.
func (e *PVMExecutor) WithSeed(seed Seed) *PVMExecutor {
	e.seed = &seed
	return e
}
