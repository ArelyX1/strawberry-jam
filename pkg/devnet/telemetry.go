package devnet

import (
	"fmt"
	"sync"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
)

// Cuánto historial se guarda. Un timeslot dura seis segundos en la cadena de
// desarrollo, así que esto son poco más de tres horas de bloque. Es un techo y
// no un objetivo: el panel pide por ventana.
const telemetryHistory = 2048

// Work es lo que un servicio recibió en un timeslot, que es lo que la
// especificación llama su trabajo en esa asignación.
type Work struct {
	ServiceID block.ServiceId
	Items     int
	Bytes     int
}

// TimeslotRecord es todo lo que pasó en un timeslot.
//
// El campo de gas viene de lo que el PVM devolvió, no de una estimación: el
// ejecutor ya lo calcula para el estado, y duplicar la cuenta aqui daria un
// numero que podria discrepar del que la cadena ya firmo.
type TimeslotRecord struct {
	Timeslot jamtime.Timeslot

	Services []Work
	// Assignments is how many services were given a core, and Refined and
	// Accumulated how many of those actually reached each stage.
	Assignments int
	Refined     int
	Accumulated int
	Failed      int

	RefineGas     uint64
	AccumulateGas uint64
	RefineCalls   uint64
	AccumCalls    uint64

	// Errors keeps the text of the failures of this timeslot, so a timeslot that
	// refused work says why rather than just counting as empty.
	Errors []string
}

// Telemetry totals: what the node has done since it started, across every
// timeslot it has run.
type Telemetry struct {
	Timeslots     uint64
	Assignments   uint64
	Refined       uint64
	Accumulated   uint64
	Failed        uint64
	RefineGas     uint64
	AccumulateGas uint64
	RefineCalls   uint64
	AccumCalls    uint64
	WorkItems     uint64
	WorkBytes     uint64
	ServicesSeen  map[string]uint64
}

// TelemetryStore keeps the per timeslot record and the running totals.
//
// A node executing blocks has to keep some of this anyway: the gas is part of
// the state it signs. This does not replace that, it just keeps the numbers
// reachable over the RPC so an operator can watch the machine work instead of
// inferring it from state roots.
type TelemetryStore struct {
	mu      sync.RWMutex
	history []TimeslotRecord
	totals  Telemetry
}

func NewTelemetryStore() *TelemetryStore {
	return &TelemetryStore{
		history: make([]TimeslotRecord, 0, telemetryHistory),
		totals:  Telemetry{ServicesSeen: map[string]uint64{}},
	}
}

// Record files one timeslot and folds it into the totals.
func (t *TelemetryStore) Record(rec TimeslotRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.totals.Timeslots++
	t.totals.Assignments += uint64(rec.Assignments)
	t.totals.Refined += uint64(rec.Refined)
	t.totals.Accumulated += uint64(rec.Accumulated)
	t.totals.Failed += uint64(rec.Failed)
	t.totals.RefineGas += rec.RefineGas
	t.totals.AccumulateGas += rec.AccumulateGas
	t.totals.RefineCalls += rec.RefineCalls
	t.totals.AccumCalls += rec.AccumCalls

	for _, w := range rec.Services {
		t.totals.WorkItems += uint64(w.Items)
		t.totals.WorkBytes += uint64(w.Bytes)
		t.totals.ServicesSeen[fmt.Sprintf("%d", w.ServiceID)]++
	}

	t.history = append(t.history, rec)
	if len(t.history) > telemetryHistory {
		t.history = t.history[len(t.history)-telemetryHistory:]
	}
}

// History returns the last limit records, oldest first. A limit above what is
// held returns everything held.
func (t *TelemetryStore) History(limit int) []TimeslotRecord {
	t.mu.RLock()
	defer t.mu.RUnlock()

	if limit <= 0 || limit > len(t.history) {
		limit = len(t.history)
	}
	out := make([]TimeslotRecord, limit)
	copy(out, t.history[len(t.history)-limit:])
	return out
}

// Latest returns the most recent record.
func (t *TelemetryStore) Latest() (TimeslotRecord, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if len(t.history) == 0 {
		return TimeslotRecord{}, false
	}
	return t.history[len(t.history)-1], true
}

// Totals returns a copy of the running totals.
func (t *TelemetryStore) Totals() Telemetry {
	t.mu.RLock()
	defer t.mu.RUnlock()

	c := t.totals
	c.ServicesSeen = make(map[string]uint64, len(t.totals.ServicesSeen))
	for k, v := range t.totals.ServicesSeen {
		c.ServicesSeen[k] = v
	}
	return c
}

// TelemetryHistory returns the last limit timeslot records.
func (r *Runtime) TelemetryHistory(limit int) []TimeslotRecord {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.telemetry.History(limit)
}

// TelemetryTotals returns the running totals.
func (r *Runtime) TelemetryTotals() Telemetry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.telemetry.Totals()
}
