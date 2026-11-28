// SPDX-License-Identifier: MPL-2.0

package controller

import (
	"encoding/json"
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

// The gift-ending notice renders in the six languages: the subject and the
// body name the day the year ends in the reader's zone, the button keeps Pro
// on ur.io's subscription page, no price or discount appears, and the footer
// says how to stop emails like it. Arabic reads right to left; a language
// outside the six is English, and no zone is UTC. The outbox stores what it
// needs and reads it back.
func TestGiftEndingTemplateText(t *testing.T) {
	// the evening of November 19 in Los Angeles
	end := time.Date(2027, 11, 20, 3, 0, 0, 0, time.UTC)
	for _, language := range giftCardLanguages {
		template := &GiftEndingTemplate{EndTime: end, Lang: language, TimeZone: "America/Los_Angeles"}
		subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
		if err != nil {
			t.Fatalf("%s: %v", language, err)
		}
		date := giftCardDate(language, time.Date(2027, 11, 19, 0, 0, 0, 0, time.UTC))
		if want := giftCardText(language, "ending.title", "date", date); subject != want {
			t.Errorf("%s: subject %q, want %q", language, subject, want)
		}
		dir := giftCardDir(language)
		if !strings.Contains(bodyHtml, `<html lang="`+language+`" dir="`+dir+`"`) {
			t.Errorf("%s: the page is not lang %s dir %s", language, language, dir)
		}
		for _, want := range []string{
			htmlEscapedText(giftCardText(language, "ending.body", "date", date)),
			`href="https://ur.io/app/subscription"`,
			htmlEscapedText(giftCardText(language, "ending.button")),
			htmlEscapedText(giftCardText(language, "ending.free")),
			htmlEscapedText(giftCardText(language, "ending.why")),
			htmlEscapedText(giftCardText(language, "ending.preheader")),
		} {
			if !strings.Contains(bodyHtml, want) {
				t.Errorf("%s: the html part lacks %q", language, want)
			}
		}
		for _, want := range []string{
			giftCardText(language, "ending.body", "date", date),
			giftCardText(language, "ending.button") + ": https://ur.io/app/subscription",
			giftCardText(language, "ending.free"),
			giftCardText(language, "ending.why"),
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("%s: the text part lacks %q:\n%s", language, want, bodyText)
			}
		}
		if strings.Contains(bodyText, "<") || strings.Contains(bodyText, "{{") || strings.Contains(bodyHtml, "{{") {
			t.Errorf("%s: markup or an unresolved action", language)
		}
		if strings.ContainsAny(subject+bodyText, "$€£%") {
			t.Errorf("%s: the notice names a price or a discount", language)
		}
	}

	// a language outside the six, and no zone
	subject, _, _, err := RenderEmailTemplate(&GiftEndingTemplate{EndTime: end, Lang: "fr"})
	if err != nil || subject != "Your gift year of URnetwork Pro ends on November 20, 2027" {
		t.Fatalf("fallback subject %q, %v", subject, err)
	}

	// what the outbox stores, read back
	stored, err := json.Marshal(&GiftEndingTemplate{EndTime: end, Lang: "es", TimeZone: "America/Mexico_City"})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeAccountMessageTemplate(giftEndingTemplateName, string(stored))
	if err != nil {
		t.Fatal(err)
	}
	if read, ok := decoded.(*GiftEndingTemplate); !ok || !read.EndTime.Equal(end) || read.Lang != "es" || read.TimeZone != "America/Mexico_City" {
		t.Fatalf("the outbox reads back %#v from %s", decoded, stored)
	}
}
