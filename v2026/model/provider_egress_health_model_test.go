package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

func TestSetProviderEgressHealthStoresAndReadsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()
		measuredAt := server.NowUtc().Truncate(time.Millisecond)

		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   clientId,
			MeasuredAt: measuredAt,
			OKCount:    25,
			Total:      26,
			ClassResults: map[string]ProviderEgressHealthClassResult{
				"dns":          {OK: 4, Total: 4},
				"connectivity": {OK: 5, Total: 5},
				"cdn":          {OK: 4, Total: 5},
				"site":         {OK: 12, Total: 12},
			},
			ReputationOK:             1,
			ReputationTotal:          4,
			FailedNames:              "cachefly",
			ReputationFailedNames:    "akamai,etsy,canva",
			TLSAuthenticationFailure: true,
		})

		health := GetProviderEgressHealth(ctx, clientId)
		if health == nil {
			t.Fatal("expected a stored egress health row, got nil")
		}
		connect.AssertEqual(t, health.ClientId, clientId)
		connect.AssertEqual(t, health.OKCount, 25)
		connect.AssertEqual(t, health.Total, 26)
		connect.AssertEqual(t, health.ReputationOK, 1)
		connect.AssertEqual(t, health.ReputationTotal, 4)
		connect.AssertEqual(t, health.FailedNames, "cachefly")
		connect.AssertEqual(t, health.ReputationFailedNames, "akamai,etsy,canva")
		connect.AssertEqual(t, health.TLSAuthenticationFailure, true)
		if !health.MeasuredAt.UTC().Equal(measuredAt) {
			t.Errorf("MeasuredAt = %s, want %s", health.MeasuredAt.UTC(), measuredAt)
		}

		// asserted as a parsed map, never as json text: key order is not stable
		connect.AssertEqual(t, len(health.ClassResults), 4)
		connect.AssertEqual(t, health.ClassResults["dns"], ProviderEgressHealthClassResult{OK: 4, Total: 4})
		connect.AssertEqual(t, health.ClassResults["cdn"], ProviderEgressHealthClassResult{OK: 4, Total: 5})
		connect.AssertEqual(t, health.ClassResults["site"], ProviderEgressHealthClassResult{OK: 12, Total: 12})

		// the reputation figures are stored beside the health figures and
		// never inside them: 25/26 is the scored classes only, and the
		// per-class tallies sum to exactly that. If reputation were ever
		// folded in, this would read 26/30.
		sumOK, sumTotal := 0, 0
		for _, c := range health.ClassResults {
			sumOK += c.OK
			sumTotal += c.Total
		}
		connect.AssertEqual(t, sumOK, health.OKCount)
		connect.AssertEqual(t, sumTotal, health.Total)
		if _, present := health.ClassResults["reputation"]; present {
			t.Error("reputation must never appear as a scored class")
		}
	})
}

func TestGetProviderEgressHealthNilWhenNeverMeasured(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		// never measured is not the same as measured-unhealthy; a zero-valued
		// row here would read as a total blackhole for every unprobed provider
		if health := GetProviderEgressHealth(ctx, server.NewId()); health != nil {
			t.Errorf("expected nil for a never-probed provider, got %+v", health)
		}
	})
}

