// Synthetic catalog and sampled-page frames exercise the three retention
// checks and the append-only list through the public signal. No database is
// read.
package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

type retentionTestPartition struct {
	name string
	heap int64
}

type retentionTestRelation struct {
	name     string
	oid      int64
	total    int64
	inserted int64
	deleted  int64
}

// One catalog frame and one sampled frame as the primary returns them.
type retentionTestSource struct {
	now           time.Time
	started       time.Time
	statsReset    time.Time
	parentKind    string
	frontier      time.Time
	partitions    []retentionTestPartition
	growth        []retentionTestRelation
	sampled       int64
	old           int64
	stamped       int64
	catalogErr    error
	reapErr       error
	mutateCatalog func([]Row) []Row
	mutateReap    func([]Row) []Row
}

func retentionTestMicros(at time.Time) string { return strconv.FormatInt(at.UnixMicro(), 10) }

// A healthy main-shaped primary at the synthetic clock: a recent payout,
// daily partitions inside retention, reaped contracts and no append-only
// relation.
func retentionTestHealthy() retentionTestSource {
	now := syntheticSettings(nil).Now()
	source := retentionTestSource{
		now: now, started: now.Add(-40 * 24 * time.Hour), statsReset: now.Add(-30 * 24 * time.Hour),
		parentKind: "p", frontier: now.Add(-3 * 24 * time.Hour),
		growth:  []retentionTestRelation{{name: "transfer_contract", oid: 16400, total: 1895 * storageRunwayTestGigabyte, inserted: 2000000000, deleted: 1000}},
		sampled: 5000, old: 400, stamped: 300,
	}
	for day := -20; day <= 3; day++ {
		at := now.Add(time.Duration(day) * 24 * time.Hour)
		source.partitions = append(source.partitions, retentionTestPartition{name: "client_reliability_p" + at.Format("20060102"), heap: 8 * 1024 * 1024 * 1024})
	}
	return source
}

func (self retentionTestSource) catalogRows() []Row {
	frontier := "none"
	if !self.frontier.IsZero() {
		frontier = retentionTestMicros(self.frontier)
	}
	statsReset := "0"
	if !self.statsReset.IsZero() {
		statsReset = retentionTestMicros(self.statsReset)
	}
	rows := []Row{{"meta", "pg-1", retentionTestMicros(self.now), retentionTestMicros(self.started), "0", statsReset, self.parentKind, frontier, "", ""}}
	for _, partition := range self.partitions {
		rows = append(rows, Row{"partition", partition.name, strconv.FormatInt(partition.heap, 10), "", "", "", "", "", "", ""})
	}
	for _, relation := range self.growth {
		rows = append(rows, Row{"growth", relation.name, strconv.FormatInt(relation.oid, 10), strconv.FormatInt(relation.total, 10),
			strconv.FormatInt(relation.inserted, 10), strconv.FormatInt(relation.deleted, 10), "", "", "", ""})
	}
	rows = append(rows, Row{"end", strconv.Itoa(len(self.partitions)), strconv.Itoa(len(self.growth)), "", "", "", "", "", "", ""})
	if self.mutateCatalog != nil {
		rows = self.mutateCatalog(rows)
	}
	return rows
}

func (self retentionTestSource) reapRows() []Row {
	rows := []Row{{"reap", "pg-1", retentionTestMicros(self.now), strconv.FormatInt(self.sampled, 10),
		strconv.FormatInt(self.old, 10), strconv.FormatInt(self.stamped, 10), "0"}}
	if self.mutateReap != nil {
		rows = self.mutateReap(rows)
	}
	return rows
}

func retentionTestSettings(t *testing.T, source retentionTestSource) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "TABLESAMPLE SYSTEM (0.0002)") {
			for _, want := range []string{"hostname -s", "transfer_contract", "1209600 * interval '1 second'", "reap_time IS NOT NULL", "pg_is_in_recovery()"} {
				if !strings.Contains(query, want) {
					t.Fatalf("reap sample lost %q", want)
				}
			}
			return source.reapRows(), source.reapErr
		}
		for _, want := range []string{"hostname -s", "pg_is_in_recovery()", "to_regclass('public.client_reliability')", "max(end_time)) * 1000000)::bigint FROM subsidy_payment", "pg_total_relation_size(c.oid) >= 100000000000"} {
			if !strings.Contains(query, want) {
				t.Fatalf("retention catalog read lost %q", want)
			}
		}
		return source.catalogRows(), source.catalogErr
	}})
	at := source.now
	settings.Now = func() time.Time { return at }
	return settings
}

