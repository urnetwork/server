package controller

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026/model"
)

// TestRedactEventText pins the feedback-text redaction: emails, ips and phone
// numbers are masked, whitespace is normalized, length is capped, and an empty
// result is dropped.
func TestRedactEventText(t *testing.T) {
	value, keep := redactEventText("  reach me at someone@example.com or 10.1.2.3 \n thanks ")
	connect.AssertEqual(t, true, keep)
	connect.AssertEqual(t, false, strings.Contains(value, "example.com"))
	connect.AssertEqual(t, false, strings.Contains(value, "10.1.2.3"))
	connect.AssertEqual(t, true, strings.Contains(value, "[redacted-email]"))
	connect.AssertEqual(t, true, strings.Contains(value, "[redacted-ip]"))
	connect.AssertEqual(t, true, strings.HasSuffix(value, "thanks"))

	value, keep = redactEventText("call +1 415 555 0100 now")
	connect.AssertEqual(t, true, keep)
	connect.AssertEqual(t, true, strings.Contains(value, "[redacted-phone]"))

	_, keep = redactEventText("   \n\t ")
	connect.AssertEqual(t, false, keep)

	long := strings.Repeat("x", 5000)
	value, keep = redactEventText(long)
	connect.AssertEqual(t, true, keep)
	connect.AssertEqual(t, maxEventTextRunes, len(value))
}

// TestNormalizeClientEvent pins the per-event checks around the schema: the
// platform enum, the token fields, the clock clamps and the text redaction.
func TestNormalizeClientEvent(t *testing.T) {
	now := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	at := now.Add(-time.Hour)
	event, err := normalizeClientEvent(&ClientEvent{
		Name:       model.EventOnboardingStepShown,
		At:         &at,
		Platform:   "iOS",
		AppVersion: "2026.9.1+42",
		Locale:     "pt-BR",
		Session:    "s_abc123",
		Props:      map[string]any{"step": "plan", "index": float64(1)},
	}, now)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, "ios", event.Platform)
	connect.AssertEqual(t, at, event.At)
	connect.AssertEqual(t, "pt-BR", event.Locale)
	connect.AssertEqual(t, int64(1), event.Props["index"])

	// missing at = now; a future clock = now; an ancient clock = the past clamp
	event, err = normalizeClientEvent(&ClientEvent{Name: model.EventConnectFirst, Platform: "web"}, now)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, now, event.At)
	future := now.Add(time.Hour)
	event, _ = normalizeClientEvent(&ClientEvent{Name: model.EventConnectFirst, Platform: "web", At: &future}, now)
	connect.AssertEqual(t, now, event.At)
	ancient := now.Add(-400 * 24 * time.Hour)
	event, _ = normalizeClientEvent(&ClientEvent{Name: model.EventConnectFirst, Platform: "web", At: &ancient}, now)
	connect.AssertEqual(t, now.Add(-clientEventPastClamp), event.At)

	// the text prop is redacted
	event, err = normalizeClientEvent(&ClientEvent{
		Name:     model.EventFeedbackSubmitted,
		Platform: "android",
		Props:    map[string]any{"rating": float64(2), "has_text": true, "text": "mail me: a@b.co"},
	}, now)
	connect.AssertEqual(t, nil, err)
	connect.AssertEqual(t, "mail me: [redacted-email]", event.Props["text"])

	// refusals
	_, err = normalizeClientEvent(&ClientEvent{Name: model.EventConnectFirst, Platform: "tvos"}, now)
	connect.AssertEqual(t, true, err != nil)
	_, err = normalizeClientEvent(&ClientEvent{Name: model.EventConnectFirst, Platform: "web", AppVersion: "1.0 beta"}, now)
	connect.AssertEqual(t, true, err != nil)
	_, err = normalizeClientEvent(&ClientEvent{Name: model.EventConnectFirst, Platform: "web", Locale: strings.Repeat("a", 33)}, now)
	connect.AssertEqual(t, true, err != nil)
	_, err = normalizeClientEvent(&ClientEvent{Name: model.EventConnectFirst, Platform: "web", Session: "a b"}, now)
	connect.AssertEqual(t, true, err != nil)
	_, err = normalizeClientEvent(&ClientEvent{Name: model.EventLandingClicked, Platform: "web", Props: map[string]any{"step": "e1"}}, now)
	connect.AssertEqual(t, true, err != nil)
	_, err = normalizeClientEvent(&ClientEvent{Name: "nope", Platform: "web"}, now)
	connect.AssertEqual(t, true, err != nil)
}

