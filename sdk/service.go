// Package svc is a small SDK for authoring JAM services.
//
// A service is declared once as a [Service] value and registered under a
// service ID. Handlers are written against a [Context], whose method set
// mirrors the PVM host-call surface (read, write, lookup, info, transfer), so
// the same declaration can be executed by the native backend in this package
// today and by the PVM once the handler has been compiled to polkavm.
//
// Services are paid for out of their own balance and only consume coretime
// when they are asked to do work, so the natural unit of demand is one service
// ID with a queue of pending items. See [Scheduler].
package svc

import (
	"errors"
	"fmt"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
)

// Phase reports which half of the accumulate cycle a handler is running in.
type Phase uint8

const (
	// PhaseRefine is the deterministic, per-core validation pass. Refinement
	// may change the service's own storage and emit a report, and may not
	// transfer balance or touch another service's storage.
	PhaseRefine Phase = iota
	// PhaseAccumulate is the single agreed pass that applies refinement
	// results to state. It is the only phase that moves balance.
	PhaseAccumulate
)

func (p Phase) String() string {
	if p == PhaseRefine {
		return "refine"
	}
	return "accumulate"
}

// LogLevel is the severity of a service log entry, ordered so that it can be
// compared against a minimum level when filtering.
type LogLevel uint8

const (
	LogDebug LogLevel = iota
	LogError
	LogWarn
	LogInfo
	LogPanic
)

// Transfer is a balance movement requested by a service during accumulation.
type Transfer struct {
	Destination block.ServiceId
	Amount      uint64
	Memo        string
}

// LogEntry is a log line emitted by a service.
type LogEntry struct {
	Level LogLevel
	Msg   string
}

var (
	// ErrStorageFull is returned when a write would push the service's
	// storage footprint past what its balance can pay for.
	ErrStorageFull = errors.New("svc: storage write exceeds threshold balance")
	// ErrInsufficientFunds is returned when a transfer or gas reservation
	// would take the service below the balance it needs to keep.
	ErrInsufficientFunds = errors.New("svc: insufficient service balance")
	// ErrWriteInRefine is returned when a handler attempts to move balance
	// or write another service's storage while refining.
	ErrWriteInRefine = errors.New("svc: operation not allowed during refine")
	// ErrUnknownService is returned when a service ID has no account.
	ErrUnknownService = errors.New("svc: unknown service id")
	// ErrDuplicateID is returned when two services are registered under the
	// same ID.
	ErrDuplicateID = errors.New("svc: duplicate service id")
	// ErrNotFound is returned when a service ID is not registered.
	ErrNotFound = errors.New("svc: service not found")
	// ErrRefineOnly is returned when a refine-only operation is attempted
	// during accumulation.
	ErrRefineOnly = errors.New("svc: refine-only operation attempted during accumulate")
	// ErrAccumulateOnly is returned when an accumulate-only operation is
	// attempted during refinement.
	ErrAccumulateOnly = errors.New("svc: accumulate-only operation attempted during refine")
	// ErrEmittedDuringAccumulate is returned when a handler emits a report
	// while accumulating.
	ErrEmittedDuringAccumulate = errors.New("svc: cannot emit report during accumulate")
)

// Context is the API a service handler is written against. Every method that
// touches state is subject to the phase rules of [Phase], and every write is
// subject to the same threshold-balance check the PVM applies.
type Context interface {
	// ServiceID returns the ID of the service running the handler.
	ServiceID() block.ServiceId
	// Phase reports whether the handler is refining or accumulating.
	Phase() Phase
	// Timeslot is the timeslot being processed.
	Timeslot() jamtime.Timeslot
	// Balance is the service's current balance.
	Balance() uint64
	// Footprint returns the current item and octet counts, which together
	// determine the service's threshold balance.
	Footprint() (items uint32, octets uint64)

	// Read returns the value this service stores under key.
	Read(key []byte) ([]byte, bool, error)
	// Write stores value under key. A zero-length value removes the entry,
	// matching the PVM's write host call. The write is rejected with
	// [ErrStorageFull] if the resulting footprint exceeds what Balance can
	// pay for, leaving the account untouched.
	Write(key, value []byte) error
	// Remove deletes the entry stored under key, if any.
	Remove(key []byte) error
	// ReadFrom reads another service's storage. Reads are always allowed,
	// but the target must exist.
	ReadFrom(target block.ServiceId, key []byte) ([]byte, bool, error)
	// Lookup returns a preimage provided to this service, or nil.
	Lookup(hash crypto.Hash) []byte
	// Transfer moves amount to destination. It is only valid while
	// accumulating.
	Transfer(t Transfer) error
	// Log records a service log line.
	Log(level LogLevel, msg string)
}

// RefineContext extends [Context] with the operations that are only available
// while refining, namely emitting the report that the core will gossip.
type RefineContext interface {
	Context
	// Emit attaches a report payload to this work item. Reports are the
	// only outward-facing product of refinement.
	Emit(payload []byte) error
	// Gas is the gas left for this item.
	Gas() uint64
}

// AccumulateContext extends [Context] with the accumulation-only operations.
type AccumulateContext interface {
	Context
	// Refinement is the payload the core emitted for this item while
	// refining.
	Refinement() []byte
}

// Service is a declared service. Handlers may be nil, in which case the
// corresponding phase is a no-op and the service only exists to hold balance
// and storage.
type Service struct {
	// ID is the JAM service index. Prefer [Registry.Register] over setting
	// this by hand so that collisions are caught at startup.
	ID   block.ServiceId
	Name string

	// Refine validates and normalises a work item. It must be deterministic:
	// every core in the assigned set runs it and their reports are compared.
	Refine func(ctx RefineContext, item []byte) ([]byte, error)
	// Accumulate applies the aggregated result of refinement to state.
	Accumulate func(ctx AccumulateContext, items []RefinedItem) ([]byte, error)
	// OnTransfer is invoked for each balance transfer delivered to this
	// service during accumulation. It sees the transfer but cannot alter
	// its amount.
	OnTransfer func(ctx AccumulateContext, t service.DeferredTransfer) error
	// Init seeds the service's own state when its account is created. It runs
	// once, with no work items, and is how genesis configuration such as an
	// issuer or opening balances is applied. It is optional.
	Init func(ctx AccumulateContext) error
}

// RefinedItem pairs a work item with the report its core emitted for it.
type RefinedItem struct {
	// Item is the original work item.
	Item []byte
	// Report is what the core emitted while refining, or nil if the core
	// emitted nothing.
	Report []byte
}

// String renders a service for logs and error messages.
func (s *Service) String() string {
	return fmt.Sprintf("%s(id=%d)", s.Name, s.ID)
}
