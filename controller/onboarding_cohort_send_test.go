// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
	"github.com/urnetwork/server/session"
)

// An email sign-up records its cohort from the request at enrollment: a farm's
// user agent is likely bot with its reason, a browser with a language likely
// human. The label decides nothing about the sequence: both rows wait for E1.
func TestEnrollNetworkOnboardingRecordsTheSignupCohort(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		enroll := func(name string, userAgent string, acceptLanguage string) server.Id {
			networkId, userId := server.NewId(), server.NewId()
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-cohort-"+name+"-"+networkId.String(), userId)
			clientSession := session.NewLocalClientSession(ctx, "", &session.ByJwt{NetworkId: networkId, UserId: userId})
			defer clientSession.Cancel()
			clientSession.Header["User-Agent"] = []string{userAgent}
			if acceptLanguage != "" {
				clientSession.Header["Accept-Language"] = []string{acceptLanguage}
			}
			EnrollNetworkOnboarding(clientSession, networkId, "synthetic-cohort-"+name+"@example.com", false)
			row := model.GetNetworkOnboarding(ctx, networkId)
			if row == nil || row.Exited() || row.NextStep != onboarding.StepE1 {
				tb.Fatal("the label changed the sequence", name, row)
			}
			return networkId
		}
		farm := enroll("farm", "urnet/1.1 (+https://farm.example/ur-network-farm)", "en-US")
		browser := enroll("browser", "Mozilla/5.0 (Macintosh; Intel Mac OS X 15_1) Safari/605.1.15", "de-DE,de;q=0.9")
		if !model.HasOnboardingEvent(ctx, farm, model.EventSignupLikelyBot) || model.HasOnboardingEvent(ctx, farm, model.EventSignupLikelyHuman) {
			tb.Fatal("the farm sign-up is not likely bot")
		}
		if !model.HasOnboardingEvent(ctx, browser, model.EventSignupLikelyHuman) || model.HasOnboardingEvent(ctx, browser, model.EventSignupLikelyBot) {
			tb.Fatal("the browser sign-up is not likely human")
		}
	})
}

