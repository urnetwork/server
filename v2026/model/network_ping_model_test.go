package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// The ping table (connect/GEOMAP.md §2.6, §5.7): its partitioned shape, the
// key that makes a copy stored at one instant a no-op, and what the derive
// phase and the dashboard read of it. The partitions themselves, the replay
// lookup across them and the growth model are in network_ping_partition_test.go.

// One stored ping of the given kind and verdict, created at createTime, with
// a fresh nonce and placeholder signatures: the model stores what the
// controller verified and checks nothing itself.
func testNetworkPing(
	pingerKind int,
	pingerId server.Id,
	targetExtenderId server.Id,
	cosign int,
	rttMs int,
	createTime time.Time,
) *NetworkPing {
	ping := &NetworkPing{
		PingerKind:       pingerKind,
		PingerId:         pingerId,
		TargetExtenderId: targetExtenderId,
		ProbeNonce:       server.NewId().Bytes(),
		RttMs:            rttMs,
		ProbeTime:        createTime,
		Cosign:           cosign,
		PingerSignature:  []byte("pinger-signature"),
		CreateTime:       createTime,
	}
	switch cosign {
	case NetworkPingCosignCosigned:
		ping.Cosignature = []byte("cosignature")
	case NetworkPingCosignRejected:
		ping.CosignReason = int(connect.ExtenderProbeVerdictReasonRttBelowObserved)
	}
	return ping
}

