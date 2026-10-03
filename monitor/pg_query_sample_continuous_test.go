package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPgQuerySampleContinuousDurableCadence(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		offset time.Duration
		want   bool
	}{{0, true}, {0, false}, {14 * time.Minute, false}, {15 * time.Minute, false}, {15*time.Minute + 40*time.Second, true}, {30*time.Minute + 79*time.Second, false}, {30*time.Minute + 80*time.Second, true}} {
		got, err := pgSampleContinuousAdmission(dir, now.Add(test.offset), nil)
		if err != nil || got != test.want {
			t.Fatalf("offset=%s admission=%t error=%v", test.offset, got, err)
		}
	}
	if _, err := pgSampleContinuousAdmission(dir, now, nil); err == nil {
		t.Fatal("backward clock accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "continuous.json"), []byte("private-untrusted-clock"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSampleContinuousAdmission(dir, now, nil); err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatal("malformed cadence did not fail privately")
	}
}

func TestPgQuerySampleContinuousSyncFailureCannotAdmit(t *testing.T) {
	now := time.Now().UTC()
	dir := t.TempDir()
	if ok, err := pgSampleContinuousAdmission(dir, now, func(*os.File) error { return errors.New("fixture") }); err == nil || ok {
		t.Fatal("failed sync admitted contact")
	}
}

func TestPgQuerySampleContinuousRepeatsSuccessAndFailure(t *testing.T) {
	for _, fails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fails], func(t *testing.T) {
			calls := 0
			now := time.Now().UTC().Truncate(time.Second)
			env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
				calls++
				if fails {
					return "", "private-error", errors.New("fixture")
				}
				return pgSampleTestEncode(t, pgSampleTestFrames(now)), "", nil
			})
			env.cfg.pgQuerySampleContinuous = true
			env.cfg.pgQuerySampleUntil = time.Time{}
			env.now = func() time.Time { return now }
			for _, offset := range []time.Duration{0, time.Minute, 14 * time.Minute, 15 * time.Minute} {
				current := now.Add(offset)
				saved := now
				now = current
				if _, err := (pgQuerySampleProbe{}).check(context.Background(), env); err != nil {
					t.Fatal(err)
				}
				now = saved
			}
			if calls != 2 {
				t.Fatalf("continuous contact count=%d want2", calls)
			}
		})
	}
}

func TestPgQuerySampleExpiredArmingIsVisible(t *testing.T) {
	calls := 0
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { calls++; return "", "", nil })
	env.cfg.pgQuerySampleUntil = env.now().Add(-time.Second)
	got, err := (pgQuerySampleProbe{}).check(context.Background(), env)
	if err != nil || calls != 0 || len(got) != 1 || got[0].class != "pg-query-sample-unavailable" || !strings.Contains(got[0].observed, "expired") {
		t.Fatal("expired arming silently lost coverage")
	}
}

func TestPgQuerySampleRepeatedSlowAndHealthyControls(t *testing.T) {
	for _, test := range []struct {
		name, state, family string
		seen, pressure      int
		age                 float64
		repeated, slow      bool
	}{
		{"short_repeated", "active", "reservation_census_prefix", 12, 6, 10, true, false},
		{"single_burst", "active", "reservation_census_prefix", 1, 1, 10, false, false},
		{"below_copies", "active", "reservation_census_prefix", 12, 0, 10, false, false},
		{"idle_last_statement", "idle in transaction", "reservation_census_prefix", 12, 12, 500, false, false},
		{"slow_peak_one", "active", "escrow_access", 12, 0, 61, false, true},
		{"bounded_maintenance", "active", "reindex_concurrent", 12, 0, 420, false, false},
		{"vacuum_control", "active", "vacuum", 12, 0, 420, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			slowSamples, maintenanceSamples := 0, 0
			if test.age >= 30 {
				slowSamples = test.seen
			}
			if test.age >= 7200 {
				maintenanceSamples = test.seen
			}
			r := pgQuerySampleReceipt{Complete: true, Load: []pgSampleLoad{{SlowSamples: slowSamples, MaintenanceSlowSamples: maintenanceSamples, State: test.state, Scope: "current", Family: test.family, Wait: "none", SeenSamples: test.seen, PressureSamples: test.pressure, QueryAge: test.age}}}
			got := pgSampleFindings(r, "synthetic-db")
			classes := map[string]bool{}
			for _, f := range got {
				classes[f.class] = true
			}
			if classes["pg-query-repeated-work"] != test.repeated || classes["pg-query-slow"] != test.slow {
				t.Fatalf("wrong classes: %v", classes)
			}
		})
	}
}

