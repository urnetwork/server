package model

// Per-country blocked state on the real tables (connect/EXTENDER.md R4,
// connect/EXTENDER-UPDATE2.md §1.3): the distinct-reporter threshold and the
// operator's probe are both required, the probe must reach the extender now
// on every family it serves, a report sent while it does not is not recorded,
// a failed probe sets aside the reports made before it, and a blocked canary
// blocks no other record.

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
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
		first := Testing_CreateBlockReportClient(ctx, server.NewId())
		if !report(first) || !report(first) || !report(Testing_CreateBlockReportClient(ctx, server.NewId())) {
			t.Fatal("a report against an extender the probe reaches was not recorded")
		}
		if blocked("aa") {
			t.Fatal("two distinct reporters made a block")
		}
		if !report(Testing_CreateBlockReportClient(ctx, server.NewId())) {
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
		if report(Testing_CreateBlockReportClient(ctx, server.NewId())) {
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
		if blocked("aa") || report(Testing_CreateBlockReportClient(ctx, server.NewId())) || testBlockReportCount(t, ctx, extenderId) != reportsBefore {
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
		if blocked("aa") || report(Testing_CreateBlockReportClient(ctx, server.NewId())) {
			t.Fatal("a stale probe success made a block, or let a report in")
		}
		testSetExtenderProbe(t, ctx, extenderId, 4, 0, nil)
		if blocked("aa") || report(Testing_CreateBlockReportClient(ctx, server.NewId())) {
			t.Fatal("an address the probe never reached made a block, or let a report in")
		}

		// and with no active address at all there is nothing to block
		testSetExtenderProbe(t, ctx, extenderId, 4, 0, &now)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_extender_address SET active = false WHERE extender_id = $1`, extenderId))
		})
		if blocked("aa") || report(Testing_CreateBlockReportClient(ctx, server.NewId())) {
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
			if !RecordNetworkExtenderBlockReport(ctx, canaryId, Testing_CreateBlockReportClient(ctx, server.NewId()), "aa", server.NowUtc()) {
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

// A probe failure as the probe task records it: the result, then the discount.
func testFailExtenderProbe(t testing.TB, ctx context.Context, extenderId server.Id, ipVersion int) int64 {
	t.Helper()
	RecordNetworkExtenderProbeResult(
		ctx,
		extenderId,
		ipVersion,
		false,
		server.NowUtc(),
		6,
		func(extender *NetworkExtender, issueTime time.Time) ([]byte, error) {
			return nil, fmt.Errorf("one failed probe revokes nothing")
		},
	)
	return DiscountNetworkExtenderBlockReports(ctx, extenderId, server.NowUtc())
}

func testSucceedExtenderProbe(t testing.TB, ctx context.Context, extenderId server.Id, ipVersion int) {
	t.Helper()
	RecordNetworkExtenderProbeResult(ctx, extenderId, ipVersion, true, server.NowUtc(), 6, nil)
}

// Root cause guarded (§1.3): the tables keep only each address's latest probe,
// so the reports sent in an outage's first minutes, before the probe saw it,
// counted again once the extender answered, and an extender that was simply
// down was blocked in their country for the rest of the report window. A
// failed probe on any family sets aside every report made before it, in every
// country, and nothing of another extender or made after it.
func TestExtenderBlockReportsBeforeAFailedProbeNeverCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		keyHex, extenderId := testGatedReleaseExtender(t, ctx, 1, "", 4, 6)
		_, otherId := testGatedReleaseExtender(t, ctx, 2, "", 4)
		source := NewNetworkExtenderBlockedSource(ctx, map[string]server.Id{keyHex: extenderId}, nil)
		threshold := connect.DefaultExtenderBlockedStateSettings().ReportThreshold
		reportAll := func(id server.Id, countryCode string) {
			t.Helper()
			for range threshold {
				if !RecordNetworkExtenderBlockReport(ctx, id, Testing_CreateBlockReportClient(ctx, server.NewId()), countryCode, server.NowUtc()) {
					t.Fatal("a report against an extender the probe reaches was not recorded")
				}
			}
		}
		reportAll(extenderId, "aa")
		reportAll(extenderId, "bb")
		reportAll(otherId, "aa")
		if !source.Blocked(keyHex, "aa") || !source.Blocked(keyHex, "bb") {
			t.Fatal("reports at the threshold, with the probe reaching the extender, are no block")
		}

		// one family fails once, and answers at the next probe
		discounted := testFailExtenderProbe(t, ctx, extenderId, 6)
		testSucceedExtenderProbe(t, ctx, extenderId, 6)
		if source.Blocked(keyHex, "aa") || source.Blocked(keyHex, "bb") {
			t.Fatal("the reports made before an outage made a block after it")
		}
		if discounted != int64(2*threshold) {
			t.Fatalf("the failed probe set aside %d reports, want the %d made before it", discounted, 2*threshold)
		}
		if testBlockReportCount(t, ctx, extenderId) != 0 || testBlockReportCount(t, ctx, otherId) != threshold {
			t.Fatal("the discount touched what it should not, or left what it should not")
		}

		// a report made after the failure is not the failure's to set aside
		failureTime := server.NowUtc()
		if !RecordNetworkExtenderBlockReport(ctx, extenderId, Testing_CreateBlockReportClient(ctx, server.NewId()), "aa", failureTime.Add(time.Second)) {
			t.Fatal("a report after the recovery was not recorded")
		}
		if discounted := DiscountNetworkExtenderBlockReports(ctx, extenderId, failureTime); discounted != 0 {
			t.Fatalf("the discount set aside %d reports made after it", discounted)
		}

		// reports made since the recovery count as before
		reportAll(extenderId, "aa")
		if !source.Blocked(keyHex, "aa") || source.Blocked(keyHex, "bb") {
			t.Fatal("reports after the recovery did not make a block there alone")
		}
	})
}

// Root cause guarded (§1.3): a report reads the probe rows, and a failed probe
// that committed and set aside the reports before that report committed left
// it to count once the extender answered again. The report holds the probe
// rows it read until it commits, so the probe waits for it, and the discount
// after the probe sees it. (A faithful revert reads the rows without the lock:
// the probe does not wait, and the report outlives its outage.)
func TestExtenderBlockReportRacingAFailedProbeIsSetAside(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		keyHex, extenderId := testGatedReleaseExtender(t, ctx, 1, "", 4)
		source := NewNetworkExtenderBlockedSource(ctx, map[string]server.Id{keyHex: extenderId}, nil)
		threshold := connect.DefaultExtenderBlockedStateSettings().ReportThreshold

		checked := make(chan struct{})
		release := make(chan struct{})
		Testing_NetworkExtenderBlockReportBarrier = func() {
			close(checked)
			<-release
		}
		defer func() {
			Testing_NetworkExtenderBlockReportBarrier = nil
		}()
		reported := make(chan bool, 1)
		go func() {
			reported <- RecordNetworkExtenderBlockReport(ctx, extenderId, Testing_CreateBlockReportClient(ctx, server.NewId()), "aa", server.NowUtc())
		}()
		// the report found the extender reached, and has not recorded yet
		<-checked
		Testing_NetworkExtenderBlockReportBarrier = nil

		probed := make(chan int64, 1)
		go func() {
			probed <- testFailExtenderProbe(t, ctx, extenderId, 4)
		}()
		// the failing probe either waits on the rows the report holds, or
		// finishes before the report records
		discounted := int64(-1)
		for waiting := false; !waiting && discounted < 0; {
			select {
			case discounted = <-probed:
				continue
			default:
			}
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `
					SELECT COUNT(*) FROM pg_stat_activity
					WHERE datname = current_database() AND wait_event_type = 'Lock'
				`)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						count := 0
						server.Raise(result.Scan(&count))
						waiting = 0 < count
					}
				})
			})
			if !waiting {
				time.Sleep(10 * time.Millisecond)
			}
		}
		close(release)
		<-reported
		if discounted < 0 {
			discounted = <-probed
		}
		if discounted != 1 || testBlockReportCount(t, ctx, extenderId) != 0 {
			t.Fatalf("a report racing the failed probe outlived it (set aside %d, %d left)", discounted, testBlockReportCount(t, ctx, extenderId))
		}

		// it answers again: one short of the threshold since is no block
		testSucceedExtenderProbe(t, ctx, extenderId, 4)
		for range threshold - 1 {
			if !RecordNetworkExtenderBlockReport(ctx, extenderId, Testing_CreateBlockReportClient(ctx, server.NewId()), "aa", server.NowUtc()) {
				t.Fatal("a report after the recovery was not recorded")
			}
		}
		if source.Blocked(keyHex, "aa") {
			t.Fatal("a report made before the outage counted after it")
		}
	})
}

// Root cause guarded: the threshold counted distinct client ids, so three devices of
// one account, or behind one firewall that account's devices share, blocked a record
// for a whole country until their reports aged out. A network counts once however
// many of its clients report. The network is the operator's own row for the client
// id the report was authenticated with, and a report that names a client the table
// no longer holds counts for nothing.
func TestExtenderBlockedStateCountsDistinctNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		threshold := connect.DefaultExtenderBlockedStateSettings().ReportThreshold
		if threshold != 3 {
			t.Fatalf("the threshold changed to %d", threshold)
		}
		report := func(extenderId server.Id, clientId server.Id) {
			t.Helper()
			if !RecordNetworkExtenderBlockReport(ctx, extenderId, clientId, "aa", server.NowUtc()) {
				t.Fatal("a report against an extender the probe reaches was not recorded")
			}
		}
		newBlocked := func(keyHex string, extenderId server.Id) func() bool {
			source := NewNetworkExtenderBlockedSource(ctx, map[string]server.Id{keyHex: extenderId}, nil)
			return func() bool {
				t.Helper()
				batch := source.BlockedKeys([]string{keyHex}, "aa")[keyHex]
				if scalar := source.Blocked(keyHex, "aa"); scalar != batch {
					t.Fatalf("batch and scalar blocked state disagree: %t, %t", batch, scalar)
				}
				return batch
			}
		}

		// three clients of one network, however many times, are one reporter
		oneKeyHex, oneId := testGatedReleaseExtender(t, ctx, 1, "", 4)
		oneBlocked := newBlocked(oneKeyHex, oneId)
		networkId := server.NewId()
		for range threshold {
			clientId := Testing_CreateBlockReportClient(ctx, networkId)
			report(oneId, clientId)
			report(oneId, clientId)
		}
		if oneBlocked() {
			t.Fatal("three clients of one network made a block")
		}
		// a second network is two, which is still not enough
		report(oneId, Testing_CreateBlockReportClient(ctx, server.NewId()))
		if oneBlocked() {
			t.Fatal("two networks made a block")
		}
		// and a third is the threshold
		report(oneId, Testing_CreateBlockReportClient(ctx, server.NewId()))
		if !oneBlocked() {
			t.Fatal("three networks made no block")
		}

		// three networks with one client each are a block
		threeKeyHex, threeId := testGatedReleaseExtender(t, ctx, 2, "", 4)
		threeBlocked := newBlocked(threeKeyHex, threeId)
		for range threshold {
			report(threeId, Testing_CreateBlockReportClient(ctx, server.NewId()))
		}
		if !threeBlocked() {
			t.Fatal("three networks made no block")
		}
		// the probe check still applies to what they say
		testSetExtenderProbe(t, ctx, threeId, 4, 1, nil)
		if threeBlocked() {
			t.Fatal("an extender failing its probe is blocked")
		}

		// clients the operator's table does not hold count for nothing, whoever
		// they say they are
		ghostKeyHex, ghostId := testGatedReleaseExtender(t, ctx, 3, "", 4)
		ghostBlocked := newBlocked(ghostKeyHex, ghostId)
		for range threshold {
			report(ghostId, server.NewId())
		}
		if ghostBlocked() {
			t.Fatal("clients the operator does not know made a block")
		}
	})
}
