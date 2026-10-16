package model

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A deterministic model of quota-full URL renewal pacing (task #57, option A).
// One prober turn runs at a time, with a fixed turn time and the work pass's
// claim stop, so a wave that arrives faster than the prober can renew it shows
// up as replacements measured after the run they replace expired. A provider
// below ten current runs recovers at the mature one-minute pace, which is how a
// catch-up cohort clusters in the first place.
type testingSpreadRule func(oldest, latest time.Time) time.Time

func testingSpreadOldRule(oldest, latest time.Time) time.Time {
	return oldest.Add(ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom)
}

// The Go form of providerUrlProbeRenewalAtSql.
func testingSpreadNewRule(oldest, latest time.Time) time.Time {
	horizon := oldest.Add(ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom)
	if spaced := latest.Add(ProviderUrlProbeRenewalSpacing); spaced.Before(horizon) {
		return spaced
	}
	return horizon
}

type testingSpreadProvider struct {
	runs     []time.Time
	next     time.Time
	pace     time.Duration
	covered  bool
	coverSet bool
}

// The latest ten runs current at t, oldest first.
func (self *testingSpreadProvider) window(t time.Time) []time.Time {
	current := []time.Time{}
	for _, run := range self.runs {
		if run.After(t.Add(-ProviderEgressProbeRefreshAge)) && !run.After(t) {
			current = append(current, run)
		}
	}
	if len(current) > ProviderUrlProbeRunTarget {
		current = current[len(current)-ProviderUrlProbeRunTarget:]
	}
	return current
}

type testingSpreadCycle struct {
	turns   int
	ageOuts int
	maxGap  time.Duration
	// the most quota-full renewals in any 30 minutes, against the cycle's mean
	peakRatio float64
	// renewals of one provider less than ten minutes after its previous run
	bunched int
	// deadlines outside [latest+spacing, oldest+horizon] that were not the horizon
	outOfBounds int
	early       int
}

type testingSpreadResult struct {
	cycles []testingSpreadCycle
	// largest share of one provider's latest ten inside any hour, at the end
	clusterAtEnd float64
}

// Simulates providers that all lost their runs at start and recover together.
// The prober stops claiming for stop of every pass, like the work pass's drain.
func testingSimulateSpread(rule testingSpreadRule, providers int, turn, pass, stop time.Duration, cycles int) testingSpreadResult {
	start := time.Date(2026, 10, 15, 16, 30, 0, 0, time.UTC)
	horizon := ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom
	cycleLength := ProviderEgressProbeRefreshAge
	fleet := make([]*testingSpreadProvider, providers)
	for i := range fleet {
		// The deficit pace is jittered per provider in [0.9, 1.1) like the SQL.
		fleet[i] = &testingSpreadProvider{next: start, pace: time.Duration(float64(time.Minute) * (0.9 + float64((i*7919)%2001)/10000))}
	}
	result := testingSpreadResult{cycles: make([]testingSpreadCycle, cycles)}
	renewals := make([][]time.Time, cycles)
	end := start.Add(time.Duration(cycles) * cycleLength)
	free := start
	for {
		chosen := -1
		for i, provider := range fleet {
			if chosen < 0 || provider.next.Before(fleet[chosen].next) {
				chosen = i
			}
		}
		at := fleet[chosen].next
		if at.Before(free) {
			at = free
		}
		// No claim during the stop at the end of each pass.
		if offset := at.Sub(start) % pass; pass-stop <= offset {
			at = at.Add(pass - offset)
		}
		if !at.Before(end) {
			break
		}
		provider := fleet[chosen]
		measured := at.Add(turn)
		free = measured
		cycle := min(int(measured.Sub(start)/cycleLength), cycles-1)
		before := provider.window(measured)
		stats := &result.cycles[cycle]
		stats.turns++
		if provider.covered && len(before) < ProviderUrlProbeRunTarget {
			// The run this replacement renews expired before it was measured.
			stats.ageOuts++
			expired := provider.runs[len(provider.runs)-len(before)-1].Add(ProviderEgressProbeRefreshAge)
			stats.maxGap = max(stats.maxGap, measured.Sub(expired))
		}
		if len(before) >= ProviderUrlProbeRunTarget {
			renewals[cycle] = append(renewals[cycle], measured)
			if latest := before[len(before)-1]; measured.Sub(latest) < 10*time.Minute {
				stats.bunched++
			}
		}
		provider.runs = append(provider.runs, measured)
		after := provider.window(measured)
		if len(after) >= ProviderUrlProbeRunTarget {
			provider.covered = true
			oldest, latest := after[0], after[len(after)-1]
			provider.next = rule(oldest, latest)
			if provider.next.After(oldest.Add(horizon)) ||
				(provider.next.Before(latest.Add(ProviderUrlProbeRenewalSpacing)) && !provider.next.Equal(oldest.Add(horizon))) {
				stats.outOfBounds++
			}
			if provider.next.Before(oldest.Add(horizon)) {
				stats.early++
			}
		} else {
			provider.next = measured.Add(provider.pace)
		}
	}
	for cycle, times := range renewals {
		sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
		peak := 0
		for i := range times {
			j := sort.Search(len(times), func(j int) bool { return !times[j].Before(times[i].Add(30 * time.Minute)) })
			peak = max(peak, j-i)
		}
		if len(times) != 0 {
			result.cycles[cycle].peakRatio = float64(peak) / (float64(len(times)) * float64(30*time.Minute) / float64(cycleLength))
		}
	}
	worst := 0
	for _, provider := range fleet {
		window := provider.window(end)
		for i := range window {
			j := sort.Search(len(window), func(j int) bool { return !window[j].Before(window[i].Add(time.Hour)) })
			worst = max(worst, j-i)
		}
	}
	result.clusterAtEnd = float64(worst) / float64(ProviderUrlProbeRunTarget)
	return result
}

func (self testingSpreadResult) String() string {
	lines := []string{}
	for i, cycle := range self.cycles {
		lines = append(lines, fmt.Sprintf("cycle %d: turns=%d renewal_peak_30min/mean=%.2f age_outs=%d max_gap=%s bunched=%d early=%d out_of_bounds=%d",
			i, cycle.turns, cycle.peakRatio, cycle.ageOuts, cycle.maxGap, cycle.bunched, cycle.early, cycle.outOfBounds))
	}
	lines = append(lines, fmt.Sprintf("largest share of one provider's ten runs in one hour at the end: %.1f", self.clusterAtEnd))
	return strings.Join(lines, "\n")
}

// A cohort that recovers together, as after an outage, measures each
// provider's runs close together. Under the oldest-run anchor every later cycle
// repeats that geometry: each provider's runs stay bunched and the cohort's
// renewals stay a wave. With the spacing, renewals move into the gaps that
// follow the bunches during the first renewal cycle, and from the second on
// every provider's runs are spread and the wave is gone. The spread never
// schedules later than the old anchor, so no run expires before its renewal.
func TestUrlProbeRenewalSpreadDissolvesClusteredCohortModel(t *testing.T) {
	const providers = 120
	turn, pass, stop := 2*time.Second, 900*time.Second, 310*time.Second
	old := testingSimulateSpread(testingSpreadOldRule, providers, turn, pass, stop, 6)
	spread := testingSimulateSpread(testingSpreadNewRule, providers, turn, pass, stop, 6)
	t.Logf("oldest-run anchor:\n%s", old)
	t.Logf("spaced renewal:\n%s", spread)

	// The defect: every provider's runs stay bunched, so the cohort's renewals
	// stay a wave, in every cycle.
	for cycle := 1; cycle < len(old.cycles); cycle++ {
		if stats := old.cycles[cycle]; stats.bunched < providers*8 || stats.peakRatio < 3 {
			t.Fatalf("model no longer reproduces the persistent cluster at cycle %d:\n%s", cycle, old)
		}
	}
	if old.clusterAtEnd < 0.8 {
		t.Fatalf("model no longer reproduces the clustered providers:\n%s", old)
	}

	// The fix: after the first renewal cycle the bunches are down at least
	// tenfold and keep shrinking, and the wave is at most half as high. The
	// residue is the model's one prober: a spaced renewal delayed by a queue
	// or a pass stop lands closer to the next renewal, which the horizon, not
	// the spacing, decides.
	for cycle := 2; cycle < len(spread.cycles); cycle++ {
		stats := spread.cycles[cycle]
		if old.cycles[cycle].bunched/10 < stats.bunched || spread.cycles[cycle-1].bunched < stats.bunched {
			t.Errorf("cycle %d still renews %d runs within ten minutes of the provider's previous run (previous cycle %d, oldest-run anchor %d)",
				cycle, stats.bunched, spread.cycles[cycle-1].bunched, old.cycles[cycle].bunched)
		}
		if old.cycles[cycle].peakRatio/2 < stats.peakRatio {
			t.Errorf("cycle %d renewal wave remains: peak/mean %.2f against %.2f", cycle, stats.peakRatio, old.cycles[cycle].peakRatio)
		}
	}
	if 0.4 < spread.clusterAtEnd {
		t.Errorf("a provider's runs stayed clustered: %.1f of ten in one hour", spread.clusterAtEnd)
	}
	for cycle, stats := range spread.cycles {
		if stats.outOfBounds != 0 {
			t.Errorf("cycle %d stored %d deadlines outside [latest+24m, oldest+3h54m]", cycle, stats.outOfBounds)
		}
		if stats.ageOuts != 0 {
			t.Errorf("cycle %d renewed %d runs after they expired", cycle, stats.ageOuts)
		}
	}
	// Early renewals are a one-time shift of each bunch into its gap, at most
	// one cycle's ten runs per provider; once spread, each cycle takes the same
	// turns as before.
	extra := 0
	for cycle := range old.cycles {
		extra += spread.cycles[cycle].turns - old.cycles[cycle].turns
		if 1 <= cycle && spread.cycles[cycle].turns != old.cycles[cycle].turns {
			t.Errorf("cycle %d turns differ once spread: old=%d new=%d", cycle, old.cycles[cycle].turns, spread.cycles[cycle].turns)
		}
	}
	if extra < 0 || providers*ProviderUrlProbeRunTarget < extra {
		t.Errorf("one-time extra turns %d outside [0, ten per provider]", extra)
	}
	t.Logf("one-time extra turns: %d (%.1f per provider)", extra, float64(extra)/providers)
}

// The rule written out against both bounds, at and around each boundary.
func TestUrlProbeRenewalSpreadRuleBounds(t *testing.T) {
	if ProviderUrlProbeRenewalSpacing != 24*time.Minute {
		t.Fatalf("spacing=%s, want RefreshAge/10 = 24m", ProviderUrlProbeRenewalSpacing)
	}
	horizon := ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom
	oldest := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		name   string
		latest time.Time
		want   time.Time
	}{
		// latest+24m before the horizon: the spacing decides.
		{"gap", oldest.Add(time.Hour), oldest.Add(time.Hour + 24*time.Minute)},
		{"spacing one second before horizon", oldest.Add(horizon - 24*time.Minute - time.Second), oldest.Add(horizon - time.Second)},
		{"equal", oldest.Add(horizon - 24*time.Minute), oldest.Add(horizon)},
		// latest+24m after the horizon: the horizon decides, as before.
		{"spacing one second after horizon", oldest.Add(horizon - 24*time.Minute + time.Second), oldest.Add(horizon)},
		{"ten runs within minutes", oldest.Add(3 * time.Minute), oldest.Add(3*time.Minute + 24*time.Minute)},
		// Ten runs evenly spread over the horizon leave no gap to fill.
		{"even", oldest.Add(211 * time.Minute), oldest.Add(horizon)},
	} {
		got := testingSpreadNewRule(oldest, c.latest)
		if !got.Equal(c.want) {
			t.Errorf("%s: deadline %s, want %s", c.name, got.Sub(oldest), c.want.Sub(oldest))
		}
		if got.After(oldest.Add(horizon)) {
			t.Errorf("%s: deadline after the replacement horizon", c.name)
		}
		if got.Before(c.latest.Add(ProviderUrlProbeRenewalSpacing)) && !got.Equal(oldest.Add(horizon)) {
			t.Errorf("%s: deadline earlier than latest+24m without the horizon requiring it", c.name)
		}
	}
	// The SQL is the same expression, with a NULL latest result left to LEAST.
	want := "LEAST(r.oldest_run_at + interval '14040 seconds', c.latest_result_at + interval '1440 seconds')"
	if got := providerUrlProbeRenewalAtSql("r.oldest_run_at", "c.latest_result_at"); got != want {
		t.Fatalf("renewal SQL=%s, want %s", got, want)
	}
}

