package monitor

import (
	"encoding/json"
	"fmt"
	"time"
)

// SIGNALS.md §1.5: a successful overlap is not an Alert. Project the existing
// bounded success pairs through fixed public service keys on diagnostics.
type logReconcileSuccess struct {
	start     time.Time
	completed time.Time
}

func (s logReconcileSuccess) valid() bool {
	return !s.start.IsZero() && !s.completed.IsZero() && s.completed.After(s.start)
}

func (s logReconcileSuccess) advancesTo(next logReconcileSuccess) bool {
	return s.valid() && next.valid() && next.start.After(s.start) &&
		next.completed.After(s.completed) &&
		next.completed.Sub(s.completed) < 2*logReconcileInterval
}

type logReconcileTimeRange struct {
	Oldest *time.Time `json:"oldest"`
	Newest *time.Time `json:"newest"`
}

func (r *logReconcileTimeRange) add(value time.Time) {
	if value.IsZero() {
		return
	}
	value = value.UTC()
	if r.Oldest == nil || value.Before(*r.Oldest) {
		r.Oldest = &value
	}
	if r.Newest == nil || value.After(*r.Newest) {
		r.Newest = &value
	}
}

const logReconcileReceiptPrefix = "monitor-log-reconcile "
const logReconcileReceiptMaxBytes = 8 * 1024

// Only these public service keys may leave the collector snapshot. An unknown
// name contributes a count, never an arbitrary identifier or a synthetic key.
var logReconcileServiceKeys = [...]string{
	"alt", "api", "app", "connect", "gossip", "grafana", "mcp", "proxy", "taskworker", "web",
}

// A row describes coverage, not request progress, a failure cause, or service
// health. Partial and future clocks remain visible; zero clocks are null.
type logReconcileCollectorScope struct {
	ServiceKey          string     `json:"service_key"`
	CollectorCount      int        `json:"collector_count"`
	Enabled             *bool      `json:"enabled"`
	CollectorStartedAt  *time.Time `json:"collector_started_at"`
	LastSuccessAt       *time.Time `json:"last_success_at"`
	PreviousWindowStart *time.Time `json:"previous_window_start"`
	PreviousCompletedAt *time.Time `json:"previous_completed_at"`
	LatestWindowStart   *time.Time `json:"latest_window_start"`
	LatestCompletedAt   *time.Time `json:"latest_completed_at"`
	Fresh               *bool      `json:"fresh"`
	ConsecutiveTwo      *bool      `json:"consecutive_two"`
	CoverageState       string     `json:"coverage_state"`
	LastFailurePresent  *bool      `json:"last_failure_present"`
}

// Return a separate UTC value without modifying the collector's clock.
func logReconcileTime(value time.Time) *time.Time {
	if value.IsZero() {
		return nil
	}
	value = value.UTC()
	return &value
}

// The ordered fixed table bounds row count and rejects arbitrary names.
func logReconcileServiceIndex(service string) int {
	for index, key := range logReconcileServiceKeys {
		if service == key {
			return index
		}
	}
	return -1
}

// Use only existing coverage predicates; error presence never names a cause.
func logReconcileScope(service string, enabled bool, started, lastSuccess time.Time, failed bool,
	previous, latest logReconcileSuccess, now time.Time) logReconcileCollectorScope {
	fresh, two := false, false
	row := logReconcileCollectorScope{
		ServiceKey: service, CollectorCount: 1, Enabled: &enabled, Fresh: &fresh, ConsecutiveTwo: &two,
		CollectorStartedAt: logReconcileTime(started), LastSuccessAt: logReconcileTime(lastSuccess),
		PreviousWindowStart: logReconcileTime(previous.start), PreviousCompletedAt: logReconcileTime(previous.completed),
		LatestWindowStart: logReconcileTime(latest.start), LatestCompletedAt: logReconcileTime(latest.completed),
		LastFailurePresent: &failed, CoverageState: "disabled",
	}
	if !enabled {
		return row
	}
	fresh = latest.valid() && !now.Before(latest.completed) && now.Sub(latest.completed) < 2*logReconcileInterval
	two = fresh && previous.advancesTo(latest)
	switch {
	case started.After(now) || lastSuccess.After(now) || latest.start.After(now) || latest.completed.After(now) ||
		previous.start.After(now) || previous.completed.After(now) ||
		(!latest.start.IsZero() && !latest.completed.IsZero() && !latest.completed.After(latest.start)):
		row.CoverageState = "invalid-time"
	case !latest.valid():
		row.CoverageState = "no-valid-window"
	case !fresh:
		row.CoverageState = "stale-window"
	case two:
		row.CoverageState = "fresh-two-windows"
	default:
		row.CoverageState = "fresh-one-window"
	}
	return row
}