func TestPgQuerySampleDeclaredOwnerSurvivesAlertReduction(t *testing.T) {
	for _, test := range []struct {
		name, owner, app, backend string
		truncated, partial        bool
	}{
		{"healthy_bulk_copy", "local", "pg_dump", "client backend", false, false},
		{"application_owner_unknown", "loopback", "unset", "client backend", false, false},
		{"unrecognized_declaration_and_truncated", "remote", "other", "parallel worker", true, false},
		{"incomplete_backup_observation", "local", "pg_dump", "client backend", false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			frames := pgSampleTestFrames(now)
			for i := 2; i < 14; i++ {
				frames[i]["total"], frames[i]["groups"] = 1, 1
				frames[i]["rows"] = [][]any{{"714727414314", "active", "Client:ClientWrite", test.owner, test.app, test.backend, "contract_close_access", "current", 1, 2200., 10600., 2200.}}
				if test.truncated {
					frames[i]["query_text_truncated"] = 1
				}
			}
			if test.partial {
				frames = frames[:len(frames)-1]
			}
			r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
			if test.partial {
				if err == nil || r.Complete {
					t.Fatal("partial backup source became qualified")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			slow, coverage := false, false
			for _, f := range pgSampleFindings(r, "synthetic-db") {
				coverage = coverage || f.class == "pg-query-sample-coverage"
				if f.class != "pg-query-slow" {
					continue
				}
				slow = true
				for _, field := range []string{"state=\"active\"", "backend=\"" + test.backend + "\"", "client_owner=" + test.owner, "declared_application=" + test.app, "max_query_s=2200.000", "max_xact_s=10600.000"} {
					if !strings.Contains(f.observed, field) {
						t.Fatal("alert lost selected-group provenance or separate ages")
					}
				}
				if f.tier != tierWarn || !strings.Contains(f.context, "not a native process identity") || !strings.Contains(f.context, "not proof of a contract-close worker") {
					t.Fatal("backup declaration suppressed symptom or acquired unproved ownership")
				}
				if strings.Contains(f.observed, "714727414314") {
					t.Fatal("private query identifier escaped alert")
				}
			}
			if !slow || coverage != test.truncated {
				t.Fatal("backup/unknown label cleared slow work or lost partial coverage")
			}
		})
	}
}

func TestPgQuerySampleCoverageNeverBecomesZeroWork(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	frames := pgSampleTestFrames(now)
	for i := 2; i < 14; i++ {
		frames[i]["query_text_truncated"] = 1
		frames[i]["groups"] = 3
		frames[i]["total"] = 10
	}
	r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
	if err != nil {
		t.Fatal(err)
	}
	got := pgSampleFindings(r, "synthetic-db")
	coverage, positive := false, false
	for _, f := range got {
		coverage = coverage || f.class == "pg-query-sample-coverage"
		positive = positive || f.class == "pg-query-repeated-work"
	}
	if !coverage || !positive || r.HistoryDeltaQualified {
		t.Fatal("partial coverage hid work or manufactured interval statistics")
	}
	for _, field := range []string{"track_activity_query_size", "pgss_version"} {
		f := pgSampleTestFrames(now)
		delete(f[0], field)
		if _, err := parsePgQuerySample(pgSampleTestEncode(t, f), now); err == nil {
			t.Fatal("missing identity coverage accepted")
		}
	}
	f := pgSampleTestFrames(now)
	delete(f[2], "query_text_truncated")
	if _, err := parsePgQuerySample(pgSampleTestEncode(t, f), now); err == nil {
		t.Fatal("missing activity coverage accepted")
	}
}

func TestPgQuerySampleRankingRetainsSlowSingleton(t *testing.T) {
	rows := make([]pgSampleLoad, 100)
	for i := range rows {
		rows[i] = pgSampleLoad{State: "active", BackendSamples: 100, QueryAge: 1, Query: "busy"}
	}
	rows[99] = pgSampleLoad{State: "active", BackendSamples: 1, QueryAge: 61, Query: "slow-singleton"}
	selected := pgSampleRetainLoad(rows)
	found := false
	for _, r := range selected {
		found = found || r.Query == "slow-singleton"
	}
	if len(selected) != 80 || !found {
		t.Fatal("load ranking discarded slow low-concurrency owner")
	}
}

func TestPgQuerySampleSourceSQLStaysCatalogOnly(t *testing.T) {
	sql := pgQuerySampleSQL("synthetic")
	for _, want := range []string{"reservation_census_prefix", "grant_all_lock", "settlement_escrow_read", "query_text_truncated", "pg_stat_clear_snapshot()"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("missing finite source %s", want)
		}
	}
	if strings.Count(sql, "SELECT pg_stat_clear_snapshot();") != 13 {
		t.Fatal("activity snapshots not refreshed")
	}
	for _, ban := range []string{"EXPLAIN ", "SELECT * FROM transfer_escrow", "pg_cancel_backend", "CREATE INDEX"} {
		if strings.Contains(sql, ban) {
			t.Fatal("unbounded or mutating source introduced")
		}
	}
}