func retentionTestRun(t *testing.T, signal Signal, source retentionTestSource) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), retentionTestSettings(t, source))
	if err != nil {
		t.Fatal("retention liveness run failed")
	}
	return alerts
}

func TestRetentionLivenessRegistered(t *testing.T) {
	found := 0
	for _, signal := range NewSignals() {
		if signal.Key() != "retention-liveness" {
			continue
		}
		found++
		if signal.Number() != "2.4g" || signal.Cadence() != time.Hour || signal.ID() != "pg/retention-liveness" {
			t.Fatal("retention liveness lost its catalog identity or hourly cadence")
		}
	}
	if found != 1 {
		t.Fatal("the §2.4g retention liveness checks are not registered")
	}
}

func TestRetentionLivenessHealthy(t *testing.T) {
	if alerts := retentionTestRun(t, NewRetentionLivenessSignal(), retentionTestHealthy()); len(alerts) != 0 {
		t.Fatalf("healthy retention alerted: %d alerts, first %s %s", len(alerts), alerts[0].Class, alerts[0].Observed)
	}
}

// The 2026-10-22 shape: the subsidy epoch ended 10-05, under 1% of old
// contract rows are stamped, and st_client_key_history only grows. The
// partition drops still run because the frontier has not yet passed the
// steady retention.
func TestRetentionLivenessSyntheticIncident(t *testing.T) {
	source := retentionTestHealthy()
	source.now = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	source.started = source.now.Add(-60 * 24 * time.Hour)
	source.statsReset = source.now.Add(-30 * 24 * time.Hour)
	source.frontier = time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	source.partitions = nil
	for day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC); day.Before(source.now.Add(72 * time.Hour)); day = day.Add(24 * time.Hour) {
		source.partitions = append(source.partitions, retentionTestPartition{name: "client_reliability_p" + day.Format("20060102"), heap: 60 * 1000 * 1000 * 1000})
	}
	source.growth = append(source.growth, retentionTestRelation{name: "st_client_key_history", oid: 16500, total: 382 * storageRunwayTestGigabyte, inserted: 81600000})
	source.sampled, source.old, source.stamped = 4664, 4000, 30
	alerts := retentionTestRun(t, NewRetentionLivenessSignal(), source)
	if len(alerts) != 2 {
		t.Fatalf("got %d alerts, want the pinned frontier and the stalled reap", len(alerts))
	}
	frontier := requireAlertClass(t, alerts, "retention-frontier-pinned")
	if frontier.Target != "pg-1/subsidy-frontier" || frontier.Severity != SeverityWarn || frontier.Sustain != 1 ||
		!strings.Contains(frontier.Observed, "frontier_age_days=17.5 holding_reliability_retention=false days_until_hold=2.5") {
		t.Fatalf("wrong frontier alert: %s", frontier.Observed)
	}
	reap := requireAlertClass(t, alerts, "contract-reap-stalled")
	if reap.Target != "pg-1/contract-reap" || reap.Severity != SeverityWarn || reap.Sustain != 2 ||
		!strings.Contains(reap.Observed, "old_rows=4000 old_rows_with_reap_time=30 stamped_share_pct=0.75") {
		t.Fatalf("wrong reap alert: %s", reap.Observed)
	}
	for _, alert := range []Alert{frontier, reap} {
		if !strings.Contains(alert.Evidence, "append_only_relations=[st_client_key_history total_gb=382.0 inserted_rows=81600000]") ||
			strings.Contains(alert.Evidence, "transfer_contract total_gb") {
			t.Fatalf("append-only list is wrong: %s", alert.Evidence)
		}
	}
	requireNoAlertClass(t, alerts, "partition-retention-stuck")
}

