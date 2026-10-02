package monitor

import (
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// Coverage failure is actionable on its first cadence, including when no
// shared slot was acquired and the probe could not create its own receipt.
// The ordinary alert/ledger retains that terminal scheduler observation. It
// does not synthesize a database attempt, a complete sample or a fresh clock.
func pgSampleFailureAlert(settings SignalSettings, signal Signal, err error) Alert {
	settings = settings.withDefaults()
	phase := "source-or-adapter"
	contact := "unknown"
	var admission *runSlotAdmissionError
	if errors.As(err, &admission) {
		phase, contact = "shared-slot-admission", "false"
	}
	mode := "disabled"
	if settings.PGQuerySampleContinuous {
		mode = "continuous"
	} else if !settings.PGQuerySampleUntil.IsZero() {
		mode = "one-shot"
	}
	attempted, completed, next := "unknown", "unknown", "unknown"
	if settings.PGQuerySampleContinuous && settings.StateDir != "" {
		if state, readErr := pgSampleReadCadence(filepath.Join(settings.StateDir, "pg-query-sample"), settings.Now()); readErr == nil {
			for _, item := range []struct {
				value time.Time
				dest  *string
			}{{state.AttemptedAt, &attempted}, {state.CompletedAt, &completed}, {state.NextEligibleAt, &next}} {
				if !item.value.IsZero() {
					*item.dest = item.value.Format(time.RFC3339Nano)
				}
			}
		}
	}
	return Alert{
		SignalNumber: signal.Number(), SignalKey: signal.Key(), SignalID: signal.ID(), SignalName: signal.Name(),
		Severity: SeverityWarn, Class: "pg-query-sample-unavailable", Target: signal.ID(),
		Environment: settings.Environment, ObservedAt: settings.Now(), Sustain: 1,
		Symptom:   "PostgreSQL query/load coverage did not complete its bounded turn",
		Observed:  fmt.Sprintf("mode=%s phase=%s error_class=%s source_contact_attempted=%s last_attempted_at=%s last_completed_at=%s next_eligible_at=%s", mode, phase, classifyObservationError(err), contact, attempted, completed, next),
		Mechanism: "A shared-slot deadline can expire before the probe runs or creates a receipt. A later source/adapter error also cannot establish current query coverage. The first missing observation remains visible.",
		Baseline:  "The enabled recurring sampler completes a bounded catalog sample under the unchanged shared4-signal/host2-command limits and15-minute durable cadence floor.",
		Action:    "Inspect the exact admission/source phase and retained sampler state; repair scheduling or the source failure. Do not treat a startup flag or absent receipt as an executed database sample, raise concurrency, or retry before the next scheduled/durable floor.",
		Verify:    "A later eligible turn retains a complete immutable12-snapshot receipt and actual source clocks. Until then coverage is unknown.",
		Playbook:  "SIGNALS.md §2.1a",
	}
}
