package model

// Per-country blocked state on the real tables (connect/EXTENDER.md R4,
// connect/EXTENDER-UPDATE2.md §1.3): the distinct-reporter threshold and the
// operator's probe are both required, the probe must reach the extender now
// on every family it serves, a report sent while it does not is not recorded,
// and a blocked canary blocks no other record.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Sets one address's latest probe: failures since its last success, and
// when that success was (nil for never).
func testSetExtenderProbe(t testing.TB, ctx context.Context, extenderId server.Id, ipVersion int, failures int, successTime *time.Time) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			UPDATE network_extender_address
			SET consecutive_probe_failures = $3, last_probe_success_time = $4, last_probe_time = $5
			WHERE extender_id = $1 AND ip_version = $2
			`,
			extenderId,
			ipVersion,
			failures,
			successTime,
			server.NowUtc(),
		))
	})
}

func testBlockReportCount(t testing.TB, ctx context.Context, extenderId server.Id) int {
	t.Helper()
	count := 0
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT COUNT(*) FROM network_extender_block_report WHERE extender_id = $1`, extenderId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&count))
			}
		})
	})
	return count
}

// Root cause guarded (§1.3): the rule took a probe success anywhere in the
// last hour as the operator reaching the extender, so the first hour of an
// outage -- or a family that stopped answering -- turned the reports it caused
// into a block for a day. The extender must be reached now, on every active
// address; reports sent while it is not are not recorded; and the threshold
// and probe window still hold.
func TestExtenderBlockedStateNeedsTheProbeToReachTheExtenderNow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		keyHex, extenderId := testGatedReleaseExtender(t, ctx, 1, "", 4, 6)
		keyHexes := []string{keyHex}
		source := NewNetworkExtenderBlockedSource(ctx, map[string]server.Id{keyHex: extenderId}, nil)
		blocked := func(countryCode string) bool {
			t.Helper()
			batch := source.BlockedKeys(keyHexes, countryCode)[keyHex]
			if scalar := source.Blocked(keyHex, countryCode); scalar != batch {
				t.Fatalf("batch and scalar blocked state disagree in %q: %t, %t", countryCode, batch, scalar)
			}
			return batch
		}
		report := func(reporter server.Id) bool {
			t.Helper()
			return RecordNetworkExtenderBlockReport(ctx, extenderId, reporter, " AA ", server.NowUtc())
		}
		now := server.NowUtc()

		// two distinct reporters, one of them twice, are not enough
		first := server.NewId()
		if !report(first) || !report(first) || !report(server.NewId()) {
			t.Fatal("a report against an extender the probe reaches was not recorded")
		}
		if blocked("aa") {
			t.Fatal("two distinct reporters made a block")
		}
		if !report(server.NewId()) {
			t.Fatal("the third report was not recorded")
		}
		if !blocked("aa") || blocked("bb") {
			t.Fatal("three reporters in a country, with the probe reaching the extender, are a block there alone")
		}

		// one family stops answering: an outage on that family, not a block
		testSetExtenderProbe(t, ctx, extenderId, 6, 1, &now)
		if blocked("aa") {
			t.Fatal("an extender failing its probe on one family is blocked")
		}
		if report(server.NewId()) {
			t.Fatal("a report against a failing extender was recorded")
		}
		testSetExtenderProbe(t, ctx, extenderId, 6, 0, &now)
		if !blocked("aa") {
			t.Fatal("the recovered family did not restore the block")
		}

		// the whole extender down
		testSetExtenderProbe(t, ctx, extenderId, 4, 2, &now)
		testSetExtenderProbe(t, ctx, extenderId, 6, 2, &now)
		reportsBefore := testBlockReportCount(t, ctx, extenderId)
		if blocked("aa") || report(server.NewId()) || testBlockReportCount(t, ctx, extenderId) != reportsBefore {
			t.Fatal("an extender that is down was blocked, or recorded a report")
		}

		// an inactive address is not a family the extender serves
		testSetExtenderProbe(t, ctx, extenderId, 4, 0, &now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender_address SET active = false WHERE extender_id = $1 AND ip_version = 6`, extenderId))
		})
		if !blocked("aa") {
			t.Fatal("an inactive failing address withheld the block")
		}

		// the probe window still holds: a success older than it is no probe
		stale := now.Add(-2 * connect.DefaultExtenderBlockedStateSettings().ProbeWindow)
		testSetExtenderProbe(t, ctx, extenderId, 4, 0, &stale)
		if blocked("aa") || report(server.NewId()) {
			t.Fatal("a stale probe success made a block, or let a report in")
		}
		testSetExtenderProbe(t, ctx, extenderId, 4, 0, nil)
		if blocked("aa") || report(server.NewId()) {
			t.Fatal("an address the probe never reached made a block, or let a report in")
		}

		// and with no active address at all there is nothing to block
		testSetExtenderProbe(t, ctx, extenderId, 4, 0, &now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender_address SET active = false WHERE extender_id = $1`, extenderId))
		})
		if blocked("aa") || report(server.NewId()) {
			t.Fatal("an extender with no active address was blocked, or recorded a report")
		}
	})
}

