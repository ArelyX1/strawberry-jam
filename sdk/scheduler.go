package svc

import (
	"slices"
	"sync"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/service"
)

// WorkItem is a unit of demand queued against a service.
type WorkItem struct {
	// Payload is the work item handed to the service during refinement.
	Payload []byte
	// Origin identifies who submitted the item, for tracing. It has no
	// consensus meaning.
	Origin string
}

// Assignment is the coretime granted to one service in a timeslot.
type Assignment struct {
	ServiceID block.ServiceId
	Items     []WorkItem
	// AlwaysOn is set for services the protocol requires to accumulate every
	// timeslot regardless of demand.
	AlwaysOn bool
}

// Scheduler decides which services get coretime in each timeslot.
//
// A service with an empty queue is dormant: it is not assigned a core, runs no
// code and pays no coretime. This is what makes an economy on-demand work, since
// the only times a service costs anything are the timeslots in which somebody
// actually asked it to do something. Surplus cores stay idle rather than being
// spent on idle work.
type Scheduler struct {
	mu       sync.Mutex
	registry *Registry
	queues   map[block.ServiceId]*[]WorkItem
	inbox    map[block.ServiceId][]service.DeferredTransfer
	alwaysOn map[block.ServiceId]bool
	// cursor rotates the starting point between timeslots so that a service
	// with a steady trickle of demand cannot be starved by a busier peer.
	cursor int
	// maxPerTimeslot caps how many items one service may have refined in a
	// single timeslot, bounding the gas a single block can owe.
	maxPerTimeslot int
}

// NewScheduler returns a scheduler over the services in r. maxPerTimeslot
// bounds how many queued items a single service may consume per timeslot.
func NewScheduler(r *Registry, maxPerTimeslot int) *Scheduler {
	if maxPerTimeslot <= 0 {
		maxPerTimeslot = 1
	}
	return &Scheduler{
		registry:       r,
		queues:         map[block.ServiceId]*[]WorkItem{},
		inbox:          map[block.ServiceId][]service.DeferredTransfer{},
		alwaysOn:       map[block.ServiceId]bool{},
		maxPerTimeslot: maxPerTimeslot,
	}
}

// RequireAlwaysOn marks a service as one the protocol needs to accumulate every
// timeslot, such as the manager service that mints and registers other services.
func (s *Scheduler) RequireAlwaysOn(id block.ServiceId) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.alwaysOn[id] = true
}

// Submit queues a work item for a service and returns the resulting queue depth.
func (s *Scheduler) Submit(id block.ServiceId, item WorkItem) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.registry.Has(id) {
		return 0, ErrNotFound
	}

	queue := s.queues[id]
	if queue == nil {
		queue = &[]WorkItem{}
		s.queues[id] = queue
	}
	*queue = append(*queue, item)
	return len(*queue), nil
}

// SubmitTransfer queues an incoming balance transfer as demand for a service.
func (s *Scheduler) SubmitTransfer(id block.ServiceId, transfer service.DeferredTransfer) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.registry.Has(id) {
		return ErrNotFound
	}
	s.inbox[id] = append(s.inbox[id], transfer)
	return nil
}

// Demand reports how much work a service has queued, counting both items and
// undelivered transfers.
func (s *Scheduler) Demand(id block.ServiceId) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demandLocked(id)
}

// ForgetCoretime puts the rotation back to the start of the chain and empties the
// queues and the inboxes.
//
// The cursor is what decides which service gets which core when two of them want
// one, and it lives in this node's memory. A node that has been running has
// turned it a number of times that has nothing to do with where it is on the
// chain, so replaying that chain from the start over the top of them hands out
// the cores in a different order than the node that wrote the blocks handed them
// out in. The services still all run, still in the same timeslots, but they run
// in a different sequence, and a service whose accumulate is not commutative
// settles to a different state. So the node rebuilds the chain, arrives at a
// root that no block ever named, and refuses to build on it.
//
// This is the same thing [github.com/eigerco/strawberry/pkg/devnet.Runtime]'s
// ForgetHandedOut does for the numbers it handed out: the parts of a node that
// are the node's own bookkeeping rather than the chain's have to go back to the
// beginning of the chain before it is replayed, or the replay is of the chain
// plus whatever the node was doing before.
func (s *Scheduler) ForgetCoretime() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursor = 0
	for id := range s.queues {
		s.queues[id] = &[]WorkItem{}
	}
	for id := range s.inbox {
		s.inbox[id] = nil
	}
}

