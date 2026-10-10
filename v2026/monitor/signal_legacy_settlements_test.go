package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func legacyRows(now time.Time) []pgRow {
	rows := []pgRow{}
	for i := range 16 {
		rows = append(rows, pgRow{fmt.Sprint(now.Unix()), fmt.Sprint(i), "-1", "-1", "-1", "t", "f"})
	}
	return rows
}
func TestLegacySettlementsAuthorityAndRetainedFailure(t *testing.T) {
	now := syntheticSettings(nil).Now()
	rows := legacyRows(now)
	head, present := "763", "t"
	source := &syntheticSource{postgresFn: func(q string) ([]Row, error) {
		if strings.Contains(q, "max(end_version_number)") {
			return []Row{{head, present}}, nil
		}
		result := []Row{}
		for _, r := range rows {
			result = append(result, Row(r))
		}
		return result, nil
	}}
	alerts, err := NewLegacySettlementsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatal("healthy full empty observation failed", alerts, err)
	}
	rows[1][2] = "59"
	alerts, err = NewLegacySettlementsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 0 {
		t.Fatal("short pending burst falsely warned", alerts, err)
	}
	rows[1][2] = "60"
	rows[2][3] = "2"
	rows[3][4] = "301"
	alerts, err = NewLegacySettlementsSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, a := range alerts {
		if a.Class == "legacy-settlement-pending" {
			for _, state := range []string{"pending", "accounting", "operational"} {
				if strings.Contains(a.Observed, "state="+state) {
					found[state] = true
				}
			}
		}
	}
	if len(found) != 3 {
		t.Fatal("retry delay or failed projection hid pending financial work", found)
	}
	rows = legacyRows(now)
	present = "f"
	if _, err = NewLegacySettlementsSignal().Run(context.Background(), syntheticSettings(source)); err == nil {
		t.Fatal("installed missing schema became healthy")
	}
	head = "762"
	if alerts, err = NewLegacySettlementsSignal().Run(context.Background(), syntheticSettings(source)); err != nil || len(alerts) != 0 {
		t.Fatal("pre-migration healthy control failed", alerts, err)
	}
	for _, fault := range []string{"missing", "duplicate", "stale", "future", "mixed_clock", "nan", "negative", "boolean", "column"} {
		t.Run(fault, func(t *testing.T) {
			rows := legacyRows(now)
			switch fault {
			case "missing":
				rows = rows[:15]
			case "duplicate":
				rows[15][1] = "0"
			case "stale":
				rows[0][0] = fmt.Sprint(now.Add(-time.Minute).Unix())
			case "future":
				rows[0][0] = fmt.Sprint(now.Add(time.Minute).Unix())
			case "mixed_clock":
				rows[0][0] = fmt.Sprint(now.Add(time.Second).Unix())
			case "nan":
				rows[0][3] = "NaN"
			case "negative":
				rows[0][2] = "-0.5"
			case "boolean":
				rows[0][5] = "unknown"
			case "column":
				rows[0] = rows[0][:6]
			}
			if _, err := parseLegacySettlements(rows, now); err == nil {
				t.Fatal("invalid source became healthy")
			}
		})
	}
}

func TestLegacySettlementsActualIndexedFailureAge(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		server.Tx(ctx, func(tx server.PgTx) {
			for i, code := range []string{"none", "accounting", "operational"} {
				id := server.NewId()
				id[15] = byte(i)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract(contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count)
      VALUES($1,$2,$3,$4,$5,100)`, id, server.NewId(), server.NewId(), server.NewId(), server.NewId()))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,failure_code,create_time,next_attempt_time)
      VALUES($1,$2,'settled',$3,clock_timestamp() AT TIME ZONE 'UTC'-interval '301 seconds',clock_timestamp() AT TIME ZONE 'UTC'+interval '15 minutes')`, id, i, code))
			}
		})
		projected := []pgRow{}
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, legacySettlementBacklogQuery)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var at, a, b, c float64
					var shard int
					var scheduled, leased bool
					server.Raise(rows.Scan(&at, &shard, &a, &b, &c, &scheduled, &leased))
					boolean := func(v bool) string {
						if v {
							return "t"
						}
						return "f"
					}
					projected = append(projected, pgRow{fmt.Sprint(at), fmt.Sprint(shard), fmt.Sprint(a), fmt.Sprint(b), fmt.Sprint(c), boolean(scheduled), boolean(leased)})
				}
			})
		})
		observations, err := parseLegacySettlements(projected, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		for i, o := range observations {
			for j, age := range o.ages {
				if i < 3 && i == j {
					if age < 301 {
						t.Fatal("backoff hid a retained intent")
					}
				} else if age != -1 {
					t.Fatal("partition/failure ownership was conflated")
				}
			}
		}
	})
}
