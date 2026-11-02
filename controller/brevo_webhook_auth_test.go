// SPDX-License-Identifier: MPL-2.0

package controller

// The Brevo webhook's credential: what POST /updates/brevo refuses and counts,
// and what `bringyourctl onboarding webhook-ensure` registers, against a fake
// of Brevo's webhook API. Every user here is synthetic, and no test contacts
// Brevo.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const (
	brevoWebhookTestUser       = "synthetic-brevo-user"
	brevoWebhookTestSecondUser = "synthetic-second-user"
	brevoWebhookTestTarget     = "https://api.example.invalid/updates/brevo"
	// the url credential the registration writes: the first usable user, an
	// empty password
	brevoWebhookTestUrlWithUser = "https://synthetic-brevo-user:@api.example.invalid/updates/brevo"
)

var brevoWebhookTestAuthorization = "Basic " + base64.StdEncoding.EncodeToString([]byte(brevoWebhookTestUser+":"))

// A blank user first, so the first usable user is the second entry.
func pushBrevoWebhookTestUsers(t *testing.T) {
	t.Helper()
	t.Cleanup(server.Vault.PushSimpleResource("brevo.yml", []byte(
		"brevo:\n  webhook_bearers:\n    - ''\n    - "+brevoWebhookTestUser+"\n    - "+brevoWebhookTestSecondUser+"\n",
	)))
}

// Output must never carry a user or its Basic value.
func requireNoBrevoWebhookSecret(t *testing.T, text string) {
	t.Helper()
	for _, user := range []string{brevoWebhookTestUser, brevoWebhookTestSecondUser} {
		if strings.Contains(text, user) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(user+":"))) {
			t.Fatalf("output carries a webhook credential:\n%s", strings.ReplaceAll(text, user, "<user>"))
		}
	}
}

type fakeBrevoWebhookRequest struct {
	method string
	path   string
	query  string
	body   map[string]any
}

// A fake of Brevo's webhook API: GET and POST /v3/webhooks, GET and PUT
// /v3/webhooks/{webhookId}. Webhooks are JSON objects; a PUT merges its fields
// into the stored webhook. The list returns every webhook whatever the type
// filter, so the code under test must check each type itself.
type fakeBrevoWebhooks struct {
	t        *testing.T
	nextId   int64
	webhooks []map[string]any
	requests []fakeBrevoWebhookRequest
	// when set, POST and PUT are answered with this status and body
	rejectStatus int
	rejectBody   string
}

func newFakeBrevoWebhooks(t *testing.T, webhooks ...map[string]any) *fakeBrevoWebhooks {
	t.Helper()
	fake := &fakeBrevoWebhooks{t: t}
	for _, webhook := range webhooks {
		var id int64
		if _, err := fmt.Sscan(fmt.Sprint(webhook["id"]), &id); err != nil {
			t.Fatalf("seeded webhook without an id: %v", err)
		}
		fake.nextId = max(fake.nextId, id)
		fake.webhooks = append(fake.webhooks, webhook)
	}
	newBrevoProtocolTestServer(t, fake)
	return fake
}

func (self *fakeBrevoWebhooks) find(id string) map[string]any {
	for _, webhook := range self.webhooks {
		if fmt.Sprint(webhook["id"]) == id {
			return webhook
		}
	}
	return nil
}

func (self *fakeBrevoWebhooks) requestsFor(method string, path string) []fakeBrevoWebhookRequest {
	requests := []fakeBrevoWebhookRequest{}
	for _, request := range self.requests {
		if request.method == method && request.path == path {
			requests = append(requests, request)
		}
	}
	return requests
}

func (self *fakeBrevoWebhooks) writes() int {
	writes := 0
	for _, request := range self.requests {
		if request.method != http.MethodGet {
			writes += 1
		}
	}
	return writes
}

