package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// The per-target hour tally counts, beside every refusal, the refusals that
// are evidence against the target (the extender ping review of 2026-11-11).
// From 2026-11-07 stale directories refused most extender pingers as unknown,
// and the ranking of refusing targets, which had only every refusal to read,
// listed extenders whose only fault was that. An unknown pinger and a rate
// limit are evidence against no one; a refusal with no reason, an acceptance
// whose co-signature did not verify, is evidence against the target. A row a
// release from before the count created reads null, and stays null as the
// newer release adds to it: its hour's evidence is unknown, never the part of
// it counted since.
func TestNetworkPingTargetHourTallyCountsTheEvidenceAgainstTheTarget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		hour := testNetworkPingDay().Add(5 * time.Hour)
		createTime := hour.Add(10 * time.Minute)
		pingerId := server.NewId()
		target := server.NewId()
		staleDirectory := server.NewId()
		storedBefore := server.NewId()

		ping := func(targetExtenderId server.Id, cosign int, reason uint32) *NetworkPing {
			ping := testNetworkPing(NetworkPingPingerKindExtender, pingerId, targetExtenderId, cosign, 40, createTime)
			if cosign == NetworkPingCosignRejected {
				ping.CosignReason = int(reason)
			}
			return ping
		}
		refusals := func(targetExtenderId server.Id, reason uint32, count int) []*NetworkPing {
			pings := []*NetworkPing{}
			for range count {
				pings = append(pings, ping(targetExtenderId, NetworkPingCosignRejected, reason))
			}
			return pings
		}
		// the target: two co-signed, a refusal with no reason and one for a
		// round trip below the observed one, three unknown pingers and a rate
		// limit, and one ping with no verdict
		pings := []*NetworkPing{
			ping(target, NetworkPingCosignCosigned, 0),
			ping(target, NetworkPingCosignCosigned, 0),
			ping(target, NetworkPingCosignUnknown, 0),
		}
		pings = append(pings, refusals(target, connect.ExtenderProbeVerdictReasonOk, 1)...)
		pings = append(pings, refusals(target, connect.ExtenderProbeVerdictReasonRttBelowObserved, 1)...)
		pings = append(pings, refusals(target, connect.ExtenderProbeVerdictReasonUnknownPinger, 3)...)
		pings = append(pings, refusals(target, connect.ExtenderProbeVerdictReasonRateLimited, 1)...)
		// a stale directory, refusing every pinger as unknown
		pings = append(pings, refusals(staleDirectory, connect.ExtenderProbeVerdictReasonUnknownPinger, 5)...)
		connect.AssertEqual(t, AddNetworkPings(ctx, pings), len(pings))

		// pings, refusals and evidence refusals per target, nil for an
		// unknown evidence count
		type targetCounts struct {
			pings              int64
			rejections         int64
			evidenceRejections *int64
		}
		read := func() map[server.Id]targetCounts {
			counts := map[server.Id]targetCounts{}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(
					ctx,
					`
					SELECT target_extender_id, ping_count, rejection_count, evidence_rejection_count
					FROM network_ping_target_hour_tally
					WHERE hour = $1
					`,
					hour,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var targetExtenderId server.Id
						var count targetCounts
						server.Raise(result.Scan(&targetExtenderId, &count.pings, &count.rejections, &count.evidenceRejections))
						counts[targetExtenderId] = count
					}
				})
			})
			return counts
		}
		requireCounts := func(counts map[server.Id]targetCounts, targetExtenderId server.Id, pings int64, rejections int64, evidenceRejections *int64) {
			t.Helper()
			count, ok := counts[targetExtenderId]
			if !ok {
				t.Fatalf("no tally row for %s", targetExtenderId)
			}
			connect.AssertEqual(t, count.pings, pings)
			connect.AssertEqual(t, count.rejections, rejections)
			if (count.evidenceRejections == nil) != (evidenceRejections == nil) ||
				(count.evidenceRejections != nil && *count.evidenceRejections != *evidenceRejections) {
				t.Fatalf("%s has evidence refusals %v, want %v", targetExtenderId, count.evidenceRejections, evidenceRejections)
			}
		}
		evidence := func(count int64) *int64 {
			return &count
		}
		counts := read()
		requireCounts(counts, target, 9, 6, evidence(2))
		requireCounts(counts, staleDirectory, 5, 5, evidence(0))

		// the row of a release from before the count, at the same hour: its
		// upsert names only the columns it knows
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				INSERT INTO network_ping_target_hour_tally (
					hour,
					target_extender_id,
					pinger_kind,
					ping_count,
					rejection_count
				)
				VALUES ($1, $2, $3, 3, 1)
				ON CONFLICT (hour, target_extender_id, pinger_kind) DO UPDATE SET
					ping_count = network_ping_target_hour_tally.ping_count + excluded.ping_count,
					rejection_count = network_ping_target_hour_tally.rejection_count + excluded.rejection_count
				`,
				hour,
				storedBefore,
				NetworkPingPingerKindExtender,
			))
		})
		// a later store adds to both rows: a bad signature against the target,
		// and a round trip below the observed one against the older row
		pings = refusals(target, connect.ExtenderProbeVerdictReasonBadSignature, 1)
		pings = append(pings, refusals(storedBefore, connect.ExtenderProbeVerdictReasonRttBelowObserved, 1)...)
		connect.AssertEqual(t, AddNetworkPings(ctx, pings), len(pings))
		counts = read()
		requireCounts(counts, target, 10, 7, evidence(3))
		requireCounts(counts, staleDirectory, 5, 5, evidence(0))
		requireCounts(counts, storedBefore, 4, 2, nil)
	})
}