// On a fresh database the table is partitioned by create_time, with the
// replay key carrying the partition column, the target and pinger read
// indexes, the relay depth column, and no key on ping_id alone, which a
// partitioned table cannot have.
func TestNetworkPingMigrationsApply(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		indexDefinitions := []string{}
		columnNames := []string{}
		var partitionKey string
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT regexp_replace(pg_get_indexdef(index_record.indexrelid), '[[:space:]]+', ' ', 'g')
				FROM pg_index AS index_record
				WHERE index_record.indrelid = to_regclass('public.network_ping')
				ORDER BY 1
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var indexDefinition string
					server.Raise(result.Scan(&indexDefinition))
					indexDefinitions = append(indexDefinitions, indexDefinition)
				}
			})
			result, err = conn.Query(
				ctx,
				`
				SELECT column_name
				FROM information_schema.columns
				WHERE table_schema = 'public' AND table_name = 'network_ping'
				ORDER BY column_name
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var columnName string
					server.Raise(result.Scan(&columnName))
					columnNames = append(columnNames, columnName)
				}
			})
			result, err = conn.Query(
				ctx,
				`SELECT coalesce(pg_get_partkeydef(to_regclass('public.network_ping')), '')`,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&partitionKey))
				}
			})
		})
		connect.AssertEqual(t, partitionKey, "RANGE (create_time)")
		wantIndexDefinitions := []string{
			"CREATE INDEX network_ping_pinger_kind_pinger_id_create_time ON ONLY public.network_ping USING btree (pinger_kind, pinger_id, create_time)",
			"CREATE INDEX network_ping_target_extender_id_create_time ON ONLY public.network_ping USING btree (target_extender_id, create_time)",
			"CREATE UNIQUE INDEX network_ping_target_pinger_nonce ON ONLY public.network_ping USING btree (target_extender_id, pinger_kind, pinger_id, probe_nonce, create_time)",
		}
		if !slices.Equal(indexDefinitions, wantIndexDefinitions) {
			t.Fatalf("network_ping indexes = %v, want %v", indexDefinitions, wantIndexDefinitions)
		}
		for _, wantColumnName := range []string{"cosign", "cosign_reason", "cosignature", "create_time", "hop_count", "ping_id", "pinger_kind", "pinger_signature"} {
			if !slices.Contains(columnNames, wantColumnName) {
				t.Fatalf("network_ping has no %s column: %v", wantColumnName, columnNames)
			}
		}
	})
}

// A ping stored twice at one instant is one row: the unique key catches it,
// and the caller counts it as rejected. The same nonce from another pinger,
// or of another kind, is its own ping.
func TestAddNetworkPingsSkipsAReplay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		targetExtenderId := server.NewId()
		ping := testNetworkPing(NetworkPingPingerKindProvider, server.NewId(), targetExtenderId, NetworkPingCosignCosigned, 30, now)

		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{ping}), 1)
		replay := *ping
		replay.PingId = server.Id{}
		replay.RttMs = 1
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{&replay}), 0)

		otherPinger := *ping
		otherPinger.PingId = server.Id{}
		otherPinger.PingerId = server.NewId()
		otherKind := *ping
		otherKind.PingId = server.Id{}
		otherKind.PingerKind = NetworkPingPingerKindExtender
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{&otherPinger, &otherKind}), 2)

		storedPings := GetNetworkPings(ctx, targetExtenderId, time.Time{})
		connect.AssertEqual(t, len(storedPings), 3)
		for _, storedPing := range storedPings {
			connect.AssertEqual(t, storedPing.RttMs, 30)
			connect.AssertEqual(t, storedPing.Cosign, NetworkPingCosignCosigned)
			connect.AssertEqual(t, string(storedPing.Cosignature), "cosignature")
			connect.AssertEqual(t, string(storedPing.PingerSignature), "pinger-signature")
		}
		connect.AssertEqual(t, AddNetworkPings(ctx, nil), 0)
	})
}

// The solver's terms are the direct co-signed pings of the window and nothing
// else: a refused, an unknown, a relayed or an older ping is not one.
func TestGetNetworkPingTermsStreamsOnlyDirectCosignedPings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// whole milliseconds, which the column stores exactly
		now := server.NowUtc().Truncate(time.Millisecond)
		windowStart := now.Add(-time.Hour)
		providerId := server.NewId()
		extenderId := server.NewId()
		targetExtenderId := server.NewId()

		cosignedByProvider := testNetworkPing(NetworkPingPingerKindProvider, providerId, targetExtenderId, NetworkPingCosignCosigned, 31, now)
		cosignedByExtender := testNetworkPing(NetworkPingPingerKindExtender, extenderId, targetExtenderId, NetworkPingCosignCosigned, 32, now)
		refused := testNetworkPing(NetworkPingPingerKindProvider, providerId, targetExtenderId, NetworkPingCosignRejected, 33, now)
		unknown := testNetworkPing(NetworkPingPingerKindExtender, extenderId, targetExtenderId, NetworkPingCosignUnknown, 34, now)
		relayed := testNetworkPing(NetworkPingPingerKindProvider, providerId, targetExtenderId, NetworkPingCosignCosigned, 35, now)
		relayed.HopCount = 1
		old := testNetworkPing(NetworkPingPingerKindProvider, providerId, targetExtenderId, NetworkPingCosignCosigned, 36, windowStart.Add(-time.Minute))
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{
			cosignedByProvider,
			cosignedByExtender,
			refused,
			unknown,
			relayed,
			old,
		}), 6)

		rttTerms := map[int]*NetworkPingTerm{}
		GetNetworkPingTerms(ctx, windowStart, func(term *NetworkPingTerm) {
			rttTerms[term.RttMs] = term
		})
		connect.AssertEqual(t, len(rttTerms), 2)
		connect.AssertEqual(t, rttTerms[31].PingerKind, NetworkPingPingerKindProvider)
		connect.AssertEqual(t, rttTerms[31].PingerId, providerId)
		connect.AssertEqual(t, rttTerms[31].TargetExtenderId, targetExtenderId)
		connect.AssertEqual(t, rttTerms[31].ProbeTime.UnixMilli(), now.UnixMilli())
		connect.AssertEqual(t, rttTerms[32].PingerKind, NetworkPingPingerKindExtender)
		connect.AssertEqual(t, rttTerms[32].PingerId, extenderId)

		// each callback gets its own value, so a caller may keep them
		keptTerms := []*NetworkPingTerm{}
		GetNetworkPingTerms(ctx, windowStart, func(term *NetworkPingTerm) {
			keptTerms = append(keptTerms, term)
		})
		connect.AssertEqual(t, len(keptTerms), 2)
		if keptTerms[0] == keptTerms[1] {
			t.Fatal("the terms share one value")
		}
	})
}

// The reputation's refusals are the refused pings of the window, relayed ones
// included, with the reason the pinger says the target gave; an unknown ping
// is not a refusal and a co-signed one is not either.
func TestGetNetworkPingRefusalsStreamsOnlyRefusedPings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		windowStart := now.Add(-time.Hour)
		pingerId := server.NewId()
		targetExtenderId := server.NewId()

		refused := testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignRejected, 41, now)
		refused.CosignReason = int(connect.ExtenderProbeVerdictReasonUnknownPinger)
		relayedRefused := testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignRejected, 42, now)
		relayedRefused.HopCount = 2
		oldRefused := testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignRejected, 43, windowStart.Add(-time.Minute))
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{
			refused,
			relayedRefused,
			oldRefused,
			testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignUnknown, 44, now),
			testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, NetworkPingCosignCosigned, 45, now),
		}), 5)

		refusals := []*NetworkPingRefusal{}
		GetNetworkPingRefusals(ctx, windowStart, func(refusal *NetworkPingRefusal) {
			refusals = append(refusals, refusal)
		})
		connect.AssertEqual(t, len(refusals), 2)
		reasons := []int{}
		for _, refusal := range refusals {
			connect.AssertEqual(t, refusal.PingerKind, NetworkPingPingerKindExtender)
			connect.AssertEqual(t, refusal.PingerId, pingerId)
			connect.AssertEqual(t, refusal.TargetExtenderId, targetExtenderId)
			reasons = append(reasons, refusal.Reason)
		}
		slices.Sort(reasons)
		connect.AssertEqual(t, reasons[0], int(connect.ExtenderProbeVerdictReasonRttBelowObserved))
		connect.AssertEqual(t, reasons[1], int(connect.ExtenderProbeVerdictReasonUnknownPinger))
	})
}

// The dashboard window counts every kind, verdict and relay, zero included,
// with distinct pingers per kind and distinct targets; the hourly feed counts
// pings and refusals per target and pinger kind.
func TestCountExtenderPings(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		providerId := server.NewId()
		extenderId := server.NewId()
		targetA := server.NewId()
		targetB := server.NewId()

		relayed := testNetworkPing(NetworkPingPingerKindProvider, providerId, targetB, NetworkPingCosignRejected, 12, now)
		relayed.HopCount = 1
		connect.AssertEqual(t, AddNetworkPings(ctx, []*NetworkPing{
			testNetworkPing(NetworkPingPingerKindProvider, providerId, targetA, NetworkPingCosignCosigned, 10, now),
			testNetworkPing(NetworkPingPingerKindProvider, providerId, targetA, NetworkPingCosignRejected, 11, now),
			relayed,
			testNetworkPing(NetworkPingPingerKindExtender, extenderId, targetA, NetworkPingCosignUnknown, 13, now),
			// outside the window
			testNetworkPing(NetworkPingPingerKindExtender, extenderId, targetB, NetworkPingCosignCosigned, 14, now.Add(-25*time.Hour)),
		}), 5)

		count := CountExtenderPings(ctx, now)
		connect.AssertEqual(t, count.Pings24h, int64(4))
		connect.AssertEqual(t, count.Targets24h, int64(2))
		connect.AssertEqual(t, len(count.Outcomes24h), 12)
		pingedOutcomes := map[ExtenderPingOutcomeCount]bool{}
		for _, outcome := range count.Outcomes24h {
			if 0 < outcome.Pings {
				pingedOutcomes[outcome] = true
			}
		}
		for _, wantOutcome := range []ExtenderPingOutcomeCount{
			{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignCosigned, Relayed: false, Pings: 1},
			{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignRejected, Relayed: false, Pings: 1},
			{PingerKind: NetworkPingPingerKindProvider, Cosign: NetworkPingCosignRejected, Relayed: true, Pings: 1},
			{PingerKind: NetworkPingPingerKindExtender, Cosign: NetworkPingCosignUnknown, Relayed: false, Pings: 1},
		} {
			if !pingedOutcomes[wantOutcome] {
				t.Fatalf("outcomes %v lack %+v", count.Outcomes24h, wantOutcome)
			}
		}
		connect.AssertEqual(t, len(pingedOutcomes), 4)
		for _, source := range count.Sources24h {
			connect.AssertEqual(t, source.Sources, int64(1))
		}

		targetIdHourCounts := map[server.Id][]ExtenderHourPingCount{}
		for _, hourCount := range CountExtenderPingsByHour(ctx, now.Truncate(time.Hour)) {
			targetIdHourCounts[hourCount.ExtenderId] = append(targetIdHourCounts[hourCount.ExtenderId], hourCount)
		}
		connect.AssertEqual(t, len(targetIdHourCounts[targetA]), 2)
		for _, hourCount := range targetIdHourCounts[targetA] {
			switch hourCount.PingerKind {
			case NetworkPingPingerKindProvider:
				connect.AssertEqual(t, hourCount.Pings, int64(2))
				connect.AssertEqual(t, hourCount.Rejections, int64(1))
			case NetworkPingPingerKindExtender:
				connect.AssertEqual(t, hourCount.Pings, int64(1))
				connect.AssertEqual(t, hourCount.Rejections, int64(0))
			}
		}
		connect.AssertEqual(t, len(targetIdHourCounts[targetB]), 1)
		connect.AssertEqual(t, targetIdHourCounts[targetB][0].Rejections, int64(1))
	})
}
