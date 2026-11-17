// Synthetic raw metric pairs exercise absolute source authority through the
// public signal. Nothing here samples a database or service gateway.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// Mutable synthetic metric rows permit malformed source values as controls.
type pgDiskSpaceTestSeries struct {
	Metric map[string]string `json:"metric"`
	Values [][]any           `json:"values"`
}

// A complete raw matrix carries original scrape times and no API warnings.
type pgDiskSpaceTestResponse struct {
	Status   string   `json:"status"`
	Warnings []string `json:"warnings,omitempty"`
	Data     struct {
		ResultType string                  `json:"resultType"`
		Result     []pgDiskSpaceTestSeries `json:"result"`
	} `json:"data"`
}

// Equal logical labels join the byte gauges; test-only identities stay synthetic.
func pgDiskSpaceTestBody(t *testing.T, available string, mutate func(*pgDiskSpaceTestResponse)) string {
	t.Helper()
	response := pgDiskSpaceTestResponse{Status: "success"}
	response.Data.ResultType = "matrix"
	at := syntheticSettings(nil).Now()
	for _, metric := range []string{"node_filesystem_avail_bytes", "node_filesystem_size_bytes"} {
		value := available
		if metric == "node_filesystem_size_bytes" {
			value = "1000000000000"
		}
		response.Data.Result = append(response.Data.Result, pgDiskSpaceTestSeries{
			Metric: map[string]string{"__name__": metric, "env": "synthetic", "job": "node", "host": "pg-1", "device": "/dev/synthetic-root", "fstype": "ext4", "mountpoint": "/"},
			Values: [][]any{{at.Add(-30 * time.Second).Unix(), value}, {at.Add(-15 * time.Second).Unix(), value}},
		})
	}
	if mutate != nil {
		mutate(&response)
	}
	output, err := json.Marshal(response)
	if err != nil {
		t.Fatal("synthetic metric encoding failed")
	}
	return string(output)
}

// Check the actual source command and gateway scope on every public run.
func pgDiskSpaceTestSettings(t *testing.T, output string, sourceErr error) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{hostFn: func(target HostSettings, command string) (string, error) {
		if target.Name != "metrics-1" || command != pgDiskSpaceCommand("synthetic", "pg-1", syntheticSettings(nil).Now()) {
			t.Fatal("disk metric source escaped its bounded inventory-scoped request")
		}
		return output, sourceErr
	}})
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

// The decimal boundary is inclusive, and a real temporary crossing still
// warns without inventing persistent growth or a critical tier.
func TestPgDiskSpaceAbsoluteBoundary(t *testing.T) {
	for _, test := range []struct {
		available string
		warn      bool
	}{
		{available: "100000000001", warn: false},
		{available: "107374182400", warn: false},
		{available: "100000000000", warn: true},
		{available: "99999999999", warn: true},
		{available: "0", warn: true},
	} {
		settings := pgDiskSpaceTestSettings(t, pgDiskSpaceTestBody(t, test.available, nil), nil)
		alerts, err := NewPgDiskSpaceSignal().Run(context.Background(), settings)
		if err != nil || (!test.warn && len(alerts) != 0) || (test.warn && len(alerts) != 1) {
			t.Fatalf("available %s did not follow the exact absolute policy", test.available)
		}
		if test.warn {
			alert := requireAlertClass(t, alerts, "pg-disk-space-low")
			if alert.Severity != SeverityWarn || alert.Sustain != 1 || alert.PageSustain != 0 || alert.unobservable || alert.Target != "pg-1/root" || alert.SignalKey != "pg-disk-space" {
				t.Fatal("measured floor warning lost its independent policy")
			}
			if !strings.Contains(alert.Observed, "available_bytes="+test.available) || !strings.Contains(alert.Observed, "safety_line_bytes=100000000000") {
				t.Fatal("absolute warning changed the source quantity or decimal unit")
			}
			requireAlertOmits(t, alert, "/dev/synthetic-root")
		}
	}
}

