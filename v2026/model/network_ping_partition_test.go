package model

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The day partitions of network_ping (connect/GEOMAP.md §5.7, D26): the sweep's
// plan, the partitions it keeps ahead and drops behind, where an insert lands,
// the replay lookup across days, and the reads that must not notice the
// partitions at all. The database tests work on synthetic days a month out, so
// a real midnight passing mid-test moves nothing they expect.

// A synthetic day a month from now, at utc midnight.
func testNetworkPingDay() time.Time {
	return networkPingPartitionDay(server.NowUtc().AddDate(0, 1, 0))
}

// The names of `partitions`, in their order.
func testNetworkPingPartitionNames(partitions []*NetworkPingPartition) []string {
	partitionNames := []string{}
	for _, partition := range partitions {
		partitionNames = append(partitionNames, partition.Name)
	}
	return partitionNames
}

// The plan creates the days from today through the days ahead that nothing
// covers, and drops exactly the partitions of this code's naming whose upper
// bound is older than the retention's cut, the cut itself kept. Pure.
func TestNetworkPingPartitionPlan(t *testing.T) {
	settings := DefaultNetworkPingPartitionSettings()
	retention := 24 * time.Hour
	today := time.Date(2031, 3, 10, 0, 0, 0, 0, time.UTC)
	// the partition of the day holding `day`, with the day's bounds
	testNetworkPingDayPartition := func(day time.Time) *NetworkPingPartition {
		day = networkPingPartitionDay(day)
		return &NetworkPingPartition{
			Name:  NetworkPingPartitionName(day),
			Lower: day,
			Upper: day.AddDate(0, 0, 1),
		}
	}
	window := []*NetworkPingPartition{
		testNetworkPingDayPartition(today.AddDate(0, 0, -1)),
		testNetworkPingDayPartition(today),
		testNetworkPingDayPartition(today.AddDate(0, 0, 1)),
		testNetworkPingDayPartition(today.AddDate(0, 0, 2)),
	}
	// one plan and what it must decide
	type planCase struct {
		name           string
		now            time.Time
		partitions     []*NetworkPingPartition
		wantCreateDays []time.Time
		wantDropNames  []string
	}
	cases := []planCase{
		{
			name:           "no partitions",
			now:            today.Add(12 * time.Hour),
			partitions:     nil,
			wantCreateDays: []time.Time{today, today.AddDate(0, 0, 1), today.AddDate(0, 0, 2)},
		},
		{
			name:       "the window the conversion makes",
			now:        today.Add(12 * time.Hour),
			partitions: window,
		},
		{
			name:           "a day later",
			now:            today.AddDate(0, 0, 1).Add(12 * time.Hour),
			partitions:     window,
			wantCreateDays: []time.Time{today.AddDate(0, 0, 3)},
			wantDropNames:  []string{NetworkPingPartitionName(today.AddDate(0, 0, -1))},
		},
		{
			name:           "the cut on an upper bound keeps it",
			now:            today.AddDate(0, 0, 1),
			partitions:     window,
			wantCreateDays: []time.Time{today.AddDate(0, 0, 3)},
		},
		{
			name:           "just past the cut drops it",
			now:            today.AddDate(0, 0, 1).Add(time.Microsecond),
			partitions:     window,
			wantCreateDays: []time.Time{today.AddDate(0, 0, 3)},
			wantDropNames:  []string{NetworkPingPartitionName(today.AddDate(0, 0, -1))},
		},
		{
			name: "another name or no bounds is left alone",
			now:  today.Add(12 * time.Hour),
			partitions: append(slices.Clone(window),
				&NetworkPingPartition{Name: "network_ping_synthetic_other", Lower: today.AddDate(0, 0, -9), Upper: today.AddDate(0, 0, -8)},
				&NetworkPingPartition{Name: "network_ping_p20000101"},
			),
		},
		{
			name: "a day another partition covers is not created",
			now:  today.Add(12 * time.Hour),
			partitions: []*NetworkPingPartition{
				testNetworkPingDayPartition(today),
				{Name: "network_ping_synthetic_two_days", Lower: today.AddDate(0, 0, 1), Upper: today.AddDate(0, 0, 3)},
			},
		},
	}
	for _, c := range cases {
		createDays, dropPartitions := PlanNetworkPingPartitions("network_ping", c.now, retention, c.partitions, settings)
		if !slices.Equal(createDays, c.wantCreateDays) {
			t.Errorf("%s: create %v, want %v", c.name, createDays, c.wantCreateDays)
		}
		if dropNames := testNetworkPingPartitionNames(dropPartitions); !slices.Equal(dropNames, c.wantDropNames) {
			t.Errorf("%s: drop %v, want %v", c.name, dropNames, c.wantDropNames)
		}
	}
}

