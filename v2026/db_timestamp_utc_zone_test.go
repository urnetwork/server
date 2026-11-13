package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
	// the zones of the child processes, where a system has no zone database
	_ "time/tzdata"

	"github.com/jackc/pgx/v5/pgtype"
)

// Names the zone a child process of TestTimestampStoresTheInstantInAnyZone
// checks in.
const timestampInstantZoneEnvName = "URNETWORK_TEST_TIMESTAMP_INSTANT_ZONE"

// The timestamp columns hold UTC without a zone, and pgx's stock encoding
// wrote a time's wall clock and dropped its location. So a time.Now or
// time.UnixMilli time in a process off UTC, a time parsed with an offset, or
// a time moved into another zone with In, was stored shifted by its offset,
// and a read parameter like it compared with a shifted value. A process takes
// its zone when it starts, so the check runs in a child process in each zone.
func TestTimestampStoresTheInstantInAnyZone(t *testing.T) {
	if zone := os.Getenv(timestampInstantZoneEnvName); zone != "" {
		testTimestampStoresTheInstant(t, zone)
		return
	}
	for _, zone := range []string{"America/Chicago", "Asia/Tokyo"} {
		command := exec.CommandContext(
			t.Context(),
			os.Args[0],
			"-test.run=^TestTimestampStoresTheInstantInAnyZone$",
			"-test.count=1",
			"-test.v",
		)
		command.Env = append(os.Environ(), "TZ="+zone, timestampInstantZoneEnvName+"="+zone)
		output, err := command.CombinedOutput()
		if err != nil || !strings.Contains(string(output), "--- PASS: TestTimestampStoresTheInstantInAnyZone") {
			// the check's own lines, without the child's logs
			checkLines := []string{}
			for _, line := range strings.Split(string(output), "\n") {
				line = strings.TrimSpace(line)
				if strings.Contains(line, "_test.go:") || strings.Contains(line, "test_util.go:") ||
					strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "panic:") {
					checkLines = append(checkLines, line)
				}
			}
			t.Errorf("in %s: %v\n%s", zone, err, strings.Join(checkLines, "\n"))
		}
	}
}

// The check, in a process whose zone is zone.
func testTimestampStoresTheInstant(t *testing.T, zone string) {
	location, err := time.LoadLocation(zone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_, processOffset := now.Zone()
	_, zoneOffset := now.In(location).Zone()
	if processOffset == 0 || processOffset != zoneOffset {
		t.Fatalf("the process's zone is not %s", zone)
	}
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	(&TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := t.Context()
		Db(ctx, func(conn PgConn) {
			RaisePgResult(conn.Exec(
				ctx,
				`CREATE TABLE test_timestamp_instant (name text PRIMARY KEY, at timestamp, ats timestamp[])`,
			))
		}, OptReadWrite())

		instant := NowUtc()
		parsed, err := time.Parse(time.RFC3339Nano, instant.In(time.FixedZone("", -(3*60*60+30*60))).Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
		local := instant.Local()
		cases := []struct {
			name  string
			value any
			want  time.Time
		}{
			{"utc", instant, instant},
			{"local", local, instant},
			{"unix milli", time.UnixMilli(instant.UnixMilli()), instant.Truncate(time.Millisecond)},
			{"parsed offset", parsed, instant},
			{"zoned", instant.In(kolkata), instant},
			{"pointer", &local, instant},
			{"pgtype", pgtype.Timestamp{Time: local, Valid: true}, instant},
		}
		Tx(ctx, func(tx PgTx) {
			for _, c := range cases {
				RaisePgResult(tx.Exec(ctx, `INSERT INTO test_timestamp_instant (name, at) VALUES ($1, $2)`, c.name, c.value))
			}
			RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO test_timestamp_instant (name, ats) VALUES ('array', $1)`,
				[]time.Time{instant, local, parsed, instant.In(kolkata)},
			))
		})

		Db(ctx, func(conn PgConn) {
			for _, c := range cases {
				// the stored wall clock is the UTC one, as PostgreSQL reads it
				var stored time.Time
				var storedText string
				Raise(conn.QueryRow(
					ctx,
					`SELECT at, to_char(at, 'YYYY-MM-DD HH24:MI:SS.US') FROM test_timestamp_instant WHERE name = $1`,
					c.name,
				).Scan(&stored, &storedText))
				if wantText := c.want.UTC().Format("2006-01-02 15:04:05.000000"); storedText != wantText || !stored.Equal(c.want) {
					t.Errorf("in %s a %s time was stored as %s, read as %s; want %s", zone, c.name, storedText, stored, wantText)
				}
			}

			var storedTexts []string
			Raise(conn.QueryRow(
				ctx,
				`SELECT array_agg(to_char(element, 'YYYY-MM-DD HH24:MI:SS.US') ORDER BY position)
				FROM test_timestamp_instant, unnest(ats) WITH ORDINALITY AS stored(element, position)
				WHERE name = 'array'`,
			).Scan(&storedTexts))
			wantText := instant.Format("2006-01-02 15:04:05.000000")
			if len(storedTexts) != 4 {
				t.Errorf("in %s a timestamp[] was stored as %v", zone, storedTexts)
			}
			for index, storedText := range storedTexts {
				if storedText != wantText {
					t.Errorf("in %s timestamp[] element %d was stored as %s, want %s", zone, index, storedText, wantText)
				}
			}

			// read parameters in the process's zone match the row written in UTC
			var matched, matchedByArray int
			Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM test_timestamp_instant WHERE name = 'utc' AND at = $1`,
				local,
			).Scan(&matched))
			Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM test_timestamp_instant WHERE name = 'utc' AND at = ANY($1::timestamp[])`,
				[]time.Time{local},
			).Scan(&matchedByArray))
			if matched != 1 || matchedByArray != 1 {
				t.Errorf("in %s a read parameter at the UTC row's instant matched %d rows, and in a timestamp[] %d; want 1", zone, matched, matchedByArray)
			}
		})
	})
}
