// Empty escrow writes need an independent detector because WAL/buffer
// contention can consume database concurrency without saturating host CPU.
package monitor

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A quiet CPU sample must not hide zero-byte escrow fanout.
func TestEscrowAmplificationSignalRegistered(t *testing.T) {
	for _, signal := range NewSignals() {
		if signal.Key() == "escrow-amplification" {
			return
		}
	}
	t.Fatal("missing independent escrow write-amplification signal")
}

// Builds only aggregate synthetic counts; no contract or balance identity
// needs to cross the monitor source boundary.
func escrowAmplificationTestRows(now time.Time, sample escrowAmplificationSample) []Row {
	row := Row{strconv.FormatInt(now.Unix(), 10)}
	for _, value := range []int64{sample.candidates, sample.contracts, sample.rows, sample.zeroRows, sample.affected, sample.limited, sample.missing} {
		row = append(row, strconv.FormatInt(value, 10))
	}
	return []Row{row}
}

// Every observation uses the same bounded direct query, independently of CPU.
func escrowAmplificationTestSettings(t *testing.T, rows []Row, sourceErr error) SignalSettings {
	t.Helper()
	source := &syntheticSource{
		postgresFn: func(query string) ([]Row, error) {
			if query != escrowAmplificationQuery {
				t.Fatal("escrow observation escaped its fixed bounded source")
			}
			return rows, sourceErr
		},
		hostFn: func(HostSettings, string) (string, error) {
			t.Fatal("escrow observation unexpectedly depended on OS CPU")
			return "", nil
		},
	}
	return syntheticSettings(source)
}

// Positive multi-grant splits are healthy; actual zero-byte writes produce
// their own finding even when the CPU source is absent or below its band.
func TestEscrowAmplificationPositiveAndZeroRowControls(t *testing.T) {
	now := syntheticSettings(nil).Now()
	for _, test := range []struct {
		name     string
		sample   escrowAmplificationSample
		severity Severity
	}{
		{name: "ordinary", sample: escrowAmplificationSample{candidates: 100, contracts: 100, rows: 100}},
		{name: "positive split", sample: escrowAmplificationSample{candidates: 100, contracts: 100, rows: 15000}},
		{name: "one empty grant", sample: escrowAmplificationSample{candidates: 100, contracts: 100, rows: 101, zeroRows: 1, affected: 1}, severity: SeverityWarn},
		{name: "below row threshold", sample: escrowAmplificationSample{candidates: 100, contracts: 100, rows: 199, zeroRows: 99, affected: 10}, severity: SeverityWarn},
		{name: "below contract threshold", sample: escrowAmplificationSample{candidates: 100, contracts: 100, rows: 200, zeroRows: 100, affected: 9}, severity: SeverityWarn},
		{name: "page boundary", sample: escrowAmplificationSample{candidates: 100, contracts: 100, rows: 200, zeroRows: 100, affected: 10}, severity: SeverityPage},
		{name: "widespread empty fanout", sample: escrowAmplificationSample{candidates: 100, contracts: 100, rows: 15000, zeroRows: 14900, affected: 100}, severity: SeverityPage},
	} {
		alerts, err := NewEscrowAmplificationSignal().Run(context.Background(), escrowAmplificationTestSettings(t, escrowAmplificationTestRows(now, test.sample), nil))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if test.sample.zeroRows == 0 {
			if len(alerts) != 0 {
				t.Fatalf("%s: positive funding falsely alerted: %+v", test.name, alerts)
			}
			continue
		}
		alert := requireAlertClass(t, alerts, "escrow-zero-byte-writes")
		if len(alerts) != 1 || alert.Severity != test.severity || alert.Sustain != 2 {
			t.Fatalf("%s: incorrect fanout finding: %+v", test.name, alerts)
		}
		for _, want := range []string{"complete=true", "False-positive qualifier", "False-negative qualifiers", "are not CPU time", "zero_byte_escrow_rows=" + strconv.FormatInt(test.sample.zeroRows, 10)} {
			if !strings.Contains(alert.Markdown(), want) {
				t.Fatalf("%s: finding omits %q", test.name, want)
			}
		}
	}
}