// Root cause guarded (§1.3): canary leak attribution alone must not blacklist
// unrelated records. A gated canary blocked in a country is the only record
// blocked there: a release in that country deals the rest of the canary's
// partition in the epoch's order, and the attribution names the one
// partition that leaked.
func TestExtenderBlockedCanaryBlocksNoOtherRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clock := newTestReleaseClock()
		route := newTestReleaseRoute(ctx, clock, nil)
		canaryKeyHex, canaryId := testGatedReleaseExtender(t, ctx, 0, connect.ExtenderChannelGated, 4)
		for i := 1; i < 5; i += 1 {
			testGatedReleaseExtender(t, ctx, i, "", 4)
		}
		for range connect.DefaultExtenderBlockedStateSettings().ReportThreshold {
			if !RecordNetworkExtenderBlockReport(ctx, canaryId, server.NewId(), "aa", server.NowUtc()) {
				t.Fatal("a report against the probed canary was not recorded")
			}
		}
		keyHexes, extenderIds := route.fleet()
		if !slices.Contains(keyHexes, canaryKeyHex) {
			t.Fatal("the gated canary is not in the gated fleet a release draws from")
		}
		blockedKeys := NewNetworkExtenderBlockedSource(ctx, extenderIds, nil).BlockedKeys(keyHexes, "aa")
		if len(blockedKeys) != 1 || !blockedKeys[canaryKeyHex] {
			t.Fatalf("blocked in the country = %v, want the canary alone", blockedKeys)
		}
		partitionCount := connect.ExtenderPartitionCount(len(keyHexes))
		placements := connect.ExtenderAttributeBlockedCanaries(
			route.secret,
			[]*connect.ExtenderCanary{{KeyHex: canaryKeyHex, Channel: connect.ExtenderChannelGated}},
			[]string{canaryKeyHex},
			partitionCount,
		)
		if len(placements) != 1 || placements[0].Channel != connect.ExtenderChannelGated {
			t.Fatalf("attribution = %v, want the canary's one partition", placements)
		}

		for _, name := range []string{"first", "second", "third", "fourth"} {
			identity := []byte("synthetic-canary-identity-" + name)
			order := testWithoutKey(route.order(identity), canaryKeyHex)
			result, err := route.release(&connect.ExtenderReleaseRequest{
				Identity:           identity,
				IdentityCreateTime: clock.Now().Add(-60 * 24 * time.Hour),
				Vantage:            "198.51.100.0/24",
				CountryCode:        "aa",
			})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(result.KeyHexes, order[:3]) {
				t.Fatalf("%s: released %v beside a blocked canary, want the epoch's order without it %v", name, result.KeyHexes, order[:3])
			}
		}
	})
}

func testWithoutKey(keyHexes []string, removed string) []string {
	kept := []string{}
	for _, keyHex := range keyHexes {
		if keyHex != removed {
			kept = append(kept, keyHex)
		}
	}
	return kept
}
