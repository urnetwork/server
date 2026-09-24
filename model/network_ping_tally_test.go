package model

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The ping tallies (connect/GEOMAP.md §2.7, §5.7): each one equal to the count
// over the rows it stands for, the dashboard's counts read from them, the days
// they keep dropped with the ping days, and nothing added for what is not
// stored.

// The defaults the tally migration's backfill wrote with: sixteen shards and
// half the planet at the solver's km per ms. Pure.
func TestNetworkPingTallyDefaults(t *testing.T) {
	settings := DefaultNetworkPingTallySettings()
	connect.AssertEqual(t, settings.HourShardCount, 16)
	connect.AssertEqual(t, settings.HalfPlanetRttMs, 200)
}

// The rows of each tally that differ from the count over network_ping's rows,
// counted both ways: zero for every tally when the tallies are exact.
func testNetworkPingTallyDifferences(t testing.TB, ctx context.Context) map[string]int {
	t.Helper()
	settings := DefaultNetworkPingTallySettings()
	tallyQueries := map[string]string{
		"hour tally": `
			SELECT hour, shard, pinger_kind, relayed, cosign, cosign_reason, ping_count, zero_rtt_count, beyond_half_planet_count
			FROM network_ping_hour_tally`,
		"hour tally from rows": `
			SELECT
				date_trunc('hour', create_time),
				get_byte(uuid_send(pinger_id), 15) % $1,
				pinger_kind,
				0 < hop_count,
				cosign,
				cosign_reason,
				count(*),
				count(*) FILTER (WHERE rtt_ms = 0),
				count(*) FILTER (WHERE $2 < rtt_ms)
			FROM network_ping
			GROUP BY 1, 2, 3, 4, 5, 6`,
		"target hour tally": `
			SELECT hour, target_extender_id, pinger_kind, ping_count, rejection_count
			FROM network_ping_target_hour_tally`,
		"target hour tally from rows": `
			SELECT date_trunc('hour', create_time), target_extender_id, pinger_kind, count(*), count(*) FILTER (WHERE cosign = 2)
			FROM network_ping
			GROUP BY 1, 2, 3`,
		"pinger day": `
			SELECT day, pinger_kind, pinger_id FROM network_ping_pinger_day`,
		"pinger day from rows": `
			SELECT DISTINCT create_time::date, pinger_kind, pinger_id FROM network_ping`,
		"target day": `
			SELECT day, target_extender_id FROM network_ping_target_day`,
		"target day from rows": `
			SELECT DISTINCT create_time::date, target_extender_id FROM network_ping`,
	}
	differences := map[string]int{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, tally := range []string{"hour tally", "target hour tally", "pinger day", "target day"} {
			tallyQuery := tallyQueries[tally]
			rowsQuery := tallyQueries[tally+" from rows"]
			arguments := []any{}
			if tally == "hour tally" {
				arguments = append(arguments, settings.HourShardCount, settings.HalfPlanetRttMs)
			}
			var differenceCount int
			server.Raise(conn.QueryRow(
				ctx,
				fmt.Sprintf(`
					SELECT count(*) FROM (
						(%[1]s EXCEPT %[2]s)
						UNION ALL
						(%[2]s EXCEPT %[1]s)
					) AS difference
				`, tallyQuery, rowsQuery),
				arguments...,
			).Scan(&differenceCount))
			differences[tally] = differenceCount
		}
	})
	return differences
}