// The offer email experiment (surface offer.email) covers the card branch of
// the offer emails: a sent E4b there carries crypto_line for the crypto arm
// only, the experiment and arm tags, and records the network's exposure; a
// store-billed E4b is outside the experiment, with no flag, tag or exposure.
func TestOfferEmailExperimentArmsTheCardBranch(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		experiment := &model.OnboardingExperiment{
			Id:         "offer_email_send_test",
			Surface:    model.ExperimentSurfaceOfferEmail,
			Status:     model.ExperimentStatusRunning,
			Allocation: map[string]float64{onboarding.OfferEmailVariantControl: 50, onboarding.OfferEmailVariantCrypto: 50},
			Variants: map[string]map[string]any{
				onboarding.OfferEmailVariantControl: {"copy": "current"},
				onboarding.OfferEmailVariantCrypto:  {"copy": "email_offer_crypto"},
			},
		}
		restore := model.Testing_SetOnboardingConfig(&model.OnboardingConfig{
			Enabled: true,
			Brevo: model.OnboardingBrevoConfig{Templates: map[string]map[string]map[string]int{
				onboarding.TemplateE4bOffer: {onboarding.VariantDefault: {"en": 198}},
			}},
			Experiments: []*model.OnboardingExperiment{experiment},
		})
		defer restore()

		var sendsLock sync.Mutex
		sends := []map[string]any{}
		// a Brevo stand-in on the test's own vault: newBrevoProtocolTestServer
		// empties the vault home, and the landing token needs the fixture's
		// signing key
		testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/smtp/email") {
				body, _ := io.ReadAll(r.Body)
				var send map[string]any
				_ = json.Unmarshal(body, &send)
				sendsLock.Lock()
				sends = append(sends, send)
				sendsLock.Unlock()
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"messageId":"<synthetic-` + server.NewId().String() + `@smtp.example>"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		}))
		previousBaseUrl, previousApiKey := brevoApiBaseUrl, brevoApiKey
		brevoApiBaseUrl = testServer.URL + "/v3"
		brevoApiKey = func() string { return "synthetic-brevo-key" }
		defer func() {
			testServer.Close()
			brevoApiBaseUrl, brevoApiKey = previousBaseUrl, previousApiKey
		}()

		networkIn := func(arm string) server.Id {
			for {
				networkId := server.NewId()
				if experiment.Assign(networkId) == arm {
					return networkId
				}
			}
		}
		send := func(networkId server.Id, storeBilled bool) map[string]any {
			model.Testing_CreateNetwork(ctx, networkId, "synthetic-offer-email-"+networkId.String(), server.NewId())
			now := server.NowUtc()
			row := &model.NetworkOnboarding{NetworkId: networkId, CreatedAt: now.Add(-7 * 24 * time.Hour), Email: true, TimeZone: "UTC",
				Platform: onboarding.PlatformWeb, Path: onboarding.PathB, NextStep: onboarding.StepE4, NextSendAt: &now}
			if !model.CreateNetworkOnboarding(ctx, row) {
				tb.Fatal("fixture row not created")
			}
			decision := onboarding.Decision{Action: onboarding.ActionSend, Template: onboarding.TemplateE4bOffer, Variant: onboarding.VariantDefault, StoreBilled: storeBilled}
			sendsLock.Lock()
			before := len(sends)
			sendsLock.Unlock()
			if outcome, err := sendOnboardingCampaignEmail(ctx, row, onboardingMailReader(ctx, row), onboarding.StepE4, decision, nil, now); err != nil || outcome != "sent" {
				tb.Fatal("send", outcome, err)
			}
			sendsLock.Lock()
			defer sendsLock.Unlock()
			if len(sends) != before+1 {
				tb.Fatal("expected one Brevo send", len(sends)-before)
			}
			return sends[len(sends)-1]
		}
		paramOf := func(s map[string]any, name string) string {
			params, _ := s["params"].(map[string]any)
			v, _ := params[name].(string)
			return v
		}
		tagsOf := func(s map[string]any) []string {
			out := []string{}
			list, _ := s["tags"].([]any)
			for _, tag := range list {
				out = append(out, tag.(string))
			}
			return out
		}

		for _, arm := range []string{onboarding.OfferEmailVariantControl, onboarding.OfferEmailVariantCrypto} {
			networkId := networkIn(arm)
			s := send(networkId, false)
			wantFlag := ""
			if arm == onboarding.OfferEmailVariantCrypto {
				wantFlag = "1"
			}
			if got := paramOf(s, "crypto_line"); got != wantFlag {
				tb.Fatal(arm, "crypto_line", got, "want", wantFlag)
			}
			tags := tagsOf(s)
			if !slices.Contains(tags, onboarding.ExperimentTag(experiment.Id)) || !slices.Contains(tags, onboarding.ArmTag(experiment.Id, arm)) {
				tb.Fatal(arm, "tags", tags)
			}
			if !model.HasOnboardingEvent(ctx, networkId, model.EventOfferEmailExposed) {
				tb.Fatal(arm, "no exposure recorded")
			}
		}
		// the store branch: outside the experiment
		storeNetworkId := networkIn(onboarding.OfferEmailVariantCrypto)
		s := send(storeNetworkId, true)
		if paramOf(s, "crypto_line") != "" || slices.Contains(tagsOf(s), onboarding.ExperimentTag(experiment.Id)) {
			tb.Fatal("a store-billed offer email was armed", s["params"], tagsOf(s))
		}
		if model.HasOnboardingEvent(ctx, storeNetworkId, model.EventOfferEmailExposed) {
			tb.Fatal("a store-billed offer email recorded an exposure")
		}
	})
}