// Versioned schema: only fixed public service keys, never arbitrary service,
// block/host identifiers, labels, contents, or errors.
// Empty time ranges are null, never zero-valued claims of completed coverage.
type logReconcileReceipt struct {
	CollectorScopes         []logReconcileCollectorScope `json:"collector_scopes"`
	ScopeProjectionComplete bool                         `json:"scope_projection_complete"`
	UnsupportedCollectors   int                          `json:"unsupported_collectors"`
	DuplicateServiceKeys    int                          `json:"duplicate_service_keys"`
	Schema                  int                          `json:"schema"`
	ObservedAt              time.Time                    `json:"observed_at"`
	Collectors              int                          `json:"collectors"`
	Enabled                 int                          `json:"enabled"`
	Fresh                   int                          `json:"fresh"`
	ConsecutiveTwo          int                          `json:"consecutive_two"`
	CollectorStartedAt      logReconcileTimeRange        `json:"collector_started_at"`
	PreviousWindowStart     logReconcileTimeRange        `json:"previous_window_start"`
	LatestWindowStart       logReconcileTimeRange        `json:"latest_window_start"`
	PreviousCompletedAt     logReconcileTimeRange        `json:"previous_completed_at"`
	LatestCompletedAt       logReconcileTimeRange        `json:"latest_completed_at"`
}

// Values copied under one collector's lock; no mutable collector state escapes.
type logReconcileCollectorSnapshot struct {
	service     string
	enabled     bool
	started     time.Time
	lastSuccess time.Time
	failed      bool
	previous    logReconcileSuccess
	latest      logReconcileSuccess
}

// Sample the observation clock after every copy. This orders the receipt after
// its inputs without making the per-collector snapshots an atomic fleet view.
func (self *logTailProbe) reconcileReceipt(clock func() time.Time) logReconcileReceipt {
	snapshots := make([]logReconcileCollectorSnapshot, 0, len(self.tailers))
	for _, tailer := range self.tailers {
		tailer.stateLock.Lock()
		snapshot := logReconcileCollectorSnapshot{
			service: tailer.service, enabled: tailer.reconcile != nil, started: tailer.startedAt,
			lastSuccess: tailer.lastReconcileTime, failed: tailer.lastReconcileError != "",
			previous: tailer.reconcileSuccesses[0], latest: tailer.reconcileSuccesses[1],
		}
		tailer.stateLock.Unlock()
		snapshots = append(snapshots, snapshot)
	}
	now := clock()
	receipt := logReconcileReceipt{Schema: 2, ObservedAt: now.UTC(), Collectors: len(snapshots),
		CollectorScopes: make([]logReconcileCollectorScope, 0, len(logReconcileServiceKeys))}
	var scopes [len(logReconcileServiceKeys)]logReconcileCollectorScope
	for _, snapshot := range snapshots {
		enabled, started := snapshot.enabled, snapshot.started
		service, lastSuccess, failed := snapshot.service, snapshot.lastSuccess, snapshot.failed
		previous, latest := snapshot.previous, snapshot.latest
		index := logReconcileServiceIndex(service)
		if index < 0 {
			receipt.UnsupportedCollectors++
		} else if scopes[index].CollectorCount == 0 {
			scopes[index] = logReconcileScope(service, enabled, started, lastSuccess, failed, previous, latest, now)
		} else {
			count := scopes[index].CollectorCount + 1
			// Do not choose one duplicate's clocks or health as authoritative.
			scopes[index] = logReconcileCollectorScope{ServiceKey: service, CollectorCount: count, CoverageState: "ambiguous"}
		}
		receipt.CollectorStartedAt.add(started)
		if !enabled {
			continue
		}
		receipt.Enabled++
		if !latest.valid() {
			continue
		}
		receipt.LatestWindowStart.add(latest.start)
		receipt.LatestCompletedAt.add(latest.completed)
		fresh := !now.Before(latest.completed) && now.Sub(latest.completed) < 2*logReconcileInterval
		if fresh {
			receipt.Fresh++
		}
		if previous.advancesTo(latest) {
			receipt.PreviousWindowStart.add(previous.start)
			receipt.PreviousCompletedAt.add(previous.completed)
			if fresh {
				receipt.ConsecutiveTwo++
			}
		}
	}
	for _, scope := range scopes {
		if scope.CollectorCount > 0 {
			receipt.CollectorScopes = append(receipt.CollectorScopes, scope)
		}
		if scope.CollectorCount > 1 {
			receipt.DuplicateServiceKeys++
		}
	}
	receipt.ScopeProjectionComplete = receipt.Collectors > 0 && receipt.UnsupportedCollectors == 0 && receipt.DuplicateServiceKeys == 0
	return receipt
}

// Encoding failure or the fixed byte bound drops only diagnostic evidence.
func encodeLogReconcileReceipt(receipt logReconcileReceipt) []byte {
	encoded, err := json.Marshal(receipt)
	if err != nil || len(encoded)+len(logReconcileReceiptPrefix)+2 > logReconcileReceiptMaxBytes {
		return nil
	}
	return encoded
}

// One existing diagnostic write per cadence, with no collection or I/O changes.
func (self *logTailProbe) writeReconcileReceipt(clock func() time.Time) {
	if self.reconcileDiagnostics == nil {
		return
	}
	encoded := encodeLogReconcileReceipt(self.reconcileReceipt(clock))
	if encoded == nil {
		return
	}
	// One bounded Write, with a leading separator in case another diagnostic
	// ended mid-line. Sink failure cannot alter collection or invent an Alert;
	// an absent or truncated receipt is not completion evidence.
	_, _ = fmt.Fprintf(self.reconcileDiagnostics, "\n%s%s\n", logReconcileReceiptPrefix, encoded)
}