// Pings across three days and their hours -- both pinger kinds, every
// verdict, refusal reasons including the rate limit, direct and relayed, round
// trips of zero and beyond half the planet, several pingers and targets --
// stored through both paths, with a same-instant copy and a replay among them
// that store nothing.
func testNetworkPingTallySeed(t testing.TB, ctx context.Context, day time.Time) (pings []*NetworkPing) {
	t.Helper()
	pingerIds := []server.Id{server.NewId(), server.NewId(), server.NewId()}
	targetIds := []server.Id{server.NewId(), server.NewId(), server.NewId(), server.NewId()}
	createTimes := []time.Time{
		day.Add(-2 * time.Hour),
		day.Add(-time.Microsecond),
		day,
		day.Add(30 * time.Minute),
		day.Add(13*time.Hour + 20*time.Minute),
		day.AddDate(0, 0, 1).Add(time.Hour),
	}
	rttMss := []int{0, 5, 150, 250, 400}
	reasons := []int{
		int(connect.ExtenderProbeVerdictReasonRttBelowObserved),
		int(connect.ExtenderProbeVerdictReasonRateLimited),
	}
	for i := 0; i < 60; i += 1 {
		cosign := NetworkPingCosigns[i%len(NetworkPingCosigns)]
		ping := testNetworkPing(
			NetworkPingPingerKinds[i%len(NetworkPingPingerKinds)],
			pingerIds[i%len(pingerIds)],
			targetIds[(i/3)%len(targetIds)],
			cosign,
			rttMss[i%len(rttMss)],
			createTimes[i%len(createTimes)],
		)
		if cosign == NetworkPingCosignRejected {
			ping.CosignReason = reasons[(i/3)%len(reasons)]
		}
		ping.HopCount = i % 3 % 2
		pings = append(pings, ping)
	}
	connect.AssertEqual(t, AddNetworkPings(ctx, pings[:30]), 30)
	// a copy stored at the same instant is not a second row
	sameInstant := *pings[0]
	sameInstant.PingId = server.Id{}
	connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{&sameInstant}), 0)
	connect.AssertEqual(t, AddReportedNetworkPings(ctx, pings[30:], day.AddDate(0, 0, -3)), 30)
	// nor is a replay at another instant
	replay := *pings[40]
	replay.PingId = server.Id{}
	replay.CreateTime = replay.CreateTime.Add(time.Hour)
	connect.AssertEqual(t, AddReportedNetworkPings(ctx, []*NetworkPing{&replay}, day.AddDate(0, 0, -3)), 0)
	return pings
}

// Every tally equals the count over the rows: the fleet's hour tally by hour,
// shard, kind, relay, verdict and reason with its round trips, the per-target
// hour tally with its refusals, and the distinct pingers and targets of each
// day. The dashboard's reads of them equal the counts over the rows of their
// window: the day's outcomes, the distincts over the days covering it, and
// each hour's per-target counts.
func TestNetworkPingTalliesMatchTheRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testNetworkPingDay()
		testNetworkPingTallySeed(t, ctx, day)
		for tally, differenceCount := range testNetworkPingTallyDifferences(t, ctx) {
			if differenceCount != 0 {
				t.Errorf("the %s differs from the rows in %d rows", tally, differenceCount)
			}
		}
		// the batch insert stores a missing co-signature as null, not empty
		var storedCosignatureCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM network_ping WHERE cosign <> $1 AND cosignature IS NOT NULL
			`, NetworkPingCosignCosigned).Scan(&storedCosignatureCount))
		})
		connect.AssertEqual(t, storedCosignatureCount, 0)

		now := day.Add(13*time.Hour + 45*time.Minute)
		windowStart := now.Truncate(time.Hour).Add(-23 * time.Hour)
		count := CountExtenderPings(ctx, now)
		// the counts over the rows of the same window
		type outcomeKey struct {
			pingerKind int
			cosign     int
			relayed    bool
		}
		wantOutcomeKeyPings := map[outcomeKey]int64{}
		var wantPings int64
		wantKindSources := map[int]int64{}
		var wantTargets int64
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `
				SELECT pinger_kind, cosign, 0 < hop_count, count(*)
				FROM network_ping
				WHERE $1 <= create_time
				GROUP BY 1, 2, 3
			`, windowStart)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var key outcomeKey
					var pings int64
					server.Raise(result.Scan(&key.pingerKind, &key.cosign, &key.relayed, &pings))
					wantOutcomeKeyPings[key] = pings
					wantPings += pings
				}
			})
			result, err = conn.Query(ctx, `
				SELECT pinger_kind, count(DISTINCT pinger_id)
				FROM network_ping
				WHERE $1 <= create_time::date AND create_time::date <= $2
				GROUP BY 1
			`, networkPingPartitionDay(windowStart), networkPingPartitionDay(now))
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var pingerKind int
					var sources int64
					server.Raise(result.Scan(&pingerKind, &sources))
					wantKindSources[pingerKind] = sources
				}
			})
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(DISTINCT target_extender_id)
				FROM network_ping
				WHERE $1 <= create_time::date AND create_time::date <= $2
			`, networkPingPartitionDay(windowStart), networkPingPartitionDay(now)).Scan(&wantTargets))
		})
		connect.AssertEqual(t, count.Pings24h, wantPings)
		if count.Pings24h == 0 {
			t.Fatal("the window holds no ping, so this proves nothing")
		}
		for _, outcome := range count.Outcomes24h {
			connect.AssertEqual(t, outcome.Pings, wantOutcomeKeyPings[outcomeKey{
				pingerKind: outcome.PingerKind,
				cosign:     outcome.Cosign,
				relayed:    outcome.Relayed,
			}])
		}
		for _, source := range count.Sources24h {
			connect.AssertEqual(t, source.Sources, wantKindSources[source.PingerKind])
		}
		connect.AssertEqual(t, count.Targets24h, wantTargets)

		// each hour's per-target counts equal the rows of the hour
		hours := []time.Time{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(ctx, `SELECT DISTINCT date_trunc('hour', create_time) FROM network_ping ORDER BY 1`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var hour time.Time
					server.Raise(result.Scan(&hour))
					hours = append(hours, hour)
				}
			})
		})
		for _, hour := range hours {
			wantCounts := []string{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `
					SELECT target_extender_id, pinger_kind, count(*), count(*) FILTER (WHERE cosign = 2)
					FROM network_ping
					WHERE $1 <= create_time AND create_time < $2
					GROUP BY 1, 2
				`, hour, hour.Add(time.Hour))
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var hourCount ExtenderHourPingCount
						server.Raise(result.Scan(&hourCount.ExtenderId, &hourCount.PingerKind, &hourCount.Pings, &hourCount.Rejections))
						wantCounts = append(wantCounts, fmt.Sprintf("%+v", hourCount))
					}
				})
			})
			gotCounts := []string{}
			for _, hourCount := range CountExtenderPingsByHour(ctx, hour) {
				gotCounts = append(gotCounts, fmt.Sprintf("%+v", hourCount))
			}
			slices.Sort(wantCounts)
			slices.Sort(gotCounts)
			connect.AssertEqual(t, gotCounts, wantCounts)
		}
	})
}