// The oldest non-empty partition decides; empty and foreign partitions are
// not storage the drop pass owns.
func TestRetentionLivenessPartitionCutoffs(t *testing.T) {
	now := syntheticSettings(nil).Now()
	day := func(days int) string {
		return "client_reliability_p" + now.Add(time.Duration(days)*24*time.Hour).Format("20060102")
	}
	for _, test := range []struct {
		name     string
		frontier time.Time
		oldest   string
		stuck    bool
	}{
		{name: "steady retention passed", frontier: now.Add(-3 * 24 * time.Hour), oldest: day(-28), stuck: true},
		{name: "steady retention inside grace", frontier: now.Add(-3 * 24 * time.Hour), oldest: day(-23)},
		{name: "frontier holds", frontier: now.Add(-25 * 24 * time.Hour), oldest: day(-27)},
		{name: "longest retention forces", frontier: now.Add(-40 * 24 * time.Hour), oldest: day(-34), stuck: true},
		{name: "no frontier", frontier: time.Time{}, oldest: day(-25), stuck: true},
	} {
		source := retentionTestHealthy()
		source.frontier = test.frontier
		source.partitions = append(source.partitions,
			retentionTestPartition{name: test.oldest, heap: 1},
			retentionTestPartition{name: day(-60), heap: 0},
			retentionTestPartition{name: "client_reliability_legacy", heap: 99})
		alerts := retentionTestRun(t, NewRetentionLivenessSignal(), source)
		found := false
		for _, alert := range alerts {
			if alert.Class != "partition-retention-stuck" {
				continue
			}
			found = true
			if alert.Target != "pg-1/client-reliability-partitions" || !strings.Contains(alert.Observed, "oldest_partition="+test.oldest+" ") ||
				!strings.Contains(alert.Observed, "ignored_partitions=1") {
				t.Fatalf("%s: wrong partition alert: %s", test.name, alert.Observed)
			}
		}
		if found != test.stuck {
			t.Fatalf("%s: stuck=%t, want %t", test.name, found, test.stuck)
		}
	}
}

// Reap share edges and sample sufficiency.
func TestRetentionLivenessReapThresholds(t *testing.T) {
	for _, test := range []struct {
		sampled, old, stamped int64
		class                 string
	}{
		{sampled: 5000, old: 1000, stamped: 50, class: ""},
		{sampled: 5000, old: 1000, stamped: 49, class: "contract-reap-stalled"},
		{sampled: 5000, old: 19, stamped: 0, class: ""},
		{sampled: 5000, old: 0, stamped: 0, class: ""},
		{sampled: 99, old: 99, stamped: 0, class: "retention-liveness-unobservable"},
	} {
		source := retentionTestHealthy()
		source.sampled, source.old, source.stamped = test.sampled, test.old, test.stamped
		alerts := retentionTestRun(t, NewRetentionLivenessSignal(), source)
		if test.class == "" {
			if len(alerts) != 0 {
				t.Fatalf("sample %d/%d/%d alerted", test.sampled, test.old, test.stamped)
			}
			continue
		}
		if alert := requireAlertClass(t, alerts, test.class); alert.Target != "pg-1/contract-reap" || len(alerts) != 1 {
			t.Fatalf("sample %d/%d/%d gave the wrong finding", test.sampled, test.old, test.stamped)
		}
	}
}

