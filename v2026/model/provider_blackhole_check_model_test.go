// Tests of the blackhole check's consecutive-failure rule (connect/GEOMAP.md
// §11.3): the state machine alone, and the rows, the dark set and the due
// queue it drives in the database.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

// A report at a time after the previous one, as the prober would submit it.
func testingBlackholeReport(clientId server.Id, checkedAt time.Time, ok bool) ProviderBlackholeCheckReport {
	failure := ""
	if !ok {
		failure = "all_destinations_failed"
	}
	return ProviderBlackholeCheckReport{ClientId: clientId, CheckedAt: checkedAt, Ok: ok, Failure: failure}
}

// Two failures and a pass: the run counts up from the first failure, keeps its
// start, and a pass clears it; each schedule is its backoff step from ingest.
func TestNextProviderBlackholeCheckFailFailPass(t *testing.T) {
	rules := DefaultProviderEgressRules()
	clientId := server.NewId()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	first, changed := NextProviderBlackholeCheck(nil, testingBlackholeReport(clientId, start, false), start.Add(time.Minute), rules)
	if !changed || first.ConsecutiveFailures != 1 || first.FirstFailedAt == nil || !first.FirstFailedAt.Equal(start) {
		t.Fatalf("first failure = %+v, want a run of 1 starting at the check", first)
	}
	if first.NextDueAt == nil || !first.NextDueAt.Equal(start.Add(time.Minute).Add(5*time.Minute)) {
		t.Fatalf("first failure due at %v, want the first backoff step from ingest", first.NextDueAt)
	}

	second, _ := NextProviderBlackholeCheck(first, testingBlackholeReport(clientId, start.Add(10*time.Minute), false), start.Add(11*time.Minute), rules)
	if second.ConsecutiveFailures != 2 || !second.FirstFailedAt.Equal(start) {
		t.Fatalf("second failure = %+v, want a run of 2 still starting at the first check", second)
	}
	if !second.NextDueAt.Equal(start.Add(11 * time.Minute).Add(15 * time.Minute)) {
		t.Fatalf("second failure due at %v, want the second backoff step", second.NextDueAt)
	}
	if second.IsDark(start.Add(11*time.Minute), rules) {
		t.Fatal("two failures are dark")
	}

	pass, _ := NextProviderBlackholeCheck(second, testingBlackholeReport(clientId, start.Add(30*time.Minute), true), start.Add(31*time.Minute), rules)
	if pass.ConsecutiveFailures != 0 || pass.FirstFailedAt != nil || pass.Failure != "" || !pass.OK {
		t.Fatalf("pass = %+v, want the run cleared", pass)
	}
	if !pass.NextDueAt.Equal(start.Add(31 * time.Minute).Add(ProviderBlackholeCheckDueAge)) {
		t.Fatalf("pass due at %v, want the ordinary due age", pass.NextDueAt)
	}
}

// Three failures inside the minimum span are a bad half hour, not a verdict;
// the fourth, past the span, is dark.
func TestNextProviderBlackholeCheckThreeFailuresUnderTheSpanAreNotDark(t *testing.T) {
	rules := DefaultProviderEgressRules()
	clientId := server.NewId()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	var row *ProviderBlackholeCheck
	for _, offset := range []time.Duration{0, 5 * time.Minute, 20 * time.Minute} {
		row, _ = NextProviderBlackholeCheck(row, testingBlackholeReport(clientId, start.Add(offset), false), start.Add(offset), rules)
	}
	if row.ConsecutiveFailures != 3 {
		t.Fatalf("run = %d, want 3", row.ConsecutiveFailures)
	}
	if row.IsDark(start.Add(20*time.Minute), rules) {
		t.Fatal("three failures spanning 20 minutes are dark under a 30 minute minimum span")
	}
	row, _ = NextProviderBlackholeCheck(row, testingBlackholeReport(clientId, start.Add(35*time.Minute), false), start.Add(35*time.Minute), rules)
	if !row.IsDark(start.Add(35*time.Minute), rules) {
		t.Fatalf("four failures spanning 35 minutes are not dark: %+v", row)
	}
}

