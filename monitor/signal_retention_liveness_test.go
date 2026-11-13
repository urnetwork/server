// Synthetic catalog frames exercise the partition retention check and the
// append-only list through the public signal. No database is read.
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

// One catalog frame as the primary returns it, and the contract reap sample the
// retired check read.
type retentionTestSource struct {
	now        time.Time
	started    time.Time
	statsReset time.Time
	parentKind string
	// The newest subsidy_payment end_time on the primary. The catalog read
	// no longer asks for it; a read that does gets it in the meta row's
	// eighth cell, as the read before 2026-10-31 did.
	subsidyFrontier time.Time
	partitions      []retentionTestPartition
	growth          []retentionTestRelation
	// The sampled transfer_contract rows, those older than 14 days, and those
	// of them with reap_time, as the reap sample before 2026-11-13 read them.
	sampled       int64
	old           int64
	stamped       int64
	catalogErr    error
	mutateCatalog func([]Row) []Row
}

func retentionTestMicros(at time.Time) string { return strconv.FormatInt(at.UnixMicro(), 10) }

// A healthy main-shaped primary at the synthetic clock: daily partitions
// inside retention (10 days since 2026-11-10) and no append-only relation.
func retentionTestHealthy() retentionTestSource {
	now := syntheticSettings(nil).Now()
	source := retentionTestSource{
		now: now, started: now.Add(-40 * 24 * time.Hour), statsReset: now.Add(-30 * 24 * time.Hour),
		parentKind: "p",
		growth:     []retentionTestRelation{{name: "transfer_contract", oid: 16400, total: 1895 * storageRunwayTestGigabyte, inserted: 2000000000, deleted: 1000}},
		sampled:    5000, old: 400, stamped: 300,
	}
	for day := -9; day <= 3; day++ {
		at := now.Add(time.Duration(day) * 24 * time.Hour)
		source.partitions = append(source.partitions, retentionTestPartition{name: "client_reliability_p" + at.Format("20060102"), heap: 8 * 1024 * 1024 * 1024})
	}
	return source
}

func (self retentionTestSource) catalogRows(readsFrontier bool) []Row {
	frontier := ""
	if readsFrontier {
		frontier = "none"
		if !self.subsidyFrontier.IsZero() {
			frontier = retentionTestMicros(self.subsidyFrontier)
		}
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

// The frame the reap sample read: sampled, old and stamped row counts.
func (self retentionTestSource) reapRows() []Row {
	return []Row{{"reap", "pg-1", retentionTestMicros(self.now), strconv.FormatInt(self.sampled, 10),
		strconv.FormatInt(self.old, 10), strconv.FormatInt(self.stamped, 10), "0"}}
}

// Every read the signal makes, in order: "catalog" or "reap".
type retentionTestReads struct {
	reads []string
}

func retentionTestSettingsWithReads(t *testing.T, source retentionTestSource, reads *retentionTestReads) SignalSettings {
	t.Helper()
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		if strings.Contains(query, "TABLESAMPLE") {
			reads.reads = append(reads.reads, "reap")
			return source.reapRows(), nil
		}
		reads.reads = append(reads.reads, "catalog")
		for _, want := range []string{"hostname -s", "pg_is_in_recovery()", "to_regclass('public.client_reliability')", "pg_total_relation_size(c.oid) >= 100000000000"} {
			if !strings.Contains(query, want) {
				t.Fatalf("retention catalog read lost %q", want)
			}
		}
		return source.catalogRows(strings.Contains(query, "FROM subsidy_payment")), source.catalogErr
	}})
	at := source.now
	settings.Now = func() time.Time { return at }
	return settings
}

func retentionTestSettings(t *testing.T, source retentionTestSource) SignalSettings {
	t.Helper()
	return retentionTestSettingsWithReads(t, source, &retentionTestReads{})
}

