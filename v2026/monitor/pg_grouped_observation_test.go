// Exercise the actual CSV decoder and public signals. These fixtures need no
// database, external command, production observation or wall-clock sleep.
package monitor

import (
	"bytes"
	"context"
	"encoding/csv"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// Round-trip synthetic source cells through the owning PostgreSQL CSV parser.
func pgGroupedFixtureRows(t *testing.T, rows []Row) []Row {
	t.Helper()
	for _, row := range rows {
		if len(row) == 0 {
			// The typed source can return an empty row; CSV has no zero-field record.
			return rows
		}
	}
	var output bytes.Buffer
	writer := csv.NewWriter(&output)
	for _, row := range rows {
		if err := writer.Write([]string(row)); err != nil {
			t.Fatal("synthetic CSV write failed")
		}
	}
	writer.Flush()
	if writer.Error() != nil {
		t.Fatal("synthetic CSV flush failed")
	}
	parsed, err := parsePgRows(output.String())
	if err != nil {
		t.Fatal("synthetic CSV parser failed")
	}
	result := make([]Row, len(parsed))
	for i, row := range parsed {
		result[i] = Row(row)
	}
	return result
}

// The optional statement profile is a complete zero endpoint unless supplied.
func pgGroupedRun(t *testing.T, signal Signal, rows []Row, profile []Row, lifecycle, aggregates string) Alerts {
	t.Helper()
	if profile == nil {
		profile = []Row{{"0", "0", "0", "0", "0", "0", "0", "0", "0"}}
	}
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if strings.Contains(query, "AS legacy_reservation_page") {
				return pgGroupedFixtureRows(t, profile), nil
			}
			return pgGroupedFixtureRows(t, rows), nil
		},
		localFn: func(_ string, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "--query=[sm]reconcile net escrow") {
				return aggregates, nil
			}
			return lifecycle, nil
		},
	}
	alerts, err := NewWithSignals(syntheticSettings(source), signal).Run(context.Background())
	if err != nil {
		t.Fatal("grouped source lost explicit coverage findings")
	}
	return alerts
}

// Require exact public classes, retaining severity and private-field rejection.
func pgGroupedRequireClasses(t *testing.T, alerts Alerts, classes ...string) {
	t.Helper()
	counts := map[string]int{}
	for _, class := range classes {
		counts[class]++
	}
	for _, alert := range alerts {
		counts[alert.Class]--
		if alert.Severity != SeverityWarn {
			t.Fatal("grouped observation changed warning severity")
		}
		if strings.Contains(alert.Markdown(), "private-grouped-sentinel") {
			t.Fatal("rejected private source field escaped into a public alert")
		}
	}
	if len(alerts) != len(classes) {
		t.Fatalf("grouped public result has %d alerts, want %d", len(alerts), len(classes))
	}
	for _, count := range counts {
		if count != 0 {
			t.Fatal("grouped public result lost its required incident or coverage class")
		}
	}
}

// Causal RED: atoi previously turned the malformed age into healthy maintenance.
func TestPgGroupedActiveMalformedAgeCannotSuppress(t *testing.T) {
	alerts := pgGroupedRun(t, NewActiveQueriesSignal(), []Row{{"81", "1", "private-grouped-sentinel", "REINDEX TABLE CONCURRENTLY synthetic_table", "0"}}, nil, "", "")
	if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatal("A5 active malformed age became healthy maintenance")
	}
	pgGroupedRequireClasses(t, alerts, "cannot-observe")
}

// Causal RED: malformed ClientRead age previously became a zero-age healthy row.
func TestPgGroupedWaitMalformedAgeCannotSuppress(t *testing.T) {
	alerts := pgGroupedRun(t, NewWaitEventsSignal(), []Row{{"Client", "ClientRead", "5", "private-grouped-sentinel", "SELECT 'private-grouped-sentinel'", "42", "unknown", "synthetic", "local"}}, nil, "", "")
	if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatal("A5 wait malformed age became healthy ClientRead")
	}
	pgGroupedRequireClasses(t, alerts, "cannot-observe")
}