// The sweep keeps the tallies on the ping days: the tables kept by day lose
// the dropped day with network_ping, the fleet's hour tally loses its hours
// before the oldest ping day kept and no others, and what remains still equals
// the rows.
func TestNetworkPingTallyDaysDropWithThePingDays(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testNetworkPingDay()
		testNetworkPingTallySeed(t, ctx, day)
		keepTimeout := 24 * time.Hour
		// the day before `day` held two of the seed's instants
		var droppedDayHourTallyCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM network_ping_hour_tally WHERE hour < $1
			`, day).Scan(&droppedDayHourTallyCount))
		})
		if droppedDayHourTallyCount == 0 {
			t.Fatal("the dropped day has no hour tally, so this proves nothing")
		}

		_, droppedPartitionNames, removedHourTallyCount := MaintainNetworkPingPartitions(
			ctx,
			day.Add(keepTimeout+time.Minute),
			keepTimeout,
			DefaultNetworkPingPartitionSettings(),
		)
		for _, table := range networkPingPartitionedTables {
			if !slices.Contains(droppedPartitionNames, networkPingTablePartitionName(table, day.AddDate(0, 0, -1))) {
				t.Errorf("the sweep kept %s's day before %s", table, day)
			}
		}
		connect.AssertEqual(t, removedHourTallyCount, droppedDayHourTallyCount)
		pingDays := []time.Time{}
		for _, partition := range GetNetworkPingPartitions(ctx) {
			pingDays = append(pingDays, partition.Lower)
		}
		for _, table := range networkPingPartitionedTables {
			tableDays := []time.Time{}
			for _, partition := range GetNetworkPingTablePartitions(ctx, table) {
				tableDays = append(tableDays, partition.Lower)
			}
			connect.AssertEqual(t, tableDays, pingDays)
		}
		var hourTallyBeforeCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `
				SELECT count(*) FROM network_ping_hour_tally WHERE hour < $1
			`, day).Scan(&hourTallyBeforeCount))
		})
		connect.AssertEqual(t, hourTallyBeforeCount, 0)
		for tally, differenceCount := range testNetworkPingTallyDifferences(t, ctx) {
			if differenceCount != 0 {
				t.Errorf("after the sweep the %s differs from the rows in %d rows", tally, differenceCount)
			}
		}
	})
}
