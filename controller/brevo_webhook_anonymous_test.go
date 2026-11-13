// SPDX-License-Identifier: MPL-2.0

package controller

// Brevo's anonymous tracking (the account's transactional settings, Tracking;
// on since 2026-11-03 about 19:00 UTC) posts opens, clicks and spam reports
// with an "an#<digits>" message id and an empty email. No send has such an id,
// so neither campaign can record the event for a network. Each one used to be
// looked up by that id, match nothing and be dropped without a count: Main
// recorded no open or click after 2026-11-03 18:36 UTC, while Brevo counted
// 279 opens and 34 clicks of onboarding mail in the week to 2026-11-10, and
// two spam reports on 2026-11-04. The handlers now count them by template
// without a lookup. The payloads are synthetic, every context forbids
// PostgreSQL, and the counters are read by name from the default registry.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/onboarding"
)

// An anonymous event in the shape Brevo documents for one (the unique proxy
// open sample in https://developers.brevo.com/docs/transactional-webhooks):
// an "an#" message id, an empty email, the tags in both forms, a 19-digit
// contact id, and "sending ip" spelled with a space.
func brevoAnonymousWebhookArgs(t *testing.T, event string, tags string) *BrevoWebhookArgs {
	t.Helper()
	body := `{
  "id": 25290,
  "email": "",
  "message-id": "an#2705147202202651768",
  "date": "2026-11-09 08:03:29",
  "tags": ` + tags + `,
  "tag": ` + strconv.Quote(tags) + `,
  "event": "` + event + `",
  "subject": "synthetic subject",
  "sending ip": "::",
  "ts": 1794211409,
  "template_id": 660,
  "ts_epoch": 1794211409710,
  "ts_event": 1794211409,
  "link": "",
  "mirror_link": "https://app-smtp.brevo.com/log/preview/synthetic",
  "user_agent": "Mozilla/5.0",
  "device_used": "DESKTOP",
  "contact_id": 4816445214646337536
}`
	var args BrevoWebhookArgs
	if err := json.Unmarshal([]byte(body), &args); err != nil {
		t.Fatalf("Brevo's anonymous %s does not decode: %v", event, err)
	}
	return &args
}

// The counter sample of name with exactly labels, or 0 when the registry has
// none (as before the counter existed).
func brevoWebhookCounterValue(t *testing.T, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) != len(labels) {
				continue
			}
			match := true
			for _, label := range metric.Label {
				if value, ok := labels[label.GetName()]; !ok || value != label.GetValue() {
					match = false
					break
				}
			}
			if match {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// The sum of every sample of the counter name, or 0 when the registry has none.
func brevoWebhookCounterTotal(t *testing.T, name string) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	total := float64(0)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			total += metric.GetCounter().GetValue()
		}
	}
	return total
}

const (
	onboardingUnattributedMetric   = "urnetwork_onboarding_webhook_unattributed_events_total"
	onboardingAttributedMetric     = "urnetwork_onboarding_webhook_events_total"
	reengagementUnattributedMetric = "urnetwork_reengagement_webhook_unattributed_events_total"
	reengagementAttributedMetric   = "urnetwork_reengagement_webhook_events_total"
)

func TestBrevoAnonymousOnboardingEventsAreCountedByTemplate(t *testing.T) {
	for _, test := range []struct {
		brevoEvent string
		tags       string
		event      string
		template   string
	}{
		{brevoEvent: "opened", tags: `["onboarding", "e4b_offer", "default"]`, event: "email.opened", template: "e4b_offer"},
		{brevoEvent: "unique_opened", tags: `["onboarding", "e2_widget", "activated"]`, event: "email.opened", template: "e2_widget"},
		{brevoEvent: "click", tags: `["onboarding", "e1_connect", "default"]`, event: "email.clicked", template: "e1_connect"},
		{brevoEvent: "spam", tags: `["onboarding", "e2_widget", "not_activated"]`, event: "email.complained", template: "e2_widget"},
		// a tag that is not a template name keeps the label bounded
		{brevoEvent: "click", tags: `["onboarding", "Not A Template!", "default"]`, event: "email.clicked", template: "other"},
	} {
		unattributed := map[string]string{"event": test.event, "template": test.template, "reason": "anonymous"}
		attributed := map[string]string{"event": test.event, "template": test.template}
		before := brevoWebhookCounterValue(t, onboardingUnattributedMetric, unattributed)
		attributedBefore := brevoWebhookCounterValue(t, onboardingAttributedMetric, attributed)

		ctx := server.WithoutPostgres(context.Background())
		handleOnboardingBrevoWebhook(ctx, brevoAnonymousWebhookArgs(t, test.brevoEvent, test.tags))

		if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
			t.Errorf("anonymous %s %s: looked up as a send (%d PostgreSQL attempts); an an# id is never a send's", test.brevoEvent, test.template, attempts)
		}
		if got := brevoWebhookCounterValue(t, onboardingUnattributedMetric, unattributed) - before; got != 1 {
			t.Errorf("anonymous %s %s: counted %v times as %s, want once", test.brevoEvent, test.template, got, test.event)
		}
		if got := brevoWebhookCounterValue(t, onboardingAttributedMetric, attributed) - attributedBefore; got != 0 {
			t.Errorf("anonymous %s %s: counted as attributed to a send", test.brevoEvent, test.template)
		}
	}
}

