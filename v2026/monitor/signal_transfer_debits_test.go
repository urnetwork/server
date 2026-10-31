// Deterministic debit evidence controls and the indexed journal query fixture.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func debitRows(now time.Time) []pgRow {
	rows := []pgRow{}
	for i := range 16 {
		rows = append(rows, pgRow{fmt.Sprint(now.Unix()), fmt.Sprint(i), "-1", "-1", "t", "f"})
	}
	return rows
}

// Exercise the public visibility path and count only the two owning queries.
func transferDebitAuthorityRun(t *testing.T, schemaRows []Row, backlogRows []pgRow) (Alerts, error, int) {
	t.Helper()
	calls := 0
	source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
		calls++
		if calls == 1 {
			if !strings.Contains(query, "max(end_version_number)") || !strings.Contains(query, "to_regclass('public.transfer_debit_journal')") {
				t.Fatal("debit observation did not begin with its schema gate")
			}
			return schemaRows, nil
		}
		if calls != 2 || query != transferDebitBacklogQuery {
			t.Fatal("debit observation made an unexpected source query")
		}
		rows := make([]Row, len(backlogRows))
		for i, row := range backlogRows {
			rows[i] = Row(row)
		}
		return rows, nil
	}}
	alerts, err := NewWithSignals(syntheticSettings(source), NewTransferDebitsSignal()).Run(context.Background())
	return alerts, err, calls
}

// A rejected matrix remains visibility loss, with no financial diagnosis.
func requireTransferDebitUnknown(t *testing.T, fixture string, alerts Alerts, err error) {
	t.Helper()
	if err == nil || len(alerts) != 1 {
		t.Fatalf("%s: invalid debit observation became healthy or a debt finding", fixture)
	}
	alert := alerts[0]
	if alert.Identity() != "monitor/visibility|cannot-observe|pg/transfer-debits|" ||
		alert.SignalNumber != "2.5a" || alert.SignalKey != "transfer-debits" ||
		alert.Severity != SeverityWarn || alert.Sustain != 2 || !alert.unobservable {
		t.Fatalf("%s: debit visibility identity or sustain changed", fixture)
	}
	requireAlertClass(t, alerts, "cannot-observe")
}

// A future-dated row is not the empty sentinel in either durable state.
func TestTransferDebitsFractionalNegativeAgeIsUnknown(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, fixture := range []struct {
		state  string
		column int
	}{
		{state: "pending", column: 2},
		{state: "redis_release", column: 3},
	} {
		rows := debitRows(now)
		rows[15][fixture.column] = "-0.5"
		alerts, err, calls := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
		if err == nil {
			t.Errorf("fractional negative %s age was accepted as healthy", fixture.state)
			continue
		}
		requireTransferDebitUnknown(t, fixture.state, alerts, err)
		if calls != 2 {
			t.Fatalf("%s: rejected age changed the bounded query count", fixture.state)
		}
	}
}

// Freshness of every row cannot replace the producer's single source clock.
func TestTransferDebitsMixedFreshClocksAreUnknown(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, fixture := range []string{"one-different", "all-different", "reverse-order"} {
		rows := debitRows(now)
		if fixture == "one-different" {
			rows[15][0] = fmt.Sprint(now.Add(time.Second).Unix())
		} else {
			for i := range rows {
				rows[i][0] = fmt.Sprint(now.Add(time.Duration(i-7) * time.Second).Unix())
			}
			if fixture == "reverse-order" {
				for i := range len(rows) / 2 {
					rows[i], rows[len(rows)-1-i] = rows[len(rows)-1-i], rows[i]
				}
			}
		}
		alerts, err, _ := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
		if err == nil {
			t.Errorf("%s: different fresh source clocks were accepted as one matrix", fixture)
			continue
		}
		requireTransferDebitUnknown(t, fixture, alerts, err)
	}
}

// A negative audit head cannot establish a valid pre-migration gate.
func TestTransferDebitsNegativeSchemaHeadIsUnknown(t *testing.T) {
	for _, present := range []string{"f", "t"} {
		alerts, err, calls := transferDebitAuthorityRun(t, []Row{{"-1", present}}, nil)
		if err == nil {
			t.Errorf("negative schema head was accepted as pre-migration with presence=%s", present)
			continue
		}
		requireTransferDebitUnknown(t, "negative-head", alerts, err)
		if calls != 1 {
			t.Fatal("invalid schema head started the journal query")
		}
	}
}