// Stale, malformed, absent, and partial data must not manufacture recovery.
func TestEscrowAmplificationUnavailableControls(t *testing.T) {
	now := syntheticSettings(nil).Now()
	valid := escrowAmplificationSample{candidates: 100, contracts: 100, rows: 100}
	for _, test := range []struct {
		name string
		rows []Row
		err  error
	}{
		{name: "transport", err: errors.New("synthetic private query text")},
		{name: "missing"},
		{name: "columns", rows: []Row{{"1"}}},
		{name: "negative", rows: []Row{{strconv.FormatInt(now.Unix(), 10), "100", "100", "-1", "0", "0", "0", "0"}}},
		{name: "stale", rows: escrowAmplificationTestRows(now.Add(-time.Minute), valid)},
		{name: "future", rows: escrowAmplificationTestRows(now.Add(time.Minute), valid)},
		{name: "no current contracts", rows: escrowAmplificationTestRows(now, escrowAmplificationSample{candidates: 100})},
		{name: "empty database", rows: escrowAmplificationTestRows(now, escrowAmplificationSample{})},
		{name: "missing allocations", rows: escrowAmplificationTestRows(now, escrowAmplificationSample{candidates: 1, contracts: 1, missing: 1})},
		{name: "capped positive scan", rows: escrowAmplificationTestRows(now, escrowAmplificationSample{candidates: 1, contracts: 1, rows: 513, limited: 1})},
		{name: "excess candidates", rows: escrowAmplificationTestRows(now, escrowAmplificationSample{candidates: 101, contracts: 100, rows: 100})},
		{name: "impossible zero count", rows: escrowAmplificationTestRows(now, escrowAmplificationSample{candidates: 1, contracts: 1, rows: 1, zeroRows: 2, affected: 1})},
	} {
		alerts, err := NewEscrowAmplificationSignal().Run(context.Background(), escrowAmplificationTestSettings(t, test.rows, test.err))
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		alert := requireAlertClass(t, alerts, "escrow-amplification-unavailable")
		if len(alerts) != 1 || alert.Severity != SeverityWarn || !strings.Contains(alert.Markdown(), "unknown, not zero") {
			t.Fatalf("%s: bad coverage result: %+v", test.name, alerts)
		}
		requireAlertOmits(t, alert, "synthetic private query text")
	}
}

// A capped query proves only a lower bound, but those confirmed empty writes
// still establish the symptom; coverage loss cannot suppress that evidence.
func TestEscrowAmplificationPartialSourceRetainsConfirmedWaste(t *testing.T) {
	now := syntheticSettings(nil).Now()
	rows := escrowAmplificationTestRows(now, escrowAmplificationSample{
		candidates: 10, contracts: 10, rows: 5130, zeroRows: 5120, affected: 10, limited: 10,
	})
	alerts, err := NewEscrowAmplificationSignal().Run(context.Background(), escrowAmplificationTestSettings(t, rows, nil))
	if err != nil || len(alerts) != 2 {
		t.Fatalf("partial source: alerts=%+v err=%v", alerts, err)
	}
	requireAlertClass(t, alerts, "escrow-amplification-unavailable")
	alert := requireAlertClass(t, alerts, "escrow-zero-byte-writes")
	if alert.Severity != SeverityPage || !strings.Contains(alert.Observed, "complete=false") {
		t.Fatalf("confirmed lower bound was lost: %+v", alert)
	}
}

// Runs the real bounded SQL against synthetic ledger rows, including valid
// zero-byte anchors, no-escrow contracts, stale/future rows, and a capped scan.
func TestEscrowAmplificationQueryEligibilityAndBounds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		server.Tx(ctx, func(tx server.PgTx) {
			for _, test := range []struct {
				bytes  int64
				payer  bool
				age    time.Duration
				rows   int
				zeroes int
			}{
				{bytes: 1024, payer: true, rows: 3, zeroes: 2},
				{bytes: 1024, payer: true, rows: 2},
				{bytes: 0, payer: true, rows: 1, zeroes: 1},
				{bytes: 1024, rows: 1, zeroes: 1},
				{bytes: 1024, payer: true, age: 3 * time.Minute, rows: 1, zeroes: 1},
				{bytes: 1024, payer: true, age: -time.Minute, rows: 1, zeroes: 1},
				{bytes: 1024, payer: true, rows: 520, zeroes: 520},
			} {
				contractId := server.NewId()
				var payerNetworkId *server.Id
				if test.payer {
					id := server.NewId()
					payerNetworkId = &id
				}
				server.RaisePgResult(tx.Exec(ctx, `
					INSERT INTO transfer_contract
					(contract_id, source_network_id, source_id, destination_network_id, destination_id, transfer_byte_count, payer_network_id, create_time)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				`, contractId, server.NewId(), server.NewId(), server.NewId(), server.NewId(), test.bytes, payerNetworkId, now.Add(-test.age)))
				for index := range test.rows {
					byteCount := 1024
					if index < test.zeroes {
						byteCount = 0
					}
					server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow (contract_id, balance_id, balance_byte_count) VALUES ($1, $2, $3)`, contractId, server.NewId(), byteCount))
				}
			}
		})
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, escrowAmplificationQuery)
			server.WithPgResult(result, err, func() {
				if !result.Next() {
					t.Fatal("aggregate query returned no row")
				}
				var timestamp int64
				var sample escrowAmplificationSample
				server.Raise(result.Scan(&timestamp, &sample.candidates, &sample.contracts, &sample.rows, &sample.zeroRows, &sample.affected, &sample.limited, &sample.missing))
				want := escrowAmplificationSample{candidates: 7, contracts: 3, rows: 518, zeroRows: 515, affected: 2, limited: 1}
				if sample != want {
					t.Fatalf("bounded query counts = %+v, want %+v", sample, want)
				}
			})
		})
	})
}
