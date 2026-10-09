package model

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// Pure tests: no database or redis.

func validServicesContactSalesArgs() *ServicesContactSalesArgs {
	return &ServicesContactSalesArgs{
		Name:                       "  Ada Lovelace ",
		Email:                      " ada@example.com ",
		Company:                    " Analytical Engines ",
		MonthlyActiveUsers:         5000,
		MonthlyDataBudgetByteCount: 10_000_000_000_000,
		Message:                    "Whitelabel VPN for our app.\nIn-app traffic only.",
	}
}

func TestNewServicesLeadValidation(t *testing.T) {
	leadId := server.NewId()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	lead, refusal := newServicesLead(validServicesContactSalesArgs(), leadId, now)
	connect.AssertEqual(t, refusal, "")
	connect.AssertEqual(t, lead.leadId, leadId)
	connect.AssertEqual(t, lead.name, "Ada Lovelace")
	connect.AssertEqual(t, lead.email, "ada@example.com")
	connect.AssertEqual(t, lead.company, "Analytical Engines")
	connect.AssertEqual(t, lead.monthlyActiveUsers, int64(5000))
	connect.AssertEqual(t, lead.monthlyDataBudgetByteCount, ByteCount(10_000_000_000_000))
	connect.AssertEqual(t, lead.message, "Whitelabel VPN for our app.\nIn-app traffic only.")

	// the use case is optional and a zero budget is allowed
	args := validServicesContactSalesArgs()
	args.Message = ""
	args.MonthlyDataBudgetByteCount = 0
	_, refusal = newServicesLead(args, leadId, now)
	connect.AssertEqual(t, refusal, "")

	refused := map[string]func(*ServicesContactSalesArgs){
		"empty name":         func(a *ServicesContactSalesArgs) { a.Name = "   " },
		"long name":          func(a *ServicesContactSalesArgs) { a.Name = strings.Repeat("n", servicesLeadMaxNameLength+1) },
		"control in name":    func(a *ServicesContactSalesArgs) { a.Name = "Ada\x07" },
		"newline in name":    func(a *ServicesContactSalesArgs) { a.Name = "Ada\nLovelace" },
		"empty email":        func(a *ServicesContactSalesArgs) { a.Email = "" },
		"not an email":       func(a *ServicesContactSalesArgs) { a.Email = "ada" },
		"display-name email": func(a *ServicesContactSalesArgs) { a.Email = "Ada <ada@example.com>" },
		"two addresses":      func(a *ServicesContactSalesArgs) { a.Email = "ada@example.com, eve@example.com" },
		"empty company":      func(a *ServicesContactSalesArgs) { a.Company = "" },
		"zero users":         func(a *ServicesContactSalesArgs) { a.MonthlyActiveUsers = 0 },
		"too many users":     func(a *ServicesContactSalesArgs) { a.MonthlyActiveUsers = servicesLeadMaxMonthlyActiveUsers + 1 },
		"negative budget":    func(a *ServicesContactSalesArgs) { a.MonthlyDataBudgetByteCount = -1 },
		"huge budget": func(a *ServicesContactSalesArgs) {
			a.MonthlyDataBudgetByteCount = servicesLeadMaxMonthlyDataBudgetByteCount + 1
		},
		"long use case":        func(a *ServicesContactSalesArgs) { a.Message = strings.Repeat("m", servicesLeadMaxMessageLength+1) },
		"nul in use case":      func(a *ServicesContactSalesArgs) { a.Message = "hi\x00there" },
		"invalid utf8 company": func(a *ServicesContactSalesArgs) { a.Company = "\xff\xfe" },
	}
	for name, change := range refused {
		args := validServicesContactSalesArgs()
		change(args)
		lead, refusal := newServicesLead(args, leadId, now)
		if refusal == "" || lead != nil {
			t.Fatalf("%s: accepted", name)
		}
	}
}

func TestSlackEscape(t *testing.T) {
	connect.AssertEqual(t, slackEscape("<!channel> & <@U123> <https://evil.example|bank>"), "&lt;!channel&gt; &amp; &lt;@U123&gt; &lt;https://evil.example|bank&gt;")
	// an entity typed into the form shows literally rather than as its character
	connect.AssertEqual(t, slackEscape("&amp;"), "&amp;amp;")
	connect.AssertEqual(t, slackEscape("plain text"), "plain text")
}

func TestServicesLeadSlackText(t *testing.T) {
	leadId := server.NewId()
	lead, refusal := newServicesLead(&ServicesContactSalesArgs{
		Name:                       "Mallory <!channel>",
		Email:                      "mallory@example.com",
		Company:                    "Evil & Co <https://evil.example|bank>",
		MonthlyActiveUsers:         1200,
		MonthlyDataBudgetByteCount: 1_500_000_000,
	}, leadId, server.NowUtc())
	connect.AssertEqual(t, refusal, "")
	text := servicesLeadSlackText(lead)

	if !strings.HasPrefix(text, "*New Services lead*\n") {
		t.Fatalf("unexpected title: %q", text)
	}
	for _, want := range []string{
		"*Name:* Mallory &lt;!channel&gt;",
		"*Email:* mallory@example.com",
		"*Company:* Evil &amp; Co &lt;https://evil.example|bank&gt;",
		"*Monthly active users:* 1200",
		"*Monthly data budget:* 1.5 GB (1500000000 bytes)",
		"*Use case:*\n(none)",
		"*Lead:* " + leadId.String(),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %q", want, text)
		}
	}
	for _, forbidden := range []string{"<!channel>", "<https://"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("unescaped %q in %q", forbidden, text)
		}
	}
}