// Missing, foreign, ambiguous and impossible gauges cannot turn into zero
// free bytes or authoritative health, even with one valid low-space gauge.
func TestPgDiskSpaceInvalidMatrixRemainsUnknown(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*pgDiskSpaceTestResponse)
	}{
		{name: "empty", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result = nil }},
		{name: "missing size", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result = r.Data.Result[:1] }},
		{name: "duplicate family", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[1] = r.Data.Result[0] }},
		{name: "extra source", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result = append(r.Data.Result, r.Data.Result[0]) }},
		{name: "warning", mutate: func(r *pgDiskSpaceTestResponse) { r.Warnings = []string{"private-source-canary"} }},
		{name: "failed API", mutate: func(r *pgDiskSpaceTestResponse) { r.Status = "error" }},
		{name: "resampled vector", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.ResultType = "vector" }},
		{name: "foreign host", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["host"] = "private-source-canary" }},
		{name: "foreign environment", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["env"] = "other" }},
		{name: "foreign job", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["job"] = "backup" }},
		{name: "archive mount", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["mountpoint"] = "/archive" }},
		{name: "unknown metric", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["__name__"] = "private-source-canary" }},
		{name: "unknown label", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["private-source-canary"] = "hidden" }},
		{name: "missing device", mutate: func(r *pgDiskSpaceTestResponse) { delete(r.Data.Result[0].Metric, "device") }},
		{name: "mismatched device", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["device"] = "/dev/private-source-canary" }},
		{name: "missing filesystem type", mutate: func(r *pgDiskSpaceTestResponse) { delete(r.Data.Result[0].Metric, "fstype") }},
		{name: "mismatched instance", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Metric["instance"] = "private-source-canary" }},
		{name: "missing values", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values = nil }},
		{name: "zero size", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[1].Values[1][1] = "0" }},
		{name: "available exceeds size", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[1].Values[1][1] = "1" }},
		{name: "negative", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][1] = "-1" }},
		{name: "nonfinite", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][1] = "NaN" }},
		{name: "infinite", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][1] = "+Inf" }},
		{name: "fractional bytes", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][1] = "1.5" }},
		{name: "inexact integer", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][1] = "9007199254740992" }},
		{name: "numeric JSON value", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][1] = 1 }},
	} {
		settings := pgDiskSpaceTestSettings(t, pgDiskSpaceTestBody(t, "99999999999", test.mutate), nil)
		alerts, err := NewPgDiskSpaceSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s: invalid source lost its single visibility observation", test.name)
		}
		alert := requireAlertClass(t, alerts, "pg-disk-space-unobservable")
		if !alert.unobservable {
			t.Fatalf("%s: invalid source can clear a prior incident", test.name)
		}
		requireAlertOmits(t, alert, "private-source-canary", "/dev/synthetic-root")
	}
}

// The matrix's latest original scrape time, ordering and gauge alignment own
// freshness; request completion cannot freshen retained or future samples.
func TestPgDiskSpaceSourceClockAuthority(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*pgDiskSpaceTestResponse)
	}{
		{name: "stale", mutate: func(r *pgDiskSpaceTestResponse) {
			r.Data.Result[0].Values = [][]any{{syntheticSettings(nil).Now().Add(-91 * time.Second).Unix(), "1"}}
		}},
		{name: "future", mutate: func(r *pgDiskSpaceTestResponse) {
			r.Data.Result[0].Values[1][0] = syntheticSettings(nil).Now().Add(time.Second).Unix()
		}},
		{name: "duplicate stamp", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][0] = r.Data.Result[0].Values[0][0] }},
		{name: "reversed samples", mutate: func(r *pgDiskSpaceTestResponse) {
			r.Data.Result[0].Values[0], r.Data.Result[0].Values[1] = r.Data.Result[0].Values[1], r.Data.Result[0].Values[0]
		}},
		{name: "latest pair incomplete", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[1].Values = r.Data.Result[1].Values[:1] }},
		{name: "source time malformed", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1][0] = "private-source-canary" }},
		{name: "pair truncated", mutate: func(r *pgDiskSpaceTestResponse) { r.Data.Result[0].Values[1] = r.Data.Result[0].Values[1][:1] }},
	} {
		alerts, err := NewPgDiskSpaceSignal().Run(context.Background(), pgDiskSpaceTestSettings(t, pgDiskSpaceTestBody(t, "1", test.mutate), nil))
		if err != nil || len(alerts) != 1 || !requireAlertClass(t, alerts, "pg-disk-space-unobservable").unobservable {
			t.Fatalf("%s: source clock uncertainty escaped visibility", test.name)
		}
	}
	output := pgDiskSpaceTestBody(t, "1", nil)
	at := syntheticSettings(nil).Now()
	for _, completedAt := range []time.Time{at.Add(-time.Second), at.Add(80 * time.Second)} {
		if _, reason := parsePgDiskSpace(output, "synthetic", "pg-1", at, completedAt); reason == "" {
			t.Fatal("completion delay or clock rollback freshened retained evidence")
		}
	}
}