// Invalid projected fields must be rejected before any maintenance/history rule.
func TestPgGroupedActiveRejectsMalformedProjection(t *testing.T) {
	base := Row{"81", "1", "7199", "REINDEX TABLE CONCURRENTLY synthetic_table", "0"}
	for _, test := range []struct {
		column int
		value  string
	}{
		{column: 0, value: "private-grouped-sentinel"},
		{column: 0, value: "9223372036854775808"},
		{column: 1, value: "0"}, {column: 1, value: "-1"},
		{column: 1, value: "9223372036854775808"},
		{column: 2, value: "119"}, {column: 2, value: "-1"},
		{column: 2, value: "2147483648"}, {column: 2, value: "1.5"},
		{column: 3, value: ""},
		{column: 4, value: "NaN"}, {column: 4, value: "-1"},
		{column: 4, value: "2147483648"}, {column: 4, value: ""},
	} {
		row := append(Row(nil), base...)
		row[test.column] = test.value
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewActiveQueriesSignal(), []Row{row}, nil, "", ""), "cannot-observe")
	}
	for width := 0; width <= 6; width++ {
		if width == 5 {
			continue
		}
		row := append(append(Row(nil), base...), "private-grouped-sentinel")[:width]
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewActiveQueriesSignal(), []Row{row}, nil, "", ""), "cannot-observe")
	}
}

// Valid SQL-domain extremes must not overflow duration or twice-history guards.
func TestPgGroupedActivePreservesHealthyAndFailureBands(t *testing.T) {
	for _, test := range []struct {
		count, oldest, sample, mean string
		violation                   bool
	}{
		{count: "1", oldest: "7199", sample: "REINDEX TABLE CONCURRENTLY synthetic_table", mean: "0"},
		{count: "1", oldest: "7200", sample: "REINDEX TABLE CONCURRENTLY synthetic_table", mean: "0", violation: true},
		{count: "1", oldest: "1280", sample: "SELECT synthetic_value", mean: "640"},
		{count: "1", oldest: "1281", sample: "SELECT synthetic_value", mean: "640", violation: true},
		{count: "1", oldest: "120", sample: "SELECT synthetic_value", mean: "0", violation: true},
		{count: "4", oldest: "180", sample: "SELECT synthetic_value", mean: "640"},
		{count: "5", oldest: "180", sample: "SELECT synthetic_value", mean: "640", violation: true},
		{count: "9223372036854775807", oldest: "180", sample: "SELECT synthetic_value", mean: "640", violation: true},
		{count: "1", oldest: "2147483647", sample: "SELECT synthetic_value", mean: "2147483647"},
	} {
		alerts := pgGroupedRun(t, NewActiveQueriesSignal(), []Row{{"-9223372036854775808", test.count, test.oldest, test.sample, test.mean}}, nil, "", "")
		if test.violation {
			pgGroupedRequireClasses(t, alerts, "persistent-active-query")
		} else {
			pgGroupedRequireClasses(t, alerts)
		}
	}
	pgGroupedRequireClasses(t, pgGroupedRun(t, NewActiveQueriesSignal(), nil, nil, "", ""))
}

// Direct callers also cannot turn a negative or overflowing duration into grace.
func TestPgGroupedReindexDurationCannotWrap(t *testing.T) {
	for _, age := range []int{-1, int(^uint(0) >> 1)} {
		if expectedConcurrentReindex("REINDEX INDEX CONCURRENTLY synthetic_index", age) {
			t.Fatal("A5 duration overflow became healthy maintenance")
		}
	}
}

