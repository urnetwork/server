package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// a fake google token endpoint: records the form it was sent and answers
// what the test configured
type googleOAuthFakeToken struct {
	server   *httptest.Server
	lastForm url.Values
	status   int
	body     map[string]any
}

func newGoogleOAuthFakeToken(t *testing.T) *googleOAuthFakeToken {
	t.Helper()
	fake := &googleOAuthFakeToken{status: http.StatusOK, body: map[string]any{"id_token": "h.p.s", "access_token": "ya29"}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		fake.lastForm = r.PostForm
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fake.status)
		json.NewEncoder(w).Encode(fake.body)
	}))
	t.Cleanup(fake.server.Close)
	return fake
}

func withGoogleOAuthTestClient(t *testing.T, fake *googleOAuthFakeToken, client *GoogleOAuthClient) {
	t.Helper()
	previousUrl := googleOAuthTokenUrl
	previousClient := googleOAuthClientFunc
	googleOAuthTokenUrl = fake.server.URL
	googleOAuthClientFunc = func() (*GoogleOAuthClient, bool) {
		return client, client != nil
	}
	t.Cleanup(func() {
		googleOAuthTokenUrl = previousUrl
		googleOAuthClientFunc = previousClient
	})
}

func googleOAuthTestClient() *GoogleOAuthClient {
	return &GoogleOAuthClient{ClientId: "web-client-id", ClientSecret: "web-client-secret"}
}

func googleOAuthCallbackRequest(query url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodGet, googleOAuthCallbackPath+"?"+query.Encode(), nil)
	req.Host = "api.bringyour.com"
	return req
}

func TestGoogleOAuthCallbackExchangesTheCode(t *testing.T) {
	fake := newGoogleOAuthFakeToken(t)
	withGoogleOAuthTestClient(t, fake, googleOAuthTestClient())

	state := appleOAuthTestState("windows")
	rec := httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"state": {state}, "code": {"4/c0de"}}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("expected no-store, got %q", rec.Header().Get("Cache-Control"))
	}
	parsed, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "urnetwork" || parsed.Host != "oauth" || parsed.Path != "/google" {
		t.Errorf("unexpected redirect target %q", parsed.String())
	}
	q := parsed.Query()
	if q.Get("state") != state || q.Get("id_token") != "h.p.s" || q.Has("error") {
		t.Errorf("unexpected query %v", q)
	}

	// the exchange carried the code, the client and the callback url the app
	// registered the code against (this request's own origin)
	form := fake.lastForm
	if form.Get("grant_type") != "authorization_code" || form.Get("code") != "4/c0de" {
		t.Errorf("unexpected exchange form %v", form)
	}
	if form.Get("client_id") != "web-client-id" || form.Get("client_secret") != "web-client-secret" {
		t.Errorf("unexpected client in the exchange %v", form)
	}
	if form.Get("redirect_uri") != "https://api.bringyour.com/auth/google/callback" {
		t.Errorf("unexpected redirect_uri %q", form.Get("redirect_uri"))
	}
}

func TestGoogleOAuthCallbackRedirectUri(t *testing.T) {
	fake := newGoogleOAuthFakeToken(t)
	client := googleOAuthTestClient()
	withGoogleOAuthTestClient(t, fake, client)

	// the front proxy's forwarding headers name the public origin
	req := googleOAuthCallbackRequest(url.Values{})
	req.Host = "10.0.0.7:8080"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "api.bringyour.com")
	if got := googleOAuthRedirectUri(req); got != "https://api.bringyour.com/auth/google/callback" {
		t.Errorf("forwarded origin: got %q", got)
	}

	// a local plain-http server (manual testing) keeps http
	req = googleOAuthCallbackRequest(url.Values{})
	req.Host = "localhost:8080"
	if got := googleOAuthRedirectUri(req); got != "http://localhost:8080/auth/google/callback" {
		t.Errorf("localhost origin: got %q", got)
	}

	// the vault can pin the registered url outright
	client.RedirectUri = "https://api.example.net/auth/google/callback"
	if got := googleOAuthRedirectUri(req); got != client.RedirectUri {
		t.Errorf("pinned redirect: got %q", got)
	}
}

