// The old four-column upsert can record a pass without resetting the streak
// columns added later. A measured pass dominates those retained columns in
// both readers and the next state transition. All data below is synthetic.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Reproduce the old upsert's exact field ownership without writing a database:
// checked_at, ok, failure and update_time change; the later columns survive.
func mixedWriterPassingBlackholeCheck() *ProviderBlackholeCheck {
	firstFailedAt := time.Date(2031, 1, 2, 0, 0, 0, 0, time.UTC)
	failedAt := firstFailedAt.Add(45 * time.Minute)
	nextDueAt := failedAt.Add(30 * time.Minute)
	row := &ProviderBlackholeCheck{
		ClientId: server.NewId(), CheckedAt: failedAt, Failure: "all_destinations_failed",
		ConsecutiveFailures: 3, FirstFailedAt: &firstFailedAt, NextDueAt: &nextDueAt,
		UpdateTime: failedAt.Add(time.Minute),
	}
	row.CheckedAt = failedAt.Add(5 * time.Minute)
	row.OK = true
	row.Failure = ""
	row.UpdateTime = row.CheckedAt.Add(time.Minute)
	return row
}

// The latest measured result is a pass, even with a mature inherited streak.
func TestProviderBlackholeMixedWriterPassIsNotDark(t *testing.T) {
	row := mixedWriterPassingBlackholeCheck()
	if row.IsDark(row.CheckedAt.Add(time.Minute), DefaultProviderEgressRules()) {
		t.Fatal("a legacy passing write remained dark through stale streak columns")
	}
}

// A pass breaks consecutiveness; the next failure starts at one and receives
// the first backoff step, without mutating the caller's previous row.
func TestProviderBlackholeMixedWriterNextFailureStartsNewRun(t *testing.T) {
	rules := DefaultProviderEgressRules()
	previous := mixedWriterPassingBlackholeCheck()
	before := *previous
	checkedAt := previous.CheckedAt.Add(5 * time.Minute)
	observedAt := checkedAt.Add(time.Minute)
	next, changed := NextProviderBlackholeCheck(previous, ProviderBlackholeCheckReport{
		ClientId: previous.ClientId, CheckedAt: checkedAt, Failure: "all_destinations_failed",
	}, observedAt, rules)
	if !changed || next.OK || next.ConsecutiveFailures != 1 || next.FirstFailedAt == nil || !next.FirstFailedAt.Equal(checkedAt) {
		t.Fatal("a new failure extended a streak interrupted by the legacy pass")
	}
	if next.IsDark(observedAt, rules) || next.NextDueAt == nil || !next.NextDueAt.Equal(observedAt.Add(rules.DarkBackoff(1))) {
		t.Fatal("a first failure inherited darkness or mature-streak scheduling")
	}
	if *previous != before || next == previous {
		t.Fatal("the next failure mutated or reused the caller's previous row")
	}
}

// An unmeasured attempt preserves the real pass and its time, not an older
// writer's stale run. It reschedules from the same zero-run state as any pass.
func TestProviderBlackholeMixedWriterUnmeasuredPreservesPassNotStreak(t *testing.T) {
	rules := DefaultProviderEgressRules()
	previous := mixedWriterPassingBlackholeCheck()
	before := *previous
	checkedAt := previous.CheckedAt.Add(5 * time.Minute)
	observedAt := checkedAt.Add(time.Minute)
	next, changed := NextProviderBlackholeCheck(previous, ProviderBlackholeCheckReport{
		ClientId: previous.ClientId, CheckedAt: checkedAt, NotMeasured: true, Failure: ProviderBlackholeNotMeasuredFailure,
	}, observedAt, rules)
	if !changed || !next.OK || next.CheckedAt != previous.CheckedAt || next.Failure != previous.Failure {
		t.Fatal("an unmeasured attempt replaced the prior measured pass")
	}
	if next.ConsecutiveFailures != 0 || next.FirstFailedAt != nil || next.IsDark(observedAt, rules) {
		t.Fatal("an unmeasured attempt retained stale failure state behind a measured pass")
	}
	if next.NextDueAt == nil || !next.NextDueAt.Equal(observedAt.Add(rules.DarkBackoff(0))) {
		t.Fatal("an unmeasured pass inherited the stale run's mature backoff")
	}
	if *previous != before || next == previous {
		t.Fatal("an unmeasured attempt mutated or reused the caller's previous row")
	}
}

