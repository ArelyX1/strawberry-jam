package svc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/service"
)

func schedulerFixture(t *testing.T, names ...string) (*Registry, *Scheduler) {
	t.Helper()
	r := NewRegistry()
	for _, name := range names {
		r.MustRegister(Service{Name: name})
	}
	return r, NewScheduler(r, 4)
}

func idsOf(assignments []Assignment) []block.ServiceId {
	ids := make([]block.ServiceId, len(assignments))
	for i, a := range assignments {
		ids[i] = a.ServiceID
	}
	return ids
}

func TestDormantServiceCostsNothing(t *testing.T) {
	_, s := schedulerFixture(t, "a", "b", "c")

	// Nothing queued: no service should be given a core.
	assert.Empty(t, s.Plan(3))
	assert.Equal(t, 3, s.IdleCores(3))
	assert.Equal(t, []block.ServiceId{0, 1, 2}, s.Dormant())
}

func TestDemandWakesExactlyTheAsker(t *testing.T) {
	_, s := schedulerFixture(t, "a", "b", "c")

	depth, err := s.Submit(1, WorkItem{Payload: []byte("x")})
	require.NoError(t, err)
	assert.Equal(t, 1, depth)

	assignments := s.Plan(8)
	assert.Equal(t, []block.ServiceId{1}, idsOf(assignments))
	assert.Equal(t, 7, s.IdleCores(8), "unused cores stay idle rather than running idle work")
	assert.Equal(t, []block.ServiceId{0, 2}, s.Dormant())
}

func TestTransfersAlsoCountAsDemand(t *testing.T) {
	_, s := schedulerFixture(t, "a", "b")

	require.NoError(t, s.SubmitTransfer(0, service.DeferredTransfer{Balance: 1}))
	assert.Equal(t, 1, s.Demand(0))
	assert.Equal(t, []block.ServiceId{0}, idsOf(s.Plan(4)))
}

func TestMostDemandedServiceRunsFirst(t *testing.T) {
	_, s := schedulerFixture(t, "a", "b", "c")

	_, err := s.Submit(0, WorkItem{Payload: []byte("1")})
	require.NoError(t, err)
	_, err = s.Submit(1, WorkItem{Payload: []byte("1")})
	require.NoError(t, err)
	_, err = s.Submit(1, WorkItem{Payload: []byte("2")})
	require.NoError(t, err)

	// Only one core, and service 1 has twice the demand.
	assert.Equal(t, []block.ServiceId{1}, idsOf(s.Plan(1)))
}

func TestEqualDemandRotatesToAvoidStarvation(t *testing.T) {
	_, s := schedulerFixture(t, "a", "b", "c")

	for _, id := range []block.ServiceId{0, 1, 2} {
		_, err := s.Submit(id, WorkItem{Payload: []byte("x")})
		require.NoError(t, err)
	}

	// With one core and equal demand, every service must eventually run.
	seen := map[block.ServiceId]bool{}
	for range 6 {
		plan := s.Plan(1)
		require.Len(t, plan, 1)
		seen[plan[0].ServiceID] = true
	}
	assert.Len(t, seen, 3, "equal-demand services should take turns")
}

func TestAlwaysOnServiceRunsWithoutDemand(t *testing.T) {
	_, s := schedulerFixture(t, "manager", "economy")
	s.RequireAlwaysOn(0)

	plan := s.Plan(4)
	require.Equal(t, []block.ServiceId{0}, idsOf(plan))
	assert.True(t, plan[0].AlwaysOn)
	assert.Equal(t, []block.ServiceId{1}, s.Dormant())
}

func TestSettleConsumesOnlyGrantedItems(t *testing.T) {
	_, s := schedulerFixture(t, "a")
	s2 := s

	for range 6 {
		_, err := s2.Submit(0, WorkItem{Payload: []byte("x")})
		require.NoError(t, err)
	}

	plan := s2.Plan(4)
	assert.Len(t, plan[0].Items, 4, "per-timeslot item cap applies")
	assert.Equal(t, 6, s2.Demand(0))

	s2.Settle(plan)
	assert.Equal(t, 2, s2.Demand(0))

	plan = s2.Plan(4)
	assert.Len(t, plan[0].Items, 2)
	s2.Settle(plan)
	assert.Equal(t, 0, s2.Demand(0))
	assert.Empty(t, s2.Plan(4))
}

func TestSettleClearsDeliveredTransfers(t *testing.T) {
	_, s := schedulerFixture(t, "a", "b")
	require.NoError(t, s.SubmitTransfer(1, service.DeferredTransfer{Balance: 5}))

	transfers := s.Transfers(1)
	require.Len(t, transfers, 1)

	s.Settle([]Assignment{{ServiceID: 1, Items: nil}})
	assert.Empty(t, s.Transfers(1))
	assert.Equal(t, 0, s.Demand(1))
}

func TestPlanDoesNotMutateQueuesBeforeSettle(t *testing.T) {
	_, s := schedulerFixture(t, "a")
	_, err := s.Submit(0, WorkItem{Payload: []byte("x")})
	require.NoError(t, err)

	s.Plan(2)
	s.Plan(2)
	assert.Equal(t, 1, s.Demand(0), "planning alone must not consume demand")
}

func TestSubmitToUnknownServiceFails(t *testing.T) {
	_, s := schedulerFixture(t, "a")
	_, err := s.Submit(42, WorkItem{})
	assert.ErrorIs(t, err, ErrNotFound)
	assert.ErrorIs(t, s.SubmitTransfer(42, service.DeferredTransfer{}), ErrNotFound)
}

func TestZeroCoresGrantsNothing(t *testing.T) {
	_, s := schedulerFixture(t, "a")
	_, err := s.Submit(0, WorkItem{Payload: []byte("x")})
	require.NoError(t, err)
	assert.Empty(t, s.Plan(0))
}
