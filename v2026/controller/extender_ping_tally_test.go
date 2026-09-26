package controller

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
)

// The tallies at the ingest (connect/GEOMAP.md §2.7): what a report stores is
// what it adds, and a report that stores nothing adds nothing.

// Every row of the four ping tallies, as text in a fixed order, so two reads
// compare whole.
func testPingTallySnapshot(t testing.TB, ctx context.Context) []string {
	t.Helper()
	tallyRows := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, query := range []string{
			`SELECT 'hour', hour::text, shard, pinger_kind, relayed, cosign, cosign_reason, ping_count, zero_rtt_count, beyond_half_planet_count FROM network_ping_hour_tally`,
			`SELECT 'target hour', hour::text, target_extender_id::text, pinger_kind, ping_count, rejection_count FROM network_ping_target_hour_tally`,
			`SELECT 'pinger day', day::text, pinger_kind, pinger_id::text FROM network_ping_pinger_day`,
			`SELECT 'target day', day::text, target_extender_id::text FROM network_ping_target_day`,
		} {
			result, err := conn.Query(ctx, query)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					values, err := result.Values()
					server.Raise(err)
					tallyRows = append(tallyRows, fmt.Sprint(values...))
				}
			})
		}
	})
	slices.Sort(tallyRows)
	return tallyRows
}

// A report adds exactly what it stored to the tallies; the same report posted
// again is a replay and adds nothing, a report refused whole adds nothing, and
// claims that do not verify add nothing.
func TestExtenderPingReportLeavesTheTalliesOnAReplayOrRefusal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultExtenderPingReportSettings()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())
		connect.AssertEqual(t, len(testPingTallySnapshot(t, ctx)), 0)

		cosigned := newTestPingAttestation(t, attestor, target.extender.PublicKey, 31, testPingNowMs())
		refused := newTestPingAttestation(t, attestor, target.extender.PublicKey, 32, testPingNowMs())
		args := &ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{
				testPingArgs(cosigned, connect.ExtenderPingCosigned, target.cosign(t, cosigned)),
				testPingArgs(refused, connect.ExtenderPingRejected, &protocol.ExtenderProbeVerdict{
					Reason: connect.ExtenderProbeVerdictReasonRttBelowObserved,
				}),
			},
		}
		result, err := ExtenderPingReport(args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 2)
		stored := testPingTallySnapshot(t, ctx)
		// an hour row, a target hour row, a pinger day and a target day for the
		// two pings, whose verdicts split the hour row in two
		connect.AssertEqual(t, len(stored), 5)

		// the same report again
		result, err = ExtenderPingReport(args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 0)
		connect.AssertEqual(t, result.Rejected, 2)
		connect.AssertEqual(t, testPingTallySnapshot(t, ctx), stored)

		// a report refused whole
		oversized := make([]*ExtenderPingArgs, settings.MaxCount+1)
		for i := range oversized {
			oversized[i] = &ExtenderPingArgs{}
		}
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{Pings: oversized}, clientSession)
		connect.AssertEqual(t, err, nil)
		if result.Error == "" {
			t.Fatal("an oversized report was not refused")
		}
		connect.AssertEqual(t, testPingTallySnapshot(t, ctx), stored)

		// claims that do not verify
		altered := testPingArgs(
			newTestPingAttestation(t, attestor, target.extender.PublicKey, 33, testPingNowMs()),
			connect.ExtenderPingUnknown,
			nil,
		)
		altered.RttMs = 1
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{altered, {}},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 0)
		connect.AssertEqual(t, result.Rejected, 2)
		connect.AssertEqual(t, testPingTallySnapshot(t, ctx), stored)

		// and the report that stores one more adds exactly one more ping
		another := newTestPingAttestation(t, attestor, target.extender.PublicKey, 34, testPingNowMs())
		result, err = ExtenderPingReport(&ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{testPingArgs(another, connect.ExtenderPingCosigned, target.cosign(t, another))},
		}, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Accepted, 1)
		var pingCount int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT sum(ping_count)::bigint FROM network_ping_hour_tally`).Scan(&pingCount))
		})
		connect.AssertEqual(t, pingCount, int64(3))
	})
}