// Replays remain no-ops, including on contradictory legacy state. Reader
// safety must not fabricate a newer measurement merely to normalize a row.
func TestProviderBlackholeMixedWriterReplayRemainsUnchanged(t *testing.T) {
	previous := mixedWriterPassingBlackholeCheck()
	before := *previous
	for _, offset := range []time.Duration{0, -time.Second} {
		next, changed := NextProviderBlackholeCheck(previous, ProviderBlackholeCheckReport{
			ClientId: previous.ClientId, CheckedAt: previous.CheckedAt.Add(offset), NotMeasured: true,
		}, previous.CheckedAt.Add(time.Minute), DefaultProviderEgressRules())
		if changed || next != previous || *previous != before {
			t.Fatal("a replay normalized or moved existing measured evidence")
		}
	}
}

// Real failing runs, TLS evidence and a new measured pass keep their existing
// thresholds, span, age and scheduling. Unmeasured failure retains its run.
func TestProviderBlackholeMixedWriterFailureAndTlsControls(t *testing.T) {
	rules := DefaultProviderEgressRules()
	failed := mixedWriterPassingBlackholeCheck()
	failed.OK = false
	failed.Failure = "all_destinations_failed"
	before := *failed
	checkedAt := failed.CheckedAt.Add(5 * time.Minute)
	observedAt := checkedAt.Add(time.Minute)
	if !failed.IsDark(observedAt, rules) {
		t.Fatal("a genuine mature failed run stopped being dark")
	}
	retained, changed := NextProviderBlackholeCheck(failed, ProviderBlackholeCheckReport{
		ClientId: failed.ClientId, CheckedAt: checkedAt, NotMeasured: true,
	}, observedAt, rules)
	if !changed || retained.OK || retained.CheckedAt != failed.CheckedAt || retained.ConsecutiveFailures != failed.ConsecutiveFailures || retained.FirstFailedAt != failed.FirstFailedAt || !retained.IsDark(observedAt, rules) {
		t.Fatal("an unmeasured attempt erased genuine prior failure evidence")
	}
	extended, changed := NextProviderBlackholeCheck(failed, ProviderBlackholeCheckReport{
		ClientId: failed.ClientId, CheckedAt: checkedAt, Failure: "all_destinations_failed",
	}, observedAt, rules)
	if !changed || extended.ConsecutiveFailures != failed.ConsecutiveFailures+1 || extended.FirstFailedAt == nil || !extended.FirstFailedAt.Equal(*failed.FirstFailedAt) {
		t.Fatal("a genuinely consecutive new failure did not extend its run")
	}
	passed, changed := NextProviderBlackholeCheck(failed, ProviderBlackholeCheckReport{
		ClientId: failed.ClientId, CheckedAt: checkedAt, Ok: true,
	}, observedAt, rules)
	if !changed || !passed.OK || passed.ConsecutiveFailures != 0 || passed.FirstFailedAt != nil || passed.Failure != "" || passed.NextDueAt == nil || !passed.NextDueAt.Equal(observedAt.Add(ProviderBlackholeCheckDueAge)) {
		t.Fatal("a new measured pass lost its ordinary reset or cadence")
	}
	tls, _ := NextProviderBlackholeCheck(nil, ProviderBlackholeCheckReport{
		ClientId: failed.ClientId, CheckedAt: checkedAt, Failure: ProviderBlackholeTlsAuthenticationFailure,
	}, observedAt, rules)
	if !tls.IsDark(observedAt, rules) || tls.IsDark(checkedAt.Add(ProviderBlackholeCheckMaxAge+time.Second), rules) {
		t.Fatal("immediate TLS evidence or its expiry changed")
	}
	if *failed != before {
		t.Fatal("a control transition mutated the caller's failed row")
	}
}

