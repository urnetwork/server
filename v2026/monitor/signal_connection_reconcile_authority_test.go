// Connection reconcile observations retain the authority of each required
// metric instead of turning missing or ambiguous evidence into measured zero.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Uses the existing wire fixture and includes both initialized optional counters.
func connectionReconcileAuthorityFixture(t *testing.T, blocks []connectionReconcileFixtureBlock) (mimirInstantResponse, time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	var response mimirInstantResponse
	if err := json.Unmarshal([]byte(connectionReconcileFixtureJSON(t, now, blocks)), &response); err != nil {
		t.Fatal(err)
	}
	for _, block := range blocks {
		for _, class := range []string{"failed_retires", "busy_retires"} {
			present := false
			for _, sample := range response.Data.Result {
				if sample.Metric["host"] == block.host && sample.Metric["block"] == block.block && sample.Metric["monitor_metric"] == class {
					present = true
					break
				}
			}
			if !present {
				sample := response.Data.Result[0]
				sample.Metric = map[string]string{"host": block.host, "block": block.block, "monitor_metric": class}
				sample.Value = []json.RawMessage{sample.Value[0], json.RawMessage(`"0"`)}
				response.Data.Result = append(response.Data.Result, sample)
			}
		}
	}
	return response, now
}

// Drives the real probe and public adapter with one synthetic Mimir payload.
func runConnectionReconcileAuthorityFixture(t *testing.T, response mimirInstantResponse, now time.Time) ([]finding, Alerts) {
	t.Helper()
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics.example" || !strings.Contains(command, "urnetwork_connect_connection_reconcile_unheld") {
			return "", fmt.Errorf("unexpected synthetic observation")
		}
		return string(payload), nil
	}}
	settings := syntheticSettings(source)
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics.example", Roles: []string{"services"}})
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (connectionReconcileProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	alerts, err := NewConnectionReconcileSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	return findings, alerts
}

// Requires exact public classes and prevents partial frames from clearing tickets.
func requireConnectionReconcileAuthorityClasses(t *testing.T, findings []finding, alerts Alerts, want ...string) {
	t.Helper()
	got := []string{}
	for _, alert := range alerts {
		got = append(got, alert.Class)
	}
	slices.Sort(got)
	want = slices.Clone(want)
	slices.Sort(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("reconcile authority classes = %v, want %v", got, want)
	}
	if slices.Contains(want, "connection-reconcile-unobservable") {
		for _, finding := range findings {
			if finding.healthy {
				t.Fatalf("partial reconcile frame emitted healthy class %s", finding.class)
			}
		}
	}
}

// A healthy sibling cannot hide a gauge-less block or its proven failed passes.
func TestConnectionReconcileAuthorityRetainsGaugeLessBlock(t *testing.T) {
	response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a.example", block: "g1", unheld: 0, okPasses: 1},
		{host: "connect-b.example", block: "g2", unheld: -1, errorPasses: 6},
	})
	blocks, reason := parseConnectionReconcileFrame(response, now)
	if len(blocks) != 2 || !strings.Contains(reason, "no_reconcile_gauge") {
		t.Fatalf("gauge-less block was dropped: blocks=%d reason=%q", len(blocks), reason)
	}
	findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
	requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable", "connection-reconcile-failing")
	if !strings.Contains(requireAlertClass(t, alerts, "connection-reconcile-failing").Observed, "connect-b.example/g2 unheld=unknown") {
		t.Fatal("gauge-less failing block was rendered as a measured zero")
	}
}

// Each missing initialized core counter is unknown, never an inferred zero.
func TestConnectionReconcileAuthorityMissingCoreCounters(t *testing.T) {
	for _, c := range []struct {
		class string
		block connectionReconcileFixtureBlock
	}{
		{class: "retired", block: connectionReconcileFixtureBlock{host: "connect.example", block: "g1", unheld: 256, retired: 8, okPasses: 1}},
		{class: "ok_passes", block: connectionReconcileFixtureBlock{host: "connect.example", block: "g1", unheld: 0, okPasses: 1, errorPasses: 6}},
		{class: "error_passes", block: connectionReconcileFixtureBlock{host: "connect.example", block: "g1", unheld: 0}},
	} {
		response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{c.block})
		for i := len(response.Data.Result) - 1; i >= 0; i-- {
			if response.Data.Result[i].Metric["monitor_metric"] == c.class {
				response.Data.Result = append(response.Data.Result[:i], response.Data.Result[i+1:]...)
			}
		}
		findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
		if len(alerts) != 1 || alerts[0].Class != "connection-reconcile-unobservable" {
			t.Fatalf("missing core %s was interpreted as zero: alerts=%v", c.class, alerts)
		}
		requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable")
	}
}