// Both gate fields must be valid before deciding that the journal is inapplicable.
func TestTransferDebitsMalformedSchemaBooleanIsUnknown(t *testing.T) {
	for _, head := range []string{"0", "757"} {
		for _, present := range []string{"", "true", "1", "unknown"} {
			alerts, err, calls := transferDebitAuthorityRun(t, []Row{{head, present}}, nil)
			if err == nil {
				t.Errorf("malformed schema boolean bypassed validation before migration at head=%s", head)
				continue
			}
			requireTransferDebitUnknown(t, "malformed-schema-boolean", alerts, err)
			if calls != 1 {
				t.Fatal("malformed schema boolean started the journal query")
			}
		}
	}
}

// A valid old schema is inapplicable; installed schemas still require the table.
func TestTransferDebitsSchemaGatePreservesValidBoundaries(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, head := range []string{"0", "757"} {
		for _, present := range []string{"f", "t"} {
			alerts, err, calls := transferDebitAuthorityRun(t, []Row{{head, present}}, nil)
			if err != nil || len(alerts) != 0 || calls != 1 {
				t.Fatalf("valid pre-migration gate changed at head=%s presence=%s", head, present)
			}
		}
	}
	for _, head := range []string{"758", "759"} {
		alerts, err, calls := transferDebitAuthorityRun(t, []Row{{head, "t"}}, debitRows(now))
		if err != nil || len(alerts) != 0 || calls != 2 {
			t.Fatalf("complete installed-schema control failed at head=%s", head)
		}
		alerts, err, calls = transferDebitAuthorityRun(t, []Row{{head, "f"}}, nil)
		requireTransferDebitUnknown(t, "installed-table-missing", alerts, err)
		if calls != 1 {
			t.Fatal("missing installed table started the journal query")
		}
	}
}

// Incomplete gate replies and invalid scalar values do not start dependent work.
func TestTransferDebitsIncompleteSchemaIsUnknown(t *testing.T) {
	for i, rows := range [][]Row{
		nil,
		{{"758"}},
		{{"758", "t", "extra"}},
		{{"758", "t"}, {"758", "t"}},
		{{"unknown", "t"}},
		{{"9223372036854775808", "t"}},
		{{"758", "unknown"}},
	} {
		alerts, err, calls := transferDebitAuthorityRun(t, rows, nil)
		requireTransferDebitUnknown(t, fmt.Sprintf("schema-%d", i), alerts, err)
		if calls != 1 {
			t.Fatalf("invalid schema fixture %d started the journal query", i)
		}
	}
}

// Empty and short bursts stay healthy, while exact threshold ages retain severity.
func TestTransferDebitsCompleteMatrixPreservesStrictAgeBands(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, state := range []struct {
		name   string
		column int
	}{
		{name: "pending", column: 2},
		{name: "redis_release", column: 3},
	} {
		for _, fixture := range []struct {
			age      string
			observed string
			severity Severity
		}{
			{age: "-1"},
			{age: "0"},
			{age: "59.999"},
			{age: "60", observed: "60.000", severity: SeverityWarn},
			{age: "299.999", observed: "299.999", severity: SeverityWarn},
			{age: "300", observed: "300.000", severity: SeverityPage},
		} {
			rows := debitRows(now)
			rows[15][state.column] = fixture.age
			alerts, err, calls := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
			if err != nil || calls != 2 {
				t.Fatalf("valid %s age=%s lost source coverage", state.name, fixture.age)
			}
			if fixture.severity == "" {
				if len(alerts) != 0 {
					t.Errorf("valid %s age=%s falsely alerted", state.name, fixture.age)
				}
				continue
			}
			if len(alerts) != 1 {
				t.Fatalf("%s age=%s lost its exact threshold finding", state.name, fixture.age)
			}
			alert := alerts[0]
			if alert.Identity() != "pg/transfer-debits|debit-writeback-lag|pg-1|partition-15/"+state.name ||
				alert.SignalNumber != "2.5a" || alert.SignalKey != "transfer-debits" ||
				alert.Severity != fixture.severity || alert.Sustain != 1 ||
				!strings.Contains(alert.Observed, "oldest_seconds="+fixture.observed) {
				t.Fatalf("%s age=%s changed its diagnosis, identity or severity", state.name, fixture.age)
			}
			requireAlertClass(t, alerts, "debit-writeback-lag")
		}
	}
}

