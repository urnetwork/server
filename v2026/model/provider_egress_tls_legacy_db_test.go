// What clears the legacy snapshot's unidentified TLS finding: the URL-less
// aggregate failures of 2026-09-07 to 09-26 (provider_egress_health_model.go,
// clearProviderEgressLegacyTlsInTx). These tests reach it only through the
// receipt path and the judge's seam, so they read the same against code that
// never clears it.
package model

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

// A provider holding the legacy flag: one aggregate run, long ago, whose TLS
// failed with no URL to name.
func testingTlsLegacyProvider(t testing.TB, ctx context.Context, at time.Time) server.Id {
	t.Helper()
	clientId := testingTlsProvider(ctx)
	SetProviderEgressHealth(ctx, &ProviderEgressHealth{
		ClientId: clientId, MeasuredAt: at, OKCount: 130, Total: 131, TLSAuthenticationFailure: true,
	})
	if !testingTlsExcluded(t, ctx, []server.Id{clientId})[clientId] {
		t.Fatal("the precondition failed: an aggregate TLS failure did not exclude its provider")
	}
	return clientId
}

// count distinct catalog sites, named prefix-0, prefix-1, ...
func testingTlsCatalogSites(prefix string, count int) []egresshealth.Destination {
	sites := []egresshealth.Destination{}
	for i := range count {
		sites = append(sites, testingTlsSite(fmt.Sprintf("%s-%d", prefix, i), fmt.Sprintf("https://%s-%d.example/", prefix, i)))
	}
	return sites
}

// Back online, a legacy-flagged provider is rechecked through its ordinary
// catalog turns and stays excluded until its TLS has verified through ten
// distinct catalog sites, the quota that makes any provider fully measured;
// a repeat draw of one site counts once. Then the flag clears and the
// provider is admitted again.
func TestLegacyTlsFlagClearsAfterCleanRechecksOnReconnect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		provider := testingTlsLegacyProvider(t, ctx, now.Add(-40*24*time.Hour))
		clients := []server.Id{provider}
		sites := testingTlsCatalogSites("catalog", 10)
		clean := func(minute int, site egresshealth.Destination) {
			SetProviderEgressHealth(ctx, testingTlsUrlResult(provider, time.Time{}, site, now.Add(time.Duration(minute)*time.Minute), false))
		}
		for i, site := range sites[:9] {
			clean(i, site)
		}
		clean(9, sites[0])
		if !testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("a legacy flag cleared before the provider verified ten distinct catalog sites")
		}
		clean(10, sites[9])
		if testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("a legacy-flagged provider whose rechecks verified ten catalog sites was not admitted again")
		}
	})
}

// A TLS failure during the rechecks becomes an exact-URL finding judged like
// any other. Judged the provider's own, it excludes the provider and holds
// the legacy flag however many other sites verify. Once a later turn
// authenticates that URL, the clean rechecks already made clear the flag.
func TestLegacyTlsFlagStaysOnAFailedRecheck(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		provider := testingTlsLegacyProvider(t, ctx, now.Add(-40*24*time.Hour))
		clients := []server.Id{provider}
		sites := testingTlsCatalogSites("catalog", 12)
		intercepted := testingTlsSite("intercepted-site", "https://intercepted.example/")
		turn := func(minute int, site egresshealth.Destination, tlsFailure bool) {
			SetProviderEgressHealth(ctx, testingTlsUrlResult(provider, time.Time{}, site, now.Add(time.Duration(minute)*time.Minute), tlsFailure))
		}
		for i, site := range sites[:5] {
			turn(i, site, false)
		}
		turn(5, intercepted, true)
		testingJudgeTlsFindings(t, ctx, now.Add(6*time.Minute), []string{intercepted.Name}, map[string]string{"intercepted.example": "verified"})
		if verdict := testingTlsVerdict(t, ctx, provider, intercepted.Url); verdict != "provider" {
			t.Fatalf("a failed recheck of a legacy-flagged provider was judged %q", verdict)
		}
		for i, site := range sites[5:] {
			turn(7+i, site, false)
		}
		if !testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("a legacy-flagged provider that failed a recheck was admitted")
		}
		if verdict := testingTlsVerdict(t, ctx, provider, intercepted.Url); verdict != "provider" {
			t.Fatalf("the failed recheck's finding was judged %q", verdict)
		}

		// the intercepted URL authenticates on a later turn
		turn(20, intercepted, false)
		if testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("a legacy-flagged provider stayed excluded once its failed URL authenticated and twelve sites verified")
		}
	})
}

// Evidence from before the provider came back never counts. A legacy-flagged
// provider whose earlier turns, stored by the previous code, verified many
// catalog sites before it went offline stays excluded while it is offline,
// the judge clears nothing, and once it is back it needs ten fresh clean
// rechecks.
func TestLegacyTlsFlagIgnoresEvidenceFromBeforeItCameBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		now := server.NowUtc().Truncate(time.Microsecond)
		provider := testingTlsLegacyProvider(t, ctx, now.Add(-40*24*time.Hour))
		clients := []server.Id{provider}
		server.Tx(ctx, func(tx server.PgTx) {
			for _, site := range testingTlsCatalogSites("earlier", 12) {
				destination, err := json.Marshal(site)
				server.Raise(err)
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO provider_egress_url_security (client_id, url_key, destination, measured_at, tls_failure)
					VALUES ($1, $2, $3::jsonb, $4, false)`,
					provider, egresshealth.UrlProbeDestinationKey(site), string(destination), now.Add(-10*24*time.Hour)))
			}
		})
		if !testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("a legacy-flagged provider that is offline was admitted on evidence from before")
		}
		testingJudgeTlsFindings(t, ctx, now, []string{}, nil)
		if !testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("a judge run cleared the legacy flag of a provider that is offline")
		}

		sites := testingTlsCatalogSites("catalog", 10)
		for i, site := range sites[:9] {
			SetProviderEgressHealth(ctx, testingTlsUrlResult(provider, time.Time{}, site, now.Add(time.Duration(i)*time.Minute), false))
		}
		if !testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("clean turns from before the provider came back counted toward clearing its legacy flag")
		}
		SetProviderEgressHealth(ctx, testingTlsUrlResult(provider, time.Time{}, sites[9], now.Add(9*time.Minute), false))
		if testingTlsExcluded(t, ctx, clients)[provider] {
			t.Fatal("ten fresh clean rechecks did not clear the legacy flag")
		}
	})
}
