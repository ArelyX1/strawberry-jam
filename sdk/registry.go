package svc

import (
	"fmt"
	"slices"
	"sort"

	"github.com/eigerco/strawberry/internal/block"
)

// Registry holds the services known to a node, keyed by JAM service ID. It
// allocates IDs so that adding a service never requires renumbering the
// existing ones.
type Registry struct {
	services map[block.ServiceId]*Service
	order    []block.ServiceId
	nextID   block.ServiceId
}

// NewRegistry returns an empty registry. The first service registered is
// assigned ID 0.
func NewRegistry() *Registry {
	return &Registry{services: map[block.ServiceId]*Service{}}
}

// Register stores s under the next free service ID and returns that ID. Use it
// when a service's index is an implementation detail; use [Registry.RegisterAt]
// when the index is part of the protocol, such as the manager service.
func (r *Registry) Register(s Service) (block.ServiceId, error) {
	id := r.nextID
	for {
		if _, taken := r.services[id]; !taken {
			break
		}
		id++
	}

	if err := r.RegisterAt(s, id); err != nil {
		return 0, err
	}
	return id, nil
}

// RegisterAt stores s under an explicit service ID.
func (r *Registry) RegisterAt(s Service, id block.ServiceId) error {
	if _, taken := r.services[id]; taken {
		return fmt.Errorf("%w: %d is already %s", ErrDuplicateID, id, r.services[id])
	}

	if s.Name == "" {
		return fmt.Errorf("%w: service %d has no name", ErrDuplicateID, id)
	}

	registered := s
	registered.ID = id
	r.services[id] = &registered
	r.order = append(r.order, id)

	if id >= r.nextID {
		r.nextID = id + 1
	}
	return nil
}

// MustRegister is [Registry.Register] for package-level declarations, where an
// error can only be a programming mistake.
func (r *Registry) MustRegister(s Service) block.ServiceId {
	id, err := r.Register(s)
	if err != nil {
		panic(err)
	}
	return id
}

// Get returns the service registered under id.
func (r *Registry) Get(id block.ServiceId) (*Service, error) {
	s, ok := r.services[id]
	if !ok {
		return nil, fmt.Errorf("%w: %d", ErrNotFound, id)
	}
	return s, nil
}

// Has reports whether id is registered.
func (r *Registry) Has(id block.ServiceId) bool {
	_, ok := r.services[id]
	return ok
}

// Len returns the number of registered services.
func (r *Registry) Len() int { return len(r.services) }

// IDs returns every registered service ID in ascending order, so that callers
// iterating services see a stable order regardless of registration order.
func (r *Registry) IDs() []block.ServiceId {
	ids := slices.Clone(r.order)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// All returns every registered service in ascending ID order.
func (r *Registry) All() []*Service {
	all := make([]*Service, 0, len(r.services))
	for _, id := range r.IDs() {
		all = append(all, r.services[id])
	}
	return all
}