// The re-engagement program records clicks, bounces, complaints and
// unsubscribes, never opens. Each program counts only its own sends' events:
// both handlers see every event.
func TestBrevoAnonymousReengagementEventsAreCountedByTemplate(t *testing.T) {
	reengagementTags := `["reengagement", "r2_reason", "lapsed"]`
	clicked := map[string]string{"event": "reengage.email.clicked", "template": "r2_reason", "reason": "anonymous"}
	complained := map[string]string{"event": "reengage.email.complained", "template": "r2_reason", "reason": "anonymous"}
	clickedBefore := brevoWebhookCounterValue(t, reengagementUnattributedMetric, clicked)
	complainedBefore := brevoWebhookCounterValue(t, reengagementUnattributedMetric, complained)
	totalBefore := brevoWebhookCounterTotal(t, reengagementUnattributedMetric)
	onboardingTotalBefore := brevoWebhookCounterTotal(t, onboardingUnattributedMetric)
	attributedBefore := brevoWebhookCounterTotal(t, reengagementAttributedMetric)

	ctx := server.WithoutPostgres(context.Background())
	for _, event := range []string{"click", "spam", "opened", "unique_opened"} {
		args := brevoAnonymousWebhookArgs(t, event, reengagementTags)
		// as BrevoWebhook calls them
		handleOnboardingBrevoWebhook(ctx, args)
		handleReengagementBrevoWebhook(ctx, args)
	}
	// an onboarding send's anonymous click is not re-engagement's
	handleReengagementBrevoWebhook(ctx, brevoAnonymousWebhookArgs(t, "click", `["onboarding", "r2_reason", "default"]`))

	if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
		t.Errorf("anonymous events were looked up as sends (%d PostgreSQL attempts)", attempts)
	}
	if got := brevoWebhookCounterValue(t, reengagementUnattributedMetric, clicked) - clickedBefore; got != 1 {
		t.Errorf("anonymous re-engagement click counted %v times, want once", got)
	}
	if got := brevoWebhookCounterValue(t, reengagementUnattributedMetric, complained) - complainedBefore; got != 1 {
		t.Errorf("anonymous re-engagement spam report counted %v times, want once", got)
	}
	// the click and the spam report only: no open, and not the onboarding click
	if got := brevoWebhookCounterTotal(t, reengagementUnattributedMetric) - totalBefore; got != 2 {
		t.Errorf("re-engagement counted %v anonymous events, want 2 (a click and a spam report)", got)
	}
	if got := brevoWebhookCounterTotal(t, onboardingUnattributedMetric) - onboardingTotalBefore; got != 0 {
		t.Errorf("onboarding counted %v of re-engagement's events", got)
	}
	if got := brevoWebhookCounterTotal(t, reengagementAttributedMetric) - attributedBefore; got != 0 {
		t.Errorf("an anonymous event was counted as attributed to a send")
	}
}

// A campaign event whose message id no send recorded is counted as well (an
// empty id is never looked up), so a change of Brevo's ids shows on the
// dashboard rather than as silence.
func TestBrevoCampaignEventOfNoRecordedSendIsCounted(t *testing.T) {
	labels := map[string]string{"event": "email.delivered", "template": "e3_last_chance", "reason": "unknown_send"}
	before := brevoWebhookCounterValue(t, onboardingUnattributedMetric, labels)
	ctx := server.WithoutPostgres(context.Background())
	handleOnboardingBrevoWebhook(ctx, &BrevoWebhookArgs{
		Event: "delivered",
		Email: "person@example.invalid",
		Tags:  []string{"onboarding", "e3_last_chance", "default"},
	})
	if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
		t.Errorf("an event without a message id was looked up (%d PostgreSQL attempts)", attempts)
	}
	if got := brevoWebhookCounterValue(t, onboardingUnattributedMetric, labels) - before; got != 1 {
		t.Errorf("a delivered event of no recorded send counted %v times, want once", got)
	}

	// another sender's mail is not the campaign's, named or anonymous
	otherBefore := brevoWebhookCounterTotal(t, onboardingUnattributedMetric)
	handleOnboardingBrevoWebhook(ctx, &BrevoWebhookArgs{Event: "delivered", Email: "person@example.invalid", Tags: []string{"receipts"}})
	handleOnboardingBrevoWebhook(ctx, brevoAnonymousWebhookArgs(t, "opened", `["receipts"]`))
	handleOnboardingBrevoWebhook(ctx, brevoAnonymousWebhookArgs(t, "opened", `[]`))
	if got := brevoWebhookCounterTotal(t, onboardingUnattributedMetric) - otherBefore; got != 0 {
		t.Errorf("%v events of another sender were counted for the campaign", got)
	}
	if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
		t.Errorf("another sender's events were looked up (%d PostgreSQL attempts)", attempts)
	}
}

// A send an experiment covers carries the experiment and arm tags after the
// program, template and variant (onboarding.CampaignTags, 2026-11-13); the
// webhook still reads the program and template from the first two.
func TestBrevoWebhookReadsTheTemplateBeforeTheExperimentTags(t *testing.T) {
	tags := onboarding.CampaignTags("reengagement", "r2_reason", "lapsed", "reengage_lapsed_1", "control")
	encoded, err := json.Marshal(tags)
	if err != nil {
		t.Fatal(err)
	}
	args := brevoAnonymousWebhookArgs(t, "spam", string(encoded))
	if template, ok := brevoWebhookCampaignTemplate(args, "reengagement"); !ok || template != "r2_reason" {
		t.Fatalf("template %q (%t) from %v", template, ok, tags)
	}
	if _, ok := brevoWebhookCampaignTemplate(args, "onboarding"); ok {
		t.Fatal("a re-engagement send read as onboarding's")
	}
}