func TestGoogleOAuthCallbackPassesGoogleErrorsThrough(t *testing.T) {
	fake := newGoogleOAuthFakeToken(t)
	withGoogleOAuthTestClient(t, fake, googleOAuthTestClient())

	state := appleOAuthTestState("linux")
	rec := httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"state": {state}, "error": {"access_denied"}, "code": {"ignored"}}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	parsed, _ := url.Parse(rec.Header().Get("Location"))
	q := parsed.Query()
	if parsed.Scheme != "urnetwork" || q.Get("error") != "access_denied" || q.Has("id_token") {
		t.Errorf("unexpected error redirect %q", parsed.String())
	}
	if fake.lastForm != nil {
		t.Error("an error return must not be exchanged")
	}

	// no code and no error: the app gets an error rather than a bare state
	rec = httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"state": {state}}))
	parsed, _ = url.Parse(rec.Header().Get("Location"))
	if parsed.Query().Get("error") == "" {
		t.Errorf("expected an error for a return without a code: %q", parsed.String())
	}
}

func TestGoogleOAuthCallbackExchangeFailures(t *testing.T) {
	fake := newGoogleOAuthFakeToken(t)
	withGoogleOAuthTestClient(t, fake, googleOAuthTestClient())
	state := appleOAuthTestState("android")

	// google refuses the code (used, expired, wrong client)
	fake.status = http.StatusBadRequest
	fake.body = map[string]any{"error": "invalid_grant", "error_description": "Bad Request"}
	rec := httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"state": {state}, "code": {"stale"}}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	parsed, _ := url.Parse(rec.Header().Get("Location"))
	if parsed.Scheme != "ur" || !strings.Contains(parsed.Query().Get("error"), "invalid_grant") || parsed.Query().Has("id_token") {
		t.Errorf("unexpected exchange-failure redirect %q", parsed.String())
	}

	// a 200 without an identity token is an error too
	fake.status = http.StatusOK
	fake.body = map[string]any{"access_token": "ya29"}
	rec = httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"state": {state}, "code": {"c0de"}}))
	parsed, _ = url.Parse(rec.Header().Get("Location"))
	if parsed.Query().Get("error") == "" || parsed.Query().Has("id_token") {
		t.Errorf("expected an error without an identity token: %q", parsed.String())
	}

	// an unreadable answer is reported by status, never as a token
	fake.server.Close()
	rec = httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"state": {state}, "code": {"c0de"}}))
	parsed, _ = url.Parse(rec.Header().Get("Location"))
	if parsed.Query().Get("error") == "" || parsed.Query().Has("id_token") {
		t.Errorf("expected an error when the token endpoint is unreachable: %q", parsed.String())
	}
}

func TestGoogleOAuthCallbackNotConfigured(t *testing.T) {
	fake := newGoogleOAuthFakeToken(t)
	withGoogleOAuthTestClient(t, fake, nil)

	rec := httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"state": {appleOAuthTestState("windows")}, "code": {"c0de"}}))
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	parsed, _ := url.Parse(rec.Header().Get("Location"))
	if parsed.Query().Get("error") != "not_configured" {
		t.Errorf("expected not_configured, got %q", parsed.String())
	}
	if fake.lastForm != nil {
		t.Error("nothing must be sent to google without a client")
	}
}

func TestGoogleOAuthCallbackRequestShape(t *testing.T) {
	fake := newGoogleOAuthFakeToken(t)
	withGoogleOAuthTestClient(t, fake, googleOAuthTestClient())

	// no state is a bad request
	rec := httptest.NewRecorder()
	GoogleOAuthCallback(rec, googleOAuthCallbackRequest(url.Values{"code": {"c0de"}}))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}

	// google redirects with GET; nothing else is served
	req := httptest.NewRequest(http.MethodPost, googleOAuthCallbackPath, strings.NewReader("state=x&code=y"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	GoogleOAuthCallback(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", rec.Code)
	}
	if fake.lastForm != nil {
		t.Error("a refused request must not be exchanged")
	}
}