func retentionTestRun(t *testing.T, signal Signal, source retentionTestSource) Alerts {
	t.Helper()
	alerts, err := signal.Run(context.Background(), retentionTestSettings(t, source))
	if err != nil {
		t.Fatal("retention liveness run failed")
	}
	return alerts
}

// A partition census whose oldest storage-holding day is past its retention
// and grace, so the stuck partition finding carries the append-only list.
func retentionTestStuck(source retentionTestSource) retentionTestSource {
	source.partitions = append(source.partitions, retentionTestPartition{
		name: "client_reliability_p" + source.now.Add(-30*24*time.Hour).Format("20060102"),
		heap: 1,
	})
	return source
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

// The 2026-11-13 shape: USDC payouts ended, so no completed payment stamps
// reap_time, under 1% of old contract rows have it, and st_client_key_history
// only grows. Contract reap stamping is not read, so the stamped share is no
// finding: the contract horizon reaper reclaims contracts without it. The
// partitions are inside the 10-day retention and its grace, so nothing is
// raised. Before the reap check retired, this raised contract-reap-stalled on
// every run.
func TestRetentionLivenessNeverReadsReapStamping(t *testing.T) {
	source := retentionTestHealthy()
	source.started = source.now.Add(-60 * 24 * time.Hour)
	source.growth = append(source.growth, retentionTestRelation{name: "st_client_key_history", oid: 16500, total: 382 * storageRunwayTestGigabyte, inserted: 81600000})
	source.sampled, source.old, source.stamped = 4664, 4000, 30
	reads := &retentionTestReads{}
	alerts, err := NewRetentionLivenessSignal().Run(context.Background(), retentionTestSettingsWithReads(t, source, reads))
	if err != nil {
		t.Fatal("retention liveness run failed")
	}
	if len(alerts) != 0 {
		t.Fatalf("got %d alerts, first %s %s", len(alerts), alerts[0].Class, alerts[0].Target)
	}
	if strings.Join(reads.reads, ",") != "catalog" {
		t.Fatalf("reads = %v, want only the catalog", reads.reads)
	}
	for _, class := range []string{"contract-reap-stalled", "retention-frontier-pinned"} {
		if _, ok := alertClassKinds[class]; ok {
			t.Errorf("the retired class %s is still classified", class)
		}
	}
}

// A watcher still holding the contract-reap-stalled incident that the reap
// check raised clears it after two complete runs of the signal, on the
// unstamped 2026-11-13 shape: the signal reports neither the class nor an
// unknown state for its target, so its runs are complete. Before the check
// retired, every run reported it again and the incident never cleared.
func TestRetentionLivenessClearsTheRetiredReapIncident(t *testing.T) {
	webhook, webhookUrl := newFakeSlackWebhook(t)
	harness := newSlackTestHarness()
	notifier := NewSlackAlertNotifier(harness.settings(webhookUrl))
	signal := NewRetentionLivenessSignal()
	notifier.Handle(signal, SignalExecution{Alerts: Alerts{{
		SignalNumber: signal.Number(),
		SignalKey:    signal.Key(),
		SignalID:     signal.ID(),
		SignalName:   signal.Name(),
		Severity:     SeverityWarn,
		Class:        "contract-reap-stalled",
		Target:       "pg-1/contract-reap",
		Environment:  "synthetic",
		Symptom:      "Only 0.8% of sampled transfer_contract rows older than 14 days have reap_time; contract retention is not reclaiming",
		Playbook:     "SIGNALS.md §2.4g",
	}}})
	source := retentionTestHealthy()
	source.sampled, source.old, source.stamped = 4664, 4000, 30
	for range 2 {
		alerts := retentionTestRun(t, signal, source)
		if len(alerts) != 0 {
			t.Fatalf("got %d alerts, first %s %s", len(alerts), alerts[0].Class, alerts[0].Target)
		}
		notifier.Handle(signal, SignalExecution{Alerts: alerts})
		harness.advance(time.Hour)
	}
	closeSlackNotifier(t, notifier)
	requireSlackTexts(t, webhook.posted(),
		"[WARN] raised (synthetic): Only 0.8% of sampled transfer_contract rows",
		"[CLEARED] WARN (synthetic): Retention liveness",
	)
}

// The 2026-10-31 shape: the subsidy frontier is 26 days old and the drop pass
// before the owner's decision held every day from 10-05, so the oldest
// partition ended 25.75 days before the observation. The retention rule counts
// it stuck: the pass drops it on its next run once the new rule is deployed,
// and a frontier-held cutoff no longer hides a stopped drop.
func TestRetentionLivenessJudgesByTheRetentionDespiteSubsidyFrontier(t *testing.T) {
	source := retentionTestHealthy()
	source.now = time.Date(2026, 10, 31, 18, 0, 0, 0, time.UTC)
	source.started = source.now.Add(-7 * 24 * time.Hour)
	source.statsReset = time.Time{}
	source.subsidyFrontier = time.Date(2026, 10, 5, 23, 59, 59, 0, time.UTC)
	source.partitions = []retentionTestPartition{{name: "client_reliability_p20261004", heap: 0}}
	for day := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC); day.Before(source.now.Add(72 * time.Hour)); day = day.Add(24 * time.Hour) {
		source.partitions = append(source.partitions, retentionTestPartition{name: "client_reliability_p" + day.Format("20060102"), heap: 50 * 1000 * 1000 * 1000})
	}
	alerts := retentionTestRun(t, NewRetentionLivenessSignal(), source)
	requireNoAlertClass(t, alerts, "retention-frontier-pinned")
	stuck := requireAlertClass(t, alerts, "partition-retention-stuck")
	if stuck.Target != "pg-1/client-reliability-partitions" ||
		!strings.Contains(stuck.Observed, "oldest_partition=client_reliability_p20261005 upper_bound_age_days=25.8 cutoff_age_days=10.0 days_past_cutoff=15.8") {
		t.Fatalf("wrong partition alert: %s", stuck.Observed)
	}
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
		{name: "retention passed", oldest: day(-14), stuck: true},
		{name: "retention inside grace", oldest: day(-12)},
		{name: "a recent subsidy frontier holds nothing", frontier: now.Add(-25 * 24 * time.Hour), oldest: day(-27), stuck: true},
		{name: "a stale subsidy frontier holds nothing", frontier: now.Add(-40 * 24 * time.Hour), oldest: day(-26), stuck: true},
		{name: "no frontier", oldest: day(-25), stuck: true},
	} {
		source := retentionTestHealthy()
		source.subsidyFrontier = test.frontier
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

// A failed or malformed read leaves the partition check unknown, never healthy.
func TestRetentionLivenessFramesAndFailuresStayUnknown(t *testing.T) {
	type mutation struct {
		name    string
		mutate  func(*retentionTestSource)
		targets []string
		reason  string
	}
	partitions := []string{"pg-1/client-reliability-partitions"}
	for _, test := range []mutation{
		{name: "catalog error", mutate: func(s *retentionTestSource) { s.catalogErr = errors.New("exit status 3: private-source-canary") }, targets: partitions, reason: "catalog-source-unavailable"},
		{name: "wrong host", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0][1] = "pg-2"; return rows }
		}, targets: partitions, reason: "source-identity-or-role-unavailable"},
		{name: "replica", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0][4] = "1"; return rows }
		}, targets: partitions, reason: "source-identity-or-role-unavailable"},
		{name: "clock", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0][2] = retentionTestMicros(s.now.Add(time.Minute)); return rows }
		}, targets: partitions, reason: "source-clock-unavailable"},
		{name: "missing end", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { return rows[:len(rows)-1] }
		}, targets: partitions, reason: "incomplete-frame"},
		{name: "count mismatch", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { return append(rows[:1], rows[2:]...) }
		}, targets: partitions, reason: "incomplete-frame"},
		{name: "unknown kind", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row {
				return append(rows, Row{"private-source-canary", "", "", "", "", "", "", "", "", ""})
			}
		}, targets: partitions, reason: "unexpected-frame-kind"},
		{name: "bad number", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[1][2] = "-1"; return rows }
		}, targets: partitions, reason: "invalid-partition-row"},
		{name: "narrow frame", mutate: func(s *retentionTestSource) {
			s.mutateCatalog = func(rows []Row) []Row { rows[0] = rows[0][:9]; return rows }
		}, targets: partitions, reason: "invalid-frame-width"},
		{name: "unpartitioned", mutate: func(s *retentionTestSource) { s.parentKind = "r" }, targets: partitions, reason: "parent-not-partitioned"},
		{name: "missing parent", mutate: func(s *retentionTestSource) { s.parentKind = "" }, targets: partitions, reason: "parent-missing"},
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
		source := retentionTestStuck(retentionTestHealthy())
		source.started, source.statsReset, source.growth = test.started, test.statsReset, test.growth
		alert := requireAlertClass(t, retentionTestRun(t, NewRetentionLivenessSignal(), source), "partition-retention-stuck")
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
		if err != nil || len(alerts) != 1 {
			t.Fatalf("%s primary did not leave the check unknown", mode)
		}
		if alerts[0].Class != "retention-liveness-unobservable" || alerts[0].Target != "primary/client-reliability-partitions" ||
			!strings.Contains(alerts[0].Observed, "primary-inventory-unavailable-or-ambiguous") {
			t.Fatalf("%s primary certified the check", mode)
		}
	}
}