// All numeric wait cells are checked, including fields otherwise used only in
// attribution; zero query age is valid for a five-backend grouping.
func TestPgGroupedWaitRejectsMalformedProjection(t *testing.T) {
	base := Row{"Client", "ClientRead", "5", "0", "SELECT synthetic_value", "42", "unknown", "synthetic", "local"}
	for _, test := range []struct {
		column int
		value  string
	}{
		{column: 0, value: ""}, {column: 1, value: ""},
		{column: 2, value: "0"}, {column: 2, value: "-1"}, {column: 2, value: "9223372036854775808"},
		{column: 3, value: "NaN"}, {column: 3, value: "-1"}, {column: 3, value: "2147483648"},
		{column: 4, value: ""}, {column: 5, value: "0"}, {column: 5, value: "2147483648"},
		{column: 6, value: "9223372036854775808"}, {column: 6, value: "private-grouped-sentinel"},
		{column: 7, value: ""}, {column: 8, value: "private-grouped-sentinel"},
	} {
		row := append(Row(nil), base...)
		row[test.column] = test.value
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewWaitEventsSignal(), []Row{row}, nil, "", ""), "cannot-observe")
	}
	for width := 0; width <= 10; width++ {
		if width == 9 {
			continue
		}
		row := append(append(Row(nil), base...), "private-grouped-sentinel")[:width]
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewWaitEventsSignal(), []Row{row}, nil, "", ""), "cannot-observe")
	}
}

// Preserve ClientRead and virtualxid grace exactly, plus true count/age failures.
func TestPgGroupedWaitPreservesHealthyAndFailureBands(t *testing.T) {
	for _, test := range []struct {
		kind, event, count, age, sample string
		violation                       bool
	}{
		{kind: "Client", event: "ClientRead", count: "5", age: "0", sample: "SELECT synthetic_value"},
		{kind: "Client", event: "ClientRead", count: "5", age: "59", sample: "SELECT synthetic_value"},
		{kind: "Client", event: "ClientRead", count: "5", age: "60", sample: "SELECT synthetic_value", violation: true},
		{kind: "Lock", event: "virtualxid", count: "1", age: "7199", sample: "REINDEX TABLE CONCURRENTLY synthetic_table"},
		{kind: "Lock", event: "virtualxid", count: "1", age: "7200", sample: "REINDEX TABLE CONCURRENTLY synthetic_table", violation: true},
		{kind: "LWLock", event: "WALWrite", count: "5", age: "0", sample: "SELECT synthetic_value", violation: true},
		{kind: "IO", event: "DataFileRead", count: "1", age: "60", sample: "SELECT synthetic_value", violation: true},
	} {
		alerts := pgGroupedRun(t, NewWaitEventsSignal(), []Row{{test.kind, test.event, test.count, test.age, test.sample, "42", "-9223372036854775808", " ", "192.0.2.1/32"}}, nil, "", "")
		if test.violation {
			pgGroupedRequireClasses(t, alerts, "wait-event-cluster")
		} else {
			pgGroupedRequireClasses(t, alerts)
		}
	}
	pgGroupedRequireClasses(t, pgGroupedRun(t, NewWaitEventsSignal(), nil, nil, "", ""))
}

// A rejected group and duplicate group cannot erase a different complete group.
func TestPgGroupedIndependentGroupsSurviveInEitherOrder(t *testing.T) {
	for _, test := range []struct {
		signal    Signal
		good, bad Row
		class     string
	}{
		{signal: NewActiveQueriesSignal(), good: Row{"11", "5", "180", "SELECT synthetic_value", "0"}, bad: Row{"12", "1", "private-grouped-sentinel", "REINDEX TABLE CONCURRENTLY synthetic_table", "0"}, class: "persistent-active-query"},
		{signal: NewWaitEventsSignal(), good: Row{"IO", "DataFileRead", "5", "0", "SELECT synthetic_value", "42", "11", "synthetic", "local"}, bad: Row{"Client", "ClientRead", "5", "private-grouped-sentinel", "SELECT synthetic_value", "43", "12", "synthetic", "local"}, class: "wait-event-cluster"},
	} {
		for _, rows := range [][]Row{{test.good, test.bad}, {test.bad, test.good}, {test.bad, test.bad, test.good}, {test.good, test.bad, test.bad}} {
			pgGroupedRequireClasses(t, pgGroupedRun(t, test.signal, rows, nil, "", ""), test.class, "cannot-observe")
		}
		pgGroupedRequireClasses(t, pgGroupedRun(t, test.signal, []Row{test.good, test.good}, nil, "", ""), "cannot-observe")
	}
}

