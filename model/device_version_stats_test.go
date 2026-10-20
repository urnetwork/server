package model

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server"
)

type deviceVersionStatsQueryTest func(context.Context, string, ...any) (pgx.Rows, error)

func (self deviceVersionStatsQueryTest) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return self(ctx, sql, args...)
}

type deviceVersionStatsRowsTest struct {
	pgx.Rows
	rows      []DeviceVersionCount
	index     int
	closed    bool
	failure   error
	afterScan func()
}

func (self *deviceVersionStatsRowsTest) Next() bool {
	self.index++
	return self.index <= len(self.rows)
}
func (self *deviceVersionStatsRowsTest) Err() error { return self.failure }
func (self *deviceVersionStatsRowsTest) Close()     { self.closed = true }
func (self *deviceVersionStatsRowsTest) Scan(dest ...any) error {
	r := self.rows[self.index-1]
	*dest[0].(*string), *dest[1].(*string), *dest[2].(*int64) = r.Population, r.Version, r.Count
	*dest[3].(*time.Time) = time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	if self.afterScan != nil {
		self.afterScan()
	}
	return nil
}

func TestDeviceVersionStatsRefusesPartialAndCanceledSnapshots(t *testing.T) {
	failed := errors.New("synthetic version read failure")
	for _, mode := range []string{"row failure", "cancel after decode", "empty", "invalid population"} {
		ctx, cancel := context.WithCancel(t.Context())
		rows := &deviceVersionStatsRowsTest{rows: []DeviceVersionCount{{Population: "client", Version: "unknown", Count: 3}}}
		switch mode {
		case "row failure":
			rows.failure = failed
		case "cancel after decode":
			rows.afterScan = cancel
		case "empty":
			rows.rows = nil
		case "invalid population":
			rows.rows[0].Population = "unrecognized"
		}
		query := deviceVersionStatsQueryTest(func(context.Context, string, ...any) (pgx.Rows, error) { return rows, nil })
		counts, observed, err := readDeviceVersionStats(ctx, query)
		cancel()
		if err == nil || len(counts) != 0 || !observed.IsZero() || !rows.closed {
			t.Fatalf("%s published partial snapshot: counts=%v observed=%v err=%v closed=%t", mode, counts, observed, err, rows.closed)
		}
	}
}

func TestDeviceVersionStatsPreservesUnknownAndFullBuildVersions(t *testing.T) {
	rows := &deviceVersionStatsRowsTest{rows: []DeviceVersionCount{
		{Population: "provider", Version: "2026.10.20-1076869840", Count: 2},
		{Population: "provider", Version: "unknown", Count: 4},
		{Population: "client", Version: "unknown", Count: 9},
		{Population: "client", Version: "other", Count: 5},
	}}
	query := deviceVersionStatsQueryTest(func(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
		if sql != deviceVersionStatsSql || len(args) != 1 || args[0] != ProvideModePublic {
			t.Fatal("version read lost public provider binding")
		}
		return rows, nil
	})
	counts, observed, err := readDeviceVersionStats(t.Context(), query)
	if err != nil || len(counts) != 4 || observed.IsZero() || counts[0] != rows.rows[0] || counts[3].Count != 5 || !rows.closed {
		t.Fatalf("version counts=%v observed=%v err=%v", counts, observed, err)
	}
}

// Real PostgreSQL fixtures exercise deduplication, missing devices, population
// filters and conservation through the version label cap in the owning query.
func TestDeviceVersionStatsSqlPopulationsAndLongTail(t *testing.T) {
	env := server.DefaultTestEnv()
	env.ApplyDbMigrations = false
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		server.Tx(t.Context(), func(tx server.PgTx) {
			_, err := tx.Exec(t.Context(), `
				CREATE TEMP TABLE network_client (client_id uuid PRIMARY KEY, device_id uuid, active boolean, source_client_id uuid) ON COMMIT DROP;
				CREATE TEMP TABLE device (device_id uuid PRIMARY KEY, client_version varchar(64)) ON COMMIT DROP;
				CREATE TEMP TABLE network_client_connection (client_id uuid, connected boolean) ON COMMIT DROP;
				CREATE TEMP TABLE network_client_location_reliability (client_id uuid, connected boolean, valid boolean) ON COMMIT DROP;
				CREATE TEMP TABLE provide_key (client_id uuid, provide_mode int) ON COMMIT DROP;
			`)
			server.Raise(err)
			for i := range 60 {
				clientId, deviceId := server.NewId(), server.NewId()
				// Distinct known builds, plus a missing device and blank version.
				if i != 0 {
					version := fmt.Sprintf("2026.10.20-%03d", i)
					if i == 1 {
						version = "  "
					}
					_, err = tx.Exec(t.Context(), `INSERT INTO device VALUES ($1,$2)`, deviceId, version)
					server.Raise(err)
				}
				_, err = tx.Exec(t.Context(), `INSERT INTO network_client VALUES ($1,$2,$3,$4)`, clientId, deviceId, i != 59, nil)
				server.Raise(err)
				// Duplicate transports, keys and reliability rows cannot multiply counts.
				_, err = tx.Exec(t.Context(), `INSERT INTO network_client_connection VALUES ($1,$2),($1,$2)`, clientId, i != 58)
				server.Raise(err)
				_, err = tx.Exec(t.Context(), `INSERT INTO network_client_location_reliability VALUES ($1,true,true),($1,true,true)`, clientId)
				server.Raise(err)
				_, err = tx.Exec(t.Context(), `INSERT INTO provide_key VALUES ($1,$2),($1,$2)`, clientId, ProvideModePublic)
				server.Raise(err)
				if i == 57 {
					_, err = tx.Exec(t.Context(), `UPDATE network_client SET source_client_id=client_id WHERE client_id=$1`, clientId)
					server.Raise(err)
				}
				if i == 56 {
					_, err = tx.Exec(t.Context(), `DELETE FROM provide_key WHERE client_id=$1`, clientId)
					server.Raise(err)
				}
				if i == 55 {
					_, err = tx.Exec(t.Context(), `UPDATE network_client_location_reliability SET valid=false WHERE client_id=$1`, clientId)
					server.Raise(err)
				}
			}
			counts, _, err := readDeviceVersionStats(t.Context(), tx)
			if err != nil {
				t.Fatal(err)
			}
			totals, unknown, other, labels := map[string]int64{}, map[string]int64{}, map[string]int64{}, map[string]int{}
			for _, count := range counts {
				totals[count.Population] += count.Count
				labels[count.Population]++
				if count.Version == "unknown" {
					unknown[count.Population] = count.Count
				}
				if count.Version == "other" {
					other[count.Population] = count.Count
				}
			}
			if totals["client"] != 57 || totals["provider"] != 55 || unknown["client"] != 2 || unknown["provider"] != 2 || other["client"] != 5 || other["provider"] != 3 || labels["client"] != 52 || labels["provider"] != 52 {
				t.Fatalf("version population conservation: totals=%v unknown=%v other=%v labels=%v", totals, unknown, other, labels)
			}
		}, server.OptNoRetry())
	})
}
