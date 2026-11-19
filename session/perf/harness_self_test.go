package perf

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/session"
)

// The harness's own helpers, without any service. The slots are Redis
// CLUSTER KEYSLOT answers (Redis 8.10).
func TestKeySlotMatchesRedis(t *testing.T) {
	for key, slot := range map[string]uint16{
		"foo":                  12182,
		"bar":                  5061,
		"hello":                866,
		"{user1000}.following": 3443,
		"{ns_0192f7a4-1c9e-7c3a-8f0e-2b1d3c4e5f60}z":   12638,
		"{rv_0192f7a4-1c9e-7c3a-8f0e-2b1d3c4e5f60_17}": 4562,
		"foo{}{bar}": 8363,
		"{nsi_07}z":  14228,
	} {
		if got := keySlot(key); got != slot {
			t.Errorf("keySlot(%q) = %d, want %d", key, got, slot)
		}
	}
}

func TestRefuseTunnelEndpoint(t *testing.T) {
	for _, authority := range []string{
		"127.0.0.1:26379",
		"127.0.0.1:6379",
		"10.213.0.1:6379",
		"10.213.0.1:6427",
		"10.213.0.1:5432",
		"10.213.0.1:6432",
		"[::1]:26379",
		"0.0.0.0:26379",
		"localhost:26379",
		"10.213.0.1",
	} {
		if refuseTunnelEndpoint(authority) == nil {
			t.Errorf("%s was admitted", authority)
		}
	}
	for _, authority := range []string{"10.213.0.1:26379", "10.213.0.1:26381", "10.213.0.1:6378", "10.213.0.1:6428"} {
		if err := refuseTunnelEndpoint(authority); err != nil {
			t.Errorf("%s was refused: %v", authority, err)
		}
	}
}

func TestScriptNameByKeyCount(t *testing.T) {
	prelude := "\nlocal now=tonumber(ARGV[1])\nlocal incarnation=ARGV[2]\n"
	network := "0192f7a4-1c9e-7c3a-8f0e-2b1d3c4e5f60"
	metadata := "{ns_" + network + "}s:0192f7a4-0000-7c3a-8f0e-2b1d3c4e5f60"
	receipt := "{ns_" + network + "}op:0192f7a4-0000-7c3a-8f0e-2b1d3c4e5f60"
	for _, item := range []struct {
		text     string
		keys     int
		fifthKey string
		want     string
	}{
		{prelude, 8, metadata, "mint"},
		{prelude, 4, "", "inventory_or_review"},
		{prelude, 4 + 3*5000, metadata, "list"},
		{prelude, 5 + 3*5000, receipt, "revoke"},
		{prelude, 5 + 3, receipt, "revoke"},
		{"\nlocal now=tonumber(ARGV[1]);local sid=ARGV[2]", 3, "", "last_use_sample"},
		{"\nfor i=3,#ARGV do redis.call('HSET',KEYS[1],ARGV[i],'x') end", 1, "", "record_tombstone"},
		{"\nlocal t=redis.call('TIME')", 1, "", "record_projection"},
		{"\nif ARGV[1]=='del' then", 1, "", "record_network"},
		{"return 1", 1, "", "other"},
	} {
		if got := scriptName(item.text, item.keys, item.fifthKey); got != item.want {
			t.Errorf("scriptName(%q, %d) = %s, want %s", item.text[:min(20, len(item.text))], item.keys, got, item.want)
		}
	}
}

func TestOpRecorderQuantilesAndClasses(t *testing.T) {
	recorder := newOpRecorder()
	base := time.Unix(1_800_000_000, 0)
	for index := 1; index <= 1000; index++ {
		recorder.observeTagged("op", "", base.Add(time.Duration(index)*time.Millisecond), time.Duration(index)*time.Microsecond, "", nil)
	}
	recorder.observeTagged("op", "", base, 0, classify(&session.SessionError{Code: "session_limit_reached", Status: 409}), nil)
	recorder.observeTagged("op", "", base, 0, classify(errors.Join(session.ErrAuthUnavailable, errors.New("down"))), nil)
	summary := recorder.summary("op", time.Time{}, time.Time{}, nil)
	if summary.Count != 1000 || summary.P50Ms != 0.5 || summary.P99Ms != 0.99 || summary.MaxMs != 1 {
		t.Fatalf("summary = %+v", summary)
	}
	if summary.Errors["refused:session_limit_reached"] != 1 || summary.Errors["unavailable"] != 1 {
		t.Fatalf("errors = %v", summary.Errors)
	}
	if summary.failureCount() != 1 {
		t.Fatalf("a designed refusal counted as a failure: %v", summary.Errors)
	}
	window := recorder.summary("op", base.Add(501*time.Millisecond), base.Add(601*time.Millisecond), nil)
	if window.Count != 100 || window.MaxMs != 0.6 {
		t.Fatalf("window = %+v", window)
	}
	if !strings.HasPrefix(classify(errors.New("409 session_retention_limit_reached")), "refused:") {
		t.Fatal("a wrapped capacity refusal was not classified")
	}
}

