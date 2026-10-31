package monitor

import (
	"strings"
	"testing"
	"time"
)

// Keep the configured public service census independent of the projection table.
// The mmm site service previously became an unsupported collector even though
// its collector and coverage counts were retained in the aggregate.
func TestReconcileReceiptConfiguredMmmServiceScope(t *testing.T) {
	now := time.Date(2026, 10, 25, 16, 0, 0, 0, time.UTC)
	services := []string{"alt", "api", "app", "connect", "gossip", "grafana", "mcp", "mmm", "proxy", "taskworker", "web"}
	tailers := make([]*logTailer, 0, len(services))
	for _, service := range services {
		tailers = append(tailers, syntheticScopePair(now, service))
	}
	receipt := (&logTailProbe{tailers: tailers}).reconcileReceipt(fixedReconcileReceiptClock(now))
	row := requireCollectorScope(t, receipt, "mmm")
	if !receipt.ScopeProjectionComplete || receipt.UnsupportedCollectors != 0 || receipt.DuplicateServiceKeys != 0 ||
		len(receipt.CollectorScopes) != 11 || receipt.Collectors != 11 || receipt.Enabled != 11 || receipt.Fresh != 11 || receipt.ConsecutiveTwo != 11 {
		t.Fatal("configured eleven-service census lost fixed-key identity or aggregate authority")
	}
	if row.CollectorCount != 1 || row.Enabled == nil || !*row.Enabled || row.Fresh == nil || !*row.Fresh ||
		row.ConsecutiveTwo == nil || !*row.ConsecutiveTwo || row.CoverageState != "fresh-two-windows" {
		t.Fatal("mmm collector did not retain its own coverage")
	}
	if encoded := encodeLogReconcileReceipt(receipt); len(encoded) == 0 || len(encoded)+len(logReconcileReceiptPrefix)+2 > logReconcileReceiptMaxBytes {
		t.Fatal("eleven fixed service rows exceeded the unchanged diagnostic cap")
	}
}

// Adding one public key must not bless arbitrary labels, disabled coverage, or
// select one of two collectors as the authoritative mmm scope.
func TestReconcileReceiptMmmUnknownDisabledAndDuplicateControls(t *testing.T) {
	now := time.Date(2026, 10, 25, 16, 0, 0, 0, time.UTC)
	disabled := syntheticScopePair(now, "mmm")
	disabled.reconcile = nil
	receipt := (&logTailProbe{tailers: []*logTailer{disabled, syntheticScopePair(now, "private-unregistered-canary")}}).reconcileReceipt(fixedReconcileReceiptClock(now))
	row := requireCollectorScope(t, receipt, "mmm")
	if receipt.ScopeProjectionComplete || receipt.UnsupportedCollectors != 1 || len(receipt.CollectorScopes) != 1 ||
		row.Enabled == nil || *row.Enabled || row.Fresh == nil || *row.Fresh || row.CoverageState != "disabled" {
		t.Fatal("unknown or disabled scope acquired supported healthy coverage")
	}
	if strings.Contains(string(encodeLogReconcileReceipt(receipt)), "private-unregistered-canary") {
		t.Fatal("unsupported service label escaped the fixed projection")
	}
	receipt = (&logTailProbe{tailers: []*logTailer{syntheticScopePair(now, "mmm"), syntheticScopePair(now, "mmm")}}).reconcileReceipt(fixedReconcileReceiptClock(now))
	row = requireCollectorScope(t, receipt, "mmm")
	if receipt.ScopeProjectionComplete || receipt.DuplicateServiceKeys != 1 || row.CollectorCount != 2 ||
		row.CoverageState != "ambiguous" || row.Enabled != nil || row.Fresh != nil || row.ConsecutiveTwo != nil {
		t.Fatal("duplicate mmm collectors acquired a selected healthy scope")
	}
}

// Exercise the fixed census with every optional failure timestamp present and
// nanosecond precision, rather than measuring only null failure fields.
func TestReconcileReceiptElevenServiceFullTimestampEncodingBound(t *testing.T) {
	now := time.Date(2026, 10, 25, 16, 0, 0, 999999999, time.UTC)
	services := []string{"alt", "api", "app", "connect", "gossip", "grafana", "mcp", "mmm", "proxy", "taskworker", "web"}
	tailers := make([]*logTailer, 0, len(services))
	for _, service := range services {
		tailer := syntheticScopePair(now, service)
		tailer.lastReconcileError = "private-error-canary"
		tailer.lastReconcileFailure = logReconcileFailure{monitorRecordedAt: now, attemptWindowStart: now.Add(-logReconcileLookback)}
		tailers = append(tailers, tailer)
	}
	receipt := (&logTailProbe{tailers: tailers}).reconcileReceipt(fixedReconcileReceiptClock(now))
	encoded := encodeLogReconcileReceipt(receipt)
	if len(encoded) == 0 || len(encoded)+len(logReconcileReceiptPrefix)+2 > logReconcileReceiptMaxBytes {
		t.Fatal("full-timestamp eleven-service census exceeded the unchanged diagnostic cap")
	}
	if strings.Contains(string(encoded), "private-error-canary") {
		t.Fatal("failure error escaped the bounded timestamp projection")
	}
}
