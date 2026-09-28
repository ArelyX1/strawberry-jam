package svc

import (
	"fmt"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/pvm"
	"github.com/eigerco/strawberry/internal/pvm/host_call"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/work"
)

// PVMExecutor runs a service whose logic lives in a polkavm guest instead of a
// Go handler. The guest is a regular JAM service code blob: it is refined and
// accumulated through the PVM like any other, with the entry point and the ecall
// indices taken from the blob because the linker assigns both.
//
// Storage is the same service account the native handlers use, so a guest and a
// native service see identical state and can be swapped without migrating it.
type PVMExecutor struct {
	blob []byte
}

// NewPVMExecutor returns an executor for a guest blob.
func NewPVMExecutor(blob []byte) *PVMExecutor {
	return &PVMExecutor{blob: blob}
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

// Initialize satisfies the runtime interface. Seeding is the chain's job here:
// the PAPU economy's issuer, asset name and opening balances are written by the
// genesis the same way they are for the native service, so a guest and a native
// service start from identical state.
func (e *PVMExecutor) Initialize(id block.ServiceId, _ jamtime.Timeslot, _ uint64, all service.ServiceState) (Result, error) {
	account, ok := all[id]
	if !ok {
		return Result{}, fmt.Errorf("%w: %d", ErrUnknownService, id)
	}
	return Result{Account: account}, nil
}