// Conflicting and equal duplicates invalidate their metric in every order.
func TestConnectionReconcileAuthorityDuplicatesAreOrderIndependent(t *testing.T) {
	for _, class := range []string{"unheld", "retired", "ok_passes", "error_passes", "failed_retires", "busy_retires"} {
		for _, values := range [][2]string{{`"0"`, `"256"`}, {`"256"`, `"0"`}, {`"0"`, `"0"`}} {
			block := connectionReconcileFixtureBlock{host: "connect.example", block: "g1", unheld: 0, okPasses: 1}
			if class == "retired" {
				block.unheld = 256
			}
			if class == "ok_passes" {
				block.errorPasses = 6
			}
			if class == "error_passes" {
				block.okPasses = 0
			}
			response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{block})
			for i, sample := range response.Data.Result {
				if sample.Metric["monitor_metric"] == class {
					response.Data.Result[i].Value = []json.RawMessage{sample.Value[0], json.RawMessage(values[0])}
					sample.Value = []json.RawMessage{sample.Value[0], json.RawMessage(values[1])}
					response.Data.Result = append(response.Data.Result, sample)
					break
				}
			}
			findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
			if len(alerts) != 1 || alerts[0].Class != "connection-reconcile-unobservable" {
				t.Fatalf("duplicate %s values %v retained last-writer authority: alerts=%v", class, values, alerts)
			}
			requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable")
			if !strings.Contains(alerts[0].Observed, "duplicate_metric") {
				t.Fatalf("duplicate %s has no structural reason", class)
			}
		}
	}
}

// Invalid siblings retain complete positives and export no raw malformed payload.
func TestConnectionReconcileAuthorityMalformedSiblingPreservesPositive(t *testing.T) {
	for _, kind := range []string{"text", "nan", "infinity", "negative", "stale", "future", "shape", "timestamp", "unknown-class", "unlabeled"} {
		for _, reversed := range []bool{false, true} {
			response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
				{host: "connect-a.example", block: "g1", unheld: 256, errorPasses: 6},
				{host: "connect-b.example", block: "g2", unheld: 0, okPasses: 1},
			})
			for i := range response.Data.Result {
				sample := &response.Data.Result[i]
				if sample.Metric["host"] != "connect-b.example" || sample.Metric["monitor_metric"] != "retired" {
					continue
				}
				switch kind {
				case "text":
					sample.Value[1] = json.RawMessage(`"synthetic-private-value"`)
				case "nan":
					sample.Value[1] = json.RawMessage(`"NaN"`)
				case "infinity":
					sample.Value[1] = json.RawMessage(`"+Inf"`)
				case "negative":
					sample.Value[1] = json.RawMessage(`"-1"`)
				case "stale":
					sample.Value[0] = json.RawMessage(fmt.Sprint(now.Add(-connectionReconcileFreshness - time.Second).Unix()))
				case "future":
					sample.Value[0] = json.RawMessage(fmt.Sprint(now.Add(31 * time.Second).Unix()))
				case "shape":
					sample.Value = sample.Value[:1]
				case "timestamp":
					sample.Value[0] = json.RawMessage(`"synthetic-private-timestamp"`)
				case "unknown-class":
					sample.Metric["monitor_metric"] = "synthetic-private-class"
				case "unlabeled":
					delete(sample.Metric, "host")
					sample.Metric["block"] = "synthetic-private-block"
				}
			}
			if reversed {
				slices.Reverse(response.Data.Result)
			}
			findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
			requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable", "connection-reconcile-stuck", "connection-reconcile-failing")
			if strings.Contains(alerts.Markdown(), "synthetic-private") {
				t.Fatalf("malformed %s leaked raw evidence", kind)
			}
			for _, class := range []string{"connection-reconcile-stuck", "connection-reconcile-failing"} {
				if strings.Contains(requireAlertClass(t, alerts, class).Observed, "connect-b.example") {
					t.Fatalf("malformed %s sibling gained positive authority", kind)
				}
			}
		}
	}
}

// A partial optional family is unknown without vetoing independent core evidence.
func TestConnectionReconcileAuthorityOptionalCompanionPreservesPositive(t *testing.T) {
	for _, missing := range []string{"failed_retires", "busy_retires"} {
		response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
			{host: "connect.example", block: "g1", unheld: 256, okPasses: 1, failedRetires: 6},
		})
		for i := len(response.Data.Result) - 1; i >= 0; i-- {
			if response.Data.Result[i].Metric["monitor_metric"] == missing {
				response.Data.Result = append(response.Data.Result[:i], response.Data.Result[i+1:]...)
			}
		}
		want := []string{"connection-reconcile-unobservable", "connection-reconcile-stuck"}
		if missing == "busy_retires" {
			want = append(want, "connection-reconcile-failing")
		}
		findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
		requireConnectionReconcileAuthorityClasses(t, findings, alerts, want...)
		if !strings.Contains(requireAlertClass(t, alerts, "connection-reconcile-stuck").Observed, missing+"=unknown") {
			t.Fatalf("missing optional companion %s was rendered as zero", missing)
		}
	}
}