// Causal RED: malformed completed duration previously became zero and healthy.
func TestPgGroupedCompletionMalformedDurationCannotBecomeHealthy(t *testing.T) {
	for _, test := range []struct {
		signal Signal
		row    Row
	}{
		{signal: NewCloseDurationSignal(), row: Row{"00000000-0000-0000-0000-000000000001", "completed", "private-grouped-sentinel", "0", "1788000000"}},
		{signal: NewNetEscrowSignal(), row: Row{"completed", "private-grouped-sentinel", "0", "00000000-0000-0000-0000-000000000001", "1788000000"}},
	} {
		alerts := pgGroupedRun(t, test.signal, []Row{test.row}, nil, "", "")
		if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
			t.Fatal("A5 malformed completed duration became healthy")
		}
		pgGroupedRequireClasses(t, alerts, "cannot-observe")
	}
}

// Enforce the five-column completed row and its full numeric/phase/UUID domain.
func TestPgGroupedCompletionProjectionAndBoundaries(t *testing.T) {
	for _, closeTask := range []bool{true, false} {
		signal := NewNetEscrowSignal()
		class := "netescrow-reconcile-overrun"
		base := Row{"completed", "119", "0", "00000000-0000-0000-0000-000000000001", "1788000000"}
		phaseColumn, durationColumn, ageColumn, taskColumn := 0, 1, 2, 3
		if closeTask {
			signal = NewCloseDurationSignal()
			class = "close-duration-overrun"
			base = Row{base[3], base[0], base[1], base[2], base[4]}
			phaseColumn, durationColumn, ageColumn, taskColumn = 1, 2, 3, 0
		}
		for _, age := range []string{"0", "2700"} {
			for _, duration := range []string{"0", "119", "120", "2147483647"} {
				row := append(Row(nil), base...)
				row[durationColumn] = duration
				row[ageColumn] = age
				alerts := pgGroupedRun(t, signal, []Row{row}, nil, "", "")
				if duration == "120" || duration == "2147483647" {
					pgGroupedRequireClasses(t, alerts, class)
				} else {
					pgGroupedRequireClasses(t, alerts)
				}
			}
		}
		for _, test := range []struct {
			column int
			value  string
		}{
			{column: phaseColumn, value: "private-grouped-sentinel"}, {column: taskColumn, value: ""},
			{column: durationColumn, value: "-1"}, {column: durationColumn, value: "2147483648"},
			{column: ageColumn, value: "-1"}, {column: ageColumn, value: "2701"},
			{column: 4, value: "0"}, {column: 4, value: "9223372036854775808"},
		} {
			row := append(Row(nil), base...)
			row[test.column] = test.value
			pgGroupedRequireClasses(t, pgGroupedRun(t, signal, []Row{row}, nil, "", ""), "cannot-observe")
		}
		for _, row := range []Row{base[:3], append(append(Row(nil), base...), "private-grouped-sentinel")} {
			pgGroupedRequireClasses(t, pgGroupedRun(t, signal, []Row{row}, nil, "", ""), "cannot-observe")
		}
		good := append(Row(nil), base...)
		good[durationColumn] = "120"
		bad := append(Row(nil), base...)
		bad[taskColumn] = "00000000-0000-0000-0000-000000000002"
		bad[durationColumn] = "private-grouped-sentinel"
		for _, rows := range [][]Row{{good, bad}, {bad, good}} {
			pgGroupedRequireClasses(t, pgGroupedRun(t, signal, rows, nil, "", ""), class, "cannot-observe")
		}
		pgGroupedRequireClasses(t, pgGroupedRun(t, signal, []Row{good, good}, nil, "", ""), "cannot-observe")
	}
}

