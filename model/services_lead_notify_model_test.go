package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// DB-backed tests (postgres + redis). Under the owner rule these run only
// after the branch, with its migrations, is merged to main.

// Past the global budget every address is refused with 429, even one with its
// own budget left. Honeypot requests count toward the budget and store
// nothing, so the test fills it without writing leads.
func TestServicesContactSalesGlobalRateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		defer server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte("slack_webhook_url: \"\"\n"))()
		servicesSalesConfigCache.Store(nil)

		honeypot := validServicesContactSalesArgs()
		honeypot.Website = "https://spam.example"
		for i := range servicesLeadGlobalAttemptsPerMinute {
			clientSession := session.Testing_CreateClientSession(ctx, nil)
			clientSession.ClientAddress = fmt.Sprintf("198.18.%d.%d:40000", i/250, 1+i%250)
			result, err := ServicesContactSales(honeypot, clientSession)
			connect.AssertEqual(t, err, nil)
			connect.AssertNotEqual(t, result.RequestId, (*server.Id)(nil))
			connect.AssertEqual(t, countServicesLeads(ctx, *result.RequestId), 0)
		}

		freshSession := session.Testing_CreateClientSession(ctx, nil)
		freshSession.ClientAddress = "198.19.0.1:40000"
		_, err := ServicesContactSales(validServicesContactSalesArgs(), freshSession)
		var rateLimit *rateLimitError
		if !errors.As(err, &rateLimit) || !strings.HasPrefix(rateLimit.Error(), "429 ") {
			t.Fatalf("expected the global 429, got %v", err)
		}
	})
}

// With a webhook configured, a stored lead is posted once, with every
// user-supplied field Slack-escaped and the "New Services lead" title, and its
// notify_time is recorded after the post succeeds.
func TestServicesContactSalesPostsTheLeadToSlack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var stateLock sync.Mutex
		bodies := []string{}
		slack := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				bodies = append(bodies, string(body))
			}()
			w.WriteHeader(http.StatusOK)
		}))
		defer slack.Close()

		defer server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte("slack_webhook_url: "+slack.URL+"/services/T0/B0/x\n"))()
		servicesSalesConfigCache.Store(nil)
		defer servicesSalesConfigCache.Store(nil)
		previousFactory := servicesLeadNotifierFactory
		servicesLeadNotifierFactory = func() *servicesLeadNotifier {
			return &servicesLeadNotifier{
				httpClient:  slack.Client(),
				retryDelays: []time.Duration{0},
				sleep:       func(context.Context, time.Duration) bool { return true },
			}
		}
		defer func() { servicesLeadNotifierFactory = previousFactory }()

		clientSession := session.Testing_CreateClientSession(ctx, nil)
		clientSession.ClientAddress = "203.0.113.50:40000"
		args := validServicesContactSalesArgs()
		args.Company = "<!channel> & <https://evil.example|Co>"
		result, err := ServicesContactSales(args, clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, (*ServicesContactSalesError)(nil))
		leadId := *result.RequestId

		// the post is async: wait for notify_time
		readNotifyTime := func() (notifyTime *time.Time) {
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, `SELECT notify_time FROM services_lead WHERE lead_id = $1`, leadId)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&notifyTime))
					}
				})
			})
			return
		}
		deadline := time.Now().Add(30 * time.Second)
		for readNotifyTime() == nil {
			if deadline.Before(time.Now()) {
				t.Fatal("the lead was not marked notified")
			}
			time.Sleep(100 * time.Millisecond)
		}

		stateLock.Lock()
		defer stateLock.Unlock()
		connect.AssertEqual(t, len(bodies), 1)
		var message struct {
			Text        string `json:"text"`
			UnfurlLinks bool   `json:"unfurl_links"`
			UnfurlMedia bool   `json:"unfurl_media"`
		}
		connect.AssertEqual(t, json.Unmarshal([]byte(bodies[0]), &message), nil)
		connect.AssertEqual(t, strings.HasPrefix(message.Text, "*New Services lead*"), true)
		connect.AssertEqual(t, message.UnfurlLinks, false)
		connect.AssertEqual(t, message.UnfurlMedia, false)
		// escaped: no mention and no disguised link survive
		connect.AssertEqual(t, strings.Contains(message.Text, "<!channel>"), false)
		connect.AssertEqual(t, strings.Contains(message.Text, "<https://evil.example|Co>"), false)
		connect.AssertEqual(t, strings.Contains(message.Text, "&lt;!channel&gt; &amp; &lt;https://evil.example|Co&gt;"), true)
	})
}
