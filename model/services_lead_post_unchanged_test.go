package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// Pure test: no database or redis.

// The Services lead post is unchanged by the sales webhook helper the leads
// now share with the payment notices (owner request 2026-10-28): the same
// request, byte for byte, through the lead's own names. This test passes on
// the code before the helper too; it guards the lead against the refactor.
func TestServicesLeadSlackPostBodyUnchanged(t *testing.T) {
	var body string
	var contentType string
	var method string
	slack := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bytes, _ := io.ReadAll(r.Body)
		body = string(bytes)
		contentType = r.Header.Get("Content-Type")
		method = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer slack.Close()

	leadId, err := server.ParseId("01890f1e-8c7a-7b4e-9d4f-0a1b2c3d4e5f")
	connect.AssertEqual(t, err, nil)
	lead, refusal := newServicesLead(&ServicesContactSalesArgs{
		Name:                       "Mallory <!channel>",
		Email:                      "mallory@example.com",
		Company:                    "Evil & Co",
		MonthlyActiveUsers:         1200,
		MonthlyDataBudgetByteCount: 1_500_000_000,
	}, leadId, time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC))
	connect.AssertEqual(t, refusal, "")

	notifier := &servicesLeadNotifier{
		httpClient:  slack.Client(),
		retryDelays: []time.Duration{0},
		sleep:       func(context.Context, time.Duration) bool { return true },
	}
	connect.AssertEqual(t, notifier.Notify(context.Background(), slack.URL+"/services/T0/B0/x", servicesLeadSlackText(lead)), nil)
	connect.AssertEqual(t, method, http.MethodPost)
	connect.AssertEqual(t, contentType, "application/json")
	connect.AssertEqual(t, body, `{"text":"*New Services lead*\n*Name:* Mallory \u0026lt;!channel\u0026gt;\n*Email:* mallory@example.com\n*Company:* Evil \u0026amp; Co\n*Monthly active users:* 1200\n*Monthly data budget:* 1.5 GB (1500000000 bytes)\n*Use case:*\n(none)\n*Lead:* 01890f1e-8c7a-7b4e-9d4f-0a1b2c3d4e5f","unfurl_links":false,"unfurl_media":false}`)

	// the shared config cache and vault resource are the lead's
	connect.AssertEqual(t, servicesSalesVaultResource, "sales.yml")
	connect.AssertEqual(t, servicesSalesConfigRefresh, 60*time.Second)
	connect.AssertEqual(t, servicesLeadSlackAttemptCount, 3)
}