// Five partial cadences cannot clear incidents; five complete healthy ones can.
func TestConnectionReconcileAuthorityPartialFrameCannotResolveIncident(t *testing.T) {
	response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a.example", block: "g1", unheld: 256, errorPasses: 6},
	})
	broken, _ := runConnectionReconcileAuthorityFixture(t, response, now)
	manager := newTicketManager("synthetic", nil)
	manager.now = func() time.Time { return now }
	for range 2 {
		manager.ingest(context.Background(), broken)
	}
	if manager.openCount() != 2 {
		t.Fatal("complete broken fixture did not open both reconcile incidents")
	}
	response, _ = connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a.example", block: "g1", unheld: 0, okPasses: 1},
		{host: "connect-b.example", block: "g2", unheld: -1, okPasses: 1},
	})
	partial, _ := runConnectionReconcileAuthorityFixture(t, response, now)
	for range manager.resolveTicks {
		manager.ingest(context.Background(), partial)
	}
	for _, class := range []string{"connection-reconcile-stuck", "connection-reconcile-failing"} {
		key := ticketIdentity{probeId: "mimir/connection-reconcile", class: class, target: "synthetic/connect-fleet"}.key()
		incident := manager.tickets[key]
		if incident == nil || !incident.open || incident.healthyStreak != 0 {
			t.Fatalf("partial frame resolved or advanced recovery for %s", class)
		}
	}
	response, _ = connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a.example", block: "g1", unheld: 0, okPasses: 1},
		{host: "connect-b.example", block: "g2", unheld: 0, okPasses: 1},
	})
	healthy, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
	requireConnectionReconcileAuthorityClasses(t, healthy, alerts)
	for range manager.resolveTicks {
		manager.ingest(context.Background(), healthy)
	}
	if manager.openCount() != 0 {
		t.Fatal("complete healthy frame did not restore ordinary ticket recovery")
	}
}

// Explicit zeros, partial increases and the existing timestamp boundaries stay valid.
func TestConnectionReconcileAuthorityMeasuredZeroAndWindowControls(t *testing.T) {
	for _, age := range []time.Duration{0, connectionReconcileFreshness, -30 * time.Second} {
		for _, block := range []connectionReconcileFixtureBlock{
			{host: "connect.example", block: "g1", unheld: 0},
			{host: "connect.example", block: "g1", unheld: 255, okPasses: 1, errorPasses: 6},
			{host: "connect.example", block: "g1", unheld: 5000, retired: 0.25, okPasses: 0.25, failedRetires: 2},
		} {
			response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{block})
			for i := range response.Data.Result {
				response.Data.Result[i].Value[0] = json.RawMessage(fmt.Sprint(now.Add(-age).Unix()))
			}
			findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
			requireConnectionReconcileAuthorityClasses(t, findings, alerts)
			if len(findings) != 3 {
				t.Fatalf("complete healthy control emitted %d findings, want 3", len(findings))
			}
			for _, finding := range findings {
				if !finding.healthy {
					t.Fatalf("complete healthy control raised %s", finding.class)
				}
			}
		}
	}
}

// The server still handles per-series resets before aggregation over the same range.
func TestConnectionReconcileAuthorityQueryKeepsCounterWindow(t *testing.T) {
	query := connectionReconcileQuery("synthetic")
	if strings.Count(query, "sum by (host,block) (increase(") != 5 || strings.Count(query, "[15m]") != 5 ||
		strings.Contains(query, "increase(sum") || strings.Contains(query, "vector(0)") {
		t.Fatal("reconcile query lost per-series counter increase or invented a zero")
	}
	for _, metric := range []string{"retired_total", "passes_total", "unretired_total"} {
		if !strings.Contains(query, "sum by (host,block) (increase(urnetwork_connect_connection_reconcile_"+metric) {
			t.Fatalf("counter %s no longer increases before aggregation", metric)
		}
	}
}

// Two distinct label tuples cannot synthesize one complete block by joining names.
func TestConnectionReconcileAuthorityLabelTuplesStaySeparate(t *testing.T) {
	response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect.example/a", block: "b", unheld: 256, okPasses: 1},
		{host: "connect.example", block: "a/b", unheld: 0, okPasses: 1},
	})
	for i := len(response.Data.Result) - 1; i >= 0; i-- {
		sample := response.Data.Result[i]
		keep := sample.Metric["host"] == "connect.example/a" && slices.Contains([]string{"unheld", "ok_passes", "error_passes"}, sample.Metric["monitor_metric"])
		keep = keep || (sample.Metric["host"] == "connect.example" && sample.Metric["monitor_metric"] == "retired")
		if !keep {
			response.Data.Result = append(response.Data.Result[:i], response.Data.Result[i+1:]...)
		}
	}
	findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
	requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable")
}