// A lease cannot hide either debt state, and a missing scheduler stays separate.
func TestTransferDebitsIndependentDebtAndWorkerAlerts(t *testing.T) {
	rows := debitRows(syntheticSettings(nil).Now())
	rows[3][2], rows[3][3], rows[3][5] = "60", "300", "t"
	rows[7][4] = "f"
	alerts, err, _ := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
	want := map[string]struct {
		severity Severity
		sustain  int
	}{
		"debit-writeback-lag|partition-3/pending":       {severity: SeverityWarn, sustain: 1},
		"debit-writeback-lag|partition-3/redis_release": {severity: SeverityPage, sustain: 1},
		"debit-worker-missing|partition-7":              {severity: SeverityWarn, sustain: 2},
	}
	if err != nil || len(alerts) != len(want) {
		t.Fatal("independent debt states or missing worker were suppressed")
	}
	for _, alert := range alerts {
		key := alert.Class + "|" + alert.Frame
		expected, ok := want[key]
		if !ok || alert.SignalID != "pg/transfer-debits" || alert.Target != "pg-1" ||
			alert.Severity != expected.severity || alert.Sustain != expected.sustain {
			t.Fatal("independent debit findings changed identity or sustain")
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatal("independent debit finding was omitted or duplicated")
	}
}

// The freshness boundary is inclusive, and shard order does not own the clock.
func TestTransferDebitsCompleteMatrixClockBoundaries(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, fixture := range []struct {
		offset  time.Duration
		unknown bool
	}{
		{offset: -30 * time.Second},
		{offset: 0},
		{offset: 30 * time.Second},
		{offset: -30*time.Second - time.Millisecond, unknown: true},
		{offset: 30*time.Second + time.Millisecond, unknown: true},
	} {
		rows := debitRows(now)
		clock := fmt.Sprintf("%.3f", float64(now.Add(fixture.offset).UnixNano())/1e9)
		for i := range rows {
			rows[i][0] = clock
			rows[i][1] = fmt.Sprint(15 - i)
		}
		alerts, err, _ := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
		if fixture.unknown {
			requireTransferDebitUnknown(t, "stale-or-future-clock", alerts, err)
		} else if err != nil || len(alerts) != 0 {
			t.Fatalf("valid clock boundary %s or reordered complete shards were rejected", fixture.offset)
		}
	}
}

// A partial or ambiguous matrix cannot authorize even an otherwise old first row.
func TestTransferDebitsPartialOrMalformedMatrixIsUnknown(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, fault := range []string{"missing", "extra", "duplicate", "short-row", "extra-field", "negative-shard", "out-of-range-shard"} {
		rows := debitRows(now)
		rows[0][2] = "300"
		switch fault {
		case "missing":
			rows = rows[:15]
		case "extra":
			rows = append(rows, rows[15])
		case "duplicate":
			rows[15][1] = "0"
		case "short-row":
			rows[15] = rows[15][:5]
		case "extra-field":
			rows[15] = append(rows[15], "extra")
		case "negative-shard":
			rows[15][1] = "-1"
		case "out-of-range-shard":
			rows[15][1] = "16"
		}
		alerts, err, _ := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
		requireTransferDebitUnknown(t, fault, alerts, err)
	}
	for _, column := range []int{2, 3} {
		for _, age := range []string{"NaN", "+Inf", "-Inf", "-1.001"} {
			rows := debitRows(now)
			rows[15][column] = age
			alerts, err, _ := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
			requireTransferDebitUnknown(t, "invalid-age", alerts, err)
		}
	}
}

// Malformed source fields remain private through errors and Markdown rendering.
func TestTransferDebitsMalformedObservationOmitsSourceCells(t *testing.T) {
	privateCell := "synthetic-private-debit-cell 192.0.2.23 debit.example"
	now := syntheticSettings(nil).Now()
	for column := range 6 {
		rows := debitRows(now)
		rows[15][column] = privateCell
		alerts, err, _ := transferDebitAuthorityRun(t, []Row{{"758", "t"}}, rows)
		requireTransferDebitUnknown(t, "private-backlog-cell", alerts, err)
		if strings.Contains(err.Error(), privateCell) {
			t.Fatalf("backlog field %d leaked in the returned parser error", column)
		}
		requireAlertOmits(t, alerts[0], privateCell, "192.0.2.23", "debit.example")
	}
	for column := range 2 {
		rows := []Row{{"758", "t"}}
		rows[0][column] = privateCell
		alerts, err, _ := transferDebitAuthorityRun(t, rows, nil)
		requireTransferDebitUnknown(t, "private-schema-cell", alerts, err)
		if strings.Contains(err.Error(), privateCell) {
			t.Fatalf("schema field %d leaked in the returned parser error", column)
		}
		requireAlertOmits(t, alerts[0], privateCell, "192.0.2.23", "debit.example")
	}
}

// Query failures preserve their cause while the public alert withholds raw text.
func TestTransferDebitsSourceFailuresStayUnknown(t *testing.T) {
	for _, failedCall := range []int{1, 2} {
		calls := 0
		sourceErr := errors.New("synthetic-private-debit-source 192.0.2.23")
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			calls++
			if calls == failedCall {
				return nil, sourceErr
			}
			if calls != 1 || !strings.Contains(query, "max(end_version_number)") {
				t.Fatal("debit source failure issued an unexpected query")
			}
			return []Row{{"758", "t"}}, nil
		}}
		alerts, err := NewWithSignals(syntheticSettings(source), NewTransferDebitsSignal()).Run(context.Background())
		requireTransferDebitUnknown(t, "source-failure", alerts, err)
		if calls != failedCall || !errors.Is(err, sourceErr) {
			t.Fatal("debit source failure retried, continued or lost its cause")
		}
		requireAlertOmits(t, alerts[0], "synthetic-private-debit-source", "192.0.2.23")
	}
}