// A check that measured nothing neither counts a failure nor clears one: it
// only moves the schedule, and it never moves the latest measured check.
func TestNextProviderBlackholeCheckNotMeasuredLeavesTheCount(t *testing.T) {
	rules := DefaultProviderEgressRules()
	clientId := server.NewId()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)

	var row *ProviderBlackholeCheck
	for _, offset := range []time.Duration{0, 10 * time.Minute} {
		row, _ = NextProviderBlackholeCheck(row, testingBlackholeReport(clientId, start.Add(offset), false), start.Add(offset), rules)
	}
	notMeasured := ProviderBlackholeCheckReport{
		ClientId:    clientId,
		CheckedAt:   start.Add(30 * time.Minute),
		Ok:          false,
		Failure:     ProviderBlackholeNotMeasuredFailure,
		NotMeasured: true,
	}
	after, changed := NextProviderBlackholeCheck(row, notMeasured, start.Add(40*time.Minute), rules)
	if !changed {
		t.Fatal("a newer unmeasured check changed nothing")
	}
	if after.ConsecutiveFailures != 2 || !after.FirstFailedAt.Equal(start) || !after.CheckedAt.Equal(row.CheckedAt) || after.Failure != row.Failure {
		t.Fatalf("unmeasured check moved the run or the check: before %+v after %+v", row, after)
	}
	if !after.NextDueAt.Equal(start.Add(40 * time.Minute).Add(15 * time.Minute)) {
		t.Fatalf("unmeasured check due at %v, want the backoff step of the run it left", after.NextDueAt)
	}

	fresh, _ := NextProviderBlackholeCheck(nil, ProviderBlackholeCheckReport{
		ClientId:    clientId,
		CheckedAt:   start,
		Ok:          false,
		Failure:     ProviderBlackholeNotMeasuredFailure,
		NotMeasured: true,
	}, start, rules)
	if fresh.ConsecutiveFailures != 0 || fresh.Failure != ProviderBlackholeNotMeasuredFailure || fresh.IsDark(start, rules) {
		t.Fatalf("a first unmeasured check = %+v, want a row that counts nothing", fresh)
	}
	if !fresh.NextDueAt.Equal(start.Add(5 * time.Minute)) {
		t.Fatalf("a first unmeasured check due at %v, want the first backoff step", fresh.NextDueAt)
	}
}

// The backoff is the schedule's step for the run's length, the last step
// repeating, and the first step for a provider with no run.
func TestProviderEgressRulesDarkBackoffSchedule(t *testing.T) {
	rules := DefaultProviderEgressRules()
	cases := []struct {
		consecutiveFailures int
		backoff             time.Duration
	}{
		{consecutiveFailures: 0, backoff: 5 * time.Minute},
		{consecutiveFailures: 1, backoff: 5 * time.Minute},
		{consecutiveFailures: 2, backoff: 15 * time.Minute},
		{consecutiveFailures: 3, backoff: 30 * time.Minute},
		{consecutiveFailures: 9, backoff: 30 * time.Minute},
	}
	for _, c := range cases {
		if backoff := rules.DarkBackoff(c.consecutiveFailures); backoff != c.backoff {
			t.Errorf("backoff after %d failures = %s, want %s", c.consecutiveFailures, backoff, c.backoff)
		}
	}
}

// A replayed or out-of-order report changes nothing, whatever it says.
func TestNextProviderBlackholeCheckIgnoresReplays(t *testing.T) {
	rules := DefaultProviderEgressRules()
	clientId := server.NewId()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	row, _ := NextProviderBlackholeCheck(nil, testingBlackholeReport(clientId, start, false), start, rules)
	for _, report := range []ProviderBlackholeCheckReport{
		testingBlackholeReport(clientId, start, true),
		testingBlackholeReport(clientId, start.Add(-time.Minute), false),
		{ClientId: clientId, CheckedAt: start, Failure: ProviderBlackholeNotMeasuredFailure, NotMeasured: true},
	} {
		if after, changed := NextProviderBlackholeCheck(row, report, start.Add(time.Hour), rules); changed || after != row {
			t.Errorf("report %+v changed the row: %+v", report, after)
		}
	}
}

