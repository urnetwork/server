package model

// One optional registration invocation owns its phase, budgets and pool
// observation. Only a returned failed acquisition adds a finite annotation.

import (
	"context"
	"encoding/json"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Cause is a fixed local sentinel name, never a context's arbitrary message.
type legacyRegistrationBudget struct {
	Budget server.DbContextBudget `json:"budget"`
	Cause  string                 `json:"cause"`
}

// Keep the parent's state separate from the turn and optional child allowance.
type legacyRegistrationBudgets struct {
	Parent       legacyRegistrationBudget `json:"parent"`
	Turn         legacyRegistrationBudget `json:"turn"`
	Registration legacyRegistrationBudget `json:"registration"`
}

// One sequential invocation owns these references until its deferred finish.
type legacyRegistrationDbObservation struct {
	parent       context.Context
	turn         context.Context
	registration context.Context
	log          connect.Logger
	timing       server.DbTiming
	phase        server.DbPhaseObservation
	entry        legacyRegistrationBudgets
	finished     bool
}

// Resolve the existing logger once; ordinary success emits nothing.
func newLegacyRegistrationDbObservation(parent, turn, registration context.Context) *legacyRegistrationDbObservation {
	observation := &legacyRegistrationDbObservation{
		parent: parent, turn: turn, registration: registration, log: connect.DefaultLogger(),
		timing: server.DbTiming{Acquire: &server.DbAcquireObservation{}},
	}
	observation.entry = observation.budgets()
	return observation
}

// These reads describe observed states, not an atomic cancellation-owner join.
func (self *legacyRegistrationDbObservation) budgets() legacyRegistrationBudgets {
	observe := func(ctx context.Context) legacyRegistrationBudget {
		cause := "other"
		switch context.Cause(ctx) {
		case nil:
			cause = "live"
		case errLegacySettlementDispatchBudget:
			cause = "dispatch_budget"
		case errLegacySettlementDiscoveryBudget:
			cause = "discovery_budget"
		case context.DeadlineExceeded:
			cause = "deadline"
		case context.Canceled:
			cause = "canceled"
		}
		return legacyRegistrationBudget{Budget: server.ObserveDbContextBudget(ctx), Cause: cause}
	}
	return legacyRegistrationBudgets{
		Parent: observe(self.parent), Turn: observe(self.turn), Registration: observe(self.registration),
	}
}

// This runs inside the existing registration call, before its caller cancels
// the child. It never recovers, wraps or replaces the database panic.
func (self *legacyRegistrationDbObservation) finish() {
	if self.finished {
		return
	}
	self.finished = true
	acquire := self.timing.Acquire
	if self.phase.Phase() != server.DbOperationAcquire || acquire == nil || !acquire.Returned || acquire.Admitted {
		return
	}
	event := struct {
		Schema   int                          `json:"schema"`
		Phase    string                       `json:"phase"`
		Entry    legacyRegistrationBudgets    `json:"entry"`
		Return   legacyRegistrationBudgets    `json:"return"`
		Database *server.DbAcquireObservation `json:"database"`
	}{1, "acquire", self.entry, self.budgets(), acquire}
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	self.annotate(encoded)
}

// A diagnostic sink panic cannot replace an already unwinding database panic.
// Only this log call is contained; the original HandleError and payload remain.
func (self *legacyRegistrationDbObservation) annotate(encoded []byte) {
	defer func() { _ = recover() }()
	self.log.Infof("[legacy_registration_db] %s", encoded)
}
