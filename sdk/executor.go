package svc

import (
	"bytes"
	"fmt"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
)

// Result is the outcome of running one service handler.
type Result struct {
	// Report is what the handler emitted while refining, nil otherwise.
	Report []byte
	// Account is the service account after the handler ran. On error the
	// caller must discard it and keep the account it started from.
	Account service.ServiceAccount
	// Transfers are the balance movements the handler requested. They are
	// deferred and must be settled by the caller.
	Transfers []service.DeferredTransfer
	// Logs are the log lines the handler produced.
	Logs []LogEntry
	// GasUsed is what the guest actually consumed, not what it was allowed.
	// The PVM executor fills this in from what InvokeWholeProgram reports; the
	// native executor runs in Go and has no gas to meter, so it leaves it at
	// zero rather than inventing a number.
	GasUsed uint64
}

// Executor runs service handlers against real JAM state. Handlers always run
// against a clone of the service account, so a handler that returns an error
// leaves the caller's state untouched.
type Executor struct {
	registry *Registry
}

// NewExecutor returns an executor that resolves services from r.
func NewExecutor(r *Registry) *Executor {
	return &Executor{registry: r}
}

// Refine runs a service's refine handler over a single work item. The account
// returned is the refined account, which the caller commits only if err is nil.
func (e *Executor) Refine(id block.ServiceId, item []byte, timeslot jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error) {
	def, err := e.registry.Get(id)
	if err != nil {
		return Result{}, err
	}

	original, ok := all[id]
	if !ok {
		return Result{}, fmt.Errorf("%w: %d", ErrUnknownService, id)
	}

	working := original.Clone()
	ctx := &nativeContext{
		phase:     PhaseRefine,
		serviceID: id,
		timeslot:  timeslot,
		gasLimit:  gasLimit,
		account:   &working,
		all:       all,
	}

	result := Result{}
	if def.Refine == nil {
		return result, nil
	}

	report, err := def.Refine(ctx, item)
	if err != nil {
		return Result{}, fmt.Errorf("%s: refine: %w", def, err)
	}
	// A handler may set the report by returning it or by emitting it. Doing
	// both is only allowed when they agree, otherwise the report is
	// ambiguous and the item cannot be refined deterministically.
	if ctx.emitted != nil {
		if report != nil && !bytes.Equal(report, ctx.emitted) {
			return Result{}, fmt.Errorf("%s: refine: returned a report that differs from the emitted one", def)
		}
		report = ctx.emitted
	}

	result.Report = report
	result.Account = working
	result.Transfers = ctx.transfers
	result.Logs = ctx.logs
	return result, nil
}

// Initialize runs a service's init handler to seed its own state when its
// account is created. It takes no work items, so it is the only handler that
// runs outside the work cycle.
func (e *Executor) Initialize(id block.ServiceId, timeslot jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error) {
	def, err := e.registry.Get(id)
	if err != nil {
		return Result{}, err
	}
	if def.Init == nil {
		return Result{}, nil
	}

	original, ok := all[id]
	if !ok {
		return Result{}, fmt.Errorf("%w: %d", ErrUnknownService, id)
	}

	working := original.Clone()
	ctx := &nativeContext{
		phase:     PhaseAccumulate,
		serviceID: id,
		timeslot:  timeslot,
		gasLimit:  gasLimit,
		account:   &working,
		all:       all,
	}

	if err := def.Init(ctx); err != nil {
		return Result{}, fmt.Errorf("%s: init: %w", def, err)
	}

	return Result{Account: working, Transfers: ctx.transfers, Logs: ctx.logs}, nil
}

// Accumulate runs a service's accumulate handler over the items refined by the
// core's assigned set, then delivers any incoming transfers.
func (e *Executor) Accumulate(id block.ServiceId, items []RefinedItem, incoming []service.DeferredTransfer, timeslot jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error) {
	def, err := e.registry.Get(id)
	if err != nil {
		return Result{}, err
	}

	original, ok := all[id]
	if !ok {
		return Result{}, fmt.Errorf("%w: %d", ErrUnknownService, id)
	}

	working := original.Clone()
	result := Result{Account: working}

	if def.Accumulate != nil {
		ctx := &nativeContext{
			phase:     PhaseAccumulate,
			serviceID: id,
			timeslot:  timeslot,
			gasLimit:  gasLimit,
			account:   &working,
			all:       all,
		}

		out, err := def.Accumulate(ctx, items)
		if err != nil {
			return Result{}, fmt.Errorf("%s: accumulate: %w", def, err)
		}

		result.Report = out
		result.Transfers = ctx.transfers
		result.Logs = ctx.logs
	}

	if def.OnTransfer != nil {
		for _, transfer := range incoming {
			ctx := &nativeContext{
				phase:     PhaseAccumulate,
				serviceID: id,
				timeslot:  timeslot,
				gasLimit:  transfer.GasLimit,
				account:   &working,
				all:       all,
				refined:   result.Report,
			}

			if err := def.OnTransfer(ctx, transfer); err != nil {
				return Result{}, fmt.Errorf("%s: on_transfer: %w", def, err)
			}
			result.Transfers = append(result.Transfers, ctx.transfers...)
			result.Logs = append(result.Logs, ctx.logs...)
		}
	}

	// The handler may have grown the account, so settle transfers against the
	// final footprint rather than the one it started with.
	threshold, err := working.ThresholdBalance()
	if err != nil {
		return Result{}, fmt.Errorf("%s: threshold balance: %w", def, err)
	}
	if threshold > working.Balance {
		return Result{}, fmt.Errorf("%w: %s needs %d, has %d", ErrStorageFull, def, threshold, working.Balance)
	}

	result.Account = working
	return result, nil
}

// Runtime is what the chain needs from a service implementation, whether the
// logic is a Go handler or a polkavm guest. Both are driven the same way, so a
// service can be swapped between them without touching the state transition.
type Runtime interface {
	Refine(id block.ServiceId, item []byte, timeslot jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error)
	Initialize(id block.ServiceId, timeslot jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error)
	Accumulate(id block.ServiceId, items []RefinedItem, incoming []service.DeferredTransfer, timeslot jamtime.Timeslot, gasLimit uint64, all service.ServiceState) (Result, error)
}

var (
	_ Runtime = (*Executor)(nil)
	_ Runtime = (*PVMExecutor)(nil)
)
