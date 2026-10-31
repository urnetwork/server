package api

// End to end through the router the api serves, against the database and
// Redis: a pack of only PeerAudit frames spends none of its client's
// per-client control budget (controller/connect_control_discard.go), so
// neither audits nor a provider's abuse reports can get its real control
// requests refused.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Serves one control request with a client token, requiring the status, and
// for a 200 an answer without a control error.
func connectControlDiscardControl(t testing.TB, self *routeAccessDbFixture, body []byte, clientToken string, status int) *httptest.ResponseRecorder {
	t.Helper()
	w := connectControlDiscardServe(self.router, body, map[string]string{"Authorization": "Bearer " + clientToken})
	if w.Code != status {
		t.Fatalf("status %d, want %d: %s", w.Code, status, w.Body.String())
	}
	if status == http.StatusOK {
		var result controller.ConnectControlResult
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Error != nil {
			t.Fatalf("an admitted control request = %s (%v)", w.Body.String(), err)
		}
	}
	return w
}

// 100 audit-only requests leave a verified client's budget as it was: on a
// fixed clock its whole burst and one are admitted afterwards, and only the
// next is refused.
func TestConnectControlDiscardSpendsNoClientBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		const nowMicros = int64(1_800_000_000_000_000)
		defer controller.Testing_SetConnectControlLimit(
			model.ConnectControlLimitSettings{RequestsPerMinute: 1, Burst: 4},
			func() int64 { return nowMicros },
		)()
		self := newRouteAccessDbFixture(t, ctx)
		_, clientToken := self.mintClient(t, self.root(), nil)

		answers := connectControlDiscardAnswers{}
		for i := range 100 {
			answers.add(connectControlDiscardServe(self.router, connectControlDiscardBody(t, connectControlDiscardAudit(i%10 == 0)), map[string]string{"Authorization": "Bearer " + clientToken}))
		}
		ping := connectControlLimitTestBody(t)
		for range 5 {
			connectControlDiscardControl(t, self, ping, clientToken, http.StatusOK)
		}
		w := connectControlDiscardControl(t, self, ping, clientToken, http.StatusTooManyRequests)
		if retryAfter := w.Header().Get("Retry-After"); retryAfter != "60" {
			t.Fatalf("Retry-After = %q, want the 60 s interval", retryAfter)
		}
		answers.requireEmpty(t, 100)
	})
}

// A provider's egress policy reports each packet it calls an incident
// (connect Client.ReportAbuse), so a remote user decides how many reports a
// provider sends. Each report used to spend the provider's own control
// budget, and past it the provider's real control was refused with 429
// (CPUOPT3 D3). With the production limit on a fixed clock, a burst of
// reports past the provider's whole budget leaves its provide registration
// admitted.
func TestConnectControlAbuseReportsSpendNoProviderBudget(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		limit := model.DefaultConnectControlLimitSettings()
		const nowMicros = int64(1_800_000_000_000_000)
		defer controller.Testing_SetConnectControlLimit(limit, func() int64 { return nowMicros })()
		self := newRouteAccessDbFixture(t, ctx)
		_, providerToken := self.mintClient(t, self.root(), nil)

		// the provider's client ignores the answers to its reports
		report := connectControlDiscardBody(t, connectControlDiscardAudit(true))
		answers := connectControlDiscardAnswers{}
		for range limit.Burst + 1 {
			answers.add(connectControlDiscardServe(self.router, report, map[string]string{"Authorization": "Bearer " + providerToken}))
		}
		provide := connectControlDiscardBody(t, &protocol.Provide{
			Keys: []*protocol.ProvideKey{{
				Mode:             protocol.ProvideMode_Public,
				ProvideSecretKey: append(server.NewId().Bytes(), server.NewId().Bytes()...),
			}},
		})
		connectControlDiscardControl(t, self, provide, providerToken, http.StatusOK)
		answers.requireEmpty(t, limit.Burst+1)
	})
}

// Tallies the answers to audit-only requests, by status and body.
type connectControlDiscardAnswers map[string]int

func (self connectControlDiscardAnswers) add(w *httptest.ResponseRecorder) {
	self[fmt.Sprintf("%d %s", w.Code, strings.TrimSpace(w.Body.String()))] += 1
}

// Requires that every one of count requests was answered the empty pack.
func (self connectControlDiscardAnswers) requireEmpty(t testing.TB, count int) {
	t.Helper()
	if len(self) != 1 || self[`200 {"pack":"","error":null}`] != count {
		t.Fatalf("audit-only answers = %v, want %d empty packs", self, count)
	}
}