// A TLS-authentication failure is the one failure that is a verdict at once;
// like every verdict it lapses with the check's max age.
func TestProviderBlackholeCheckTlsFailureIsDarkAtOnce(t *testing.T) {
	rules := DefaultProviderEgressRules()
	start := time.Date(2026, 9, 24, 10, 0, 0, 0, time.UTC)
	row, _ := NextProviderBlackholeCheck(nil, ProviderBlackholeCheckReport{
		ClientId:  server.NewId(),
		CheckedAt: start,
		Ok:        false,
		Failure:   ProviderBlackholeTlsAuthenticationFailure,
	}, start, rules)
	if !row.IsDark(start, rules) {
		t.Fatal("a TLS-authentication failure is not dark at once")
	}
	if row.IsDark(start.Add(ProviderBlackholeCheckMaxAge+time.Minute), rules) {
		t.Fatal("a TLS-authentication failure past the max age is still dark")
	}
}

// A current failing check must override a passing health measurement.
//
// This is the whole reason the check exists. Egress health sweeps the fleet
// over hours to days, so a provider that goes dark keeps its passing tally --
// and its place in the public list -- until the next sweep reaches it. The
// check closes that window, once its failures are a verdict.
func TestBlackholedProviderFailsTheHealthGate(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		healthy := server.NewId()
		blackholed := server.NewId()

		for _, clientId := range []server.Id{healthy, blackholed} {
			SetProviderEgressHealth(ctx, &ProviderEgressHealth{
				ClientId:   clientId,
				MeasuredAt: now.Add(-time.Hour),
				OKCount:    100, Total: 100,
			})
		}

		Testing_SetProviderBlackholed(ctx, blackholed, now.Add(-time.Minute))
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: healthy, CheckedAt: now.Add(-time.Minute), OK: true,
		})

		f := newProviderCountFilter(ctx, true)

		if !f.passesHealth(healthy) {
			t.Errorf("a provider measured healthy and checked ok must pass the gate")
		}
		if f.passesHealth(blackholed) {
			t.Errorf("a provider whose current checks say nothing got through must NOT pass the gate, " +
				"even with a passing health measurement -- that combination is exactly a provider that went dark since it was last swept")
		}
	})
}

// A verdict that has aged out must not be read as "blackholed".
//
// The signal can only ever remove providers, so when its evidence lapses the
// provider falls back to being judged on egress health alone. Treating "not
// checked recently" as "dark" would empty the list the moment the sweep
// stalled.
func TestStaleBlackholeCheckDoesNotExclude(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		clientId := server.NewId()
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   clientId,
			MeasuredAt: now.Add(-time.Hour),
			OKCount:    100, Total: 100,
		})
		Testing_SetProviderBlackholed(ctx, clientId, now.Add(-ProviderBlackholeCheckMaxAge-time.Minute))

		if !newProviderCountFilter(ctx, true).passesHealth(clientId) {
			t.Errorf("a dark verdict older than %s must not keep excluding the provider: "+
				"a stalled sweep would otherwise drain the list", ProviderBlackholeCheckMaxAge)
		}
	})
}

// The upsert is monotonic: an out-of-order or replayed row must not move a
// provider's last-checked time backwards, which would hand it straight back to
// the sweep and, worse, could resurrect a stale verdict over a current one.
func TestSetProviderBlackholeCheckIsMonotonic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		clientId := server.NewId()

		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: clientId, CheckedAt: now.Add(-time.Minute), OK: true,
		})
		// an older report arriving late
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: clientId, CheckedAt: now.Add(-time.Hour),
			OK: false, Failure: "tunnel_failed",
		})

		c := GetProviderBlackholeCheck(ctx, clientId)
		connect.AssertNotEqual(t, c, nil)
		if !c.OK {
			t.Errorf("a report older than the stored one overwrote it: ok=%v failure=%q checked_at=%s",
				c.OK, c.Failure, c.CheckedAt)
		}
	})
}

