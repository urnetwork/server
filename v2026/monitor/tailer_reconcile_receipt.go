package monitor

import (
	"encoding/json"
	"fmt"
	"time"
)

// SIGNALS.md §1.5: a successful overlap is not an Alert. Retain only a bounded
// pair of query starts/completions and emit aggregate proof on diagnostics.
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

// Fixed schema: no service/block/host identifiers, labels, contents, or errors.
// Empty time ranges are null, never zero-valued claims of completed coverage.
type logReconcileReceipt struct {
	Schema              int                   `json:"schema"`
	ObservedAt          time.Time             `json:"observed_at"`
	Collectors          int                   `json:"collectors"`
	Enabled             int                   `json:"enabled"`
	Fresh               int                   `json:"fresh"`
	ConsecutiveTwo      int                   `json:"consecutive_two"`
	CollectorStartedAt  logReconcileTimeRange `json:"collector_started_at"`
	PreviousWindowStart logReconcileTimeRange `json:"previous_window_start"`
	LatestWindowStart   logReconcileTimeRange `json:"latest_window_start"`
	PreviousCompletedAt logReconcileTimeRange `json:"previous_completed_at"`
	LatestCompletedAt   logReconcileTimeRange `json:"latest_completed_at"`
}

func (self *logTailProbe) reconcileReceipt(now time.Time) logReconcileReceipt {
	receipt := logReconcileReceipt{Schema: 1, ObservedAt: now.UTC(), Collectors: len(self.tailers)}
	for _, tailer := range self.tailers {
		tailer.stateLock.Lock()
		enabled, started := tailer.reconcile != nil, tailer.startedAt
		previous, latest := tailer.reconcileSuccesses[0], tailer.reconcileSuccesses[1]
		tailer.stateLock.Unlock()
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
	return receipt
}

func (self *logTailProbe) writeReconcileReceipt(now time.Time) {
	if self.reconcileDiagnostics == nil {
		return
	}
	encoded, err := json.Marshal(self.reconcileReceipt(now))
	if err != nil {
		return
	}
	// One bounded Write, with a leading separator in case another diagnostic
	// ended mid-line. Sink failure cannot alter collection or invent an Alert;
	// an absent or truncated receipt is not completion evidence.
	_, _ = fmt.Fprintf(self.reconcileDiagnostics, "\n%s%s\n", logReconcileReceiptPrefix, encoded)
}