// Execute the shared SQL fragment over a synthetic CTE, then require both
// implementations to match independent expected values. Agreement alone can
// hide the same false-dark bug in Go and SQL. No real tables or writes exist.
func TestProviderBlackholeDarkSqlMatchesIsDark(t *testing.T) {
	if os.Getenv("WARP_ENV") != "local" {
		t.Fatal("blackhole SQL fixture requires the attested local test environment")
	}
	rules := DefaultProviderEgressRules()
	passed := mixedWriterPassingBlackholeCheck()
	now := passed.CheckedAt.Add(time.Minute)
	checkedAt := passed.CheckedAt
	firstFailedAt := checkedAt.Add(-rules.DarkMinimumSpan())
	shortFirst := firstFailedAt.Add(time.Second)
	expiredAt := now.Add(-ProviderBlackholeCheckMaxAge - time.Second)
	expiredFirst := expiredAt.Add(-rules.DarkMinimumSpan())
	edgeAt := now.Add(-ProviderBlackholeCheckMaxAge)
	edgeFirst := edgeAt.Add(-rules.DarkMinimumSpan())
	states := []struct {
		name string
		row  ProviderBlackholeCheck
		dark bool
	}{
		{name: "legacy pass with retained run", row: *passed},
		{name: "pass with contradictory TLS and run", row: ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true, Failure: ProviderBlackholeTlsAuthenticationFailure, ConsecutiveFailures: 3, FirstFailedAt: &firstFailedAt}},
		{name: "clean pass", row: ProviderBlackholeCheck{CheckedAt: checkedAt, OK: true}},
		{name: "mature failure", row: ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &firstFailedAt}, dark: true},
		{name: "two failures", row: ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 2, FirstFailedAt: &firstFailedAt}},
		{name: "short span", row: ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &shortFirst}},
		{name: "null run start", row: ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3}},
		{name: "unmeasured first row", row: ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: ProviderBlackholeNotMeasuredFailure}},
		{name: "TLS first failure", row: ProviderBlackholeCheck{CheckedAt: checkedAt, Failure: ProviderBlackholeTlsAuthenticationFailure, ConsecutiveFailures: 1, FirstFailedAt: &checkedAt}, dark: true},
		{name: "expired TLS", row: ProviderBlackholeCheck{CheckedAt: expiredAt, Failure: ProviderBlackholeTlsAuthenticationFailure, ConsecutiveFailures: 1, FirstFailedAt: &expiredFirst}},
		{name: "expired mature failure", row: ProviderBlackholeCheck{CheckedAt: expiredAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &expiredFirst}},
		{name: "inclusive expiry edge", row: ProviderBlackholeCheck{CheckedAt: edgeAt, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: &edgeFirst}, dark: true},
	}
	type sqlState struct {
		Number              int        `json:"case_number"`
		CheckedAt           time.Time  `json:"checked_at"`
		OK                  bool       `json:"ok"`
		Failure             string     `json:"failure"`
		ConsecutiveFailures int        `json:"consecutive_failures"`
		FirstFailedAt       *time.Time `json:"first_failed_at"`
	}
	sqlStates := make([]sqlState, len(states))
	for i, state := range states {
		sqlStates[i] = sqlState{Number: i, CheckedAt: state.row.CheckedAt, OK: state.row.OK, Failure: state.row.Failure, ConsecutiveFailures: state.row.ConsecutiveFailures, FirstFailedAt: state.row.FirstFailedAt}
	}
	payload, err := json.Marshal(sqlStates)
	if err != nil {
		t.Fatal(err)
	}
	query := `WITH pbc AS (
    SELECT * FROM jsonb_to_recordset($2::jsonb) AS synthetic_row(
        case_number integer, checked_at timestamp, ok boolean, failure text,
        consecutive_failures integer, first_failed_at timestamp
    )
)
SELECT case_number, COALESCE(` + ProviderBlackholeDarkSql("pbc", "$1::timestamp", rules) + `, false)
FROM pbc ORDER BY case_number`
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	observed := make(map[int]bool, len(states))
	var queryErr error
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, query, now.Add(-ProviderBlackholeCheckMaxAge), string(payload))
		if err != nil {
			queryErr = err
			return
		}
		defer rows.Close()
		for rows.Next() {
			var number int
			var dark bool
			if err := rows.Scan(&number, &dark); err != nil {
				queryErr = err
				return
			}
			if _, exists := observed[number]; exists || number < 0 || number >= len(states) {
				queryErr = fmt.Errorf("synthetic blackhole SQL returned duplicate or unknown case")
				return
			}
			observed[number] = dark
		}
		queryErr = rows.Err()
	}, server.OptNoRetry(), server.OptReadOnly())
	if queryErr != nil || len(observed) != len(states) {
		t.Fatalf("synthetic SQL control failed before assertions: cases=%d error=%v", len(observed), queryErr)
	}
	for i, state := range states {
		if got := observed[i]; got != state.dark {
			t.Errorf("%s SQL dark=%t, want %t", state.name, got, state.dark)
		}
		if got := state.row.IsDark(now, rules); got != state.dark {
			t.Errorf("%s Go dark=%t, want %t", state.name, got, state.dark)
		}
	}
}