// TestSetProviderEgressHealthUpsertReplaces is the lifecycle the table exists
// for: one row per provider carrying the LATEST run. A second run for the same
// client_id must replace the first, not accumulate beside it -- otherwise a
// consumer reading "the provider's health" gets an arbitrary one of N rows.
func TestSetProviderEgressHealthUpsertReplaces(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   clientId,
			MeasuredAt: server.NowUtc().Add(-1 * time.Hour),
			OKCount:    0,
			Total:      26,
			ClassResults: map[string]ProviderEgressHealthClassResult{
				"dns": {OK: 0, Total: 26},
			},
			ReputationOK:             0,
			ReputationTotal:          4,
			FailedNames:              "everything",
			ReputationFailedNames:    "akamai",
			TLSAuthenticationFailure: true,
		})

		later := server.NowUtc()
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   clientId,
			MeasuredAt: later,
			OKCount:    26,
			Total:      26,
			ClassResults: map[string]ProviderEgressHealthClassResult{
				"dns": {OK: 26, Total: 26},
			},
			ReputationOK:    2,
			ReputationTotal: 4,
			// the recovered run has no failures at all: an upsert that only
			// wrote the non-empty columns would leave "everything" behind
			FailedNames:           "",
			ReputationFailedNames: "",
		})

		var rowCount int
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT COUNT(*) FROM provider_egress_health WHERE client_id = $1`,
				clientId,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&rowCount))
				}
			})
		})
		connect.AssertEqual(t, rowCount, 1)

		health := GetProviderEgressHealth(ctx, clientId)
		if health == nil {
			t.Fatal("expected a stored egress health row, got nil")
		}
		connect.AssertEqual(t, health.OKCount, 26)
		connect.AssertEqual(t, health.ReputationOK, 2)
		connect.AssertEqual(t, health.FailedNames, "")
		connect.AssertEqual(t, health.ReputationFailedNames, "")
		connect.AssertEqual(t, health.TLSAuthenticationFailure, false)
		connect.AssertEqual(t, health.ClassResults["dns"], ProviderEgressHealthClassResult{OK: 26, Total: 26})
		if health.MeasuredAt.UTC().Before(later.Add(-time.Minute)) {
			t.Errorf("MeasuredAt = %s, want the later run's %s", health.MeasuredAt.UTC(), later)
		}
	})
}

// A health measurement that has aged out must stop gating the provider in.
//
// This is the failure that put blackholes in the public list. Health drives the
// gate, but nothing re-measured a provider on its own schedule: the due queue
// keyed re-probes off provider_egress_location's age, so a provider with a
// fresh location was never re-probed and its passing tally sat unchanged for
// days. Measured on beta: 98.6% of gated providers were advertised on evidence
// older than six hours, and 12 of 12 sampled from the stalest cohort answered
// ok=0/131 when actually probed -- total blackholes, still advertised, because
// a measurement taken days ago said they were fine.
//
// The bound in GetAllProviderEgressHealthCounts is what closes that. A provider
// past ProviderEgressHealthMaxAge is absent from the map and so fails
// passesHealth closed, exactly as a never-measured one does -- both mean "no
// current evidence this provider carries traffic".
//
// The fresh provider is not decoration. It is identical to the stale one in
// every scored figure, differing ONLY in measured_at, so it is what makes this
// a test of the age bound rather than of the gate being reachable at all: a
// passesHealth broken to return false for everything would still satisfy the
// stale assertion on its own.
func TestStaleEgressHealthStopsGatingTheProvider(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()

		fresh := server.NewId()
		stale := server.NewId()

		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   fresh,
			MeasuredAt: now.Add(-time.Hour),
			OKCount:    100, Total: 100,
		})
		// deliberately a minute PAST the boundary rather than exactly on it:
		// server.NowUtc() advances between this write and the query, so an
		// exact-boundary fixture would land on whichever side the elapsed
		// microseconds put it
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId:   stale,
			MeasuredAt: now.Add(-ProviderEgressHealthMaxAge - time.Minute),
			OKCount:    100, Total: 100,
		})

		f := newProviderCountFilter(ctx, true)

		if !f.passesHealth(fresh) {
			t.Errorf("a provider measured 100/100 an hour ago must pass the gate")
		}
		if f.passesHealth(stale) {
			t.Errorf("a provider whose only measurement is older than ProviderEgressHealthMaxAge (%s) still passes "+
				"the gate. Stale evidence is not evidence: nothing re-measures a provider on the gate's schedule, so a "+
				"provider that went dark keeps its passing tally and stays advertised until some other sweep reaches it",
				ProviderEgressHealthMaxAge)
		}
	})
}

// A hard TLS-authenticity failure does not become safe merely because the
// prober stalls. It remains excluded until a later clean run replaces the row;
// otherwise an interceptor is silently re-admitted at the age boundary.
func TestTLSAuthenticationFailurePersistsUntilCleanRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientId := server.NewId()

		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: clientId, MeasuredAt: server.NowUtc().Add(-2 * ProviderEgressHealthMaxAge),
			OKCount: 130, Total: 131, TLSAuthenticationFailure: true,
		})
		if !GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("an aged TLS-authentication failure was silently forgotten before a clean re-test")
		}

		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: clientId, MeasuredAt: server.NowUtc(),
			OKCount: 131, Total: 131, TLSAuthenticationFailure: false,
		})
		if GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx)[clientId] {
			t.Fatal("a later clean run did not restore the provider")
		}
	})
}
