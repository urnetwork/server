package handlers

// POST /updates/brevo as Brevo calls it. The account's transactional webhook
// authenticates with `auth` bearer, so its events arrive as
// "Authorization: Bearer <a brevo.webhook_bearers user>"; its marketing
// webhook carries the user in the url, so its events arrive as Basic. Every
// user here is synthetic, and the payloads use event names the campaign
// handlers ignore, so nothing reaches storage.

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
)

// A transactional "sent" event in the shape Brevo documents
// (https://developers.brevo.com/docs/transactional-webhooks): `tags` an array,
// `tag` a JSON-encoded array in a string, integer ids and times, a 13-digit
// `ts_epoch`.
const brevoRouteTestSentEvent = `{
  "event": "request",
  "email": "person@example.com",
  "id": 26224,
  "date": "2026-11-03 08:55:03",
  "ts": 1793724903,
  "message-id": "<202611031655.12345678901@smtp-relay.mailin.fr>",
  "ts_event": 1793724903,
  "subject": "Your first connection",
  "X-Mailin-custom": "synthetic-custom-header",
  "sending_ip": "192.0.2.10",
  "template_id": 31,
  "tag": "[\"onboarding\",\"e1_connect\",\"control\"]",
  "tags": ["onboarding", "e1_connect", "control"],
  "ts_epoch": 1793724903223
}`

func TestBrevoWebhookRouteAcceptsBothRegisteredCredentials(t *testing.T) {
	t.Cleanup(server.Vault.PushSimpleResource("brevo.yml", []byte("brevo:\n  webhook_bearers:\n    - ''\n    - synthetic-brevo-user\n")))

	for name, authorization := range map[string]string{
		// the transactional webhook's `auth` bearer
		"bearer": "Bearer synthetic-brevo-user",
		// the marketing webhook's url userinfo, without a password
		"basic": "Basic " + base64.StdEncoding.EncodeToString([]byte("synthetic-brevo-user:")),
	} {
		for _, body := range []string{brevoRouteTestSentEvent, "{}"} {
			recorder := serveWebhookRoute(t, BrevoWebhook, body, map[string]string{"Authorization": authorization})
			if recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != "{}" {
				t.Errorf("%s answered %d %q", name, recorder.Code, recorder.Body.String())
			}
		}
	}

	for name, authorization := range map[string]string{
		"other bearer": "Bearer synthetic-other-user",
		"none":         "",
	} {
		recorder := serveWebhookRoute(t, BrevoWebhook, brevoRouteTestSentEvent, map[string]string{"Authorization": authorization})
		if recorder.Code != http.StatusUnauthorized {
			t.Errorf("%s answered %d %q", name, recorder.Code, recorder.Body.String())
		}
	}
}