// ForgetWork empties the queues and the inboxes, and leaves everything else —
// including which services the protocol needs every timeslot — as it was.
//
// It is what a node does to its own pending work when it starts following a chain
// that a peer wrote. What is queued belongs to the branch this node was on, and
// running it against somebody else's blocks settles work that no block names.
//
// That is not a theoretical problem. A node with a payout queued that receives a
// peer's block for a timeslot it is not authoring used to execute that timeslot
// with both in the queue: the block's work, and its own, which settled there
// instead of in a later timeslot. Its state was then the chain's state plus one
// extra transfer, it wrote the next block on top of that, and the block named a
// root no other node could rebuild — so every peer refused to adopt it and the
// chain stopped with all of them healthy.
func (s *Scheduler) ForgetWork() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id := range s.queues {
		s.queues[id] = &[]WorkItem{}
	}
	for id := range s.inbox {
		s.inbox[id] = nil
	}
}

// Drain takes everything that is queued, leaving the queues empty, and says which
// service each item was for.
//
// It is how work a node had queued of its own is picked up before the node starts
// following a chain somebody else wrote, so that following a chain does not lose
// it and does not run it either.
func (s *Scheduler) Drain() map[block.ServiceId][]WorkItem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[block.ServiceId][]WorkItem, len(s.queues))
	for id, queue := range s.queues {
		if queue == nil || len(*queue) == 0 {
			continue
		}
		out[id] = append(out[id], (*queue)...)
		*queue = (*queue)[:0]
	}
	return out
}

func (s *Scheduler) demandLocked(id block.ServiceId) int {
	n := 0
	if queue := s.queues[id]; queue != nil {
		n += len(*queue)
	}
	return n + len(s.inbox[id])
}

// Pending reports total queued demand across all services.
func (s *Scheduler) Pending() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	total := 0
	for id := range s.registry.services {
		total += s.demandLocked(id)
	}
	return total
}

// Demanding returns the services with queued work, most demanded first. Ties
// are broken by service ID so that every node plans the same order.
func (s *Scheduler) Demanding() []block.ServiceId {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demandingLocked()
}

func (s *Scheduler) demandingLocked() []block.ServiceId {
	var ids []block.ServiceId
	for id := range s.registry.services {
		if s.alwaysOn[id] || s.demandLocked(id) > 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// Plan grants coretime to at most cores services. Services are served in order
// of demand, most demanded first. Within a group of equally demanded services
// the order rotates between timeslots, so when cores are scarce a service with
// a steady trickle of demand is not starved by a bursty peer at the same level.
//
// Plan does not consume the queue; call [Scheduler.Settle] once the timeslot
// has been applied to the chain.
func (s *Scheduler) Plan(cores int) []Assignment {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cores <= 0 {
		return nil
	}

	// Group by demand level so priority is expressed before rotation.
	byLevel := map[int][]block.ServiceId{}
	for _, id := range s.demandingLocked() {
		level := s.demandLocked(id)
		byLevel[level] = append(byLevel[level], id)
	}
	levels := make([]int, 0, len(byLevel))
	for level := range byLevel {
		levels = append(levels, level)
	}
	slices.Sort(levels)
	slices.Reverse(levels)

	assignments := make([]Assignment, 0, min(cores, len(levels)))
	for _, level := range levels {
		ids := byLevel[level]
		if start := s.cursor % len(ids); start != 0 {
			ids = append(slices.Clone(ids[start:]), ids[:start]...)
		}
		for _, id := range ids {
			if len(assignments) == cores {
				break
			}
			assignment := Assignment{ServiceID: id, AlwaysOn: s.alwaysOn[id]}
			if queue := s.queues[id]; queue != nil {
				assignment.Items = slices.Clone((*queue)[:min(len(*queue), s.maxPerTimeslot)])
			}
			assignments = append(assignments, assignment)
		}
		if len(assignments) == cores {
			break
		}
	}

	s.cursor++
	return assignments
}

// Transfers returns the transfers queued for a service since the last call.
func (s *Scheduler) Transfers(id block.ServiceId) []service.DeferredTransfer {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.inbox[id])
}

// Settle marks the items a timeslot consumed as done and clears the transfers
// that were delivered. Only call it for assignments that were actually applied
// to the chain, or the demand will be lost.
func (s *Scheduler) Settle(assignments []Assignment) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, assignment := range assignments {
		if queue := s.queues[assignment.ServiceID]; queue != nil {
			consumed := min(len(assignment.Items), len(*queue))
			*queue = (*queue)[consumed:]
		}
		delete(s.inbox, assignment.ServiceID)
	}
}

// Dormant returns the registered services with no demand and no protocol
// requirement to run. These are the services costing nothing right now.
func (s *Scheduler) Dormant() []block.ServiceId {
	s.mu.Lock()
	defer s.mu.Unlock()

	var ids []block.ServiceId
	for id := range s.registry.services {
		if !s.alwaysOn[id] && s.demandLocked(id) == 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return ids
}

// IdleCores reports how many of the given cores would go unused for the current
// demand, which is the coretime the chain saves by staying on demand.
func (s *Scheduler) IdleCores(cores int) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	needed := len(s.demandingLocked())
	if needed >= cores {
		return 0
	}
	return cores - needed
}
