// Quota-full renewals carry a stable per-provider offset (owner decision
// 2026-10-14), so a cohort whose measurements aligned no longer comes due as
// one wave every four hours. The pure tests drive the renewal rule through a
// deterministic per-provider trace; the DB tests check the SQL against the same
// oracle and the census/admission boundary.
package model

import (
	"crypto/md5"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Go oracle of providerUrlProbeRenewalOffsetSql: the first 32 bits of md5 over
// the canonical uuid text, in whole seconds below the jitter bound.
func testingUrlProbeRenewalOffset(clientId server.Id) time.Duration {
	sum := md5.Sum([]byte(clientId.String()))
	return time.Duration(binary.BigEndian.Uint32(sum[:4])%uint32(ProviderUrlProbeRenewalJitter/time.Second)) * time.Second
}

// Go oracle of providerUrlProbeRenewalAtSql.
func testingUrlProbeRenewalAt(clientId server.Id, oldestRunAt time.Time) time.Time {
	return oldestRunAt.Add(ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom + testingUrlProbeRenewalOffset(clientId))
}

// The deadline before the offset existed: the oldest counted run plus 3h54m.
func testingUrlProbeUnjitteredRenewalAt(oldestRunAt time.Time) time.Time {
	return oldestRunAt.Add(ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom)
}

// The identities testingSeedUrlProbeFleet inserts, md5('synthetic-url-provider-'||i)::uuid,
// so pure tests and DB fixtures see the same offsets.
func testingUrlProbeSyntheticClientIds(count int) []server.Id {
	clientIds := make([]server.Id, 0, count)
	for i := 1; i <= count; i++ {
		sum := md5.Sum([]byte(fmt.Sprintf("synthetic-url-provider-%d", i)))
		clientIds = append(clientIds, server.RequireIdFromBytes(sum[:]))
	}
	return clientIds
}

// One admitted renewal: the oldest counted run it replaces, its claim, and its
// accepted measurement.
type testingUrlProbeRenewal struct {
	oldestRunAt time.Time
	claimAt     time.Time
	measuredAt  time.Time
}

// One quota-full provider's renewals under a deadline rule, until the next claim
// would pass until. A renewal is claimed at its deadline, or at the previous
// measurement once that deadline has passed (one live claim per provider), and
// is measured latency later. History is sorted and holds at least ten runs, and
// the returned run times stay sorted.
func testingUrlProbeRenewalTrace(renewalAt func(oldestRunAt time.Time) time.Time, history []time.Time, latency time.Duration, until time.Time) (renewals []testingUrlProbeRenewal, runAts []time.Time) {
	runAts = slices.Clone(history)
	for {
		oldestRunAt := runAts[len(runAts)-ProviderUrlProbeRunTarget]
		claimAt := renewalAt(oldestRunAt)
		if latestRunAt := runAts[len(runAts)-1]; claimAt.Before(latestRunAt) {
			claimAt = latestRunAt
		}
		if claimAt.After(until) {
			return
		}
		renewal := testingUrlProbeRenewal{oldestRunAt: oldestRunAt, claimAt: claimAt, measuredAt: claimAt.Add(latency)}
		renewals = append(renewals, renewal)
		runAts = append(runAts, renewal.measuredAt)
	}
}

// Accepted runs inside the rolling window (at-4h, at] of sorted run times.
func testingUrlProbeRunsInWindow(runAts []time.Time, at time.Time) int {
	upper := sort.Search(len(runAts), func(i int) bool { return runAts[i].After(at) })
	lower := sort.Search(len(runAts), func(i int) bool { return runAts[i].After(at.Add(-ProviderEgressProbeRefreshAge)) })
	return upper - lower
}

// The largest share of times that fall into one wall-clock minute.
func testingUrlProbeMaxMinuteShare(ats []time.Time) float64 {
	minuteCounts := map[time.Time]int{}
	maximum := 0
	for _, at := range ats {
		minute := at.Truncate(time.Minute)
		minuteCounts[minute]++
		maximum = max(maximum, minuteCounts[minute])
	}
	return float64(maximum) / float64(len(ats))
}

// Ten runs 24 minutes apart, all current at start: the even quota geometry.
func testingUrlProbeSpreadHistory(start time.Time) []time.Time {
	runAts := []time.Time{}
	for run := range ProviderUrlProbeRunTarget {
		runAts = append(runAts, start.Add(-ProviderEgressProbeRefreshAge+10*time.Minute+time.Duration(run)*24*time.Minute))
	}
	return runAts
}

// Ten runs at one instant: the worst-case cluster the renewal DB controls use.
func testingUrlProbeClusterHistory(start time.Time) []time.Time {
	runAts := []time.Time{}
	for range ProviderUrlProbeRunTarget {
		runAts = append(runAts, start.Add(-ProviderEgressProbeRefreshAge+ProviderUrlProbeRenewalHeadroom))
	}
	return runAts
}

// Root cause: a cohort that measured together renewed together, because each
// deadline was its oldest run plus 3h54m. With the stable offset the same
// cohort comes due across the offset range at once, and since the offset also
// lengthens each provider's renewal period, the spread grows every cycle and
// the cohort never re-forms its wave.
func TestUrlProbeRenewalJitterSpreadsAlignedCohort(t *testing.T) {
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	until := start.Add(21 * ProviderEgressProbeRefreshAge)
	latency := 25 * time.Second
	clientIds := testingUrlProbeSyntheticClientIds(2048)
	cycles := []int{1, 6, 20}
	// Uniform offsets in [0, 90 s) put about 2/(3n) of the cohort into one minute
	// at its nth renewal cycle.
	cycleMaxShares := map[int]float64{1: 0.7, 6: 0.15, 20: 0.05}
	for _, history := range [][]time.Time{testingUrlProbeSpreadHistory(start), testingUrlProbeClusterHistory(start)} {
		unjittered, _ := testingUrlProbeRenewalTrace(testingUrlProbeUnjitteredRenewalAt, history, latency, until)
		cycleClaimAts := map[int][]time.Time{}
		cycleUnjitteredClaimAts := map[int][]time.Time{}
		for _, clientId := range clientIds {
			renewals, _ := testingUrlProbeRenewalTrace(func(oldestRunAt time.Time) time.Time {
				return testingUrlProbeRenewalAt(clientId, oldestRunAt)
			}, history, latency, until)
			for _, cycle := range cycles {
				// The first renewal of each cycle; ten renewals make one cycle.
				index := (cycle - 1) * ProviderUrlProbeRunTarget
				if len(renewals) <= index || len(unjittered) <= index {
					t.Fatalf("trace ended before cycle %d", cycle)
				}
				cycleClaimAts[cycle] = append(cycleClaimAts[cycle], renewals[index].claimAt)
				cycleUnjitteredClaimAts[cycle] = append(cycleUnjitteredClaimAts[cycle], unjittered[index].claimAt)
			}
		}
		for _, cycle := range cycles {
			before := testingUrlProbeMaxMinuteShare(cycleUnjitteredClaimAts[cycle])
			after := testingUrlProbeMaxMinuteShare(cycleClaimAts[cycle])
			t.Logf("cycle=%d unjittered_max_minute_share=%.3f jittered_max_minute_share=%.3f", cycle, before, after)
			if before != 1 {
				t.Fatalf("cycle %d: the unjittered rule no longer reproduces the aligned wave: %.3f", cycle, before)
			}
			if cycleMaxShares[cycle] < after {
				t.Fatalf("cycle %d: an aligned cohort still comes due together: %.3f of it in one minute", cycle, after)
			}
		}
	}
}

// The offset only delays the deadline the quota already needs: every renewal
// replaces a run that expires inside the headroom, never one with ten runs
// surviving it, and no history gets more renewals or a fuller window than the
// unjittered rule would give it. Measured runs arrive at most at today's rate.
func TestUrlProbeRenewalJitterNeverExceedsQuota(t *testing.T) {
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	until := start.Add(60 * ProviderEgressProbeRefreshAge)
	// A rolling window gains a run only when one is measured.
	maximumCoverage := func(runAts []time.Time) int {
		maximum := 0
		for _, runAt := range runAts {
			maximum = max(maximum, testingUrlProbeRunsInWindow(runAts, runAt))
		}
		return maximum
	}
	for _, latency := range []time.Duration{time.Second, 25 * time.Second, 270 * time.Second} {
		for _, history := range [][]time.Time{testingUrlProbeSpreadHistory(start), testingUrlProbeClusterHistory(start)} {
			unjittered, unjitteredRunAts := testingUrlProbeRenewalTrace(testingUrlProbeUnjitteredRenewalAt, history, latency, until)
			unjitteredMaximum := maximumCoverage(unjitteredRunAts)
			for offset := time.Duration(0); offset < ProviderUrlProbeRenewalJitter; offset += time.Second {
				renewals, runAts := testingUrlProbeRenewalTrace(func(oldestRunAt time.Time) time.Time {
					return testingUrlProbeUnjitteredRenewalAt(oldestRunAt).Add(offset)
				}, history, latency, until)
				for _, renewal := range renewals {
					if renewal.claimAt.Before(testingUrlProbeUnjitteredRenewalAt(renewal.oldestRunAt)) {
						t.Fatalf("offset=%s latency=%s: renewal claimed before the quota horizon: %+v", offset, latency, renewal)
					}
					// Runs measured by the claim and still current when the headroom
					// ends; ten of them would make the claim an extra turn.
					measured := sort.Search(len(runAts), func(i int) bool { return runAts[i].After(renewal.claimAt) })
					expired := sort.Search(len(runAts), func(i int) bool {
						return runAts[i].After(renewal.claimAt.Add(ProviderUrlProbeRenewalHeadroom - ProviderEgressProbeRefreshAge))
					})
					if surviving := measured - expired; ProviderUrlProbeRunTarget <= surviving {
						t.Fatalf("offset=%s latency=%s: renewal admitted while %d runs survive the horizon", offset, latency, surviving)
					}
				}
				if len(unjittered) < len(renewals) {
					t.Fatalf("offset=%s latency=%s: %d renewals exceed the unjittered %d", offset, latency, len(renewals), len(unjittered))
				}
				if maximum := maximumCoverage(runAts); unjitteredMaximum < maximum {
					t.Fatalf("offset=%s latency=%s: a rolling window holds %d runs, above the unjittered %d", offset, latency, maximum, unjitteredMaximum)
				}
			}
		}
	}
}

// No offset starves a provider. At the largest offset the default 220-second
// turn plus the 50-second scheduling margin still measure before the oldest run
// expires, and ten simultaneous expiries renewed with 25-second turns stay
// covered. The bound matters: two minutes, or a mid-range offset of a 30-minute
// jitter, loses a run on every cycle.
func TestUrlProbeRenewalJitterKeepsCoverage(t *testing.T) {
	start := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	until := start.Add(30 * ProviderEgressProbeRefreshAge)
	// The fewest runs any rolling window holds while renewals are still being
	// simulated. A window loses a run only at that run's expiry.
	coverage := func(history []time.Time, offset, latency time.Duration) int {
		_, runAts := testingUrlProbeRenewalTrace(func(oldestRunAt time.Time) time.Time {
			return testingUrlProbeUnjitteredRenewalAt(oldestRunAt).Add(offset)
		}, history, latency, until)
		minimum := ProviderUrlProbeRunTarget
		for _, runAt := range runAts {
			if expiresAt := runAt.Add(ProviderEgressProbeRefreshAge); !expiresAt.After(until.Add(-ProviderEgressProbeRefreshAge)) {
				minimum = min(minimum, testingUrlProbeRunsInWindow(runAts, expiresAt))
			}
		}
		return minimum
	}
	for offset := time.Duration(0); offset < ProviderUrlProbeRenewalJitter; offset += time.Second {
		if covered := coverage(testingUrlProbeSpreadHistory(start), offset, 220*time.Second+50*time.Second); covered < ProviderUrlProbeRunTarget {
			t.Fatalf("offset=%s: the default turn and scheduling margin lost coverage (%d runs)", offset, covered)
		}
		if covered := coverage(testingUrlProbeClusterHistory(start), offset, 25*time.Second); covered < ProviderUrlProbeRunTarget {
			t.Fatalf("offset=%s: a ten-run cluster renewed with 25-second turns lost coverage (%d runs)", offset, covered)
		}
	}
	if covered := coverage(testingUrlProbeSpreadHistory(start), ProviderUrlProbeRenewalJitter+30*time.Second, 220*time.Second+50*time.Second); ProviderUrlProbeRunTarget <= covered {
		t.Fatal("a two-minute offset still covers the default turn and margin; the bound is not justified by them")
	}
	if covered := coverage(testingUrlProbeSpreadHistory(start), 15*time.Minute, 25*time.Second); ProviderUrlProbeRunTarget <= covered {
		t.Fatal("a mid-range offset of a 30-minute jitter kept coverage; recheck the range rationale")
	}
	// Every offset the hash can produce stays inside the range covered above.
	for _, clientId := range testingUrlProbeSyntheticClientIds(4096) {
		if offset := testingUrlProbeRenewalOffset(clientId); offset < 0 || ProviderUrlProbeRenewalJitter <= offset {
			t.Fatalf("offset %s outside [0,%s)", offset, ProviderUrlProbeRenewalJitter)
		}
	}
}

// The offset hash spreads providers over the whole range, and every jittered
// deadline precedes the exact four-hour expiry geometry the legacy repair
// matches, so the repair can never take a current renewal deadline for one.
func TestUrlProbeRenewalOffsetIsSpreadAndBelowLegacyGeometry(t *testing.T) {
	if ProviderUrlProbeRenewalHeadroom <= ProviderUrlProbeRenewalJitter {
		t.Fatal("a renewal offset can reach the exact legacy four-hour deadline")
	}
	offsetCounts := map[time.Duration]int{}
	clientIds := testingUrlProbeSyntheticClientIds(4096)
	oldestRunAt := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, clientId := range clientIds {
		offsetCounts[testingUrlProbeRenewalOffset(clientId)]++
		if !testingUrlProbeRenewalAt(clientId, oldestRunAt).Before(oldestRunAt.Add(ProviderEgressProbeRefreshAge)) {
			t.Fatal("a renewal deadline reached its oldest run's expiry")
		}
	}
	values := int(ProviderUrlProbeRenewalJitter / time.Second)
	mean := len(clientIds) / values
	for offset := range values {
		if count := offsetCounts[time.Duration(offset)*time.Second]; count < mean/2 || mean*2 < count {
			t.Fatalf("offset %ds holds %d of %d providers; the hash does not spread the cohort", offset, count, len(clientIds))
		}
	}
}

// The census and admission must agree on dueness, so every site derives the
// quota-full deadline from the one shared expression with its own aliases. The
// unjittered arithmetic that let them drift apart must not remain anywhere.
func TestUrlProbeDuenessSitesShareRenewalDeadline(t *testing.T) {
	pop := server.Config.PushSimpleResource(ProviderEgressProbeResourceName, []byte("url_probe_result_version: 1\n"))
	defer pop()
	currentProviderEgressRules.Store(nil)
	defer currentProviderEgressRules.Store(nil)
	base := fmt.Sprint(int((ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom) / time.Second))
	shared := base + " + " + providerUrlProbeRenewalOffsetSql("")[:len("mod(('x'||substr(md5(")]
	type site struct {
		name      string
		sql       string
		fragments []string
	}
	sites := []site{
		{name: "accepted history writer", sql: providerUrlProbeAcceptedCycleSql(), fragments: []string{
			providerUrlProbeRenewalAtSql("cycle.client_id", "recent.oldest_run_at")}},
		{name: "local-failure completion", sql: providerUrlProbeCompletionCycleSql(), fragments: []string{
			providerUrlProbeMeasuredWorkDueSql("recent", "cycle.client_id", "$3", "$8")}},
	}
	for _, shardCount := range []int{1, 8} {
		for _, priority := range []bool{false, true} {
			sites = append(sites, site{name: fmt.Sprintf("admission shards=%d priority=%v", shardCount, priority),
				sql: providerUrlProbeDueSql(0, shardCount, priority), fragments: []string{
					providerUrlProbeMeasuredWorkDueSql("recent", "candidates.client_id", "$1", "$6"),
					providerUrlProbeRenewalAtSql("measured.client_id", "measured.oldest_run_at")}})
		}
	}
	for _, s := range sites {
		for _, fragment := range s.fragments {
			if !strings.Contains(s.sql, fragment) {
				t.Fatalf("%s does not use the shared renewal deadline %q", s.name, fragment)
			}
		}
		if count := strings.Count(s.sql, base); count == 0 || count != strings.Count(s.sql, shared) {
			t.Fatalf("%s computes a renewal deadline outside the shared offset expression", s.name)
		}
		if strings.Contains(s.sql, "interval '"+base+" seconds'") || strings.Contains(s.sql, "$7 * interval") {
			t.Fatalf("%s retains the unjittered renewal arithmetic", s.name)
		}
	}
	// The census counts the stored deadline that these writers set; it never
	// recomputes renewal dueness from history on its own.
	census := providerUrlProbeFleetSql(SelectedProviderUrlProbePolicyVersion())
	if !strings.Contains(census, "COUNT(*) FILTER (WHERE next_attempt_at <= $1 OR client_id IS NULL)") || strings.Contains(census, base) {
		t.Fatal("the census derives dueness other than from the stored renewal deadline")
	}
}

// The SQL offset is the Go oracle's, for the fixture identities and fresh ids.
func TestUrlProbeRenewalOffsetSqlMatchesGoOracle(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clientIds := testingUrlProbeSyntheticClientIds(64)
		for range 64 {
			clientIds = append(clientIds, server.NewId())
		}
		oldestRunAt := server.NowUtc().Truncate(time.Microsecond)
		server.Db(ctx, func(conn server.PgConn) {
			for _, clientId := range clientIds {
				var seconds int64
				var renewalAt time.Time
				server.Raise(conn.QueryRow(ctx, `SELECT `+providerUrlProbeRenewalOffsetSql("$1::uuid")+`, `+
					providerUrlProbeRenewalAtSql("$1::uuid", "$2::timestamp"), clientId, oldestRunAt).Scan(&seconds, &renewalAt))
				if time.Duration(seconds)*time.Second != testingUrlProbeRenewalOffset(clientId) || !renewalAt.Equal(testingUrlProbeRenewalAt(clientId, oldestRunAt)) {
					t.Fatalf("SQL renewal offset %ds/%s differs from the Go oracle %s for %s", seconds, renewalAt, testingUrlProbeRenewalOffset(clientId), clientId)
				}
			}
		})
	})
}

