package controller

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// appleOfferCodeTestProvider is a private App Store Connect fixture: every
// batch request succeeds and downloads the given codes.
type appleOfferCodeTestProvider struct {
	batches atomic.Int64
}

func newAppleOfferCodeTestProvider(t testing.TB, codes []string) *appleOfferCodeTestProvider {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	server.Raise(err)
	der, err := x509.MarshalECPrivateKey(key)
	server.Raise(err)
	credentials := &appStoreConnectCredentials{KeyId: "synthetic-key", IssuerId: "synthetic-issuer",
		PrivateKey: string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))}
	previousUrl, previousCredentials := appStoreConnectBaseUrl, appStoreConnectCredentialsFunc
	t.Cleanup(func() { appStoreConnectBaseUrl, appStoreConnectCredentialsFunc = previousUrl, previousCredentials })
	appStoreConnectCredentialsFunc = func() *appStoreConnectCredentials { return credentials }

	provider := &appleOfferCodeTestProvider{}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			provider.batches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"synthetic-batch"}}`))
			return
		}
		_, _ = w.Write([]byte("Code\n" + strings.Join(codes, "\n") + "\n"))
	}))
	t.Cleanup(fixture.Close)
	appStoreConnectBaseUrl = fixture.URL
	return provider
}

// setAppleOfferTestConfig runs the welcome offer as configured on Main: 25%
// off, valid 5 days, the App Store one-time code top-up on.
func setAppleOfferTestConfig(t testing.TB, apple model.OnboardingAppleOfferCodesConfig) {
	config := model.Onboarding()
	previous := *config
	t.Cleanup(func() { *config = previous })
	config.Enabled = true
	config.Offer.PercentOff = 25
	config.Offer.MonthsFree = 3
	config.Offer.ValidityDays = 5
	config.Offer.AppleCustomOfferCode = ""
	config.Offer.Apple = apple
}

// issueTestWelcomeOffer issues a new network's welcome offer the way the app
// and the campaign do (controller IssueOnboardingOffer), drawing from the pool.
func issueTestWelcomeOffer(t testing.TB, ctx context.Context) *model.OnboardingOffer {
	networkId := server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("appleoffercode%s", networkId), server.NewId())
	cfg := model.Onboarding()
	offer, created, err := model.IssueOnboardingOffer(ctx, &model.IssueOnboardingOfferArgs{
		NetworkId:          networkId,
		IssuedBy:           model.OnboardingOfferIssuedByInApp,
		Surface:            model.OnboardingOfferSurfaceFinalScreen,
		Tier:               "standard",
		PercentOff:         cfg.Offer.PercentOff,
		MonthsFree:         cfg.Offer.MonthsFree,
		Validity:           cfg.OfferValidity(),
		StripeCouponId:     cfg.Offer.StripeCouponId,
		PlayOfferTag:       cfg.Offer.PlayOfferTag,
		AppleOfferCode:     cfg.Offer.AppleCustomOfferCode,
		AppleOfferCodePool: true,
	})
	if err != nil || !created || offer == nil {
		t.Fatalf("could not issue the welcome offer: created = %t, err = %v", created, err)
	}
	return offer
}

// Failure mode (2026-10, Main): with the shipped onboarding.yml (validity_days 5,
// offer.apple.expiry_days 5) the daily top-up generated a batch every five days
// and not one code was ever handed out: 2,000 codes generated, none of 5,451
// welcome offers given an App Store code. A code is handed only to an offer it
// outlives; a batch expired five days after it was generated, and every offer
// issued after it expired later.
func TestAppleOfferCodeTopUpBatchReachesTheNextOffer(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		setAppleOfferTestConfig(t, model.OnboardingAppleOfferCodesConfig{
			OfferCodeId: "synthetic-offer", BatchSize: 500, MinAvailable: 200, ExpiryDays: 5,
		})
		provider := newAppleOfferCodeTestProvider(t, []string{"SYNTHETIC-BATCH-1", "SYNTHETIC-BATCH-2"})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()

		result := runControllerAuditTask(t, owner, AppleOfferCodeTopUp, AppleOfferCodeTopUpPost, &AppleOfferCodeTopUpArgs{})
		if result.Added != 2 || provider.batches.Load() != 1 {
			t.Fatalf("the top-up did not load its batch: added %d codes in %d batches", result.Added, provider.batches.Load())
		}

		// an offer issued after the top-up, as every offer is
		offer := issueTestWelcomeOffer(t, ctx)
		if offer.AppleOfferCode == nil || !strings.HasPrefix(*offer.AppleOfferCode, "SYNTHETIC-BATCH-") {
			t.Fatalf(
				"the welcome offer issued after the top-up got no App Store code (offer expires %s); the batch expires before every offer it should serve",
				offer.ExpiresAt.Format(time.RFC3339),
			)
		}
	})
}

// Failure mode: the top-up counted every unexpired code as available, including
// codes that expire before an offer issued now would. A pool of those held off
// the next batch until it had expired, so no offer got a code meanwhile.
func TestAppleOfferCodeTopUpReplacesCodesNoOfferCanTake(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		setAppleOfferTestConfig(t, model.OnboardingAppleOfferCodesConfig{
			OfferCodeId: "synthetic-offer", BatchSize: 500, MinAvailable: 2, ExpiryDays: 7,
		})
		provider := newAppleOfferCodeTestProvider(t, []string{"SYNTHETIC-FRESH-1", "SYNTHETIC-FRESH-2"})
		owner := session.NewLocalClientSession(ctx, "", nil)
		defer owner.Cancel()

		// an earlier batch: unexpired, but expiring within an offer's validity,
		// so no offer issued now can take its codes
		staleExpiresAt := server.NowUtc().Add(3 * 24 * time.Hour)
		added, err := model.LoadAppleOfferCodePool(ctx, []*model.AppleOfferCode{
			{Code: "SYNTHETIC-STALE-1", ExpiresAt: staleExpiresAt},
			{Code: "SYNTHETIC-STALE-2", ExpiresAt: staleExpiresAt},
		})
		if err != nil || added != 2 {
			t.Fatalf("could not load the earlier batch: added %d, err %v", added, err)
		}

		result := runControllerAuditTask(t, owner, AppleOfferCodeTopUp, AppleOfferCodeTopUpPost, &AppleOfferCodeTopUpArgs{})
		if provider.batches.Load() != 1 || result.Added != 2 {
			t.Fatalf(
				"the top-up skipped a pool no offer can take (reason %q): %d batches, %d codes added",
				result.Reason, provider.batches.Load(), result.Added,
			)
		}

		offer := issueTestWelcomeOffer(t, ctx)
		if offer.AppleOfferCode == nil || !strings.HasPrefix(*offer.AppleOfferCode, "SYNTHETIC-FRESH-") {
			t.Fatalf("the welcome offer got no usable App Store code")
		}

		// the remaining fresh code still counts for an offer issued at the next
		// daily top-up; the stale ones never do
		after := server.NowUtc().Add(model.Onboarding().OfferValidity() + 24*time.Hour)
		if available := model.CountAvailableAppleOfferCodes(ctx, after); available != 1 {
			t.Fatalf("%d codes counted for the next top-up, want the 1 fresh code left", available)
		}
	})
}
