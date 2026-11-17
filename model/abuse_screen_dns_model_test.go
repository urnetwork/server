//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// §21.6's screen at the gates the DNS tables carry (abuse_screen_model.go; ABUSE-M5-08), on the DNS tests' worlds,
// which the sidecar_pending build holds.

// A brand's own name passes once the network verified one of the brand's domains (a network_domain fixture row), and
// a brand with a keyword still goes to review.
func TestScreenNetworkNameVerifiedBrandAllowed(t *testing.T) {
	requireLegalHoldTables(t)
	if !server.SidecarMg6Linked() {
		t.Skip("MG6 is pending: network_domain runs with -tags sidecar_pending")
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := testScreenNetwork(t, ctx)
		if err := ScreenNetworkHost(ctx, networkId, "paypal"); !errors.Is(err, ErrAbuseScreenDenied) {
			t.Fatalf("before the domain: %v", err)
		}
		testDnsNetworkDomain(t, ctx, networkId, testScreenBrandDomain(t, "paypal"), true)
		if err := ScreenNetworkHost(ctx, networkId, "paypal"); err != nil {
			t.Fatalf("the brand with its domain verified: %v", err)
		}
		if err := ScreenNetworkName(ctx, networkId, "paypal"); err != nil {
			t.Fatalf("the brand as a name with its domain verified: %v", err)
		}
		if AbuseScreenReviewOf(ScreenNetworkHost(ctx, networkId, "paypal-support")) == nil {
			t.Fatal("a brand with a keyword passed on the domain alone")
		}
		facts := ScreenFactsForNetwork(ctx, networkId, "paypal", server.NowUtc())
		if facts.Probation || len(facts.VerifiedDomains) != 1 || facts.ReviewAllowed {
			t.Fatalf("the facts: %+v", facts)
		}
	})
}

// A record owner's host label through §12.8's write: a review answers QuotaRefusal{limit_key: screen_review} and
// writes nothing, its case opened once though the write's transaction rolled back, and a reviewer's allow lets the
// retry land. An unverified brand stays refused as before.
func TestDnsRecordsScreenReviewHoldsUntilAllowed(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		owner := "paypal-login." + world.apex
		for range 2 {
			serial := world.serial(t, ctx)
			_, err := world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`))
			apiError := testDnsApiError(t, err)
			if apiError.Status != http.StatusTooManyRequests || apiError.Code != sidecar.DnsApiCodeQuota ||
				apiError.Refusal == nil || apiError.Refusal.LimitKey != AbuseScreenReviewLimitKey {
				t.Fatalf("a held host: %+v", apiError)
			}
			if world.serial(t, ctx) != serial {
				t.Fatal("a held host wrote")
			}
		}
		cases := testScreenCases(t, ctx, world.networkId)["paypal-login"]
		if len(cases) != 1 || cases[0].TargetKind != AbuseTargetKindHost || cases[0].Category != AbuseCategoryBrand {
			t.Fatalf("the cases: %+v", cases)
		}
		// the refusal names the case it opened (QD-1014)
		_, err := world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`))
		if apiError := testDnsApiError(t, err); apiError.Refusal == nil || apiError.Refusal.CaseId != cases[0].CaseId.String() {
			t.Fatalf("the refusal names %+v", apiError.Refusal)
		}
		testScreenAllow(t, ctx, cases[0].CaseId, "paypal-login")
		if _, err := world.write(ctx, testDnsUpsert(owner, "TXT", `"x"`)); err != nil {
			t.Fatalf("the allowed host: %v", err)
		}
		_, err = world.write(ctx, testDnsUpsert("paypal."+world.apex, "TXT", `"x"`))
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusForbidden || apiError.Code != sidecar.DnsApiCodeRefused {
			t.Fatalf("an unverified brand: %+v", apiError)
		}
	})
}