// A cohort that measured the same ten runs is stored at spread deadlines, and
// at every deadline the census and admission flip together: one microsecond
// before it neither counts nor claims the provider, and at it both do.
func TestUrlProbeRenewalJitterSpreadsAlignedCohortInDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Second)
		startedAt := now.Add(-8 * time.Hour)
		clientIds := testingUrlCompletionClients(t, startedAt, 64)
		oldestRunAt := now.Add(-3 * time.Hour)
		for _, clientId := range clientIds {
			for run := range ProviderUrlProbeRunTarget {
				testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: startedAt,
					MeasuredAt: oldestRunAt.Add(time.Duration(run) * time.Minute), OKCount: run % 2, Total: 1})
			}
		}
		clientIdRenewalAts := map[server.Id]time.Time{}
		renewalAts := []time.Time{}
		for _, clientId := range clientIds {
			cycle := testingReadUrlCompletionCycle(t, ctx, clientId)
			if want := testingUrlProbeRenewalAt(clientId, oldestRunAt); !cycle.next.Equal(want) {
				t.Fatalf("stored renewal deadline %s differs from the oracle %s", cycle.next, want)
			}
			clientIdRenewalAts[clientId] = cycle.next
			renewalAts = append(renewalAts, cycle.next)
		}
		if share := testingUrlProbeMaxMinuteShare(renewalAts); 0.8 < share {
			t.Fatalf("an aligned cohort was stored at one deadline: %.3f of it in one minute", share)
		}
		slices.SortFunc(renewalAts, func(a, b time.Time) int { return a.Compare(b) })
		renewalAts = slices.Compact(renewalAts)
		if len(renewalAts) < 16 {
			t.Fatalf("only %d distinct deadlines for 64 providers", len(renewalAts))
		}
		// Each deadline is visited in order, so every earlier one is claimed.
		claimed := map[server.Id]bool{}
		for _, renewalAt := range renewalAts {
			before := renewalAt.Add(-time.Microsecond)
			if fleet := testingGetUrlProbeFleet(t, ctx, before); fleet.Due != 0 {
				t.Fatalf("census counted a renewal before its deadline: %+v", fleet)
			}
			if early := ClaimProviderUrlProbeDue(ctx, before, len(clientIds), 0, 1); len(early) != 0 {
				t.Fatalf("admission claimed a renewal before its deadline: %+v", early)
			}
			want := 0
			for clientId, clientRenewalAt := range clientIdRenewalAts {
				if !claimed[clientId] && clientRenewalAt.Equal(renewalAt) {
					want++
				}
			}
			if fleet := testingGetUrlProbeFleet(t, ctx, renewalAt); fleet.Due != want {
				t.Fatalf("census due=%d at a deadline, want %d", fleet.Due, want)
			}
			due := ClaimProviderUrlProbeDue(ctx, renewalAt, len(clientIds), 0, 1)
			if len(due) != want {
				t.Fatalf("admission claimed %d at a deadline the census counts %d for", len(due), want)
			}
			for _, provider := range due {
				if !clientIdRenewalAts[provider.ClientId].Equal(renewalAt) || provider.RunsNeeded != 0 {
					t.Fatalf("admission claimed a provider outside this deadline: %+v", provider)
				}
				claimed[provider.ClientId] = true
			}
		}
		if len(claimed) != len(clientIds) {
			t.Fatalf("claimed %d of %d renewals", len(claimed), len(clientIds))
		}
	})
}

