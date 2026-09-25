package svc

import (
	"bytes"
	"fmt"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/crypto"
	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
)

// nativeContext is the native backend's implementation of [RefineContext] and
// [AccumulateContext]. It runs a handler in-process against a real
// [service.ServiceAccount], so any state it touches is the same state the PVM
// would have touched, and the same threshold-balance rule applies to writes.
type nativeContext struct {
	phase     Phase
	serviceID block.ServiceId
	timeslot  jamtime.Timeslot
	gasLimit  uint64
	gasUsed   uint64

	// account is the working copy of this service's account. Handlers mutate
	// it in place; it is only committed by the caller if the handler returns
	// without error.
	account *service.ServiceAccount
	// all gives cross-service read access. It is not written to: a service
	// can never mutate another service's account.
	all service.ServiceState

	emitted   []byte
	refined   []byte
	transfers []service.DeferredTransfer
	logs      []LogEntry
}

var (
	_ RefineContext     = (*nativeContext)(nil)
	_ AccumulateContext = (*nativeContext)(nil)
)

func (c *nativeContext) ServiceID() block.ServiceId { return c.serviceID }
func (c *nativeContext) Phase() Phase               { return c.phase }
func (c *nativeContext) Timeslot() jamtime.Timeslot { return c.timeslot }
func (c *nativeContext) Balance() uint64            { return c.account.Balance }

func (c *nativeContext) Footprint() (uint32, uint64) {
	return c.account.GetTotalNumberOfItems(), c.account.GetTotalNumberOfOctets()
}

func (c *nativeContext) Read(key []byte) ([]byte, bool, error) {
	return c.ReadFrom(c.serviceID, key)
}

func (c *nativeContext) ReadFrom(target block.ServiceId, key []byte) ([]byte, bool, error) {
	account := c.account
	if target != c.serviceID {
		other, exists := c.all[target]
		if !exists {
			return nil, false, fmt.Errorf("%w: %d", ErrUnknownService, target)
		}
		account = &other
	}

	k, err := statekey.NewStorage(target, key)
	if err != nil {
		return nil, false, err
	}

	value, ok := account.GetStorage(k)
	if !ok {
		return nil, false, nil
	}
	return bytes.Clone(value), true, nil
}

// Write stores value under key, rejecting the write if the resulting storage
// footprint costs more than the service's balance can cover. This mirrors the
// PVM write host call, including its treatment of a zero-length value as a
// deletion.
func (c *nativeContext) Write(key, value []byte) error {
	k, err := statekey.NewStorage(c.serviceID, key)
	if err != nil {
		return err
	}

	next := c.account.Clone()

	if len(value) == 0 {
		if previous, ok := c.account.GetStorage(k); ok {
			if err := next.DeleteStorage(k, uint64(len(key)), uint64(len(previous))); err != nil {
				return err
			}
		}
	} else if err := next.InsertStorage(k, uint64(len(key)), value); err != nil {
		return err
	}

	threshold, err := next.ThresholdBalance()
	if err != nil {
		return err
	}
	if threshold > next.Balance {
		return fmt.Errorf("%w: need %d, have %d", ErrStorageFull, threshold, next.Balance)
	}

	*c.account = next
	return nil
}

func (c *nativeContext) Remove(key []byte) error {
	return c.Write(key, nil)
}

func (c *nativeContext) Lookup(hash crypto.Hash) []byte {
	preimage, ok := c.account.PreimageLookup[hash]
	if !ok {
		return nil
	}
	return bytes.Clone(preimage)
}

// Transfer queues a balance movement. Transfers are deferred rather than
// applied in place, so the caller settles them once accumulation succeeds.
func (c *nativeContext) Transfer(t Transfer) error {
	if c.phase != PhaseAccumulate {
		return fmt.Errorf("%w: transfer", ErrWriteInRefine)
	}
	if t.Amount > c.account.Balance {
		return fmt.Errorf("%w: transfer of %d from balance %d", ErrInsufficientFunds, t.Amount, c.account.Balance)
	}

	memo := service.Memo{}
	if len(t.Memo) > len(memo) {
		return fmt.Errorf("memo of %d bytes exceeds %d", len(t.Memo), len(memo))
	}
	copy(memo[:], t.Memo)

	c.transfers = append(c.transfers, service.DeferredTransfer{
		SenderServiceIndex:   c.serviceID,
		ReceiverServiceIndex: t.Destination,
		Balance:              t.Amount,
		Memo:                 memo,
		GasLimit:             c.gasLimit,
	})

	return nil
}

func (c *nativeContext) Log(level LogLevel, msg string) {
	c.logs = append(c.logs, LogEntry{Level: level, Msg: msg})
}

func (c *nativeContext) Emit(payload []byte) error {
	if c.phase != PhaseRefine {
		return ErrEmittedDuringAccumulate
	}
	c.emitted = payload
	return nil
}

func (c *nativeContext) Gas() uint64 {
	if c.gasUsed >= c.gasLimit {
		return 0
	}
	return c.gasLimit - c.gasUsed
}

func (c *nativeContext) Refinement() []byte { return c.refined }
