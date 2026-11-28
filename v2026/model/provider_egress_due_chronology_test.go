// Historical location fixtures preserve the ordering that distinguishes a
// later health sample from a client-verdict request for another full probe.
package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Recreate a location originally stored at its observation time. The ordinary
// setter stamps current update_time, which would model a later forced recheck
// when paired with an old observation and a pre-captured health timestamp.
func testingSetHistoricalProviderEgressLocation(t testing.TB, ctx context.Context, location *ProviderEgressLocation) {
	t.Helper()
	SetProviderEgressLocation(ctx, location)
	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(ctx, `
			UPDATE provider_egress_location
			SET update_time = observed_at
			WHERE client_id = $1 AND observed_at = $2
		`, location.ClientId, location.ObservedAt.UTC())
		server.Raise(err)
		if tag.RowsAffected() != 1 {
			t.Fatal("historical location fixture did not update exactly one matching observation")
		}
	})
}

// A quorum after fresh health still requests a full probe; an acknowledged
// health sample at or after that request completes it without another probe.
func TestProviderEgressDueRechecksLaterClientVerdictWithFreshHealth(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		location := &Location{
			LocationType: LocationTypeCountry,
			Country:      "Synthetic Country",
			CountryCode:  "zz",
		}
		CreateLocation(ctx, location)
		clientId := server.NewId()
		testing_connectProbeableProvider(t, ctx, clientId, location.LocationId, "192.0.2.1:0", ProvideModePublic)
		UpdateClientLocationReliabilities(ctx, now.Add(-time.Hour), now)
		testingSetHistoricalProviderEgressLocation(t, ctx, &ProviderEgressLocation{
			ClientId: clientId, LocationId: location.LocationId,
			CountryCode: "zz", ObservedAt: now.Add(-ProviderEgressLocationMaxAge/2 - time.Hour),
		})
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: clientId, MeasuredAt: now.Add(-30 * time.Minute), OKCount: 1, Total: 1,
		})
		minObservedAt := now.Add(-ProviderEgressLocationMaxAge / 2)
		minAttemptAt := now.Add(-ProviderEgressProbeAttemptBackoff)
		if due := GetProviderEgressLocationDue(ctx, minObservedAt, minAttemptAt, 1); len(due) != 0 {
			t.Fatalf("fresh health unexpectedly due before client verdict: %v", due)
		}
		if !ReprioritiseProviderEgressProbe(ctx, clientId, now) {
			t.Fatal("later client verdict did not reprioritise the provider")
		}
		reprioritised := GetProviderEgressLocation(ctx, clientId)
		if reprioritised == nil || !reprioritised.UpdateTime.After(now.Add(-30*time.Minute)) {
			t.Fatal("client verdict did not retain an update after the preceding health sample")
		}
		due, diagnostics := GetProviderEgressLocationDueShardedWithDiagnostics(ctx, minObservedAt, minAttemptAt, 1, 0, 1)
		if !slices.Equal(due, []server.Id{clientId}) || diagnostics.Selected[ProviderEgressDueStaleLocation].Current != 1 {
			t.Fatalf("later client verdict due = %v, diagnostics = %+v; want one current stale-location recheck", due, diagnostics)
		}
		if GetFreshProviderEgressLocation(ctx, clientId, ProviderEgressLocationMaxAge) == nil {
			t.Fatal("forced recheck must retain the location for provider selection")
		}
		SetProviderEgressHealth(ctx, &ProviderEgressHealth{
			ClientId: clientId, MeasuredAt: reprioritised.UpdateTime, OKCount: 1, Total: 1,
		})
		due, diagnostics = GetProviderEgressLocationDueShardedWithDiagnostics(ctx, minObservedAt, minAttemptAt, 1, 0, 1)
		if len(due) != 0 || diagnostics != (ProviderEgressDueDiagnostics{}) {
			t.Fatalf("health after client verdict did not complete the recheck: due = %v, diagnostics = %+v", due, diagnostics)
		}
	})
}
