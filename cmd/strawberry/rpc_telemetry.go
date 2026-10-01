package main

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/eigerco/strawberry/internal/block"
	"github.com/eigerco/strawberry/internal/jamtime"
)

// Handlers over what the node actually did: the work of every timeslot, the gas
// it cost, and how the accumulate queue looked when someone asked.
//
// These read the record the runtime keeps as it executes blocks. That record is
// written from the same numbers the chain signs, so what an operator reads here
// and what a validator signed are the same numbers, not two estimates of each
// other.

func (p *papucoinHandlers) telemetryCall(req rpcReq) *rpcResp {
	params, err := paramsOf(req.Params)
	if err != nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: err.Error()}}
	}

	// The window defaults to something a chart can hold: a timeslot is six
	// seconds in dev, so 180 is about eighteen minutes.
	limit := 180
	if len(params) > 0 {
		if n, err := strconv.Atoi(string(params[0])); err == nil && n > 0 {
			if n > 2048 {
				n = 2048
			}
			limit = n
		}
	}

	history := p.runtime.TelemetryHistory(limit)
	totals := p.runtime.TelemetryTotals()

	records := make([]map[string]interface{}, 0, len(history))
	for _, rec := range history {
		services := make([]map[string]interface{}, 0, len(rec.Services))
		for _, w := range rec.Services {
			services = append(services, map[string]interface{}{
				"service": fmt.Sprintf("%d", w.ServiceID),
				"items":   w.Items,
				"bytes":   w.Bytes,
			})
		}
		errs := rec.Errors
		if errs == nil {
			errs = []string{}
		}
		records = append(records, map[string]interface{}{
			"timeslot":      fmt.Sprintf("%d", rec.Timeslot),
			"assignments":   rec.Assignments,
			"refined":       rec.Refined,
			"accumulated":   rec.Accumulated,
			"failed":        rec.Failed,
			"refineCalls":   rec.RefineCalls,
			"accumCalls":    rec.AccumCalls,
			"refineGas":     rec.RefineGas,
			"accumulateGas": rec.AccumulateGas,
			"services":      services,
			"errors":        errs,
		})
	}

	seen := make(map[string]uint64, len(totals.ServicesSeen))
	for k, v := range totals.ServicesSeen {
		seen[k] = v
	}

	return &rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]interface{}{
		"records": records,
		"totals": map[string]interface{}{
			"timeslots":     totals.Timeslots,
			"assignments":   totals.Assignments,
			"refined":       totals.Refined,
			"accumulated":   totals.Accumulated,
			"failed":        totals.Failed,
			"refineGas":     totals.RefineGas,
			"accumulateGas": totals.AccumulateGas,
			"refineCalls":   totals.RefineCalls,
			"accumCalls":    totals.AccumCalls,
			"workItems":     totals.WorkItems,
			"workBytes":     totals.WorkBytes,
			"servicesSeen":  seen,
		},
	}}
}

// worksCall returns the work of the most recent timeslots, flattened per
// service. The panel charts it against gas, so it wants one row per service per
// timeslot rather than the nested form the telemetry record uses.
func (p *papucoinHandlers) worksCall(req rpcReq) *rpcResp {
	params, err := paramsOf(req.Params)
	if err != nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: err.Error()}}
	}

	limit := 120
	if len(params) > 0 {
		if n, err := strconv.Atoi(string(params[0])); err == nil && n > 0 {
			if n > 2048 {
				n = 2048
			}
			limit = n
		}
	}

	history := p.runtime.TelemetryHistory(limit)
	rows := make([]map[string]interface{}, 0, len(history))
	for _, rec := range history {
		for _, w := range rec.Services {
			rows = append(rows, map[string]interface{}{
				"timeslot": fmt.Sprintf("%d", rec.Timeslot),
				"service":  fmt.Sprintf("%d", w.ServiceID),
				"items":    w.Items,
				"bytes":    w.Bytes,
			})
		}
	}
	return &rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]interface{}{
		"rows":  rows,
		"count": len(rows),
	}}
}

// joinAccumulateCall reports the accumulate queue the way a service asking to
// join it would see it.
//
// It is a read on purpose. A service that joins the queue has to be scheduled
// onto a core by a validator, which is a consensus action, and a node answering
// an RPC cannot do that. What it can do, and what this does, is say whether a
// service is already scheduled, whether its account clears the threshold, and
// what it is holding, which is what decides whether joining would even work.
func (p *papucoinHandlers) joinAccumulateCall(req rpcReq) *rpcResp {
	params, err := paramsOf(req.Params)
	if err != nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: err.Error()}}
	}
	if len(params) == 0 {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{
			Code: -32602, Message: "join_accumulate needs a service id",
		}}
	}

	var id uint32
	if err := json.Unmarshal(params[0], &id); err != nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{
			Code: -32602, Message: fmt.Sprintf("service id must be a number: %v", err),
		}}
	}

	account, ok := p.runtime.ServiceAccount(block.ServiceId(id))
	queue, slots := p.runtime.QueueDepth()

	// A service that is already being scheduled should not be told it may join.
	scheduled := p.runtime.IsScheduled(block.ServiceId(id))

	eligible := ok && !scheduled && account.Balance > 0

	return &rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]interface{}{
		"service":            id,
		"exists":             ok,
		"scheduled":          scheduled,
		"balance":            fmt.Sprintf("%d", account.Balance),
		"gasLimit":           fmt.Sprintf("%d", account.GasLimitForAccumulator),
		"gasLimitOnTransfer": fmt.Sprintf("%d", account.GasLimitOnTransfer),
		"codeHash":           fmt.Sprintf("%x", account.CodeHash),
		"queuePending":       queue,
		"queueSlots":         slots,
		"couldJoin":          eligible,
		// Said plainly, because a caller could otherwise read couldJoin as a
		// promise: this node cannot put a service on a core.
		"reason": "read only: joining the accumulate queue is scheduled by a validator, not by this RPC",
	}}
}

// blockLogCall returns one row per block the node has produced, with the work
// the block carried. This is the tail the panel prints as it goes.
func (p *papucoinHandlers) blockLogCall(req rpcReq) *rpcResp {
	params, err := paramsOf(req.Params)
	if err != nil {
		return &rpcResp{JSONRPC: "2.0", ID: req.ID, Error: &rpcErr{Code: -32602, Message: err.Error()}}
	}

	limit := 60
	if len(params) > 0 {
		if n, err := strconv.Atoi(string(params[0])); err == nil && n > 0 {
			if n > 512 {
				n = 512
			}
			limit = n
		}
	}

	history := p.runtime.TelemetryHistory(limit)
	rows := make([]map[string]interface{}, 0, len(history))
	for i := len(history) - 1; i >= 0; i-- {
		rec := history[i]
		items := 0
		bytes := 0
		services := make([]string, 0, len(rec.Services))
		for _, w := range rec.Services {
			items += w.Items
			bytes += w.Bytes
			services = append(services, fmt.Sprintf("%d", w.ServiceID))
		}
		rows = append(rows, map[string]interface{}{
			"timeslot":      jamtime.Timeslot(rec.Timeslot),
			"gas":           rec.RefineGas + rec.AccumulateGas,
			"refineGas":     rec.RefineGas,
			"accumulateGas": rec.AccumulateGas,
			"items":         items,
			"bytes":         bytes,
			"services":      services,
			"assignments":   rec.Assignments,
			"failed":        rec.Failed,
			"errors":        rec.Errors,
		})
	}
	return &rpcResp{JSONRPC: "2.0", ID: req.ID, Result: map[string]interface{}{"blocks": rows}}
}