// TestClientEventAssignment pins which experiment an event is stamped with.
func TestClientEventAssignment(t *testing.T) {
	assignments := map[string]*model.ExperimentAssignment{
		model.ExperimentSurfaceOfferInApp:       {ExperimentId: "offer_screen", Variant: "control"},
		model.ExperimentSurfaceOfferFinalScreen: {ExperimentId: "final_copy", Variant: "months"},
	}
	a := clientEventAssignment(assignments, &model.OnboardingEvent{Name: model.EventOfferScreenShown, Props: map[string]any{"surface": "intro_step"}})
	connect.AssertEqual(t, "offer_screen", a.ExperimentId)
	a = clientEventAssignment(assignments, &model.OnboardingEvent{Name: model.EventOfferScreenShown, Props: map[string]any{"surface": "final_screen"}})
	connect.AssertEqual(t, "final_copy", a.ExperimentId)
	a = clientEventAssignment(assignments, &model.OnboardingEvent{Name: model.EventOfferCtaTapped, Props: map[string]any{"plan": "yearly"}})
	connect.AssertEqual(t, "offer_screen", a.ExperimentId)
	a = clientEventAssignment(assignments, &model.OnboardingEvent{Name: model.EventOfferScreenShown, Props: map[string]any{"surface": "account"}})
	connect.AssertEqual(t, true, a == nil)
	a = clientEventAssignment(assignments, &model.OnboardingEvent{Name: model.EventOnboardingStepShown})
	connect.AssertEqual(t, true, a == nil)
}

func TestProductUpdatesFromCreateArgs(t *testing.T) {
	connect.AssertEqual(t, true, ProductUpdatesFromCreateArgs(&model.NetworkCreateArgs{}))
	off := false
	connect.AssertEqual(t, false, ProductUpdatesFromCreateArgs(&model.NetworkCreateArgs{ProductUpdates: &off}))
	on := true
	connect.AssertEqual(t, true, ProductUpdatesFromCreateArgs(&model.NetworkCreateArgs{ProductUpdates: &on}))
}

func TestSolanaPlanDurationOnboarding(t *testing.T) {
	connect.AssertEqual(t, SubscriptionYearDuration+14*24*time.Hour, solanaPlanDuration(model.SolanaPlanYearlyOnboarding))
	connect.AssertEqual(t, SubscriptionYearDuration, solanaPlanDuration(model.SolanaPlanYearly))
	connect.AssertEqual(t, 30*24*time.Hour, solanaPlanDuration(model.SolanaPlanMonthly))
}

func TestPlayHasOfferTag(t *testing.T) {
	sub := &PlaySubscription{LineItems: []*PlaySubscriptionPurchaseLineItem{
		{ProductId: "supporter"},
		{ProductId: "supporter", OfferDetails: &PlayOfferDetails{OfferTags: []string{"other", "onboarding25"}}},
	}}
	connect.AssertEqual(t, true, sub.HasOfferTag("onboarding25"))
	connect.AssertEqual(t, false, sub.HasOfferTag("welcome"))
	connect.AssertEqual(t, false, sub.HasOfferTag(""))
}

func TestStripeInvoiceSubscriptionRef(t *testing.T) {
	ref := &stripeInvoiceSubscriptionRef{}
	connect.AssertEqual(t, "", ref.subscriptionId())
	ref.Subscription = []byte(`"sub_123"`)
	connect.AssertEqual(t, "sub_123", ref.subscriptionId())
	ref.Subscription = []byte(`{"id": "sub_obj"}`)
	connect.AssertEqual(t, "sub_obj", ref.subscriptionId())
	ref.Subscription = nil
	connect.AssertEqual(t, "", ref.subscriptionId())
	connect.AssertEqual(t, "sub_line", (&stripeInvoiceSubscriptionRef{Lines: &struct {
		Data []struct {
			Subscription json.RawMessage `json:"subscription"`
		} `json:"data"`
	}{Data: []struct {
		Subscription json.RawMessage `json:"subscription"`
	}{{Subscription: []byte(`"sub_line"`)}}}}).subscriptionId())
}