// Day by day, the pass creates the next day ahead and drops the day whose last
// ping passed the keep timeout, with its rows and nothing else, and keeps
// every tally kept by day on exactly the ping days; a repeat within the hour
// does nothing.
func TestNetworkPingPartitionsCreatedAheadAndDroppedBehind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultNetworkPingPartitionSettings()
		keepTimeout := 24 * time.Hour
		day := testNetworkPingDay()
		now := day.Add(12 * time.Hour)
		// the names among `partitionNames` of the partitions this code makes
		// for `table`
		tablePartitionNames := func(partitionNames []string, table string) []string {
			ownPartitionNames := []string{}
			for _, partitionName := range partitionNames {
				if networkPingOwnPartition(table, partitionName) {
					ownPartitionNames = append(ownPartitionNames, partitionName)
				}
			}
			return ownPartitionNames
		}
		// the names of the partitions of `table` for `days`
		dayPartitionNames := func(table string, days ...time.Time) []string {
			partitionNames := []string{}
			for _, partitionDay := range days {
				partitionNames = append(partitionNames, networkPingTablePartitionName(table, partitionDay))
			}
			return partitionNames
		}

		// a month out, every real partition is past the keep timeout
		tableRealPartitionNames := map[string][]string{}
		for _, table := range networkPingPartitionedTables {
			tableRealPartitionNames[table] = testNetworkPingPartitionNames(GetNetworkPingTablePartitions(ctx, table))
		}
		createdPartitionNames, droppedPartitionNames, _ := MaintainNetworkPingPartitions(ctx, now, keepTimeout, settings)
		for _, table := range networkPingPartitionedTables {
			connect.AssertEqual(t, tablePartitionNames(createdPartitionNames, table), dayPartitionNames(table, day, day.AddDate(0, 0, 1), day.AddDate(0, 0, 2)))
			connect.AssertEqual(t, tablePartitionNames(droppedPartitionNames, table), tableRealPartitionNames[table])
		}

		targetExtenderId := server.NewId()
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{
			testNetworkPing(NetworkPingPingerKindExtender, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 20, now),
			testNetworkPing(NetworkPingPingerKindExtender, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 21, day.AddDate(0, 0, 1).Add(time.Hour)),
		}), 2)

		// a day on, the day is inside the keep timeout still
		createdPartitionNames, droppedPartitionNames, _ = MaintainNetworkPingPartitions(ctx, now.AddDate(0, 0, 1), keepTimeout, settings)
		for _, table := range networkPingPartitionedTables {
			connect.AssertEqual(t, tablePartitionNames(createdPartitionNames, table), dayPartitionNames(table, day.AddDate(0, 0, 3)))
		}
		connect.AssertEqual(t, len(droppedPartitionNames), 0)
		connect.AssertEqual(t, len(GetNetworkPings(ctx, targetExtenderId, time.Time{})), 2)

		// two days on, its last ping is past the keep timeout and it goes whole,
		// in every table
		createdPartitionNames, droppedPartitionNames, _ = MaintainNetworkPingPartitions(ctx, now.AddDate(0, 0, 2), keepTimeout, settings)
		for _, table := range networkPingPartitionedTables {
			connect.AssertEqual(t, tablePartitionNames(createdPartitionNames, table), dayPartitionNames(table, day.AddDate(0, 0, 4)))
			connect.AssertEqual(t, tablePartitionNames(droppedPartitionNames, table), dayPartitionNames(table, day))
		}
		keptPings := GetNetworkPings(ctx, targetExtenderId, time.Time{})
		connect.AssertEqual(t, len(keptPings), 1)
		connect.AssertEqual(t, keptPings[0].RttMs, 21)

		createdPartitionNames, droppedPartitionNames, _ = MaintainNetworkPingPartitions(ctx, now.AddDate(0, 0, 2).Add(time.Hour), keepTimeout, settings)
		connect.AssertEqual(t, len(createdPartitionNames), 0)
		connect.AssertEqual(t, len(droppedPartitionNames), 0)
		for _, table := range networkPingPartitionedTables {
			partitions := GetNetworkPingTablePartitions(ctx, table)
			connect.AssertEqual(t, testNetworkPingPartitionNames(partitions), dayPartitionNames(
				table,
				day.AddDate(0, 0, 1),
				day.AddDate(0, 0, 2),
				day.AddDate(0, 0, 3),
				day.AddDate(0, 0, 4),
			))
			for _, partition := range partitions {
				connect.AssertEqual(t, partition.Upper, partition.Lower.AddDate(0, 0, 1))
			}
		}
	})
}