func TestFormatServicesLeadByteCount(t *testing.T) {
	cases := map[ByteCount]string{
		0:                     "0 B",
		999:                   "999 B",
		1000:                  "1 KB",
		1_500_000_000:         "1.5 GB",
		10_000_000_000_000:    "10 TB",
		2_000_000_000_000_000: "2 PB",
	}
	for byteCount, want := range cases {
		connect.AssertEqual(t, formatServicesLeadByteCount(byteCount), want)
	}
}

func TestParseServicesSalesConfig(t *testing.T) {
	connect.AssertEqual(t, parseServicesSalesConfig([]byte("slack_webhook_url: https://hooks.slack.com/services/T0/B0/x\n")), "https://hooks.slack.com/services/T0/B0/x")
	connect.AssertEqual(t, parseServicesSalesConfig([]byte("slack_webhook_url: \"  https://hooks.slack.com/services/T0/B0/x  \"\n")), "https://hooks.slack.com/services/T0/B0/x")
	connect.AssertEqual(t, parseServicesSalesConfig([]byte("slack_webhook_url: http://hooks.slack.com/services/T0/B0/x\n")), "")
	connect.AssertEqual(t, parseServicesSalesConfig([]byte("slack_webhook_url: https:///no-host\n")), "")
	connect.AssertEqual(t, parseServicesSalesConfig([]byte("other_key: 1\n")), "")
	connect.AssertEqual(t, parseServicesSalesConfig([]byte("::: not yaml")), "")
	connect.AssertEqual(t, parseServicesSalesConfig(nil), "")
}

func testServicesLeadNotifier(httpClient *http.Client) *servicesLeadNotifier {
	return &servicesLeadNotifier{
		httpClient:  httpClient,
		retryDelays: []time.Duration{0},
		sleep: func(ctx context.Context, delay time.Duration) bool {
			return true
		},
	}
}

func TestServicesLeadNotifierRetriesThenSucceeds(t *testing.T) {
	var requestCount atomic.Int32
	var lastBody atomic.Value
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		lastBody.Store(body)
		connect.AssertEqual(t, r.Header.Get("Content-Type"), "application/json")
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer slack.Close()

	err := testServicesLeadNotifier(slack.Client()).Notify(context.Background(), slack.URL+"/services/T0/B0/secret", "*New Services lead*")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, requestCount.Load(), int32(2))

	message := map[string]any{}
	connect.AssertEqual(t, json.Unmarshal(lastBody.Load().([]byte), &message), nil)
	connect.AssertEqual(t, message["text"], "*New Services lead*")
	connect.AssertEqual(t, message["unfurl_links"], false)
	connect.AssertEqual(t, message["unfurl_media"], false)
}

func TestServicesLeadNotifierRetriesRateLimit(t *testing.T) {
	var requestCount atomic.Int32
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer slack.Close()

	err := testServicesLeadNotifier(slack.Client()).Notify(context.Background(), slack.URL, "text")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, requestCount.Load(), int32(2))
}

func TestServicesLeadNotifierStopsOnClientError(t *testing.T) {
	var requestCount atomic.Int32
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer slack.Close()

	err := testServicesLeadNotifier(slack.Client()).Notify(context.Background(), slack.URL, "text")
	var statusErr *errServicesLeadSlackStatus
	if !errors.As(err, &statusErr) || statusErr.statusCode != http.StatusNotFound {
		t.Fatalf("unexpected error: %v", err)
	}
	connect.AssertEqual(t, requestCount.Load(), int32(1))
}

func TestServicesLeadNotifierGivesUp(t *testing.T) {
	var requestCount atomic.Int32
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer slack.Close()

	err := testServicesLeadNotifier(slack.Client()).Notify(context.Background(), slack.URL, "text")
	connect.AssertNotEqual(t, err, nil)
	connect.AssertEqual(t, requestCount.Load(), int32(servicesLeadSlackAttemptCount))
}

// The webhook url is a credential: a transport error must not carry it.
func TestServicesLeadNotifierErrorOmitsUrl(t *testing.T) {
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	webhookUrl := slack.URL + "/services/T0SECRET/B0SECRET/tokensecret"
	slack.Close()

	err := testServicesLeadNotifier(&http.Client{Timeout: time.Second}).Notify(context.Background(), webhookUrl, "text")
	if err == nil {
		t.Fatal("a closed server accepted the post")
	}
	for _, secret := range []string{"T0SECRET", "B0SECRET", "tokensecret", "/services/"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaks the webhook url: %s", err)
		}
	}
}

// A canceled notifier stops between attempts.
func TestServicesLeadNotifierStopsWhenCanceled(t *testing.T) {
	var requestCount atomic.Int32
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer slack.Close()

	ctx, cancel := context.WithCancel(context.Background())
	notifier := testServicesLeadNotifier(slack.Client())
	notifier.sleep = func(ctx context.Context, delay time.Duration) bool {
		cancel()
		return sleepServicesLeadRetry(ctx, time.Hour)
	}
	err := notifier.Notify(ctx, slack.URL, "text")
	connect.AssertEqual(t, errors.Is(err, context.Canceled), true)
	connect.AssertEqual(t, requestCount.Load(), int32(1))
}