// Mixed positive, missing and duplicate metrics render identically under permutation.
func TestConnectionReconcileAuthorityMixedFramePermutation(t *testing.T) {
	response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect-a.example", block: "g1", unheld: 256, errorPasses: 6},
		{host: "connect-b.example", block: "g2", unheld: -1, okPasses: 1},
	})
	for _, sample := range response.Data.Result {
		if sample.Metric["host"] == "connect-b.example" && sample.Metric["monitor_metric"] == "retired" {
			response.Data.Result = append(response.Data.Result, sample)
			break
		}
	}
	var want string
	for i := range len(response.Data.Result) {
		findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
		requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable", "connection-reconcile-stuck", "connection-reconcile-failing")
		if i == 0 {
			want = alerts.Markdown()
		} else if alerts.Markdown() != want {
			t.Fatal("permuting a partial frame changed its public evidence")
		}
		response.Data.Result = append(response.Data.Result[1:], response.Data.Result[0])
	}
}

// Malformed duplicates keep their reasons and independent pass failures in either order.
func TestConnectionReconcileAuthorityMalformedDuplicatePermutation(t *testing.T) {
	var want string
	for _, reversed := range []bool{false, true} {
		response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
			{host: "connect-a.example", block: "g1", unheld: 256, errorPasses: 6},
			{host: "connect-b.example", block: "g2", unheld: 0, errorPasses: 6},
		})
		for _, sample := range response.Data.Result {
			if sample.Metric["monitor_metric"] == "unheld" {
				sample.Value = []json.RawMessage{sample.Value[0], json.RawMessage(`"synthetic-private-duplicate"`)}
				response.Data.Result = append(response.Data.Result, sample)
			}
		}
		if reversed {
			slices.Reverse(response.Data.Result)
		}
		findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
		requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable", "connection-reconcile-failing")
		visibility := requireAlertClass(t, alerts, "connection-reconcile-unobservable")
		if !strings.Contains(visibility.Observed, "duplicate_metric,invalid_or_stale_metric") {
			t.Fatal("malformed duplicate did not preserve both structural reasons")
		}
		if !reversed {
			want = alerts.Markdown()
		} else if alerts.Markdown() != want {
			t.Fatal("malformed duplicate order changed public evidence or block order")
		}
		if strings.Contains(alerts.Markdown(), "synthetic-private") {
			t.Fatal("malformed duplicate leaked raw evidence")
		}
	}
}

// Empty discovery is unknown; the legacy absent optional family remains compatible.
func TestConnectionReconcileAuthorityEmptyAndLegacyControls(t *testing.T) {
	response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect.example", block: "g1", unheld: 0, okPasses: 1},
	})
	for i := len(response.Data.Result) - 1; i >= 0; i-- {
		if slices.Contains([]string{"failed_retires", "busy_retires"}, response.Data.Result[i].Metric["monitor_metric"]) {
			response.Data.Result = append(response.Data.Result[:i], response.Data.Result[i+1:]...)
		}
	}
	findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
	requireConnectionReconcileAuthorityClasses(t, findings, alerts)
	response.Data.Result = nil
	findings, alerts = runConnectionReconcileAuthorityFixture(t, response, now)
	requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable")
}

// A malformed companion cannot erase a proven pass failure or leak through its details.
func TestConnectionReconcileAuthorityMalformedFieldKeepsSameBlockPositive(t *testing.T) {
	response, now := connectionReconcileAuthorityFixture(t, []connectionReconcileFixtureBlock{
		{host: "connect.example", block: "g1", unheld: 256, errorPasses: 6},
	})
	for i := range response.Data.Result {
		if response.Data.Result[i].Metric["monitor_metric"] == "retired" {
			response.Data.Result[i].Value[1] = json.RawMessage(`"synthetic-private-retired"`)
		}
	}
	findings, alerts := runConnectionReconcileAuthorityFixture(t, response, now)
	requireConnectionReconcileAuthorityClasses(t, findings, alerts, "connection-reconcile-unobservable", "connection-reconcile-failing")
	if !strings.Contains(requireAlertClass(t, alerts, "connection-reconcile-failing").Observed, "retired_15m=unknown") {
		t.Fatal("invalid retired metric was rendered as measured zero")
	}
	if strings.Contains(alerts.Markdown(), "synthetic-private") {
		t.Fatal("independent positive leaked its malformed companion")
	}
}