// A row lands in the partition of its create time's utc day, on either side of
// a boundary, and a row for a day with no partition makes its day's partition
// rather than failing.
func TestNetworkPingInsertLandsInItsDayPartition(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testNetworkPingDay()
		MaintainNetworkPingPartitions(ctx, day, 24*time.Hour, DefaultNetworkPingPartitionSettings())

		targetExtenderId := server.NewId()
		createTimes := []time.Time{
			day.Add(-time.Microsecond),
			day,
			day.Add(12 * time.Hour),
			day.AddDate(0, 0, 1).Add(-time.Microsecond),
			day.AddDate(0, 0, 1),
			// no partition is kept this far ahead
			day.AddDate(0, 0, 9),
		}
		pings := []*NetworkPing{}
		for i, createTime := range createTimes {
			pings = append(pings, testNetworkPing(NetworkPingPingerKindProvider, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 30+i, createTime))
		}
		connect.AssertEqual(t, AddNetworkPings(ctx, pings), len(pings))

		pingIdPartitionNames := map[server.Id]string{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT ping_id, tableoid::regclass::text
				FROM network_ping
				WHERE target_extender_id = $1
				`,
				targetExtenderId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var pingId server.Id
					var partitionName string
					server.Raise(result.Scan(&pingId, &partitionName))
					pingIdPartitionNames[pingId] = partitionName
				}
			})
		})
		for _, storedPing := range GetNetworkPings(ctx, targetExtenderId, time.Time{}) {
			wantPartitionName := NetworkPingPartitionName(storedPing.CreateTime)
			if pingIdPartitionNames[storedPing.PingId] != wantPartitionName {
				t.Errorf("a ping created %s is in %s, want %s", storedPing.CreateTime, pingIdPartitionNames[storedPing.PingId], wantPartitionName)
			}
		}
		connect.AssertEqual(t, len(pingIdPartitionNames), len(pings))
		if !slices.Contains(testNetworkPingPartitionNames(GetNetworkPingPartitions(ctx)), NetworkPingPartitionName(day.AddDate(0, 0, 9))) {
			t.Fatal("the insert did not make the missing day's partition")
		}
	})
}

// A store into a day no partition holds creates the day at once, on the plain
// and the reported path. The database's check violation for the missing day is
// one the transaction counts as transient and retries until its one-minute
// retry window ends, so a store that let it through took that whole minute
// before creating the day; a bound of a third of it tells the two apart by
// construction rather than by timing.
func TestNetworkPingInsertCreatesAMissingDayAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testNetworkPingDay()
		MaintainNetworkPingPartitions(ctx, day, 24*time.Hour, DefaultNetworkPingPartitionSettings())
		targetExtenderId := server.NewId()

		// neither day is kept this far ahead
		plainPing := testNetworkPing(NetworkPingPingerKindProvider, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 70, day.AddDate(0, 0, 8))
		reportedPing := testNetworkPing(NetworkPingPingerKindProvider, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 71, day.AddDate(0, 0, 9))
		for _, store := range []func() int{
			func() int {
				return AddNetworkPings(ctx, []*NetworkPing{plainPing})
			},
			func() int {
				return AddReportedNetworkPings(ctx, []*NetworkPing{reportedPing}, reportedPing.CreateTime.Add(-48*time.Hour))
			},
		} {
			storeStart := time.Now()
			connect.AssertEqual(t, store(), 1)
			if storeTimeout := time.Since(storeStart); 20*time.Second < storeTimeout {
				t.Fatalf("a store into a missing day took %s, the transaction's retry window rather than one creation", storeTimeout)
			}
		}
		partitionNames := testNetworkPingPartitionNames(GetNetworkPingPartitions(ctx))
		for _, missingDay := range []time.Time{plainPing.CreateTime, reportedPing.CreateTime} {
			if !slices.Contains(partitionNames, NetworkPingPartitionName(missingDay)) {
				t.Fatalf("partitions %v lack the created %s", partitionNames, NetworkPingPartitionName(missingDay))
			}
		}
		connect.AssertEqual(t, len(GetNetworkPings(ctx, targetExtenderId, time.Time{})), 2)
	})
}

// The key carries the day, so a nonce posted again on the next day is a new
// row to the key; the ingest's lookup across the replay window rejects it, and
// a repeat inside one batch too.
func TestAddReportedNetworkPingsRejectsAReplayOnTheNextDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testNetworkPingDay()
		MaintainNetworkPingPartitions(ctx, day, 24*time.Hour, DefaultNetworkPingPartitionSettings())
		replayWindow := 48 * time.Hour

		targetExtenderId := server.NewId()
		ping := testNetworkPing(NetworkPingPingerKindExtender, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 40, day.Add(20*time.Hour))
		connect.AssertEqual(t, AddReportedNetworkPings(ctx, []*NetworkPing{ping}, ping.CreateTime.Add(-replayWindow)), 1)

		// the same claim posted the next day, into the next day's partition
		nextDay := *ping
		nextDay.PingId = server.Id{}
		nextDay.CreateTime = day.AddDate(0, 0, 1).Add(3 * time.Hour)
		connect.AssertEqual(t, AddReportedNetworkPings(ctx, []*NetworkPing{&nextDay}, nextDay.CreateTime.Add(-replayWindow)), 0)
		// the key alone would have taken it
		keyOnly := nextDay
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{&keyOnly}), 1)
		connect.AssertEqual(t, len(GetNetworkPings(ctx, targetExtenderId, time.Time{})), 2)

		// a claim twice in one batch is stored once
		twice := testNetworkPing(NetworkPingPingerKindExtender, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 41, day.Add(21*time.Hour))
		twiceAgain := *twice
		twiceAgain.PingId = server.Id{}
		twiceAgain.CreateTime = twice.CreateTime.Add(time.Second)
		connect.AssertEqual(t, AddReportedNetworkPings(ctx, []*NetworkPing{twice, &twiceAgain}, twice.CreateTime.Add(-replayWindow)), 1)

		// past the window a copy is not looked for: the claim's own time window
		// has closed on it by then
		late := *ping
		late.PingId = server.Id{}
		late.CreateTime = keyOnly.CreateTime.Add(replayWindow + time.Hour)
		connect.AssertEqual(t, AddReportedNetworkPings(ctx, []*NetworkPing{&late}, late.CreateTime.Add(-replayWindow)), 1)
	})
}

// Two posts of one claim at once store it once: the second post's lookup
// waits on the first's pinger lock and then sees the first's committed row.
// The first post is held open between its insert and its commit, and the
// second is released only once it waits on the lock; a lookup that did not
// wait, or waited under a snapshot taken before the first committed, stores
// the claim twice.
func TestAddReportedNetworkPingsWaitsForAConcurrentPost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		day := testNetworkPingDay()
		MaintainNetworkPingPartitions(ctx, day, 24*time.Hour, DefaultNetworkPingPartitionSettings())

		targetExtenderId := server.NewId()
		ping := testNetworkPing(NetworkPingPingerKindExtender, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 50, day.Add(time.Hour))
		replayMinCreateTime := day.AddDate(0, 0, -2)

		firstInserted := make(chan struct{})
		firstRelease := make(chan struct{})
		firstDone := make(chan struct{})
		// a failure below must not leave the first post's transaction open
		releaseFirst := sync.OnceFunc(func() {
			close(firstRelease)
		})
		defer releaseFirst()
		go func() {
			defer close(firstDone)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
					networkPingReplayLockKey(ping.PingerKind, ping.PingerId),
				))
				server.RaisePgResult(tx.Exec(
					ctx,
					`
					INSERT INTO network_ping (
						ping_id, pinger_kind, pinger_id, target_extender_id, probe_nonce, rtt_ms,
						probe_time, cosign, pinger_signature, cosignature, create_time
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
					`,
					server.NewId(),
					ping.PingerKind,
					ping.PingerId,
					ping.TargetExtenderId,
					ping.ProbeNonce,
					ping.RttMs,
					ping.ProbeTime,
					ping.Cosign,
					ping.PingerSignature,
					ping.Cosignature,
					ping.CreateTime,
				))
				close(firstInserted)
				<-firstRelease
			})
		}()
		<-firstInserted

		secondInserted := make(chan int, 1)
		second := *ping
		second.CreateTime = ping.CreateTime.Add(time.Second)
		go func() {
			secondInserted <- AddReportedNetworkPings(ctx, []*NetworkPing{&second}, replayMinCreateTime)
		}()

		// the barrier: the second post waits on a lock in this database
		lockWaiting := false
		for i := 0; i < 500 && !lockWaiting; i += 1 {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT count(*)
					FROM pg_locks
					WHERE
						locktype = 'advisory' AND
						NOT granted AND
						database = (SELECT oid FROM pg_database WHERE datname = current_database())
					`,
				)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						var waitingCount int
						server.Raise(result.Scan(&waitingCount))
						lockWaiting = 0 < waitingCount
					}
				})
			})
			if !lockWaiting {
				select {
				case <-secondInserted:
					t.Fatal("the second post stored without waiting for the first")
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if !lockWaiting {
			t.Fatal("the second post never waited on the pinger lock")
		}
		releaseFirst()
		<-firstDone

		connect.AssertEqual(t, <-secondInserted, 0)
		connect.AssertEqual(t, len(GetNetworkPings(ctx, targetExtenderId, time.Time{})), 1)
	})
}

