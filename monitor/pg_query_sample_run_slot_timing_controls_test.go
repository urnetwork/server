package monitor

import (
	"context"
	"testing"
	"time"
)

// A different finite-budget signal shares the pool without receiving PG facts.
type pgRunSlotOrdinaryControl struct {
	Signal
	t      *testing.T
	called bool
}

// Keep the finite-owner branch while selecting an ordinary signal identity.
func (self *pgRunSlotOrdinaryControl) ID() string               { return "ordinary/timing-control" }
func (self *pgRunSlotOrdinaryControl) runBudget() time.Duration { return pgQuerySampleBudget }

// Both new pool hooks must be harmless when this caller has no PG recorder.
func (self *pgRunSlotOrdinaryControl) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	self.called = true
	if pgSampleTransportFromContext(ctx) != nil {
		self.t.Error("ordinary signal received PG timing or transport authority")
	}
	return nil, nil
}

// Preserve nil-observer behavior through actual request, grant and release.
func TestPgRunSlotTimingOrdinarySignalKeepsObservationAbsent(t *testing.T) {
	signal := &pgRunSlotOrdinaryControl{Signal: NewPgQuerySampleSignal(), t: t}
	slots := newRunSlotPool(runLoopMaxConcurrentSignals)
	if _, err := runSignalInSlot(t.Context(), slots, signal, syntheticSettings(nil)); err != nil || !signal.called || slots.active != 0 || len(slots.waiting) != 0 {
		t.Fatal("ordinary bounded signal changed its execution or grant ownership")
	}
}