func (self *fakeBrevoWebhooks) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	writeJson := func(statusCode int, value any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if err := json.NewEncoder(w).Encode(value); err != nil {
			self.t.Errorf("encode fake response: %v", err)
		}
	}
	reject := func() bool {
		if self.rejectStatus == 0 {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(self.rejectStatus)
		if _, err := w.Write([]byte(self.rejectBody)); err != nil {
			self.t.Errorf("write fake rejection: %v", err)
		}
		return true
	}

	var body map[string]any
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		self.t.Errorf("read fake request: %v", err)
	}
	if 0 < len(bytes.TrimSpace(raw)) {
		if err := json.Unmarshal(raw, &body); err != nil {
			self.t.Errorf("%s %s sent a body that is no JSON object: %v", r.Method, r.URL.Path, err)
		}
	}
	self.requests = append(self.requests, fakeBrevoWebhookRequest{
		method: r.Method,
		path:   r.URL.Path,
		query:  r.URL.RawQuery,
		body:   body,
	})

	switch {
	case r.URL.Path == "/v3/webhooks" && r.Method == http.MethodGet:
		writeJson(http.StatusOK, map[string]any{"webhooks": self.webhooks})
	case r.URL.Path == "/v3/webhooks" && r.Method == http.MethodPost:
		if reject() {
			return
		}
		self.nextId += 1
		webhook := map[string]any{"id": self.nextId}
		for key, value := range body {
			webhook[key] = value
		}
		self.webhooks = append(self.webhooks, webhook)
		writeJson(http.StatusCreated, map[string]any{"id": self.nextId})
	case strings.HasPrefix(r.URL.Path, "/v3/webhooks/"):
		webhook := self.find(strings.TrimPrefix(r.URL.Path, "/v3/webhooks/"))
		if webhook == nil {
			writeJson(http.StatusNotFound, map[string]any{"code": "document_not_found", "message": "Webhook not found"})
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeJson(http.StatusOK, webhook)
		case http.MethodPut:
			if reject() {
				return
			}
			for key, value := range body {
				webhook[key] = value
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			self.t.Errorf("unexpected Brevo request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	default:
		self.t.Errorf("unexpected Brevo request %s %s", r.Method, r.URL.Path)
		writeJson(http.StatusNotFound, map[string]any{"code": "not_found"})
	}
}

// A body field as a list of strings.
func fakeBrevoStrings(t *testing.T, value any) []string {
	t.Helper()
	values, ok := value.([]any)
	if !ok {
		t.Fatalf("not a list: %v", value)
	}
	strs := []string{}
	for _, v := range values {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("not a string: %v", v)
		}
		strs = append(strs, s)
	}
	return strs
}

// A body's headers as key: value.
func fakeBrevoHeaders(t *testing.T, value any) map[string]string {
	t.Helper()
	values, ok := value.([]any)
	if !ok {
		t.Fatalf("headers are not a list: %v", value)
	}
	headers := map[string]string{}
	for _, v := range values {
		header, ok := v.(map[string]any)
		if !ok {
			t.Fatalf("header is not an object: %v", v)
		}
		headers[fmt.Sprint(header["key"])] = fmt.Sprint(header["value"])
	}
	return headers
}

// The Authorization values a standard HTTP client sends to a url that carries
// userinfo, the way Brevo's username and password authentication posts.
func brevoWebhookClientAuthorization(t *testing.T, webhookUrl string) []string {
	t.Helper()
	var received []string
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received = r.Header.Values("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer receiver.Close()
	parsed, err := url.Parse(webhookUrl)
	if err != nil {
		t.Fatal(err)
	}
	receiverUrl, err := url.Parse(receiver.URL)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Scheme = receiverUrl.Scheme
	parsed.Host = receiverUrl.Host
	response, err := receiver.Client().Post(parsed.String(), "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return received
}

func allBrevoWebhookEvents() []any {
	events := []any{}
	for _, event := range onboardingBrevoWebhookEvents {
		events = append(events, event)
	}
	return events
}

func ensureBrevoWebhookForTest(t *testing.T, carrier brevoWebhookCredentialCarrier) string {
	t.Helper()
	outcome, err := ensureBrevoTransactionalWebhook(context.Background(), brevoWebhookTestTarget, onboardingBrevoWebhookEvents, carrier)
	if err != nil {
		requireNoBrevoWebhookSecret(t, err.Error())
		t.Fatalf("ensure: %v", err)
	}
	requireNoBrevoWebhookSecret(t, outcome)
	return outcome
}

// No webhook: one is created, transactional, with every event, unbatched, and
// with a credential the handler accepts as Brevo sends it.
func TestEnsureBrevoWebhookCreatesWithCredential(t *testing.T) {
	for _, carrier := range []brevoWebhookCredentialCarrier{brevoWebhookCredentialUrl, brevoWebhookCredentialHeader} {
		t.Run(string(carrier), func(t *testing.T) {
			pushBrevoWebhookTestUsers(t)
			fake := newFakeBrevoWebhooks(t)

			outcome := ensureBrevoWebhookForTest(t, carrier)

			lists := fake.requestsFor(http.MethodGet, "/v3/webhooks")
			if len(lists) != 1 || lists[0].query != "type=transactional" {
				t.Fatalf("lists = %+v", lists)
			}
			creates := fake.requestsFor(http.MethodPost, "/v3/webhooks")
			if len(creates) != 1 || fake.writes() != 1 {
				t.Fatalf("writes = %+v", fake.requests)
			}
			created := creates[0].body
			if created["type"] != "transactional" || created["batched"] != false {
				t.Fatalf("created type %v batched %v", created["type"], created["batched"])
			}
			if events := fakeBrevoStrings(t, created["events"]); !slices.Equal(events, onboardingBrevoWebhookEvents) {
				t.Fatalf("created events %v", events)
			}
			switch carrier {
			case brevoWebhookCredentialUrl:
				if created["url"] != brevoWebhookTestUrlWithUser {
					t.Fatal("created url does not carry the first usable user with an empty password")
				}
				if _, ok := created["headers"]; ok {
					t.Fatal("a url credential was also sent as headers")
				}
				auths := brevoWebhookClientAuthorization(t, created["url"].(string))
				if !slices.Equal(auths, []string{brevoWebhookTestAuthorization}) {
					t.Fatal("a client posting to the registered url does not send the Basic value")
				}
				if err := verifyBrevoWebhookAuthorization(map[string][]string{"Authorization": auths}); err != nil {
					t.Fatalf("the handler refuses what the registered url sends: %v", err)
				}
			case brevoWebhookCredentialHeader:
				if created["url"] != brevoWebhookTestTarget {
					t.Fatal("a header credential left a credential in the url")
				}
				headers := fakeBrevoHeaders(t, created["headers"])
				if len(headers) != 1 || headers["Authorization"] != brevoWebhookTestAuthorization {
					t.Fatal("created headers are not exactly the Basic value")
				}
				if err := verifyBrevoWebhookAuthorization(map[string][]string{"Authorization": {headers["Authorization"]}}); err != nil {
					t.Fatalf("the handler refuses the registered header: %v", err)
				}
			}
			if !strings.HasPrefix(outcome, "created: transactional webhook 1 ") ||
				!strings.Contains(outcome, "user sy***er with an empty password") ||
				!strings.Contains(outcome, "check: Brevo's copy carries the credential and every event, unbatched") {
				t.Fatalf("outcome:\n%s", outcome)
			}
		})
	}
}

// A webhook that already carries an accepted credential and every event is
// left as it is.
func TestEnsureBrevoWebhookLeavesWebhookWithCredential(t *testing.T) {
	tests := []struct {
		name    string
		carrier brevoWebhookCredentialCarrier
		webhook map[string]any
	}{
		{
			name:    "url",
			carrier: brevoWebhookCredentialUrl,
			webhook: map[string]any{"url": brevoWebhookTestUrlWithUser},
		},
		{
			// another accepted user, the empty password without its separator
			name:    "url with the second user",
			carrier: brevoWebhookCredentialUrl,
			webhook: map[string]any{"url": "https://" + brevoWebhookTestSecondUser + "@api.example.invalid/updates/brevo"},
		},
		{
			name:    "header",
			carrier: brevoWebhookCredentialHeader,
			webhook: map[string]any{
				"url":     brevoWebhookTestTarget,
				"headers": []any{map[string]any{"key": "Authorization", "value": brevoWebhookTestAuthorization}},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pushBrevoWebhookTestUsers(t)
			webhook := map[string]any{
				"id":          7,
				"type":        "transactional",
				"description": "URnetwork onboarding campaign (server-registered)",
				"events":      allBrevoWebhookEvents(),
				"batched":     false,
			}
			for key, value := range test.webhook {
				webhook[key] = value
			}
			fake := newFakeBrevoWebhooks(t, webhook)

			outcome := ensureBrevoWebhookForTest(t, test.carrier)

			if fake.writes() != 0 {
				t.Fatalf("changed a webhook that has everything: %+v", fake.requests)
			}
			if !strings.HasPrefix(outcome, "exists: transactional webhook 7 ") || !strings.Contains(outcome, "events: all 10 present") {
				t.Fatalf("outcome:\n%s", outcome)
			}
		})
	}
}

// The webhook the old registration left, without a credential, two of the
// events and with batching on, is updated in place: no second webhook.
func TestEnsureBrevoWebhookUpdatesWebhookWithoutCredential(t *testing.T) {
	for _, carrier := range []brevoWebhookCredentialCarrier{brevoWebhookCredentialUrl, brevoWebhookCredentialHeader} {
		t.Run(string(carrier), func(t *testing.T) {
			pushBrevoWebhookTestUsers(t)
			events := []any{}
			for _, event := range onboardingBrevoWebhookEvents {
				if event != "delivered" && event != "click" {
					events = append(events, event)
				}
			}
			fake := newFakeBrevoWebhooks(t, map[string]any{
				"id":          12,
				"type":        "transactional",
				"url":         brevoWebhookTestTarget,
				"description": "URnetwork onboarding campaign (server-registered)",
				"events":      events,
				"batched":     true,
			})

			outcome := ensureBrevoWebhookForTest(t, carrier)

			updates := fake.requestsFor(http.MethodPut, "/v3/webhooks/12")
			if len(updates) != 1 || fake.writes() != 1 {
				t.Fatalf("writes = %+v", fake.requests)
			}
			updated := updates[0].body
			if updated["batched"] != false || updated["description"] != "URnetwork onboarding campaign (server-registered)" {
				t.Fatalf("update batched %v description %v", updated["batched"], updated["description"])
			}
			updatedEvents := fakeBrevoStrings(t, updated["events"])
			for _, event := range onboardingBrevoWebhookEvents {
				if !slices.Contains(updatedEvents, event) {
					t.Fatalf("update lacks the event %s", event)
				}
			}
			switch carrier {
			case brevoWebhookCredentialUrl:
				if updated["url"] != brevoWebhookTestUrlWithUser {
					t.Fatal("updated url does not carry the credential")
				}
				if _, ok := updated["headers"]; ok {
					t.Fatal("a url credential update sent headers")
				}
			case brevoWebhookCredentialHeader:
				if updated["url"] != brevoWebhookTestTarget {
					t.Fatal("a header credential update put a credential in the url")
				}
				if headers := fakeBrevoHeaders(t, updated["headers"]); len(headers) != 1 || headers["Authorization"] != brevoWebhookTestAuthorization {
					t.Fatal("updated headers are not exactly the Basic value")
				}
			}
			for _, want := range []string{
				"updated: transactional webhook 12 ",
				"(it had no credential)",
				"events: added delivered, click",
				"batching: turned off",
				"check: Brevo's copy carries the credential and every event, unbatched",
			} {
				if !strings.Contains(outcome, want) {
					t.Fatalf("outcome lacks %q:\n%s", want, outcome)
				}
			}

			// a second run finds nothing left to do
			second := ensureBrevoWebhookForTest(t, carrier)
			if !strings.HasPrefix(second, "exists: transactional webhook 12 ") || fake.writes() != 1 {
				t.Fatalf("second run changed something:\n%s", second)
			}
		})
	}
}

// A url carrying credentials is still this endpoint: the old registration
// compared whole urls and would have created a second webhook. Its credential
// is replaced, and never printed.
func TestEnsureBrevoWebhookRecognizesUrlWithCredentials(t *testing.T) {
	pushBrevoWebhookTestUsers(t)
	fake := newFakeBrevoWebhooks(t, map[string]any{
		"id":     21,
		"type":   "transactional",
		"url":    "https://someone-else:hunter2-password@API.example.invalid/updates/brevo/",
		"events": allBrevoWebhookEvents(),
	})

	outcome := ensureBrevoWebhookForTest(t, brevoWebhookCredentialUrl)

	if len(fake.requestsFor(http.MethodPost, "/v3/webhooks")) != 0 {
		t.Fatal("created a second webhook for an endpoint whose url carries credentials")
	}
	updates := fake.requestsFor(http.MethodPut, "/v3/webhooks/21")
	if len(updates) != 1 || updates[0].body["url"] != brevoWebhookTestUrlWithUser {
		t.Fatalf("updates = %+v", updates)
	}
	if strings.Contains(outcome, "someone-else") || strings.Contains(outcome, "hunter2-password") {
		t.Fatalf("outcome prints the replaced credential:\n%s", outcome)
	}
	if !strings.Contains(outcome, "(it had a credential in the url that this server does not accept)") {
		t.Fatalf("outcome:\n%s", outcome)
	}
}

// A webhook of another type at the endpoint is not the transactional webhook:
// it is left as it is and a transactional one is created.
func TestEnsureBrevoWebhookIgnoresOtherType(t *testing.T) {
	pushBrevoWebhookTestUsers(t)
	fake := newFakeBrevoWebhooks(t, map[string]any{
		"id":     3,
		"type":   "marketing",
		"url":    brevoWebhookTestTarget,
		"events": []any{"unsubscribed"},
	})

	outcome := ensureBrevoWebhookForTest(t, brevoWebhookCredentialUrl)

	if len(fake.requestsFor(http.MethodPut, "/v3/webhooks/3")) != 0 {
		t.Fatal("changed the marketing webhook")
	}
	creates := fake.requestsFor(http.MethodPost, "/v3/webhooks")
	if len(creates) != 1 || creates[0].body["type"] != "transactional" {
		t.Fatalf("creates = %+v", creates)
	}
	if !strings.HasPrefix(outcome, "created: transactional webhook 4 ") ||
		!strings.Contains(outcome, "note: webhook 3 (marketing) also posts to this endpoint") {
		t.Fatalf("outcome:\n%s", outcome)
	}
}

// Of two transactional webhooks at the endpoint the one with everything
// stays; the other is reported, with the refusals it causes, and not touched.
func TestEnsureBrevoWebhookReportsDuplicate(t *testing.T) {
	pushBrevoWebhookTestUsers(t)
	fake := newFakeBrevoWebhooks(
		t,
		map[string]any{"id": 30, "type": "transactional", "url": brevoWebhookTestTarget, "events": allBrevoWebhookEvents()},
		map[string]any{"id": 31, "type": "transactional", "url": brevoWebhookTestUrlWithUser, "events": allBrevoWebhookEvents()},
	)

	outcome := ensureBrevoWebhookForTest(t, brevoWebhookCredentialUrl)

	if fake.writes() != 0 {
		t.Fatalf("writes = %+v", fake.requests)
	}
	if !strings.HasPrefix(outcome, "exists: transactional webhook 31 ") ||
		!strings.Contains(outcome, "warning: webhook 30 https://api.example.invalid/updates/brevo also posts transactional events to this endpoint") ||
		!strings.Contains(outcome, "it carries no accepted credential") {
		t.Fatalf("outcome:\n%s", outcome)
	}
}

// Brevo's error text can echo the url with its credential: the error never
// carries it.
func TestEnsureBrevoWebhookRedactsProviderErrors(t *testing.T) {
	pushBrevoWebhookTestUsers(t)
	fake := newFakeBrevoWebhooks(t)
	fake.rejectStatus = http.StatusBadRequest
	fake.rejectBody = `{"code":"invalid_parameter","message":"url ` + brevoWebhookTestUrlWithUser + ` rejected for ` + brevoWebhookTestUser + `"}`

	_, err := ensureBrevoTransactionalWebhook(context.Background(), brevoWebhookTestTarget, onboardingBrevoWebhookEvents, brevoWebhookCredentialUrl)
	if err == nil {
		t.Fatal("a rejected create succeeded")
	}
	requireNoBrevoWebhookSecret(t, err.Error())
	if !strings.Contains(err.Error(), "status 400") || !strings.Contains(err.Error(), "code invalid_parameter") {
		t.Fatalf("error: %v", err)
	}
}

// Nothing is registered from a configured url that carries a credential or is
// not https, or with a user that a url credential would escape.
func TestEnsureBrevoWebhookRefusesUnsafeConfiguration(t *testing.T) {
	pushBrevoWebhookTestUsers(t)
	fake := newFakeBrevoWebhooks(t)
	for _, webhookUrl := range []string{
		"https://configured-user:configured-password@api.example.invalid/updates/brevo",
		"http://api.example.invalid/updates/brevo",
		"api.example.invalid/updates/brevo",
	} {
		_, err := ensureBrevoTransactionalWebhook(context.Background(), webhookUrl, onboardingBrevoWebhookEvents, brevoWebhookCredentialUrl)
		if err == nil || strings.Contains(err.Error(), "configured-password") || strings.Contains(err.Error(), "configured-user") {
			t.Fatalf("configured url was not refused safely: %v", err)
		}
	}

	t.Cleanup(server.Vault.PushSimpleResource("brevo.yml", []byte("brevo:\n  webhook_bearers:\n    - 'synthetic user/with:reserved@characters'\n")))
	_, err := ensureBrevoTransactionalWebhook(context.Background(), brevoWebhookTestTarget, onboardingBrevoWebhookEvents, brevoWebhookCredentialUrl)
	if err == nil || !strings.Contains(err.Error(), "--credential=header") {
		t.Fatalf("a user a url would escape was not refused: %v", err)
	}
	if len(fake.requests) != 0 {
		t.Fatalf("a refused configuration reached Brevo: %+v", fake.requests)
	}

	if _, err := parseBrevoWebhookCredentialCarrier("bearer"); err == nil {
		t.Fatal("an unknown carrier was accepted")
	}
	if carrier, err := parseBrevoWebhookCredentialCarrier(""); err != nil || carrier != brevoWebhookCredentialUrl {
		t.Fatalf("the default carrier is %q, %v", carrier, err)
	}
}

// Each request to POST /updates/brevo is counted by result, a refusal by its
// reason, before any event is recorded.
func TestBrevoWebhookRefusalsAreCountedByReason(t *testing.T) {
	results := []string{
		brevoWebhookAccepted,
		brevoWebhookRefusedNoCredential,
		brevoWebhookRefusedWrongCredential,
		brevoWebhookRefusedVerificationUnavailable,
	}
	counts := func() map[string]float64 {
		counts := map[string]float64{}
		for _, result := range results {
			counts[result] = testutil.ToFloat64(brevoWebhookRequestsTotal.WithLabelValues(result))
		}
		return counts
	}
	call := func(header map[string][]string) error {
		t.Helper()
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("brevo webhook panicked instead of refusing: %v", r)
			}
		}()
		_, err := BrevoWebhook(&BrevoWebhookArgs{}, &session.ClientSession{Ctx: context.Background(), Header: header})
		return err
	}
	before := counts()

	pop := server.Vault.PushSimpleResource("brevo.yml", []byte("brevo:\n  webhook_bearers: []\n"))
	if err := call(map[string][]string{"Authorization": {brevoWebhookTestAuthorization}}); !errors.Is(err, errWebhookVerificationUnavailable) {
		t.Errorf("without users: %v", err)
	}
	pop()

	pushBrevoWebhookTestUsers(t)
	for _, header := range []map[string][]string{
		{},
		{"Authorization": {""}},
	} {
		if err := call(header); !errors.Is(err, errBrevoWebhookNotAuthorized) {
			t.Errorf("without a credential: %v", err)
		}
	}
	for _, authorization := range []string{
		"Basic " + base64.StdEncoding.EncodeToString([]byte("synthetic-other-user:")),
		// the right user, as a bearer token rather than Basic
		"Bearer " + brevoWebhookTestUser,
	} {
		if err := call(map[string][]string{"Authorization": {authorization}}); !errors.Is(err, errBrevoWebhookNotAuthorized) {
			t.Errorf("with a wrong credential: %v", err)
		}
	}
	if err := call(map[string][]string{"Authorization": {brevoWebhookTestAuthorization}}); err != nil {
		t.Errorf("with the credential: %v", err)
	}

	after := counts()
	for result, want := range map[string]float64{
		brevoWebhookAccepted:                       1,
		brevoWebhookRefusedNoCredential:            2,
		brevoWebhookRefusedWrongCredential:         2,
		brevoWebhookRefusedVerificationUnavailable: 1,
	} {
		if got := after[result] - before[result]; got != want {
			t.Errorf("%s counted %v, want %v", result, got, want)
		}
	}
	if brevoWebhookRefusalReports.interval != time.Minute {
		t.Errorf("refusals are reported every %s, not at most once a minute", brevoWebhookRefusalReports.interval)
	}
	if schemes := brevoAuthorizationSchemes([]string{"Bearer x", "basic y", "Token z", "Basic w"}); schemes != "Bearer, Basic, other" {
		t.Errorf("schemes = %q", schemes)
	}
}
