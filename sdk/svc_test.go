package svc

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/service"
	"github.com/eigerco/strawberry/internal/state/serialization/statekey"
)

func mustStorageKey(t *testing.T, id block.ServiceId, key []byte) statekey.StateKey {
	t.Helper()
	k, err := statekey.NewStorage(id, key)
	require.NoError(t, err)
	return k
}

func testAccount(balance uint64) service.ServiceState {
	account := service.NewServiceAccount()
	account.Balance = balance
	return service.ServiceState{0: account, 1: account}
}

func TestRegistryAllocatesDistinctIDs(t *testing.T) {
	r := NewRegistry()

	first := r.MustRegister(Service{Name: "a"})
	second := r.MustRegister(Service{Name: "b"})

	assert.Equal(t, block.ServiceId(0), first)
	assert.Equal(t, block.ServiceId(1), second)
	assert.Equal(t, 2, r.Len())
	assert.Equal(t, []block.ServiceId{0, 1}, r.IDs())
}

func TestRegistryRejectsDuplicatesAndAnonyms(t *testing.T) {
	r := NewRegistry()
	require.NoError(t, r.RegisterAt(Service{Name: "a"}, 7))

	err := r.RegisterAt(Service{Name: "b"}, 7)
	assert.ErrorIs(t, err, ErrDuplicateID)

	assert.Error(t, r.RegisterAt(Service{Name: ""}, 9))
}

func TestRegistryGetUnknown(t *testing.T) {
	r := NewRegistry()
	_, err := r.Get(3)
	assert.ErrorIs(t, err, ErrNotFound)
}

func TestWriteRejectsStorageBeyondBalance(t *testing.T) {
	// The basic minimum balance is 100, so a service holding exactly 100 can
	// only afford an empty footprint.
	state := service.ServiceState{0: func() service.ServiceAccount {
		a := service.NewServiceAccount()
		a.Balance = service.BasicMinimumBalance
		return a
	}()}

	r := NewRegistry()
	r.MustRegister(Service{
		Name: "writer",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			return nil, ctx.Write([]byte("k"), make([]byte, 64))
		},
	})

	_, err := NewExecutor(r).Accumulate(0, nil, nil, 0, 0, state)
	assert.ErrorIs(t, err, ErrStorageFull)
}

func TestWriteAndRemoveAccountForFootprint(t *testing.T) {
	state := service.ServiceState{0: func() service.ServiceAccount {
		a := service.NewServiceAccount()
		a.Balance = 1_000_000
		return a
	}()}

	var before service.ServiceAccount
	r := NewRegistry()
	r.MustRegister(Service{
		Name: "writer",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			before = ctx.(*nativeContext).account.Clone()
			return nil, ctx.Write([]byte("k"), []byte("v"))
		},
	})

	result, err := NewExecutor(r).Accumulate(0, nil, nil, 0, 0, state)
	require.NoError(t, err)

	items, octets := before.GetTotalNumberOfItems(), before.GetTotalNumberOfOctets()
	newItems, newOctets := result.Account.GetTotalNumberOfItems(), result.Account.GetTotalNumberOfOctets()
	assert.Greater(t, newItems, items)
	assert.Greater(t, newOctets, octets)

	// A zero-length write deletes, restoring the footprint exactly. The
	// deleting service must use the same ID, since storage is namespaced per
	// service and the state key is derived from it.
	del := NewRegistry()
	del.MustRegister(Service{
		Name: "remover",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			return nil, ctx.Write([]byte("k"), nil)
		},
	})
	cleared, err := NewExecutor(del).Accumulate(0, nil, nil, 0, 0, service.ServiceState{0: result.Account})
	require.NoError(t, err)
	assert.Equal(t, items, cleared.Account.GetTotalNumberOfItems())
	assert.Equal(t, octets, cleared.Account.GetTotalNumberOfOctets())
}

func TestReadRoundTrip(t *testing.T) {
	r := NewRegistry()
	var readBack []byte
	var found bool
	r.MustRegister(Service{
		Name: "rw",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			require.NoError(t, ctx.Write([]byte("alpha"), []byte("beta")))
			var err error
			readBack, found, err = ctx.Read([]byte("alpha"))
			return nil, err
		},
	})

	state := service.ServiceState{0: func() service.ServiceAccount {
		a := service.NewServiceAccount()
		a.Balance = 1_000_000
		return a
	}()}

	_, err := NewExecutor(r).Accumulate(0, nil, nil, 0, 0, state)
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, []byte("beta"), readBack)
}

func TestCrossServiceReadOnlyReachesExistingServices(t *testing.T) {
	state := testAccount(1_000_000)
	peer := state[1]
	require.NoError(t, peer.InsertStorage(mustStorageKey(t, 1, []byte("k")), 1, []byte("v")))
	state[1] = peer

	r := NewRegistry()
	var got []byte
	var err error
	r.MustRegister(Service{
		Name: "reader",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			got, _, err = ctx.ReadFrom(1, []byte("k"))
			return nil, err
		},
	})
	_, err = NewExecutor(r).Accumulate(0, nil, nil, 0, 0, state)
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), got)

	// An unknown target is an error, not a silent miss.
	prober := r.MustRegister(Service{
		Name: "reader2",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			_, _, err := ctx.ReadFrom(99, []byte("k"))
			return nil, err
		},
	})
	_, err = NewExecutor(r).Accumulate(prober, nil, nil, 0, 0, state)
	assert.ErrorIs(t, err, ErrUnknownService)
}

