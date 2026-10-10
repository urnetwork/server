package monitor

import (
	"context"
	"fmt"
	"github.com/urnetwork/server/v2026"
	"strings"
	"testing"
	"time"
)

func debitRows(now time.Time) []pgRow {
	rows := []pgRow{}
	for i := range 16 {
		rows = append(rows, pgRow{fmt.Sprint(now.Unix()), fmt.Sprint(i), "-1", "-1", "t", "f"})
	}
	return rows
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