// Ingest keeps the run on the row: each failure counts up from the first,
// schedules its backoff step, and a pass clears it.
func TestRecordProviderBlackholeChecksCarriesTheCount(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rules := DefaultProviderEgressRules()
		clientId := server.NewId()
		// stored timestamps keep microseconds
		start := server.NowUtc().Add(-time.Hour).Truncate(time.Microsecond)

		RecordProviderBlackholeChecks(ctx, []ProviderBlackholeCheckReport{
			testingBlackholeReport(clientId, start, false),
		}, rules)
		row := GetProviderBlackholeCheck(ctx, clientId)
		if row == nil || row.ConsecutiveFailures != 1 || row.FirstFailedAt == nil || !row.FirstFailedAt.Equal(start) {
			t.Fatalf("first failure stored as %+v", row)
		}
		if row.NextDueAt == nil || row.NextDueAt.Before(server.NowUtc().Add(4*time.Minute)) {
			t.Fatalf("first failure due at %v, want the first backoff step out", row.NextDueAt)
		}

		// a batch that names the provider twice is two checks in order
		RecordProviderBlackholeChecks(ctx, []ProviderBlackholeCheckReport{
			testingBlackholeReport(clientId, start.Add(10*time.Minute), false),
			testingBlackholeReport(clientId, start.Add(40*time.Minute), false),
		}, rules)
		row = GetProviderBlackholeCheck(ctx, clientId)
		if row.ConsecutiveFailures != 3 || !row.FirstFailedAt.Equal(start) {
			t.Fatalf("three failures stored as %+v", row)
		}
		if !row.IsDark(server.NowUtc(), rules) || !GetAllProviderBlackholedClientIds(ctx)[clientId] {
			t.Fatal("three failures spanning 40 minutes are not in the dark set")
		}

		RecordProviderBlackholeChecks(ctx, []ProviderBlackholeCheckReport{
			testingBlackholeReport(clientId, start.Add(50*time.Minute), true),
		}, rules)
		row = GetProviderBlackholeCheck(ctx, clientId)
		if row.ConsecutiveFailures != 0 || row.FirstFailedAt != nil || !row.OK {
			t.Fatalf("pass stored as %+v, want the run cleared", row)
		}
		if GetAllProviderBlackholedClientIds(ctx)[clientId] {
			t.Fatal("a passing check did not lift the verdict")
		}
	})
}

// A check that measured nothing, reaching a provider with a run of failures,
// moves only its schedule.
func TestRecordProviderBlackholeChecksNotMeasuredOnlyReschedules(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rules := DefaultProviderEgressRules()
		clientId := server.NewId()
		start := server.NowUtc().Add(-time.Hour).Truncate(time.Microsecond)

		RecordProviderBlackholeChecks(ctx, []ProviderBlackholeCheckReport{
			testingBlackholeReport(clientId, start, false),
			testingBlackholeReport(clientId, start.Add(10*time.Minute), false),
		}, rules)
		before := GetProviderBlackholeCheck(ctx, clientId)
		RecordProviderBlackholeChecks(ctx, []ProviderBlackholeCheckReport{{
			ClientId:    clientId,
			CheckedAt:   start.Add(30 * time.Minute),
			Ok:          false,
			Failure:     ProviderBlackholeNotMeasuredFailure,
			NotMeasured: true,
		}}, rules)
		after := GetProviderBlackholeCheck(ctx, clientId)
		if after.ConsecutiveFailures != before.ConsecutiveFailures || !after.CheckedAt.Equal(before.CheckedAt) ||
			after.Failure != before.Failure || !after.FirstFailedAt.Equal(*before.FirstFailedAt) {
			t.Fatalf("an unmeasured check moved the run: before %+v after %+v", before, after)
		}
		if !after.NextDueAt.After(*before.NextDueAt) {
			t.Fatalf("an unmeasured check did not reschedule: before %v after %v", before.NextDueAt, after.NextDueAt)
		}
	})
}