func TestTransferDebitsRequiresEveryPartitionAndCurrentClock(t *testing.T) {
	now := time.Now()
	for _, fault := range []string{"missing", "duplicate", "stale", "nan", "bad_boolean"} {
		t.Run(fault, func(t *testing.T) {
			rows := debitRows(now)
			switch fault {
			case "missing":
				rows = rows[:15]
			case "duplicate":
				rows[15][1] = "0"
			case "stale":
				rows[0][0] = fmt.Sprint(now.Add(-time.Minute).Unix())
			case "nan":
				rows[0][2] = "NaN"
			case "bad_boolean":
				rows[0][4] = "unknown"
			}
			if _, err := parseTransferDebits(rows, now); err == nil {
				t.Fatal("invalid observation became healthy")
			}
		})
	}
}
func TestTransferDebitsLagAndHealthyControls(t *testing.T) {
	now := syntheticSettings(nil).Now()
	rows := debitRows(now)
	rows[3][2] = "61"
	rows[4][3] = "301"
	rows[5][4] = "f"
	source := &syntheticSource{postgresFn: func(q string) ([]Row, error) {
		if strings.Contains(q, "max(end_version_number)") {
			return []Row{{"758", "t"}}, nil
		}
		result := []Row{}
		for _, r := range rows {
			result = append(result, Row(r))
		}
		return result, nil
	}}
	alerts, err := NewTransferDebitsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	warn, page := false, false
	for _, a := range alerts {
		if a.Class == "debit-writeback-lag" {
			warn = warn || strings.Contains(a.Observed, "61.000")
			page = page || strings.Contains(a.Observed, "301.000")
		}
	}
	if !warn || !page {
		t.Fatal("pending or released debt not visible", alerts)
	}
	rows = debitRows(now)
	alerts, err = NewTransferDebitsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatal("healthy/empty/short burst falsely warned", alerts, err)
	}
	rows[0][2] = "59"
	alerts, err = NewTransferDebitsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatal("bounded burst falsely warned", alerts, err)
	}
}