// Source series order is immaterial; no maximum, mean or selected source
// can turn a duplicate cohort into a valid pair.
func TestPgDiskSpaceReversedFamiliesKeepTheSameVerdict(t *testing.T) {
	for _, available := range []string{"1", "100000000001"} {
		var baseline Alerts
		for _, reverse := range []bool{false, true} {
			output := pgDiskSpaceTestBody(t, available, func(r *pgDiskSpaceTestResponse) {
				if reverse {
					r.Data.Result[0], r.Data.Result[1] = r.Data.Result[1], r.Data.Result[0]
				}
			})
			alerts, err := NewPgDiskSpaceSignal().Run(context.Background(), pgDiskSpaceTestSettings(t, output, nil))
			if err != nil {
				t.Fatal("reordered complete source failed")
			}
			if !reverse {
				baseline = alerts
				continue
			}
			if len(alerts) != len(baseline) || (len(alerts) == 1 && (alerts[0].Class != baseline[0].Class || alerts[0].Observed != baseline[0].Observed)) {
				t.Fatal("source family order changed the absolute verdict")
			}
		}
	}
}

// A valid-looking prefix never outranks producer failure, excessive bytes or
// malformed framing, and private causes must not appear in rendered findings.
func TestPgDiskSpaceFailedAndBoundedSource(t *testing.T) {
	valid := pgDiskSpaceTestBody(t, "1", nil)
	for _, test := range []struct {
		output string
		err    error
	}{
		{output: valid, err: errors.New("exit status 1: private-source-canary")},
		{output: valid + strings.Repeat(" ", pgDiskSpaceMaxBytes), err: nil},
		{output: valid + "private-source-canary", err: nil},
	} {
		alerts, err := NewPgDiskSpaceSignal().Run(context.Background(), pgDiskSpaceTestSettings(t, test.output, test.err))
		if err != nil || len(alerts) != 1 {
			t.Fatal("source failure published an absolute verdict")
		}
		alert := requireAlertClass(t, alerts, "pg-disk-space-unobservable")
		requireAlertOmits(t, alert, "private-source-canary", "/dev/synthetic-root")
		if test.err != nil && !strings.Contains(alert.Observed, "error_class=observation-command-failed") {
			t.Fatal("fixed diagnostics discarded native source failure taxonomy")
		}
	}
}

// Invalid or excluded inventory cannot select another database or contact a
// denied gateway; no source-derived target is ever dialed.
func TestPgDiskSpaceInventoryAuthority(t *testing.T) {
	for _, mode := range []string{"no primary", "multiple primaries", "no gateway", "excluded primary", "excluded gateway", "disabled primary"} {
		settings := syntheticSettings(&syntheticSource{hostFn: func(HostSettings, string) (string, error) {
			t.Fatal("unowned or denied inventory contacted a source")
			return "", nil
		}})
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
		switch mode {
		case "no primary":
			settings.Hosts = settings.Hosts[1:]
		case "multiple primaries":
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		case "no gateway":
			settings.Hosts = settings.Hosts[:2]
		case "excluded primary":
			settings.ExcludedHosts = []string{"pg-1"}
		case "excluded gateway":
			settings.ExcludedHosts = []string{"metrics-1"}
		case "disabled primary":
			settings.disabledHosts = []HostSettings{settings.Hosts[0]}
			settings.Hosts = settings.Hosts[1:]
		}
		alerts, err := NewPgDiskSpaceSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) == 0 {
			t.Fatalf("%s: inventory lost coverage uncertainty", mode)
		}
		for _, alert := range alerts {
			if !alert.unobservable {
				t.Fatalf("%s: invalid inventory certified a product state", mode)
			}
		}
	}
}

// Both unscoped and scoped callers retain lifecycle plus original source
// cause, instead of publishing a successful floor observation after cancel.
func TestPgDiskSpaceCancellationKeepsOriginalCause(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		for _, beforeContact := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			cause := errors.New("private-source-canary")
			settings := pgDiskSpaceTestSettings(t, "", nil)
			settings.Source = &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				if beforeContact {
					t.Fatal("canceled work contacted a source")
				}
				cancel()
				return pgDiskSpaceTestBody(t, "1", nil), cause
			}}
			if scoped {
				settings.ExcludedHosts = []string{"redis-1"}
			}
			if beforeContact {
				cancel()
			}
			alerts, err := NewPgDiskSpaceSignal().Run(ctx, settings)
			cancel()
			if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (!beforeContact && !errors.Is(err, cause)) {
				t.Fatal("canceled source lost original causes or published a verdict")
			}
			if strings.Contains(err.Error(), "private-source-canary") {
				t.Fatal("lifecycle error exposed its private source")
			}
		}
	}
}