// A completion with a malformed duration cannot prove that a stale heartbeat
// ended, but incomplete completions cannot prove the heartbeat is still active.
func TestPgGroupedCompletionAmbiguityDoesNotResurfaceActiveHeartbeat(t *testing.T) {
	for _, test := range []struct {
		signal   Signal
		function string
		row      Row
	}{
		{signal: NewCloseDurationSignal(), function: "CloseExpiredContracts", row: Row{"00000000-0000-0000-0000-000000000001", "completed", "private-grouped-sentinel", "0", "1788000000"}},
		{signal: NewNetEscrowSignal(), function: "ReconcileNetEscrow", row: Row{"completed", "private-grouped-sentinel", "0", "00000000-0000-0000-0000-000000000001", "1788000000"}},
	} {
		log := "[00000000-0000-0000-0000-000000000001]eval active(130.0s) synthetic/work." + test.function + "({})"
		pgGroupedRequireClasses(t, pgGroupedRun(t, test.signal, []Row{test.row}, nil, log, ""), "cannot-observe")
	}
}

// A valid reservation row remains independent of rejected completion siblings.
func TestPgGroupedReservationProjectionAndBoundaries(t *testing.T) {
	for _, age := range []string{"120", "1800", "1801", "2147483647"} {
		class := "netescrow-reservation-overrun"
		if age == "1801" || age == "2147483647" {
			class = "netescrow-reservation-detached"
		}
		good := Row{"reservation-page", "1", age, "", "0"}
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), []Row{good}, nil, "", ""), class)
		bad := Row{"completed", "private-grouped-sentinel", "0", "00000000-0000-0000-0000-000000000001", "1788000000"}
		for _, rows := range [][]Row{{good, bad}, {bad, good}} {
			pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), rows, nil, "", ""), class, "cannot-observe")
		}
	}
	duplicate := Row{"reservation-page", "1", "120", "", "0"}
	completed := Row{"completed", "120", "0", "00000000-0000-0000-0000-000000000001", "1788000000"}
	pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), []Row{duplicate, completed, duplicate}, nil, "", ""), "netescrow-reconcile-overrun", "cannot-observe")
	for _, row := range []Row{
		{"reservation-page", "1", "private-grouped-sentinel", "", "0"},
		{"reservation-page", "0", "120", "", "0"}, {"reservation-page", "2147483648", "120", "", "0"},
		{"reservation-page", "1", "119", "", "0"}, {"reservation-page", "1", "2147483648", "", "0"},
		{"reservation-page", "1", "120", "private-grouped-sentinel", "0"}, {"reservation-page", "1", "120", "", "1"},
		{"reservation-page", "1", "120"},
	} {
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), []Row{row}, nil, "", ""), "cannot-observe")
	}
}

// Every optional PGSS field is checked without erasing the independently
// completed task incident. Endpoint counters remain interval-unqualified.
func TestPgGroupedNetEscrowProfileRejectsMalformedNumbers(t *testing.T) {
	completed := []Row{{"completed", "120", "0", "00000000-0000-0000-0000-000000000001", "1788000000"}}
	base := Row{"1", "1000", "1000", "1", "10", "10", "1", "0", "0"}
	for column := range base {
		for _, value := range []string{"", "NaN", "Inf", "-1", "1e309", "1e-9999", "private-grouped-sentinel"} {
			row := append(Row(nil), base...)
			row[column] = value
			alerts := pgGroupedRun(t, NewNetEscrowSignal(), completed, []Row{row}, "", "")
			pgGroupedRequireClasses(t, alerts, "netescrow-reconcile-overrun", "cannot-observe")
			if strings.Contains(alerts.Markdown(), "reservation_page_calls=") {
				t.Fatal("invalid profile reached task attribution")
			}
		}
	}
	for _, row := range []Row{base[:8], append(append(Row(nil), base...), "0"), {"0", "1", "0", "0", "0", "0", "0", "0", "0"}, {"1", "1", "2", "0", "0", "0", "1", "0", "0"}, {"1", "1", "1", "0", "0", "0", "2", "0", "0"}, {"1", "1", "1", "0", "0", "0", "0", "1", "2"}} {
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), completed, []Row{row}, "", ""), "netescrow-reconcile-overrun", "cannot-observe")
	}
	for _, profile := range [][]Row{[]Row{}, {base, base}} {
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), completed, profile, "", ""), "netescrow-reconcile-overrun", "cannot-observe")
	}
	for _, profile := range [][]Row{{base}, {{"0", "0", "0", "0", "0", "0", "0", "0", "0"}}, {{"9223372036854775807", "1e308", "1e308", "0", "0", "0", "9223372036854775807", "0", "0"}}} {
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), completed, profile, "", ""), "netescrow-reconcile-overrun")
	}
}