// The dark set is the run's threshold, its span and the check's max age, and
// a TLS-authentication failure at once; the SQL and IsDark agree on every row.
func TestGetAllProviderBlackholedClientIdsHonoursThresholdSpanAndMaxAge(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		rules := DefaultProviderEgressRules()
		now := server.NowUtc()
		at := func(offset time.Duration) *time.Time {
			value := now.Add(offset)
			return &value
		}
		cases := []struct {
			name string
			row  ProviderBlackholeCheck
			dark bool
		}{
			{name: "threshold over the span", row: ProviderBlackholeCheck{OK: false, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: at(-40 * time.Minute), CheckedAt: now.Add(-time.Minute)}, dark: true},
			{name: "under the threshold", row: ProviderBlackholeCheck{OK: false, Failure: "all_destinations_failed", ConsecutiveFailures: 2, FirstFailedAt: at(-2 * time.Hour), CheckedAt: now.Add(-time.Minute)}, dark: false},
			{name: "under the span", row: ProviderBlackholeCheck{OK: false, Failure: "all_destinations_failed", ConsecutiveFailures: 3, FirstFailedAt: at(-21 * time.Minute), CheckedAt: now.Add(-time.Minute)}, dark: false},
			{name: "past the max age", row: ProviderBlackholeCheck{OK: false, Failure: "all_destinations_failed", ConsecutiveFailures: 5, FirstFailedAt: at(-5 * time.Hour), CheckedAt: now.Add(-ProviderBlackholeCheckMaxAge - time.Minute)}, dark: false},
			{name: "tls at once", row: ProviderBlackholeCheck{OK: false, Failure: ProviderBlackholeTlsAuthenticationFailure, ConsecutiveFailures: 1, FirstFailedAt: at(-time.Minute), CheckedAt: now.Add(-time.Minute)}, dark: true},
			{name: "tls past the max age", row: ProviderBlackholeCheck{OK: false, Failure: ProviderBlackholeTlsAuthenticationFailure, ConsecutiveFailures: 1, FirstFailedAt: at(-4 * time.Hour), CheckedAt: now.Add(-ProviderBlackholeCheckMaxAge - time.Minute)}, dark: false},
			{name: "not measured", row: ProviderBlackholeCheck{OK: false, Failure: ProviderBlackholeNotMeasuredFailure, CheckedAt: now.Add(-time.Minute)}, dark: false},
			{name: "passing", row: ProviderBlackholeCheck{OK: true, CheckedAt: now.Add(-time.Minute)}, dark: false},
		}
		clientIds := make([]server.Id, len(cases))
		for i, c := range cases {
			clientIds[i] = server.NewId()
			row := c.row
			row.ClientId = clientIds[i]
			SetProviderBlackholeCheck(ctx, &row)
		}
		darkClientIds := GetAllProviderBlackholedClientIds(ctx)
		for i, c := range cases {
			if darkClientIds[clientIds[i]] != c.dark {
				t.Errorf("%s: in the dark set = %t, want %t", c.name, darkClientIds[clientIds[i]], c.dark)
			}
			if row := GetProviderBlackholeCheck(ctx, clientIds[i]); row.IsDark(server.NowUtc(), rules) != darkClientIds[clientIds[i]] {
				t.Errorf("%s: IsDark disagrees with the SQL dark set", c.name)
			}
		}
	})
}

