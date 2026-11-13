// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
)

// The crypto notice names the coin, the day the paid year ended and the day Pro
// stops, and says a renewal costs no paid day.
func TestProEndingTemplateText(t *testing.T) {
	end := time.Date(2026, 12, 20, 0, 0, 0, 0, time.UTC)
	template := &ProEndingTemplate{TermEndTime: end.Add(-manualPaymentGracePeriod), EndTime: end, Asset: model.SubscriptionMarketZcash}
	subject, _, text, err := RenderEmailTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	if subject != "Renew UR Pro by December 20, 2026 to keep it" {
		t.Fatalf("subject %q", subject)
	}
	for _, want := range []string{"paid in Zcash, ended on November 20, 2026", "keeps\nworking until December 20, 2026", "https://ur.io/app/subscription", "loses\nyou nothing"} {
		if !strings.Contains(text, want) {
			t.Fatalf("text part lacks %q:\n%s", want, text)
		}
	}
	if (&ProEndingTemplate{Asset: model.SubscriptionMarketSolana}).AssetName() != "USDC" {
		t.Fatal("the solana market is USDC to the reader")
	}
}

// The trial notice tells each store's buyer where to cancel, and quotes no
// price.
func TestTrialEndingTemplateText(t *testing.T) {
	end := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	for store, want := range map[string]string{
		model.OnboardingStoreApple: "Subscriptions in your Apple Account settings",
		model.OnboardingStorePlay:  "open the Play Store",
	} {
		_, html, text, err := RenderEmailTemplate(&TrialEndingTemplate{EndTime: end, Store: store})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(text, want) || !strings.Contains(text, "November 3, 2026") {
			t.Fatalf("%s: text part lacks %q or the date:\n%s", store, want, text)
		}
		if (&TrialEndingTemplate{Store: store}).Stripe() || strings.Contains(html, "Manage subscription</a>") {
			t.Fatalf("%s: a store trial has no ur.io manage button", store)
		}
		if strings.ContainsAny(text, "$€£") {
			t.Fatalf("%s: the notice quotes a price", store)
		}
	}
}

// The notices run at 03:00 UTC, the next one strictly after now.
func TestNextServiceRemindersAt(t *testing.T) {
	at := NextServiceRemindersAt(time.Date(2026, 11, 1, 2, 59, 0, 0, time.UTC))
	if !at.Equal(time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v", at)
	}
	at = NextServiceRemindersAt(time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC))
	if !at.Equal(time.Date(2026, 11, 2, 3, 0, 0, 0, time.UTC)) {
		t.Fatalf("got %v", at)
	}
}