// Causal RED: an overflowing formatted byte total previously cast negative and
// silently removed the only large-drift observation.
func TestPgGroupedNetEscrowLogOverflowCannotBecomeHealthy(t *testing.T) {
	log := "[sm]reconcile net escrow: 1 balances, 1 networks drifted, over-reserved 9223372036854775808b, under-reserved 0b"
	alerts := pgGroupedRun(t, NewNetEscrowSignal(), nil, nil, "", log)
	if len(alerts) != 1 || alerts[0].Class != "cannot-observe" {
		t.Fatal("A5 overflowing drift bytes became healthy")
	}
	pgGroupedRequireClasses(t, alerts, "cannot-observe")
}

// Invalid count/byte records stay unknown; ordinary zero and threshold values
// preserve their meaning and exact int64 limits do not round upward.
func TestPgGroupedNetEscrowLogNumericAuthority(t *testing.T) {
	for _, fields := range []string{
		"9223372036854775808 balances, 1 networks drifted, over-reserved 256gib, under-reserved 0b",
		"1 balances, 9223372036854775808 networks drifted, over-reserved 256gib, under-reserved 0b",
		"1 balances, 2 networks drifted, over-reserved 256gib, under-reserved 0b",
		"1 balances, 0 networks drifted, over-reserved 256gib, under-reserved 0b",
		"1 balances, 1 networks drifted, over-reserved NaNb, under-reserved 0b",
		"1 balances, 1 networks drifted, over-reserved Infb, under-reserved 0b",
		"1 balances, 1 networks drifted, over-reserved -1b, under-reserved 0b",
		"1 balances, 1 networks drifted, over-reserved 999999999999999999999999999999999999999999tib, under-reserved 0b",
		"1 balances, 1 networks drifted, over-reserved 256gib, under-reserved 0b private-grouped-sentinel",
	} {
		pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), nil, nil, "", "[sm]reconcile net escrow: "+fields), "cannot-observe")
	}
	for _, value := range []string{"255.99gib", "256gib", "9223372036854775807b"} {
		alerts := pgGroupedRun(t, NewNetEscrowSignal(), nil, nil, "", "[sm]reconcile net escrow: 1 balances, 1 networks drifted, over-reserved "+value+", under-reserved 0b")
		if value == "255.99gib" {
			pgGroupedRequireClasses(t, alerts)
		} else {
			pgGroupedRequireClasses(t, alerts, "netescrow-large-drift")
		}
	}
	pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), nil, nil, "", "[sm]reconcile net escrow: 0 balances, 0 networks drifted, over-reserved 0b, under-reserved 0b"))
	for _, value := range []string{"NaN", "Inf", "-1", "9223372036854775808", "9223372036854775807.1"} {
		if _, ok := parseNetEscrowBytes(value, "b"); ok {
			t.Fatal("invalid byte value crossed the exact signed-byte domain")
		}
	}
	if value, ok := parseNetEscrowBytes("9223372036854775807", "b"); !ok || strconv.FormatInt(value, 10) != "9223372036854775807" {
		t.Fatal("exact maximum byte count rounded outside its domain")
	}
}