// Each read's failure leaves only its own checks unknown, never healthy.
func TestRetentionLivenessFramesAndFailuresStayUnknown(t *testing.T) {
	type mutation struct {
		name    string
		mutate  func(*retentionTestSource)
		targets []string
		reason  string
	}
	both := []string{"pg-1/subsidy-frontier", "pg-1/client-reliability-partitions"}
	for _, test := range []mutation{
		{name: "catalog error", mutate: func(s *retentionTestSource) { s.catalogErr = errors.New("exit status 3: private-source-canary") }, targets: both, reason: "catalog-source-unavailable"},
		{name: "wrong host", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0][1] = "pg-2"; return rows }
		}, targets: both, reason: "source-identity-or-role-unavailable"},
		{name: "replica", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0][4] = "1"; return rows }
		}, targets: both, reason: "source-identity-or-role-unavailable"},
		{name: "clock", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0][2] = retentionTestMicros(s.now.Add(time.Minute)); return rows }
		}, targets: both, reason: "source-clock-unavailable"},
		{name: "missing end", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { return rows[:len(rows)-1] }
		}, targets: both, reason: "incomplete-frame"},
		{name: "count mismatch", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { return append(rows[:1], rows[2:]...) }
		}, targets: both, reason: "incomplete-frame"},
		{name: "unknown kind", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row {
				return append(rows, Row{"private-source-canary", "", "", "", "", "", "", "", "", ""})
			}
		}, targets: both, reason: "unexpected-frame-kind"},
		{name: "bad number", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[1][2] = "-1"; return rows }
		}, targets: both, reason: "invalid-partition-row"},
		{name: "narrow frame", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0] = rows[0][:9]; return rows }
		}, targets: both, reason: "invalid-frame-width"},
		{name: "unpartitioned", mutate: func(s *retentionTestSource) { s.parentKind = "r" }, targets: []string{"pg-1/client-reliability-partitions"}, reason: "parent-not-partitioned"},
		{name: "missing parent", mutate: func(s *retentionTestSource) { s.parentKind = "" }, targets: []string{"pg-1/client-reliability-partitions"}, reason: "parent-missing"},
		{name: "reap error", mutate: func(s *retentionTestSource) { s.reapErr = errors.New("private-source-canary") }, targets: []string{"pg-1/contract-reap"}, reason: "reap-sample-unavailable"},
		{name: "reap host", mutate: func(s *retentionTestSource) {
			s.mutateReap = func(rows []Row) []Row { rows[0][1] = "pg-2"; return rows }
		}, targets: []string{"pg-1/contract-reap"}, reason: "reap-sample-frame-invalid"},
		{name: "reap counts", mutate: func(s *retentionTestSource) { s.stamped = s.old + 1 }, targets: []string{"pg-1/contract-reap"}, reason: "reap-sample-frame-invalid"},
		{name: "reap clock", mutate: func(s *retentionTestSource) {
			s.mutateReap = func(rows []Row) []Row { rows[0][2] = retentionTestMicros(s.now.Add(-time.Hour)); return rows }
		}, targets: []string{"pg-1/contract-reap"}, reason: "source-clock-unavailable"},
	} {
		source := retentionTestHealthy()
		test.mutate(&source)
		alerts := retentionTestRun(t, NewRetentionLivenessSignal(), source)
		if len(alerts) != len(test.targets) {
			t.Fatalf("%s: got %d alerts, want %d unknown checks", test.name, len(alerts), len(test.targets))
		}
		for index, alert := range alerts {
			if alert.Class != "retention-liveness-unobservable" || !alert.unobservable || alert.Target != test.targets[index] ||
				!strings.Contains(alert.Observed, "reason="+test.reason) {
				t.Fatalf("%s: wrong unknown check: %s %s %s", test.name, alert.Class, alert.Target, alert.Observed)
			}
			requireAlertOmits(t, alert, "private-source-canary")
		}
	}
	settings := DefaultRetentionLivenessSettings()
	settings.MaxPartitions = 4
	alerts := retentionTestRun(t, NewRetentionLivenessSignalWithSettings(settings), retentionTestHealthy())
	if len(alerts) != 1 || alerts[0].Class != "retention-liveness-unobservable" || alerts[0].Target != "pg-1/client-reliability-partitions" ||
		!strings.Contains(alerts[0].Observed, "reason=partition-census-overbound") {
		t.Fatal("an over-cap partition census was judged or reached another check")
	}
}

