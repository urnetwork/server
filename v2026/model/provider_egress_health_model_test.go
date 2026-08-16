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
			ReputationOK:          1,
			ReputationTotal:       4,
			FailedNames:           "cachefly",
			ReputationFailedNames: "akamai,etsy,canva",
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
			ReputationOK:          0,
			ReputationTotal:       4,
			FailedNames:           "everything",
			ReputationFailedNames: "akamai",
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
		connect.AssertEqual(t, health.ClassResults["dns"], ProviderEgressHealthClassResult{OK: 26, Total: 26})
		if health.MeasuredAt.UTC().Before(later.Add(-time.Minute)) {
			t.Errorf("MeasuredAt = %s, want the later run's %s", health.MeasuredAt.UTC(), later)
		}
	})
}
