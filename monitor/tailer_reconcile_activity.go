package monitor

import (
	"encoding/json"
	"fmt"
	"time"
)

const logReconcileActivityPrefix = "monitor-log-reconcile-activity "
const logReconcileActivityMaxBytes = 4 * 1024

// Only the single logical reconciliation owner writes this state. A completed
// page is a successful Warpctl return, including a saturated page. HTTP retries
// inside that child and individual source records have no representation.
type logReconcileAttempt struct {
	number         uint64
	pagesCompleted uint64
	startedAt      time.Time
	lastPageAt     time.Time
	active         bool
	clockRegressed bool
}

// A later clock reading cannot erase an earlier backwards page clock.
func (attempt *logReconcileAttempt) recordPage(at time.Time) {
	if at.Before(attempt.startedAt) || (attempt.pagesCompleted > 0 && at.Before(attempt.lastPageAt)) {
		attempt.clockRegressed = true
	}
	attempt.pagesCompleted++
	attempt.lastPageAt = at
}

// Preserve the generation-local sequence while clearing the completed attempt.
func (attempt *logReconcileAttempt) finish() {
	*attempt = logReconcileAttempt{number: attempt.number}
}

// A returned page needs a local clock inside this attempt and observation.
func (attempt logReconcileAttempt) validAt(generation *time.Time, observed time.Time) bool {
	if generation == nil || attempt.number == 0 || attempt.startedAt.IsZero() || attempt.clockRegressed ||
		attempt.startedAt.Before(*generation) || attempt.startedAt.After(observed) {
		return false
	}
	if attempt.pagesCompleted == 0 {
		return attempt.lastPageAt.IsZero()
	}
	return !attempt.lastPageAt.IsZero() && !attempt.lastPageAt.Before(attempt.startedAt) && !attempt.lastPageAt.After(observed)
}

type logReconcileActivityScope struct {
	ServiceKey         string     `json:"service_key"`
	CollectorCount     int        `json:"collector_count"`
	CollectorStartedAt *time.Time `json:"collector_started_at"`
	State              string     `json:"state"`
	AttemptNumber      *uint64    `json:"attempt_number"`
	AttemptStartedAt   *time.Time `json:"attempt_started_at"`
	PagesCompleted     *uint64    `json:"pages_completed"`
	LastPageAt         *time.Time `json:"last_page_completed_at"`
}

type logReconcileActivityReceipt struct {
	Schema                  int                         `json:"schema"`
	ObservedAt              time.Time                   `json:"observed_at"`
	Collectors              int                         `json:"collectors"`
	ScopeProjectionComplete bool                        `json:"scope_projection_complete"`
	UnsupportedCollectors   int                         `json:"unsupported_collectors"`
	DuplicateServiceKeys    int                         `json:"duplicate_service_keys"`
	CollectorScopes         []logReconcileActivityScope `json:"collector_scopes"`
}

// Reuse the fixed coverage projection for identity and generation only.
func newLogReconcileActivityReceipt(snapshots []logReconcileCollectorSnapshot, coverage logReconcileReceipt) logReconcileActivityReceipt {
	receipt := logReconcileActivityReceipt{
		Schema: 1, ObservedAt: coverage.ObservedAt, Collectors: coverage.Collectors,
		ScopeProjectionComplete: coverage.ScopeProjectionComplete, UnsupportedCollectors: coverage.UnsupportedCollectors,
		DuplicateServiceKeys: coverage.DuplicateServiceKeys, CollectorScopes: make([]logReconcileActivityScope, 0, len(coverage.CollectorScopes)),
	}
	var attempts [len(logReconcileServiceKeys)]logReconcileAttempt
	for _, snapshot := range snapshots {
		if index := logReconcileServiceIndex(snapshot.service); index >= 0 {
			attempts[index] = snapshot.attempt
		}
	}
	for _, scope := range coverage.CollectorScopes {
		row := logReconcileActivityScope{ServiceKey: scope.ServiceKey, CollectorCount: scope.CollectorCount,
			CollectorStartedAt: scope.CollectorStartedAt, State: "ambiguous"}
		if scope.CollectorCount == 1 && scope.Enabled != nil {
			attempt := attempts[logReconcileServiceIndex(scope.ServiceKey)]
			row.State = "idle"
			switch {
			case !*scope.Enabled:
				row.State = "disabled"
			case attempt.active:
				row.State = "active"
				row.AttemptNumber, row.PagesCompleted = &attempt.number, &attempt.pagesCompleted
				row.AttemptStartedAt, row.LastPageAt = logReconcileTime(attempt.startedAt), logReconcileTime(attempt.lastPageAt)
				if !attempt.validAt(scope.CollectorStartedAt, coverage.ObservedAt) {
					row.State = "invalid-time"
				}
			case scope.CollectorStartedAt == nil || scope.CollectorStartedAt.After(coverage.ObservedAt):
				row.State = "invalid-time"
			}
		}
		receipt.CollectorScopes = append(receipt.CollectorScopes, row)
	}
	return receipt
}

// An encoding or size failure affects only the activity diagnostic. One framed
// write keeps the exact prefix recoverable after unterminated child stderr.
func (self *logTailProbe) writeReconcileActivity(snapshots []logReconcileCollectorSnapshot, coverage logReconcileReceipt) {
	encoded, err := json.Marshal(newLogReconcileActivityReceipt(snapshots, coverage))
	if err == nil && len(encoded)+len(logReconcileActivityPrefix)+2 <= logReconcileActivityMaxBytes {
		_, _ = fmt.Fprintf(self.activityDiagnostics, "\n%s%s\n", logReconcileActivityPrefix, encoded)
	}
}