// The append-only list needs a statistics window of at least seven days and
// renders only plain relation names.
func TestRetentionLivenessAppendOnlyList(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, test := range []struct {
		name       string
		started    time.Time
		statsReset time.Time
		growth     []retentionTestRelation
		want       string
	}{
		{name: "recent reset", started: now.Add(-60 * 24 * time.Hour), statsReset: now.Add(-2 * 24 * time.Hour),
			growth: []retentionTestRelation{{name: "audit_log", oid: 1, total: 200 * storageRunwayTestGigabyte, inserted: 5}}, want: "append_only_relations=unqualified-stats-window"},
		{name: "never reset, young postmaster", started: now.Add(-3 * 24 * time.Hour),
			growth: []retentionTestRelation{{name: "audit_log", oid: 1, total: 200 * storageRunwayTestGigabyte, inserted: 5}}, want: "append_only_relations=unqualified-stats-window"},
		{name: "never reset, old postmaster", started: now.Add(-30 * 24 * time.Hour),
			growth: []retentionTestRelation{{name: "audit_log", oid: 1, total: 200 * storageRunwayTestGigabyte, inserted: 5}}, want: "append_only_relations=[audit_log total_gb=200.0 inserted_rows=5] stats_window_days=30.0"},
		{name: "unplain name", started: now.Add(-30 * 24 * time.Hour), statsReset: now.Add(-10 * 24 * time.Hour),
			growth: []retentionTestRelation{{name: "Private Source Canary", oid: 42, total: 150 * storageRunwayTestGigabyte, inserted: 1}}, want: "append_only_relations=[relation-42 total_gb=150.0 inserted_rows=1]"},
		{name: "deleting relation", started: now.Add(-30 * 24 * time.Hour), statsReset: now.Add(-10 * 24 * time.Hour),
			growth: []retentionTestRelation{{name: "audit_log", oid: 1, total: 200 * storageRunwayTestGigabyte, inserted: 5, deleted: 1}}, want: "append_only_relations=none"},
	} {
		source := retentionTestHealthy()
		source.started, source.statsReset, source.growth = test.started, test.statsReset, test.growth
		source.sampled, source.old, source.stamped = 5000, 1000, 0
		alert := requireAlertClass(t, retentionTestRun(t, NewRetentionLivenessSignal(), source), "contract-reap-stalled")
		if !strings.Contains(alert.Evidence, test.want) {
			t.Fatalf("%s: list %q not in %s", test.name, test.want, alert.Evidence)
		}
		requireAlertOmits(t, alert, "Private Source Canary")
	}
}

// Missing or ambiguous primaries never read a database.
func TestRetentionLivenessInventoryAuthority(t *testing.T) {
	for _, mode := range []string{"missing", "ambiguous"} {
		settings := syntheticSettings(&syntheticSource{postgresFn: func(string) ([]Row, error) {
			t.Fatal("retention liveness read without one primary")
			return nil, nil
		}})
		if mode == "missing" {
			settings.Hosts = settings.Hosts[1:]
		} else {
			settings.Hosts = append(settings.Hosts, HostSettings{Name: "pg-2", Roles: []string{"pg-primary"}})
		}
		alerts, err := NewRetentionLivenessSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 3 {
			t.Fatalf("%s primary did not leave every check unknown", mode)
		}
		for _, alert := range alerts {
			if alert.Class != "retention-liveness-unobservable" || !strings.Contains(alert.Observed, "primary-inventory-unavailable-or-ambiguous") {
				t.Fatalf("%s primary certified a check", mode)
			}
		}
	}
}

// Cancellation during either read returns lifecycle with the original cause.
func TestRetentionLivenessCancellationKeepsOriginalCause(t *testing.T) {
	for _, stage := range []string{"before", "catalog", "reap"} {
		ctx, cancel := context.WithCancel(context.Background())
		cause := errors.New("private-source-canary")
		source := retentionTestHealthy()
		settings := retentionTestSettings(t, source)
		settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if stage == "before" {
				t.Fatal("canceled work read the database")
			}
			reap := strings.Contains(query, "TABLESAMPLE")
			if (stage == "catalog" && !reap) || (stage == "reap" && reap) {
				cancel()
				return nil, cause
			}
			if reap {
				return source.reapRows(), nil
			}
			return source.catalogRows(), nil
		}}
		if stage == "before" {
			cancel()
		}
		alerts, err := NewRetentionLivenessSignal().Run(ctx, settings)
		cancel()
		if len(alerts) != 0 || !errors.Is(err, context.Canceled) || (stage != "before" && !errors.Is(err, cause)) {
			t.Fatalf("%s: canceled run lost its causes or published findings", stage)
		}
		if strings.Contains(err.Error(), "private-source-canary") {
			t.Fatal("lifecycle error exposed its private source")
		}
	}
}
