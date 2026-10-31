// Exercises bucket-version diagnostic authority and privacy through the public
// log signal. Synthetic successful log windows never contact Mimir or Grafana.
package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// One fixed log window, with no registry discovery or additional source read.
func mimirBucketVersionAlerts(t *testing.T, body string) Alerts {
	t.Helper()
	calls := 0
	source := &syntheticSource{localFn: func(string, ...string) (string, error) { calls++; return body, nil }}
	settings := syntheticSettings(source)
	settings.LogServices = []string{"grafana"}
	alerts, err := NewLogErrorsSignal().Run(context.Background(), settings)
	if err != nil || calls != 1 {
		t.Fatal("synthetic bucket log window changed source cost or completion")
	}
	return alerts
}

// Zero and nonzero version gaps retain the WARN policy while avoiding an
// invented index age, missed-sync diagnosis or successful-query claim.
func TestMimirBucketVersionWarningDoesNotInventAgeOrSyncCause(t *testing.T) {
	for _, versions := range []struct {
		ours      int
		requested int
		diff      int
	}{
		{ours: 0, requested: 2000, diff: -2000},
		{ours: 1000, requested: 2800, diff: -1800},
	} {
		line := fmt.Sprintf(`caller=bucket.go:1248 user=private-bucket-tenant ours=%d requested=%d diff=%d msg="bucket index version (updated_at) is older than requested" private-bucket-suffix`, versions.ours, versions.requested, versions.diff)
		alerts := mimirBucketVersionAlerts(t, line)
		if len(alerts) != 1 {
			t.Fatal("numeric bucket version gap changed alert count")
		}
		alert := alerts[0]
		if alert.Class != "mimir-bucket-index-lag" || alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.unobservable || !strings.Contains(alert.Observed, "rate=1/min") {
			t.Fatal("bucket version warning lost its existing policy")
		}
		for _, want := range []string{"not a measured age", "does not prove missed syncs", "does not establish the running producer version"} {
			if !strings.Contains(alert.Mechanism, want) {
				t.Fatal("bucket version warning invented index age or sync authority")
			}
		}
		if !strings.Contains(alert.Context, "not by itself a failed query or proof of successful RPC completion") {
			t.Fatal("bucket version warning invented an RPC outcome")
		}
		if !strings.Contains(alert.Evidence, fmt.Sprintf("ours=%d requested=%d diff=%d", versions.ours, versions.requested, versions.diff)) {
			t.Fatal("fixed bucket version tuple changed")
		}
		requireAlertOmits(t, alert, "private-bucket-tenant", "private-bucket-suffix")
	}
}

// Public evidence preserves only the finite caller/version projection. Missing
// or malformed fields cannot fall back to a raw tenant or arbitrary metadata.
func TestMimirBucketVersionSampleOmitsTenantAndUnknownMetadata(t *testing.T) {
	for _, fields := range []struct {
		body    string
		unknown bool
	}{
		{body: "ours=0 requested=2000 diff=-2000"},
		{body: "ours=2026-08-31T21:56:50Z requested=2026-08-31T22:26:50Z diff=-1800"},
		{body: "ours=private-bucket-version requested=2000 diff=-2000", unknown: true},
		{body: "diff=-2000", unknown: true},
	} {
		line := `caller=bucket.go:1248 user=private-bucket-tenant url=https://private-bucket.example/key ` + fields.body + ` msg="bucket index version (updated_at) is older than requested" private-bucket-suffix`
		alerts := mimirBucketVersionAlerts(t, line)
		if len(alerts) != 1 || alerts[0].Class != "mimir-bucket-index-lag" || alerts[0].Severity != SeverityWarn {
			t.Fatal("sample projection changed warning classification")
		}
		if strings.Contains(alerts[0].Evidence, "version fields unavailable") != fields.unknown {
			t.Fatal("sample projection invented valid version fields")
		}
		requireAlertOmits(t, alerts[0], "private-bucket-tenant", "private-bucket.example", "private-bucket-version", "private-bucket-suffix")
	}
}

// Sub-threshold records remain below this log rule; silence alone is not a
// metric freshness verdict or proof of source coverage and recovery.
func TestMimirBucketVersionThresholdRemainsUnchanged(t *testing.T) {
	for _, diff := range []int{-873, -1799} {
		line := fmt.Sprintf(`caller=bucket.go:1248 ours=10000 requested=%d diff=%d msg="bucket index version (updated_at) is older than requested"`, 10000-diff, diff)
		if alerts := mimirBucketVersionAlerts(t, line); len(alerts) != 0 {
			t.Fatal("sub-threshold bucket version gap became a warning")
		}
	}
}