// Keeping a rejected record in sequence prevents manufacturing a reversal
// between two valid records that were never adjacent in the source window.
func TestPgGroupedNetEscrowMalformedLogCannotJoinReversal(t *testing.T) {
	under := "[sm]reconcile net escrow: 1 balances, 1 networks drifted, over-reserved 0b, under-reserved 300gib"
	over := "[sm]reconcile net escrow: 1 balances, 1 networks drifted, over-reserved 300gib, under-reserved 0b"
	bad := "[sm]reconcile net escrow: private-grouped-sentinel"
	for _, log := range []string{under + "\n" + bad + "\n" + over, over + "\n" + bad + "\n" + under} {
		alerts := pgGroupedRun(t, NewNetEscrowSignal(), nil, nil, "", log)
		if strings.Contains(alerts.Markdown(), "matched_reversal=true") || !strings.Contains(alerts.Markdown(), "matched_reversal=unknown_incomplete_source") {
			t.Fatal("A5 rejected aggregate manufactured adjacent reversal evidence")
		}
		pgGroupedRequireClasses(t, alerts, "netescrow-large-drift", "cannot-observe")
	}
	alerts := pgGroupedRun(t, NewNetEscrowSignal(), nil, nil, "", bad+"\n"+under+"\n"+over)
	pgGroupedRequireClasses(t, alerts, "netescrow-large-drift", "cannot-observe")
	if !strings.Contains(alerts.Markdown(), "matched_reversal=true") {
		t.Fatal("independent complete reversal was erased by a separate malformed pass")
	}
	pgGroupedRequireClasses(t, pgGroupedRun(t, NewNetEscrowSignal(), nil, nil, "", strings.Repeat("[sm]reconcile net escrow: 0 balances, 0 networks drifted, over-reserved 0b, under-reserved 0b\n", 100)), "cannot-observe")
}

// The catalog owns the parser and partial-coverage interpretation for each key.
func TestPgGroupedCatalogRetainsObservationAuthority(t *testing.T) {
	data, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"active-queries", "wait-events", "close-duration", "netescrow"} {
		_, section, found := strings.Cut(string(data), "Probe: `"+key+"`")
		if !found {
			t.Fatalf("catalog omitted %s", key)
		}
		section, _, _ = strings.Cut(section, "\n### ")
		for _, want := range []string{"cannot-observe", "False-positive qualifier:", "False-negative qualifier:"} {
			if !strings.Contains(section, want) {
				t.Fatalf("%s omitted %s", key, want)
			}
		}
	}
}

// Query failures cannot erase a separately proven failed attempt or large drift.
func TestPgGroupedQueryFailureRetainsIndependentIncident(t *testing.T) {
	for _, test := range []struct {
		signal          Signal
		function, class string
	}{
		{signal: NewCloseDurationSignal(), function: "CloseExpiredContracts", class: "close-duration-overrun"},
		{signal: NewNetEscrowSignal(), function: "ReconcileNetEscrow", class: "netescrow-reconcile-overrun"},
	} {
		source := &syntheticSource{
			postgresFn: func(string) ([]Row, error) { return nil, fmt.Errorf("decode private-grouped-sentinel") },
			localFn: func(_ string, args ...string) (string, error) {
				if strings.Contains(strings.Join(args, " "), "--query=[sm]reconcile net escrow") {
					return "", nil
				}
				return "[00000000-0000-0000-0000-000000000001]eval error(130.0s) (reschedule) synthetic/work." + test.function + "({}) = Timeout", nil
			},
		}
		alerts, err := NewWithSignals(syntheticSettings(source), test.signal).Run(context.Background())
		if err != nil {
			t.Fatal("query failure discarded independent failed attempt")
		}
		if test.function == "ReconcileNetEscrow" {
			pgGroupedRequireClasses(t, alerts, test.class, "cannot-observe", "cannot-observe")
		} else {
			pgGroupedRequireClasses(t, alerts, test.class, "cannot-observe")
		}
	}
}