// The pure model's rule is the SQL the writer, admission and completion run.
func TestUrlProbeRenewalSpreadSqlMatchesModelRule(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		oldest := time.Date(2026, 10, 16, 3, 0, 0, 123000, time.UTC)
		horizon := ProviderEgressProbeRefreshAge - ProviderUrlProbeRenewalHeadroom
		for _, latest := range []time.Time{
			oldest,
			oldest.Add(9 * time.Minute),
			oldest.Add(horizon - ProviderUrlProbeRenewalSpacing - time.Microsecond),
			oldest.Add(horizon - ProviderUrlProbeRenewalSpacing),
			oldest.Add(horizon - ProviderUrlProbeRenewalSpacing + time.Microsecond),
			oldest.Add(216 * time.Minute),
			oldest.Add(ProviderEgressProbeRefreshAge),
		} {
			var got time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT `+providerUrlProbeRenewalAtSql("$1::timestamp", "$2::timestamp"), oldest, latest).Scan(&got))
			})
			if want := testingSpreadNewRule(oldest, latest); !got.Equal(want) {
				t.Fatalf("latest=oldest+%s: SQL deadline %s, model rule %s", latest.Sub(oldest), got, want)
			}
		}
		var got time.Time
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT `+providerUrlProbeRenewalAtSql("$1::timestamp", "NULL::timestamp"), oldest).Scan(&got))
		})
		if !got.Equal(oldest.Add(horizon)) {
			t.Fatalf("NULL latest result: SQL deadline %s, want oldest+3h54m", got)
		}
	})
}