func TestPgQuerySampleSlowCountRequiresTwoOldSnapshots(t *testing.T) {
	for _, test := range []struct {
		name, family string
		ages         []float64
		want         bool
	}{
		{"one_old_one_young", "escrow_access", []float64{31, 1}, false},
		{"single_old_burst", "escrow_access", []float64{61}, false},
		{"two_old", "escrow_access", []float64{30, 32}, true},
		{"below_boundary", "escrow_access", []float64{29.99, 29.99}, false},
		{"maintenance_one_old", "vacuum", []float64{7200, 31}, false},
		{"maintenance_two_old", "vacuum", []float64{7200, 7202}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			frames := pgSampleTestFrames(now)
			for i := 0; i < 12; i++ {
				f := frames[i+2]
				f["rows"] = [][]any{}
				f["groups"] = 0
				f["total"] = 0
				if i < len(test.ages) {
					f["rows"] = [][]any{{"1", "active", "none", "loopback", "unset", "client backend", test.family, "current", 1, test.ages[i], 100., 0.}}
					f["groups"] = 1
					f["total"] = 1
				}
			}
			r, err := parsePgQuerySample(pgSampleTestEncode(t, frames), now)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range pgSampleFindings(r, "test-db") {
				found = found || f.class == "pg-query-slow"
			}
			if found != test.want {
				t.Fatalf("old snapshots yielded slow=%t want%t", found, test.want)
			}
		})
	}
}
func TestPgQuerySampleContinuousTerminalStateAndCrashFloor(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	if ok, err := pgSampleContinuousAdmission(dir, now, nil); err != nil || !ok {
		t.Fatal(err)
	}
	if ok, err := pgSampleContinuousAdmission(dir, now.Add(15*time.Minute), nil); err != nil || ok {
		t.Fatal("crash reservation lost owner budget")
	}
	terminal := now.Add(23 * time.Second)
	if err := pgSampleContinuousFinish(dir, now, terminal, true, "", strings.Repeat("a", 64), nil); err != nil {
		t.Fatal(err)
	}
	for _, d := range []time.Duration{15*time.Minute - time.Nanosecond, 15 * time.Minute} {
		ok, err := pgSampleContinuousAdmission(dir, terminal.Add(d), nil)
		if err != nil || ok != (d == 15*time.Minute) {
			t.Fatal("terminal cadence violated")
		}
	}
	second := terminal.Add(15 * time.Minute)
	if err := pgSampleContinuousFinish(dir, second, second.Add(time.Second), false, "deadline", strings.Repeat("b", 64), nil); err != nil {
		t.Fatal(err)
	}
	state, err := pgSampleReadCadence(dir, second.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if !state.CompletedAt.Equal(terminal) || state.Outcome != "deadline" || !state.TerminalAt.Equal(second.Add(time.Second)) {
		t.Fatal("failure erased completed clock or terminal reason")
	}
}
func TestPgQuerySampleContinuousOverlappingOwners(t *testing.T) {
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { t.Fatal("overlap contacted"); return "", "", nil })
	env.cfg.pgQuerySampleContinuous = true
	env.cfg.pgQuerySampleUntil = time.Time{}
	lock, err := lockProviderState(context.Background(), env.cfg.stateDir, "pg-query-sample-cadence")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (pgQuerySampleProbe{}).check(ctx, env); err == nil || !strings.Contains(err.Error(), "cadence lock") {
		t.Fatal("canceled overlapping owner entered")
	}
}

