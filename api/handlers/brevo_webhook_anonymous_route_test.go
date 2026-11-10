package handlers

// POST /updates/brevo with the events Brevo's anonymous tracking posts for a
// campaign send: an "an#<digits>" message id and an empty email, the shape of
// Brevo's own sample (https://developers.brevo.com/docs/transactional-webhooks,
// unique proxy open). The account has sent opens, clicks and spam reports this
// way since 2026-11-03 about 19:00 UTC, and the route accepted each one and
// recorded nothing. Each is answered 200, counted by template, and never
// looked up as a send. The user and payloads are synthetic, and the request
// context forbids PostgreSQL, so nothing reaches storage.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

const brevoRouteTestAnonymousEvent = `{
  "id": 25290,
  "email": "",
  "message-id": "an#2705147202202651768",
  "date": "2026-11-09 08:03:29",
  "tags": ["onboarding", "e4b_offer", "default"],
  "tag": "[\"onboarding\", \"e4b_offer\", \"default\"]",
  "event": "EVENT",
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

// The counter sample of name with exactly labels, or 0 when the registry has
// none (as before the counter existed).
func brevoRouteCounterValue(t *testing.T, name string, labels map[string]string) float64 {
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

func TestBrevoWebhookRouteCountsAnonymousCampaignEvents(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("brevo.yml", []byte("brevo:\n  webhook_bearers:\n    - synthetic-brevo-user\n")))

	const unattributed = "urnetwork_onboarding_webhook_unattributed_events_total"
	for _, test := range []struct {
		brevoEvent string
		event      string
	}{
		{brevoEvent: "opened", event: "email.opened"},
		{brevoEvent: "unique_opened", event: "email.opened"},
		{brevoEvent: "click", event: "email.clicked"},
		{brevoEvent: "spam", event: "email.complained"},
	} {
		labels := map[string]string{"event": test.event, "template": "e4b_offer", "reason": "anonymous"}
		before := brevoRouteCounterValue(t, unattributed, labels)

		ctx := server.WithoutPostgres(context.Background())
		body := strings.Replace(brevoRouteTestAnonymousEvent, `"EVENT"`, `"`+test.brevoEvent+`"`, 1)
		req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/updates/brevo", strings.NewReader(body))
		// the transactional webhook's `auth` bearer
		req.Header.Set("Authorization", "Bearer synthetic-brevo-user")
		recorder := httptest.NewRecorder()
		BrevoWebhook(recorder, req)

		if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
			t.Errorf("anonymous %s answered %d %q", test.brevoEvent, recorder.Code, recorder.Body.String())
		}
		if attempts := server.PacketPostgresAttempts(ctx); attempts != 0 {
			t.Errorf("anonymous %s was looked up as a send (%d PostgreSQL attempts)", test.brevoEvent, attempts)
		}
		if got := brevoRouteCounterValue(t, unattributed, labels) - before; got != 1 {
			t.Errorf("anonymous %s counted %v times as %s, want once", test.brevoEvent, got, test.event)
		}
	}
}