// The actual public row writer must normalize a supplied passing row, without
// mutating its caller or silently changing an explicitly supplied schedule.
func TestProviderBlackholePassingWriteClearsStreak(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		row := mixedWriterPassingBlackholeCheck()
		row.CheckedAt = server.NowUtc().Add(-time.Minute).Truncate(time.Microsecond)
		row.Failure = ProviderBlackholeTlsAuthenticationFailure
		before := *row
		SetProviderBlackholeCheck(ctx, row)
		stored := GetProviderBlackholeCheck(ctx, row.ClientId)
		if stored == nil || !stored.OK || stored.Failure != "" ||
			stored.ConsecutiveFailures != 0 || stored.FirstFailedAt != nil {
			t.Fatalf("passing write retained failure evidence: %+v", stored)
		}
		if stored.NextDueAt == nil || !stored.NextDueAt.Equal(*row.NextDueAt) {
			t.Fatal("normalizing a passing row changed its explicit schedule")
		}
		if *row != before {
			t.Fatal("normalizing a passing row mutated the caller")
		}
	})
}

// Reproduce the old writer's real four-column UPDATE over a newer failed run.
// Both the SQL admission set and the next ingest must honor the intervening pass.
func TestProviderBlackholeMixedWriterDatabasePassBreaksRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		now := server.NowUtc().Truncate(time.Microsecond)
		Testing_SetProviderBlackholed(ctx, clientId, now.Add(-2*time.Minute))
		if !GetAllProviderBlackholedClientIds(ctx)[clientId] {
			t.Fatal("failed-run control was not dark before the legacy pass")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				UPDATE provider_blackhole_check
				SET checked_at = $2, ok = true, failure = '', update_time = $2
				WHERE client_id = $1
			`, clientId, now.Add(-time.Minute)))
		})
		passed := GetProviderBlackholeCheck(ctx, clientId)
		if passed == nil || !passed.OK || passed.ConsecutiveFailures == 0 {
			t.Fatal("legacy-write fixture did not retain the old failure columns")
		}
		if passed.IsDark(now, GetProviderEgressRules()) || GetAllProviderBlackholedClientIds(ctx)[clientId] {
			t.Error("the legacy measured pass remained in the current-dark set")
		}
		RecordProviderBlackholeChecks(ctx, []ProviderBlackholeCheckReport{{
			ClientId: clientId, CheckedAt: now, Failure: "all_destinations_failed",
		}}, GetProviderEgressRules())
		failed := GetProviderBlackholeCheck(ctx, clientId)
		if failed == nil || failed.OK || failed.ConsecutiveFailures != 1 ||
			failed.FirstFailedAt == nil || !failed.FirstFailedAt.Equal(now) {
			t.Fatalf("new failure extended a run broken by a legacy pass: %+v", failed)
		}
	})
}