// The due query offers every row whose next check has come due before any
// provider never checked, oldest due first, and nothing inside its backoff.
func TestGetProviderBlackholeCheckDueHonoursTheBackoff(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		never := server.NewId()
		retryDue := server.NewId()
		retryWaiting := server.NewId()
		passDue := server.NewId()
		legacyDue := server.NewId()
		fresh := server.NewId()
		for i, clientId := range []server.Id{never, retryDue, retryWaiting, passDue, legacyDue, fresh} {
			testing_connectProbeableProvider(t, ctx, clientId, city.LocationId, []string{"192.0.2.1:0", "192.0.2.2:0", "192.0.2.3:0", "192.0.2.4:0", "192.0.2.5:0", "192.0.2.6:0"}[i], ProvideModePublic)
		}
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)

		at := func(offset time.Duration) *time.Time {
			value := now.Add(offset)
			return &value
		}
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: retryDue, CheckedAt: now.Add(-20 * time.Minute), OK: false, Failure: "all_destinations_failed",
			ConsecutiveFailures: 1, FirstFailedAt: at(-20 * time.Minute), NextDueAt: at(-2 * time.Minute),
		})
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: retryWaiting, CheckedAt: now.Add(-time.Minute), OK: false, Failure: "all_destinations_failed",
			ConsecutiveFailures: 1, FirstFailedAt: at(-time.Minute), NextDueAt: at(4 * time.Minute),
		})
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: passDue, CheckedAt: now.Add(-2 * time.Hour), OK: true, NextDueAt: at(-30 * time.Minute),
		})
		// a row written before next_due_at existed is due its old age after its check
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: legacyDue, CheckedAt: now.Add(-24 * time.Hour), OK: true,
		})
		SetProviderBlackholeCheck(ctx, &ProviderBlackholeCheck{
			ClientId: fresh, CheckedAt: now.Add(-time.Minute), OK: true, NextDueAt: at(ProviderBlackholeCheckDueAge),
		})

		due := GetProviderBlackholeCheckDue(ctx, now, 100, 0, 1)
		index := func(clientId server.Id) int {
			return slices.Index(due, clientId)
		}
		for _, clientId := range []server.Id{retryWaiting, fresh} {
			if 0 <= index(clientId) {
				t.Errorf("due = %v contains %s inside its backoff", due, clientId)
			}
		}
		for _, clientId := range []server.Id{never, retryDue, passDue, legacyDue} {
			if index(clientId) < 0 {
				t.Fatalf("due = %v is missing %s", due, clientId)
			}
		}
		// oldest due first: the legacy row (due 22.5 hours ago), the pass
		// (30 minutes ago), the retry (2 minutes ago), then the first check
		if !(index(legacyDue) < index(passDue) && index(passDue) < index(retryDue) && index(retryDue) < index(never)) {
			t.Errorf("due order = %v, want the legacy row, the pass, the retry, then the never-checked provider", due)
		}
	})
}

// A materialized connected row can outlive the client state that made it
// eligible. The blackhole sweep must spend its bounded slots only on active
// top-level providers, never derivative return-traffic identities or inactive
// clients that still retain a Public key.
func TestGetProviderBlackholeCheckDueExcludesDerivedAndInactiveClients(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		city := &Location{
			LocationType: LocationTypeCity,
			City:         "Palo Alto",
			Region:       "California",
			Country:      "United States",
			CountryCode:  "us",
		}
		CreateLocation(ctx, city)

		sourceClientId := testingCreateProviderClient(ctx, networkId, nil, true)
		activeClientId := testingCreateProviderClient(ctx, networkId, nil, true)
		derivedClientId := testingCreateProviderClient(ctx, networkId, &sourceClientId, true)
		inactiveClientId := testingCreateProviderClient(ctx, networkId, nil, false)
		for _, clientId := range []server.Id{activeClientId, derivedClientId, inactiveClientId} {
			testingInsertProviderLocationReliability(ctx, clientId, networkId, city)
		}

		due := GetProviderBlackholeCheckDue(ctx, server.NowUtc(), 100, 0, 1)
		if !slices.Contains(due, activeClientId) {
			t.Errorf("due = %v, missing active top-level provider %s", due, activeClientId)
		}
		for _, clientId := range []server.Id{derivedClientId, inactiveClientId} {
			if slices.Contains(due, clientId) {
				t.Errorf("due = %v, contains derived or inactive provider %s", due, clientId)
			}
		}
	})
}