// Cancellation before or during the read returns lifecycle with the original
// cause.
func TestRetentionLivenessCancellationKeepsOriginalCause(t *testing.T) {
	for _, stage := range []string{"before", "catalog"} {
		ctx, cancel := context.WithCancel(context.Background())
		cause := errors.New("private-source-canary")
		source := retentionTestHealthy()
		settings := retentionTestSettings(t, source)
		settings.Source = &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			if stage == "before" {
				t.Fatal("canceled work read the database")
			}
			cancel()
			return nil, cause
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

// Once the contract tables are partitioned, a contract table arrives summed
// over its partitions and is listed as reclaimed by partition drops, whose
// deletes leave with the dropped partition's statistics. Plain tables read as
// before.
func TestRetentionLivenessContractPartitions(t *testing.T) {
	for _, partitioned := range []bool{false, true} {
		source := retentionTestStuck(retentionTestHealthy())
		source.growth = []retentionTestRelation{{name: "transfer_contract", oid: 16400, total: 1895 * storageRunwayTestGigabyte, inserted: 2000000000}}
		if partitioned {
			source.mutateCatalog = func(rows []Row) []Row {
				for _, row := range rows {
					if row[0] == "growth" {
						row[6] = "1"
					}
				}
				return rows
			}
		}
		alert := requireAlertClass(t, retentionTestRun(t, NewRetentionLivenessSignal(), source), "partition-retention-stuck")
		listed := strings.Contains(alert.Evidence, "append_only_relations=[transfer_contract total_gb=1895.0 inserted_rows=2000000000 partitioned=true reclaimed_by=partition-drops]")
		if listed != partitioned {
			t.Fatalf("partitioned=%t: listed=%t: %s", partitioned, listed, alert.Evidence)
		}
		if !partitioned && !strings.Contains(alert.Evidence, "append_only_relations=[transfer_contract total_gb=1895.0 inserted_rows=2000000000]") {
			t.Fatalf("the plain list changed: %s", alert.Evidence)
		}
	}
	query := retentionLivenessProbe{settings: DefaultRetentionLivenessSettings()}.catalogQuery()
	for _, want := range []string{
		"contract_root.oid = pg_partition_root(c.oid)",
		"contract_root.relname IN ('transfer_contract','transfer_escrow','contract_close','transfer_escrow_sweep','contract_close_report','contract_extender')",
		"HAVING sum(total_bytes) >= 100000000000",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("retention catalog read lost %q", want)
		}
	}
}
