// Optional qualification observations connect existing transaction boundaries
// to a private protocol trace. They never supply ownership or change budgets.
package model

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
)

type legacyFinancialDiagnosticKey struct{}

const legacyFinancialDiagnosticPhaseLimit = 64

type legacyFinancialDiagnosticPhase struct {
	Stage     string `json:"stage"`
	Guard     string `json:"guard"`
	ElapsedNs int64  `json:"elapsed_ns"`
	at        time.Time
}

type legacyFinancialDiagnosticObservation struct {
	Kind          string                           `json:"kind"`
	WallNs        int64                            `json:"wall_ns"`
	Phases        []legacyFinancialDiagnosticPhase `json:"phases"`
	DroppedPhases int                              `json:"dropped_phases"`
	backendPID    uint32
	started       time.Time
	bound         time.Time
	finished      time.Time
}

type legacyFinancialDiagnostic struct {
	observe func(legacyFinancialDiagnosticObservation)
	value   legacyFinancialDiagnosticObservation
}

// No ordinary caller installs this private key. One sequential transaction
// owns the record until its joined completion; the callback receives a copy.
func newLegacyFinancialDiagnostic(ctx context.Context, kind string) *legacyFinancialDiagnostic {
	observe, _ := ctx.Value(legacyFinancialDiagnosticKey{}).(func(legacyFinancialDiagnosticObservation))
	if observe == nil {
		return nil
	}
	return &legacyFinancialDiagnostic{observe: observe, value: legacyFinancialDiagnosticObservation{
		Kind: kind, started: time.Now(),
	}}
}

// The startup PID is only a correlation key for the direct disposable fixture.
// It is never an owner key, an admission proof, or an emitted production field.
func (self *legacyFinancialDiagnostic) bind(tx server.PgTx) {
	if self == nil {
		return
	}
	self.value.backendPID = tx.Conn().PgConn().PID()
	self.value.bound = time.Now()
	self.phase("bound", "ready")
}

func (self *legacyFinancialDiagnostic) phase(stage, guard string) {
	if self == nil {
		return
	}
	if len(self.value.Phases) >= legacyFinancialDiagnosticPhaseLimit {
		self.value.DroppedPhases++
		return
	}
	now := time.Now()
	self.value.Phases = append(self.value.Phases, legacyFinancialDiagnosticPhase{
		Stage: stage, Guard: guard, ElapsedNs: now.Sub(self.value.started).Nanoseconds(), at: now,
	})
}

func (self *legacyFinancialDiagnostic) finish() {
	if self == nil || !self.value.finished.IsZero() {
		return
	}
	self.phase("joined", "finished")
	self.value.finished = time.Now()
	self.value.WallNs = self.value.finished.Sub(self.value.started).Nanoseconds()
	self.observe(self.value)
}

func observeLegacyFinancialBudgetBoundary(ctx context.Context, stage []string, guard string) {
	budget, _ := ctx.Value(legacyFinancialCohortBudgetKey{}).(*legacyFinancialCohortBudget)
	if budget == nil || budget.diagnostic == nil {
		return
	}
	name := "unspecified"
	if len(stage) > 0 {
		name = stage[0]
	}
	// Only these existing program boundaries can enter the diagnostic record.
	switch name {
	case "unspecified", "setup", "intents", "headers", "owner", "membership", "ownership", "grant_rows", "escrow_rows", "financial_reads", "outcomes", "post_outcomes", "financial_writes", "provenance", "post_provenance":
	default:
		name = "unknown"
	}
	budget.diagnostic.phase(name, guard)
}
