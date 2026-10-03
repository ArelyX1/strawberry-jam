package devnet

import (
	"testing"

	"github.com/eigerco/strawberry/internal/jamtime"
	"github.com/eigerco/strawberry/sdk/papucoin"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Queued work is only worth anything if a timeslot hands it a core.
//
// A node with work waiting and a timeslot that hands out nothing looks, from the
// outside, exactly like a chain that is running: it keeps a timeslot, it produces
// blocks, its tip moves on every few seconds. Nothing is wrong with any of that and
// nothing happens either. A transfer sits in the queue, the balance it was going to
// move never moves, and every way of looking at the node says the chain is fine.
//
// The panel draws the work a timeslot carried, and it drew nothing, which is how
// this shows up.
func TestQueuedWorkGetsACore(t *testing.T) {
	rt := newTestRuntime(t)
	item := signed(t, papucoin.Item{
		Method: papucoin.MethodTransfer,
		Sender: addressOf(t, 3),
		Nonce:  1,
		To:     addressOf(t, 4),
		Amount: "10",
	}, 3)

	submit(t, rt, item)
	pending, _ := rt.Pending()
	require.Positive(t, pending, "the item should be waiting for a core")

	_, err := rt.Run(jamtime.Timeslot(9199501))
	require.NoError(t, err)

	history := rt.TelemetryHistory(1)
	require.Len(t, history, 1, "the timeslot should have been recorded")
	rec := history[0]

	assert.Positive(t, rec.Assignments,
		"work was queued, so the timeslot has to hand it a core")
	assert.NotEmpty(t, rec.Services,
		"the panel draws one row per service per timeslot, and there was a service")
	total := 0
	for _, w := range rec.Services {
		total += w.Items
	}
	assert.Positive(t, total, "the queued item should have been handed to refine")
	assert.Positive(t, rec.RefineCalls, "refine is what runs the queued item")
	assert.Positive(t, rec.AccumCalls, "and accumulate is what settles it")
}