// A deadline stored before the offset existed is the unjittered oldest+3h54m.
// Admission at that instant parks it at the jittered deadline instead of
// claiming, and the census then agrees it is not due until that deadline.
func TestUrlProbeRenewalJitterReparksUnjitteredDeadline(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Second)
		startedAt := now.Add(-8 * time.Hour)
		clientIds := testingUrlCompletionClients(t, startedAt, 5)
		clientId := clientIds[0]
		for _, candidate := range clientIds {
			if testingUrlProbeRenewalOffset(clientId) < testingUrlProbeRenewalOffset(candidate) {
				clientId = candidate
			}
		}
		if testingUrlProbeRenewalOffset(clientId) == 0 {
			t.Fatal("fixture has no provider with a renewal offset")
		}
		oldestRunAt := now.Add(-3 * time.Hour)
		for run := range ProviderUrlProbeRunTarget {
			testingSetUrlProbeHealth(ctx, &ProviderEgressHealth{RunId: server.NewId(), ClientId: clientId, CycleStartedAt: startedAt,
				MeasuredAt: oldestRunAt.Add(time.Duration(run) * time.Minute), OKCount: 1, Total: 1})
		}
		unjitteredAt := testingUrlProbeUnjitteredRenewalAt(oldestRunAt)
		renewalAt := testingUrlProbeRenewalAt(clientId, oldestRunAt)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=
				CASE WHEN client_id=$1 THEN $2::timestamp ELSE $2::timestamp+interval '1 day' END`, clientId, unjitteredAt))
		})
		if due := ClaimProviderUrlProbeDue(ctx, unjitteredAt, 5, 0, 1); len(due) != 0 {
			t.Fatalf("an unjittered stored deadline was claimed before the renewal offset: %+v", due)
		}
		if cycle := testingReadUrlCompletionCycle(t, ctx, clientId); !cycle.next.Equal(renewalAt) || cycle.ordinal != 0 {
			t.Fatalf("admission did not park at the jittered deadline: next=%s want=%s ordinal=%d", cycle.next, renewalAt, cycle.ordinal)
		}
		if fleet := testingGetUrlProbeFleet(t, ctx, renewalAt.Add(-time.Microsecond)); fleet.Due != 0 {
			t.Fatalf("census counted the parked renewal before its deadline: %+v", fleet)
		}
		if fleet := testingGetUrlProbeFleet(t, ctx, renewalAt); fleet.Due != 1 {
			t.Fatalf("census missed the renewal at its deadline: %+v", fleet)
		}
		due := ClaimProviderUrlProbeDue(ctx, renewalAt, 5, 0, 1)
		if len(due) != 1 || due[0].ClientId != clientId || due[0].RunsNeeded != 0 {
			t.Fatalf("the jittered deadline did not issue its one renewal: %+v", due)
		}
	})
}

// A local failure retries only work the provider still owes at completion. A
// quota-full provider inside its offset keeps the renewal deadline, rather than
// re-entering the retry the unjittered horizon would have granted it.
func TestUrlProbeRenewalJitterGatesLocalRetry(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Second)
		clientIds := testingUrlCompletionClients(t, now.Add(-8*time.Hour), 5)
		clientId := clientIds[0]
		for _, candidate := range clientIds {
			if testingUrlProbeRenewalOffset(clientId) < testingUrlProbeRenewalOffset(candidate) {
				clientId = candidate
			}
		}
		offset := testingUrlProbeRenewalOffset(clientId)
		if offset < 2*time.Second {
			t.Fatal("fixture has no provider with a usable renewal offset")
		}
		due := testingClaimUrlCompletion(t, ctx, clientId, now)
		// Ten runs whose unjittered horizon has passed at the completion while
		// the jittered deadline has not: oldest + 3h54m < completion < deadline.
		finished := now.Add(time.Second)
		oldestRunAt := finished.Add(-ProviderEgressProbeRefreshAge + ProviderUrlProbeRenewalHeadroom - offset/2)
		server.Tx(ctx, func(tx server.PgTx) {
			for run := range ProviderUrlProbeRunTarget {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_health_history
					(run_id,client_id,measured_at,ok_count,total_count,class_results,tls_authentication_failure,url_probe,url_probe_policy_version)
					VALUES($1,$2,$3,1,1,'{}',false,true,1)`, server.NewId(), clientId, oldestRunAt.Add(time.Duration(run)*time.Second)))
			}
		})
		if !testingUrlProbeUnjitteredRenewalAt(oldestRunAt).Before(finished) || !finished.Before(testingUrlProbeRenewalAt(clientId, oldestRunAt)) {
			t.Fatal("fixture completion is not inside the renewal offset")
		}
		before := testingReadUrlCompletionCycle(t, ctx, clientId)
		testingCompleteUrlClaim(t, ctx, due, finished, "tunnel_failed")
		if after := testingReadUrlCompletionCycle(t, ctx, clientId); !after.next.Equal(before.next) || after.history != ProviderUrlProbeRunTarget {
			t.Fatalf("a local failure inside the renewal offset reopened the provider: before=%s after=%s", before.next, after.next)
		}
	})
}
