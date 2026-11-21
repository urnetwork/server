// The settlement epoch's reader of the durable exit record
// (GetProviderEgressExitDiffersClientIds) over a mainnet epoch's window: seven
// days from 18:25Z. A recorded day counts for the epoch exactly when its first
// or its last differing judgement lies in [start, end), which in a window of a
// day or more is when any of its judgements does. A day before or after the
// window, a boundary day whose judgements all fall outside it, and a row that
// counts no differing judgement are no evidence.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

func TestGetProviderEgressExitDiffersClientIdsReadsTheEpochWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		start := time.Date(2026, 11, 20, 18, 25, 0, 0, time.UTC)
		end := start.Add(7 * 24 * time.Hour)
		at := func(month time.Month, day int, hour int, minute int, second int) time.Time {
			return time.Date(2026, month, day, hour, minute, second, 0, time.UTC)
		}
		record := func(first time.Time, last time.Time, differsCount int) server.Id {
			t.Helper()
			clientId := server.NewId()
			day := time.Date(first.Year(), first.Month(), first.Day(), 0, 0, 0, 0, time.UTC)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO provider_egress_exit_day (
						day,
						client_id,
						network_id,
						differs_count,
						cross_family_count,
						first_observed_at,
						last_observed_at
					)
					VALUES ($1, $2, $3, $4, 0, $5, $6)
					`,
					day,
					clientId,
					server.NewId(),
					differsCount,
					first,
					last,
				))
			})
			return clientId
		}

		in := map[string]server.Id{
			"the start day, last after the start":        record(at(11, 20, 10, 0, 0), at(11, 20, 19, 0, 0), 2),
			"a judgement at the start":                   record(start, start, 1),
			"a day inside the window":                    record(at(11, 23, 3, 0, 0), at(11, 23, 21, 0, 0), 5),
			"the end day, first before the end":          record(at(11, 27, 18, 24, 59), at(11, 27, 23, 0, 0), 2),
			"the end day, first and last before the end": record(at(11, 27, 1, 0, 0), at(11, 27, 18, 0, 0), 3),
		}
		out := map[string]server.Id{
			"the start day, all before the start": record(at(11, 20, 1, 0, 0), at(11, 20, 18, 24, 59), 2),
			"a judgement at the end":              record(end, end, 1),
			"the end day, all after the end":      record(at(11, 27, 19, 0, 0), at(11, 27, 23, 0, 0), 2),
			"the day before the window":           record(at(11, 19, 12, 0, 0), at(11, 19, 22, 0, 0), 4),
			"the day after the window":            record(at(11, 28, 0, 0, 0), at(11, 28, 12, 0, 0), 4),
			"a row with no differing judgement":   record(at(11, 23, 3, 0, 0), at(11, 23, 3, 0, 0), 0),
		}

		clientIds := GetProviderEgressExitDiffersClientIds(ctx, start, end)
		for name, clientId := range in {
			if !clientIds[clientId] {
				t.Errorf("%s: not read for the window", name)
			}
		}
		for name, clientId := range out {
			if clientIds[clientId] {
				t.Errorf("%s: read for the window", name)
			}
		}
		if len(clientIds) != len(in) {
			t.Fatalf("read %d clients, want %d", len(clientIds), len(in))
		}
		// an empty window reads nothing
		if clientIds := GetProviderEgressExitDiffersClientIds(ctx, end, start); len(clientIds) != 0 {
			t.Fatalf("an empty window read %d clients", len(clientIds))
		}
	})
}