func TestPgQuerySampleContinuousSettingsAndBoundedHistory(t *testing.T) {
	s := syntheticSettings(nil)
	s.PGQuerySampleContinuous = true
	s.StateDir = ""
	if s.Validate() == nil {
		t.Fatal("recurring sampler without state accepted")
	}
	s.StateDir = t.TempDir()
	s.PGQuerySampleUntil = s.Now().Add(time.Hour)
	if s.Validate() == nil {
		t.Fatal("mutually exclusive modes accepted")
	}
	s.PGQuerySampleUntil = time.Time{}
	env, err := newProbeEnv(s.withDefaults())
	if err != nil || !env.cfg.pgQuerySampleContinuous {
		t.Fatal("recurring mode lost at settings boundary")
	}
	calls := 0
	now := time.Now().UTC().Truncate(time.Second)
	env = pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		calls++
		return pgSampleTestEncode(t, pgSampleTestFrames(now)), "", nil
	})
	env.cfg.pgQuerySampleContinuous = true
	env.cfg.pgQuerySampleUntil = time.Time{}
	env.now = func() time.Time { return now }
	for i := 0; i < 100; i++ {
		if _, err := (pgQuerySampleProbe{}).check(context.Background(), env); err != nil {
			t.Fatal(err)
		}
		now = now.Add(pgQuerySampleCadence)
	}
	entries, err := os.ReadDir(filepath.Join(env.cfg.stateDir, "pg-query-sample"))
	if err != nil {
		t.Fatal(err)
	}
	if calls != 100 || len(entries) != 101 {
		t.Fatalf("finite recurring receipts were lost: contacts=%d files=%d", calls, len(entries))
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".attempt") {
			t.Fatal("recurring attempt markers grow forever")
		}
	}
}
func TestPgQuerySampleDisabledAndNotDueAreNotHealthy(t *testing.T) {
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("disabled or cadence-held sampler contacted")
		return "", "", nil
	})
	env.cfg.pgQuerySampleUntil = time.Time{}
	got, err := (pgQuerySampleProbe{}).check(context.Background(), env)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].observed, "configured-disabled") {
		t.Fatal("disabled observation silently healthy")
	}
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if ok, err := pgSampleContinuousAdmission(dir, env.now(), nil); err != nil || !ok {
		t.Fatal(err)
	}
	env.cfg.pgQuerySampleContinuous = true
	got, err = (pgQuerySampleProbe{}).check(context.Background(), env)
	if err != nil || len(got) != 1 || !strings.Contains(got[0].observed, "cadence-not-due") || !strings.Contains(got[0].observed, "next_eligible_at=") {
		t.Fatal("cadence hold manufactured a fresh observation")
	}
}

func TestPgQuerySampleImmutableReceipt(t *testing.T) {
	dir := t.TempDir()
	body := []byte("{\"synthetic\":true}\n")
	first, err := pgSampleStoreReceipt(dir, body)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pgSampleStoreReceipt(dir, body)
	if err != nil || first != second {
		t.Fatal("immutable duplicate changed identity")
	}
	next, err := pgSampleStoreReceipt(dir, []byte("{\"synthetic\":false}\n"))
	if err != nil || next == first {
		t.Fatal("distinct receipt replaced original")
	}
	original, err := os.ReadFile(filepath.Join(dir, "receipt-"+first+".json"))
	if err != nil || string(original) != string(body) {
		t.Fatal("original evidence overwritten")
	}
	if err := os.WriteFile(filepath.Join(dir, "receipt-"+first+".json"), []byte("corrupt fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := pgSampleStoreReceipt(dir, body); err == nil {
		t.Fatal("conflicting immutable body silently overwritten")
	}
}