// The gates and the Markdown render for a run that skipped every phase, and
// for one with data in every section, without a service.
func TestReportRendersEveryState(t *testing.T) {
	config := &perfConfig{label: "self-test", profile: quickProfile(), phases: map[string]bool{}}
	empty := &harness{t: t, config: config, ops: newOpRecorder(), report: newPerfReport(config)}
	empty.evaluateGates()
	empty.project()
	if text := empty.markdown(); !strings.Contains(text, "| G1 ") || !strings.Contains(text, "SKIP") {
		t.Fatalf("empty report:\n%s", text)
	}

	full := &harness{t: t, config: config, ops: newOpRecorder(), report: newPerfReport(config)}
	now := time.Now()
	for _, op := range []string{"capacity.list@1000", "capacity.list@max", "capacity.revoke.others@1000", "capacity.revoke.others@max", "capacity.list@5000", "storm.revoke.others@5000", "steady.lease.check", "steady.api.auth", "steady.hotproxy.create"} {
		full.ops.observeTagged(op, "", now, 3*time.Millisecond, "", nil)
	}
	full.ops.observeTagged("steady.hotproxy.create", "", now, 0, "refused:session_limit_reached", nil)
	full.ops.observeTagged("storm.check", "storm-node", now.Add(time.Second), time.Millisecond, "", nil)
	cost := &clusterCost{Wall: 1, CpuSeconds: 0.5, Commands: map[string]*commandStat{"eval": {Calls: 10, Usec: 100}}, PerNodeCpu: map[string]float64{}}
	full.report.MaxStateBytes = referenceMaxStateBytes
	full.report.Units = append(full.report.Units, &unitCost{Name: "check.hit", Ops: 10, Cost: cost})
	full.report.Units[0].derive()
	full.report.addMemory("live session", 1000, 680000, nil)
	full.report.addOutcome("removed clients' credentials at their next check", "refused", 10, 0)
	full.report.addOutcome("hosted proxy credentials after sign-out-all", "accepted", 0, 10)
	marks := []time.Time{now, now.Add(time.Second), now.Add(2 * time.Second)}
	full.report.Storms = append(full.report.Storms, &stormReport{
		Name: "sign out other sessions", LiveSessions: 5000, RevokeCost: cost, CleanupCost: cost,
		Windows: full.windows("storm", "storm.check", marks, []string{"before", "revoke"}),
	})
	full.report.Slow = append(full.report.Slow, &slowSummary{Name: "storm.revoke.others@5000", ByScript: map[string]*slowScript{"eval:revoke": {Count: 1, MaxMs: 150, MaxKeys: 15005, MaxNode: "node1"}}})
	full.report.Steady = &steadyReport{DurationSeconds: 60, Cost: cost, OutboxStart: 0, OutboxEnd: 500, Series: []*seriesPoint{{Seconds: 10}}}
	full.report.Outbox = append(full.report.Outbox, &outboxReport{Rows: 100000, RepairMs: []float64{1500}, ObserveMs: 150})
	full.evaluateGates()
	full.project()
	text := full.markdown()
	verdicts := map[string]string{}
	for _, gate := range full.report.Gates {
		verdicts[gate.Id] = gate.Verdict
	}
	for id, want := range map[string]string{"G1": "PASS", "G7b": "WARN", "G12": "FAIL", "G14": "FAIL", "G15": "FAIL", "G16": "PASS", "G17": "FAIL"} {
		if verdicts[id] != want {
			t.Errorf("%s = %s, want %s", id, verdicts[id], want)
		}
	}
	if !strings.Contains(text, "## Projected Redis CPU") || !strings.Contains(text, "## Storm") {
		t.Fatalf("full report:\n%s", text)
	}
}

// A refusal a model call raises reaches the harness as the route handler sees
// it, and is both a refusal and a failure.
func TestRouteCallClassifiesRaisedRefusals(t *testing.T) {
	err := routeCall(func() error {
		panic(&session.SessionError{Code: "session_limit_reached", Status: 409})
	})
	if class := classify(err); class != "panic:refused:session_limit_reached" {
		t.Fatalf("class = %q", class)
	}
	summary := &opSummary{Errors: map[string]int{classify(err): 1}}
	if summary.failureCount() != 1 {
		t.Fatal("a raised refusal is not a failure")
	}
	if routeCall(func() error { return nil }) != nil {
		t.Fatal("a clean call failed")
	}
}