// The production query sees pending and applied debt independently, with a
// fixed partition matrix even when every scheduler key is absent.
func TestTransferDebitsActualIndexedBacklog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		balance := server.NewId()
		balance[15] = 3
		other := server.NewId()
		other[15] = 7
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_debit_journal(contract_id,balance_id,shard,debit_byte_count,applied,create_time) VALUES($1,$2,3,11,false,clock_timestamp() AT TIME ZONE 'UTC'-interval '65 seconds'),($3,$4,7,23,true,clock_timestamp() AT TIME ZONE 'UTC'-interval '301 seconds')`, server.NewId(), balance, server.NewId(), other))
		})
		var observations []transferDebitObservation
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, transferDebitBacklogQuery)
			server.WithPgResult(rows, err, func() {
				projected := []pgRow{}
				for rows.Next() {
					var at, pending, release float64
					var shard int
					var scheduled, leased bool
					server.Raise(rows.Scan(&at, &shard, &pending, &release, &scheduled, &leased))
					boolean := func(v bool) string {
						if v {
							return "t"
						}
						return "f"
					}
					projected = append(projected, pgRow{fmt.Sprint(at), fmt.Sprint(shard), fmt.Sprint(pending), fmt.Sprint(release), boolean(scheduled), boolean(leased)})
				}
				observations, err = parseTransferDebits(projected, time.Now())
				server.Raise(err)
			})
		})
		if len(observations) != 16 || observations[3].pending < 65 || observations[3].release != -1 || observations[7].release < 301 || observations[7].pending != -1 {
			t.Fatal("durable states were conflated", observations)
		}
	})
}

// A complete independent observation that pages, for the isolation control
// below. It reads one fixed query and reports one exact frame.
type transferDebitsIndependentPageProbe struct{}

const transferDebitsIndependentPageQuery = `SELECT 'synthetic independent page'`

func (transferDebitsIndependentPageProbe) id() string             { return "pg/synthetic-independent" }
func (transferDebitsIndependentPageProbe) tier() string           { return tierWarn }
func (transferDebitsIndependentPageProbe) cadence() time.Duration { return time.Minute }
func (self transferDebitsIndependentPageProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	if _, err := env.runner.pg(ctx, transferDebitsIndependentPageQuery); err != nil {
		return nil, err
	}
	return []finding{{probeId: self.id(), tier: tierPage, class: "synthetic-independent-page", target: pgTarget(env),
		frame: "partition-3/pending", sustain: 1, symptom: "A synthetic independent observation pages.",
		baseline: "The synthetic observation never pages.", observed: "synthetic=page", mechanism: "Synthetic fixture.",
		action: "None; this is a test fixture.", verify: "None.", playbook: "SIGNALS.md §2.5"}}, nil
}

// Missing debit coverage must not erase a separate complete finding.
func TestTransferDebitsUnknownPreservesIndependentSignal(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, debitFirst := range []bool{false, true} {
		calls := 0
		source := &syntheticSource{postgresFn: func(query string) ([]Row, error) {
			calls++
			if strings.Contains(query, "max(end_version_number)") {
				return []Row{{"763", "t"}}, nil
			}
			var rows []pgRow
			switch query {
			case transferDebitBacklogQuery:
				rows = debitRows(now)[:15]
			case transferDebitsIndependentPageQuery:
				rows = []pgRow{{"synthetic independent page"}}
			default:
				t.Fatal("independent observation made an unexpected query")
			}
			result := make([]Row, len(rows))
			for i, row := range rows {
				result[i] = Row(row)
			}
			return result, nil
		}}
		independentSignal := &signalAdapter{number: "2.5x", key: "synthetic-independent", name: "Synthetic independent observation", probe: transferDebitsIndependentPageProbe{}}
		signals := []Signal{independentSignal, NewTransferDebitsSignal()}
		if debitFirst {
			signals[0], signals[1] = signals[1], signals[0]
		}
		alerts, err := NewWithSignals(syntheticSettings(source), signals...).Run(context.Background())
		if err == nil || calls != 3 || len(alerts) != 2 {
			t.Fatal("debit visibility failure suppressed or replaced an independent observation", calls, len(alerts), err)
		}
		unknown := requireAlertClass(t, alerts, "cannot-observe")
		if unknown.Identity() != "monitor/visibility|cannot-observe|pg/transfer-debits|" {
			t.Fatal("debit visibility failure was assigned to the independent signal")
		}
		independent := requireAlertClass(t, alerts, "synthetic-independent-page")
		if independent.Identity() != "pg/synthetic-independent|synthetic-independent-page|pg-1|partition-3/pending" || independent.Severity != SeverityPage {
			t.Fatal("independent complete page was suppressed or weakened", independent.Identity(), independent.Severity)
		}
	}
}