// The derive phase's and the dashboard's reads see a window across a
// partition boundary exactly as they saw one table: the terms, the refusals,
// the relayed attestations, the target's pings in order, the day's counts and
// the hour on either side of midnight.
func TestNetworkPingReadsUnchangedAcrossPartitions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		boundary := testNetworkPingDay()
		MaintainNetworkPingPartitions(ctx, boundary, 24*time.Hour, DefaultNetworkPingPartitionSettings())
		before := boundary.Add(-30 * time.Minute).Truncate(time.Millisecond)
		after := boundary.Add(30 * time.Minute).Truncate(time.Millisecond)

		pingerId := server.NewId()
		targetExtenderId := server.NewId()
		relayed := testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignCosigned, 63, after)
		relayed.HopCount = 1
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{
			testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignCosigned, 60, before),
			testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignCosigned, 61, after),
			testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignRejected, 62, before),
			relayed,
			// outside every window below
			testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignCosigned, 64, boundary.Add(-26*time.Hour)),
		}), 5)
		windowStart := boundary.Add(-time.Hour)

		termRtts := []int{}
		GetNetworkPingTerms(ctx, windowStart, func(term *NetworkPingTerm) {
			termRtts = append(termRtts, term.RttMs)
		})
		slices.Sort(termRtts)
		connect.AssertEqual(t, termRtts, []int{60, 61})

		refusals := []*NetworkPingRefusal{}
		GetNetworkPingRefusals(ctx, windowStart, func(refusal *NetworkPingRefusal) {
			refusals = append(refusals, refusal)
		})
		connect.AssertEqual(t, len(refusals), 1)

		relayedCounts := []NetworkPingAttestationCount{}
		GetNetworkPingRelayedCosignCounts(ctx, windowStart, func(count *NetworkPingAttestationCount) {
			relayedCounts = append(relayedCounts, *count)
		})
		connect.AssertEqual(t, relayedCounts, []NetworkPingAttestationCount{{
			PingerKind:       NetworkPingPingerKindExtender,
			PingerId:         pingerId,
			TargetExtenderId: targetExtenderId,
			Count:            1,
		}})

		// oldest first: the two before the boundary, then the two after it
		storedRtts := []int{}
		for _, storedPing := range GetNetworkPings(ctx, targetExtenderId, windowStart) {
			storedRtts = append(storedRtts, storedPing.RttMs)
		}
		connect.AssertEqual(t, len(storedRtts), 4)
		slices.Sort(storedRtts[:2])
		slices.Sort(storedRtts[2:])
		connect.AssertEqual(t, storedRtts, []int{60, 62, 61, 63})

		count := CountExtenderPings(ctx, boundary.Add(time.Hour))
		connect.AssertEqual(t, count.Pings24h, int64(4))
		connect.AssertEqual(t, count.Targets24h, int64(1))
		for _, source := range count.Sources24h {
			if source.PingerKind == NetworkPingPingerKindExtender {
				connect.AssertEqual(t, source.Sources, int64(1))
			}
		}

		beforeHourCounts := CountExtenderPingsByHour(ctx, boundary.Add(-time.Hour))
		connect.AssertEqual(t, beforeHourCounts, []ExtenderHourPingCount{{
			ExtenderId: targetExtenderId,
			PingerKind: NetworkPingPingerKindExtender,
			Pings:      2,
			Rejections: 1,
		}})
		afterHourCounts := CountExtenderPingsByHour(ctx, boundary)
		connect.AssertEqual(t, afterHourCounts, []ExtenderHourPingCount{{
			ExtenderId: targetExtenderId,
			PingerKind: NetworkPingPingerKindExtender,
			Pings:      2,
			Rejections: 0,
		}})
	})
}