func TestTransferForbiddenDuringRefine(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Service{
		Name: "mover",
		Refine: func(ctx RefineContext, _ []byte) ([]byte, error) {
			return nil, ctx.Transfer(Transfer{Destination: 1, Amount: 1})
		},
	})

	_, err := NewExecutor(r).Refine(0, nil, 0, 0, testAccount(1_000_000))
	assert.ErrorIs(t, err, ErrWriteInRefine)
}

func TestRefineEmitsReportAndIsolatesFailures(t *testing.T) {
	state := testAccount(1_000_000)
	own := state[0]
	require.NoError(t, own.InsertStorage(mustStorageKey(t, 0, []byte("k")), 1, []byte("original")))
	state[0] = own

	r := NewRegistry()
	r.MustRegister(Service{
		Name: "refiner",
		Refine: func(ctx RefineContext, item []byte) ([]byte, error) {
			_ = ctx.Write([]byte("k"), append([]byte("refined:"), item...))
			return []byte("report"), ctx.Emit([]byte("emitted"))
		},
	})

	ex := NewExecutor(r)

	// A handler that both returns and emits is ambiguous and must be rejected.
	_, err := ex.Refine(0, []byte("x"), 0, 0, state)
	assert.Error(t, err)

	ok := r.MustRegister(Service{
		Name: "ok",
		Refine: func(ctx RefineContext, _ []byte) ([]byte, error) {
			return nil, ctx.Emit([]byte("emitted"))
		},
	})
	res, err := ex.Refine(ok, []byte("x"), 0, 0, service.ServiceState{ok: state[0]})
	require.NoError(t, err)
	assert.Equal(t, []byte("emitted"), res.Report)
}

func TestFailedHandlerLeavesStateUntouched(t *testing.T) {
	state := testAccount(1_000_000)

	r := NewRegistry()
	r.MustRegister(Service{
		Name: "bad",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			if err := ctx.Write([]byte("k"), make([]byte, 128)); err != nil {
				return nil, err
			}
			return nil, errors.New("boom")
		},
	})

	original := state[0]
	snapshot := original.Clone()
	_, err := NewExecutor(r).Accumulate(0, nil, nil, 0, 0, state)
	assert.Error(t, err)

	// The caller's state map must be untouched, since handlers run on clones.
	untouched := state[0]
	assert.Equal(t, snapshot.GetTotalNumberOfOctets(), untouched.GetTotalNumberOfOctets())
	_, present := untouched.GetStorage(mustStorageKey(t, 0, []byte("k")))
	assert.False(t, present)
}

func TestAccumulateCannotEmit(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Service{
		Name: "e",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			return nil, ctx.(*nativeContext).Emit([]byte("nope"))
		},
	})
	_, err := NewExecutor(r).Accumulate(0, nil, nil, 0, 0, testAccount(1_000_000))
	assert.ErrorIs(t, err, ErrEmittedDuringAccumulate)
}

func TestStorageIsNamespacedPerService(t *testing.T) {
	// Two services may use the same key without colliding, because the state
	// key is derived from the writing service's ID.
	state := testAccount(1_000_000)

	writer := NewRegistry()
	writer.MustRegister(Service{
		Name: "writer",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			return nil, ctx.Write([]byte("shared"), []byte("mine"))
		},
	})
	_, err := NewExecutor(writer).Accumulate(0, nil, nil, 0, 0, state)
	require.NoError(t, err)

	peek := NewRegistry()
	var seen []byte
	var found bool
	require.NoError(t, peek.RegisterAt(Service{
		Name: "peek",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			var err error
			seen, found, err = ctx.Read([]byte("shared"))
			return nil, err
		},
	}, 1))
	// Service 1 shares the key name but must not see service 0's value.
	_, err = NewExecutor(peek).Accumulate(1, nil, nil, 0, 0, state)
	require.NoError(t, err)
	assert.False(t, found)
	assert.Nil(t, seen)
}

func TestOnTransferReceivesTransfer(t *testing.T) {
	r := NewRegistry()
	var seen service.DeferredTransfer
	r.MustRegister(Service{
		Name: "receiver",
		OnTransfer: func(_ AccumulateContext, t service.DeferredTransfer) error {
			seen = t
			return nil
		},
	})

	incoming := service.DeferredTransfer{
		SenderServiceIndex:   1,
		ReceiverServiceIndex: 0,
		Balance:              500,
		GasLimit:             10,
	}
	_, err := NewExecutor(r).Accumulate(0, nil, []service.DeferredTransfer{incoming}, 0, 0, testAccount(1_000_000))
	require.NoError(t, err)
	assert.Equal(t, incoming, seen)
}

func TestTransferAccumulatesForSettlement(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Service{
		Name: "sender",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			return nil, ctx.Transfer(Transfer{Destination: 1, Amount: 250, Memo: "rent"})
		},
	})

	res, err := NewExecutor(r).Accumulate(0, nil, nil, 0, 0, testAccount(1_000_000))
	require.NoError(t, err)
	require.Len(t, res.Transfers, 1)
	assert.Equal(t, block.ServiceId(1), res.Transfers[0].ReceiverServiceIndex)
	assert.Equal(t, uint64(250), res.Transfers[0].Balance)
}

func TestTransferBeyondBalanceRejected(t *testing.T) {
	r := NewRegistry()
	r.MustRegister(Service{
		Name: "broke",
		Accumulate: func(ctx AccumulateContext, _ []RefinedItem) ([]byte, error) {
			return nil, ctx.Transfer(Transfer{Destination: 1, Amount: 10_000_000})
		},
	})
	_, err := NewExecutor(r).Accumulate(0, nil, nil, 0, 0, testAccount(1000))
	assert.ErrorIs(t, err, ErrInsufficientFunds)
}
