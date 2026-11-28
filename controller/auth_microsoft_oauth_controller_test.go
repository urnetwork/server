package controller

// Sign in with Microsoft's start and callback (auth_microsoft_oauth_controller.go):
// the start sends the browser to Microsoft with the server's own client, the
// callback exchanges the code and returns the identity token to the app's
// scheme or, for the web, in ur.io's fragment, and every refusal goes back to
// the attempt without reaching Microsoft.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

type microsoftOAuthFakeToken struct {
	server   *httptest.Server
	lastForm url.Values
	requests int
	status   int
	body     map[string]any
}

func newMicrosoftOAuthFakeToken(t *testing.T) *microsoftOAuthFakeToken {
	t.Helper()
	fake := &microsoftOAuthFakeToken{status: http.StatusOK, body: map[string]any{"id_token": "h.p.s", "access_token": "eyJ0"}}
	fake.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fake.requests += 1
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

func withMicrosoftOAuthTestClient(t *testing.T, fake *microsoftOAuthFakeToken, client *MicrosoftOAuthClient) {
	t.Helper()
	previousUrl := microsoftOAuthTokenUrl
	previousClient := microsoftOAuthClientFunc
	if fake != nil {
		microsoftOAuthTokenUrl = fake.server.URL
	}
	microsoftOAuthClientFunc = func() (*MicrosoftOAuthClient, bool) {
		return client, client != nil
	}
	t.Cleanup(func() {
		microsoftOAuthTokenUrl = previousUrl
		microsoftOAuthClientFunc = previousClient
	})
}

func microsoftOAuthTestClient() *MicrosoftOAuthClient {
	return &MicrosoftOAuthClient{ClientId: "app-client-id", ClientSecret: "app-client-secret"}
}

const microsoftOAuthTestNonce = "n0nce-ABCDEFGHIJKLMNOPQRSTUVWXYZ_0123456789"

func microsoftOAuthTestRequest(path string, query url.Values) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil)
	req.Host = "api.bringyour.com"
	return req
}

func microsoftOAuthTestLocation(t *testing.T, rec *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("expected no-store, got %q", rec.Header().Get("Cache-Control"))
	}
	location, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	return location
}

func TestMicrosoftOAuthStartSendsTheBrowserToMicrosoft(t *testing.T) {
	withMicrosoftOAuthTestClient(t, nil, microsoftOAuthTestClient())
	state := appleOAuthTestState("android")
	rec := httptest.NewRecorder()
	MicrosoftOAuthStart(rec, microsoftOAuthTestRequest("/auth/microsoft/start", url.Values{"state": {state}, "nonce": {microsoftOAuthTestNonce}}))
	location := microsoftOAuthTestLocation(t, rec)
	if location.Scheme+"://"+location.Host+location.Path != microsoftOAuthAuthorizeUrl {
		t.Fatalf("redirect to %q", location.String())
	}
	q := location.Query()
	for key, want := range map[string]string{
		"client_id":     "app-client-id",
		"response_type": "code",
		"response_mode": "query",
		"redirect_uri":  "https://api.bringyour.com/auth/microsoft/callback",
		"scope":         "openid email profile",
		"state":         state,
		"nonce":         microsoftOAuthTestNonce,
		"prompt":        "select_account",
	} {
		if q.Get(key) != want {
			t.Errorf("%s = %q, want %q", key, q.Get(key), want)
		}
	}
	if q.Has("client_secret") {
		t.Fatal("the start leaked the client secret")
	}
}

// A malformed state has nowhere to go back to; a malformed nonce or a server
// without a Microsoft client goes back to the app with the error, never to
// Microsoft; the web platform goes back to ur.io's fragment.
func TestMicrosoftOAuthStartRefusals(t *testing.T) {
	withMicrosoftOAuthTestClient(t, nil, microsoftOAuthTestClient())
	for _, state := range []string{"", "has space", "has/slash", string(make([]byte, 1025))} {
		rec := httptest.NewRecorder()
		MicrosoftOAuthStart(rec, microsoftOAuthTestRequest("/auth/microsoft/start", url.Values{"state": {state}, "nonce": {microsoftOAuthTestNonce}}))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("state %q: status %d, want 400", state, rec.Code)
		}
	}

	state := appleOAuthTestState("windows")
	for _, nonce := range []string{"", "short", "has space in it and is long enough"} {
		rec := httptest.NewRecorder()
		MicrosoftOAuthStart(rec, microsoftOAuthTestRequest("/auth/microsoft/start", url.Values{"state": {state}, "nonce": {nonce}}))
		location := microsoftOAuthTestLocation(t, rec)
		if location.Scheme != "urnetwork" || location.Host != "oauth" || location.Path != "/microsoft" || location.Query().Get("error") != "invalid_request" || location.Query().Get("state") != state {
			t.Errorf("nonce %q: redirect %q", nonce, location.String())
		}
	}

	withMicrosoftOAuthTestClient(t, nil, nil)
	rec := httptest.NewRecorder()
	MicrosoftOAuthStart(rec, microsoftOAuthTestRequest("/auth/microsoft/start", url.Values{"state": {appleOAuthTestState("ios")}, "nonce": {microsoftOAuthTestNonce}}))
	location := microsoftOAuthTestLocation(t, rec)
	if location.Scheme != "urnetwork" || location.Query().Get("error") != "not_configured" {
		t.Errorf("not configured: redirect %q", location.String())
	}

	webState := appleOAuthTestState("web")
	rec = httptest.NewRecorder()
	MicrosoftOAuthStart(rec, microsoftOAuthTestRequest("/auth/microsoft/start", url.Values{"state": {webState}, "nonce": {microsoftOAuthTestNonce}}))
	location = microsoftOAuthTestLocation(t, rec)
	if location.Scheme+"://"+location.Host+location.Path != microsoftOAuthDefaultWebReturnUrl || location.RawQuery != "" {
		t.Fatalf("web not configured: redirect %q", location.String())
	}
	fragment, err := url.ParseQuery(location.Fragment)
	if err != nil || fragment.Get("error") != "not_configured" || fragment.Get("state") != webState {
		t.Fatalf("web not configured: fragment %q", location.Fragment)
	}
}

func TestMicrosoftOAuthCallbackExchangesTheCode(t *testing.T) {
	fake := newMicrosoftOAuthFakeToken(t)
	withMicrosoftOAuthTestClient(t, fake, microsoftOAuthTestClient())

	for platform, scheme := range map[string]string{"android": "ur", "ios": "urnetwork", "macos": "urnetwork", "windows": "urnetwork", "linux": "urnetwork"} {
		state := appleOAuthTestState(platform)
		rec := httptest.NewRecorder()
		MicrosoftOAuthCallback(rec, microsoftOAuthTestRequest(microsoftOAuthCallbackPath, url.Values{"state": {state}, "code": {"M.C5_BAY.c0de"}}))
		location := microsoftOAuthTestLocation(t, rec)
		if location.Scheme != scheme || location.Host != "oauth" || location.Path != "/microsoft" {
			t.Errorf("%s: redirect %q", platform, location.String())
		}
		q := location.Query()
		if q.Get("state") != state || q.Get("id_token") != "h.p.s" || q.Has("error") {
			t.Errorf("%s: query %v", platform, q)
		}
	}
	form := fake.lastForm
	for key, want := range map[string]string{
		"grant_type":    "authorization_code",
		"code":          "M.C5_BAY.c0de",
		"client_id":     "app-client-id",
		"client_secret": "app-client-secret",
		"redirect_uri":  "https://api.bringyour.com/auth/microsoft/callback",
		"scope":         "openid email profile",
	} {
		if form.Get(key) != want {
			t.Errorf("exchange %s = %q, want %q", key, form.Get(key), want)
		}
	}
}

// The web platform's identity token goes back in the fragment of ur.io's
// callback page, never in a query a server would log.
func TestMicrosoftOAuthCallbackReturnsTheWebInTheFragment(t *testing.T) {
	fake := newMicrosoftOAuthFakeToken(t)
	client := microsoftOAuthTestClient()
	withMicrosoftOAuthTestClient(t, fake, client)

	state := appleOAuthTestState("web")
	rec := httptest.NewRecorder()
	MicrosoftOAuthCallback(rec, microsoftOAuthTestRequest(microsoftOAuthCallbackPath, url.Values{"state": {state}, "code": {"c0de"}}))
	location := microsoftOAuthTestLocation(t, rec)
	if location.Scheme+"://"+location.Host+location.Path != microsoftOAuthDefaultWebReturnUrl || location.RawQuery != "" {
		t.Fatalf("redirect %q", location.String())
	}
	fragment, err := url.ParseQuery(location.Fragment)
	if err != nil || fragment.Get("state") != state || fragment.Get("id_token") != "h.p.s" {
		t.Fatalf("fragment %q", location.Fragment)
	}

	client.WebReturnUrl = "https://beta.ur.io/auth/microsoft/callback"
	rec = httptest.NewRecorder()
	MicrosoftOAuthCallback(rec, microsoftOAuthTestRequest(microsoftOAuthCallbackPath, url.Values{"state": {state}, "code": {"c0de"}}))
	location = microsoftOAuthTestLocation(t, rec)
	if location.Host != "beta.ur.io" {
		t.Fatalf("the pinned web return: %q", location.String())
	}
}

func TestMicrosoftOAuthCallbackPassesErrorsThrough(t *testing.T) {
	fake := newMicrosoftOAuthFakeToken(t)
	withMicrosoftOAuthTestClient(t, fake, microsoftOAuthTestClient())

	state := appleOAuthTestState("linux")
	rec := httptest.NewRecorder()
	MicrosoftOAuthCallback(rec, microsoftOAuthTestRequest(microsoftOAuthCallbackPath, url.Values{"state": {state}, "error": {"access_denied"}, "error_description": {"AADSTS65004: User declined to consent."}, "code": {"ignored"}}))
	q := microsoftOAuthTestLocation(t, rec).Query()
	if q.Get("error") != "access_denied" || q.Has("id_token") || fake.requests != 0 {
		t.Fatalf("query %v, %d exchanges", q, fake.requests)
	}

	rec = httptest.NewRecorder()
	MicrosoftOAuthCallback(rec, microsoftOAuthTestRequest(microsoftOAuthCallbackPath, url.Values{"code": {"c0de"}}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("no state: status %d, want 400", rec.Code)
	}
}

func TestMicrosoftOAuthCallbackExchangeFailures(t *testing.T) {
	fake := newMicrosoftOAuthFakeToken(t)
	withMicrosoftOAuthTestClient(t, fake, microsoftOAuthTestClient())
	state := appleOAuthTestState("windows")
	for _, c := range []struct {
		status int
		body   map[string]any
		want   string
	}{
		{http.StatusBadRequest, map[string]any{"error": "invalid_grant", "error_description": "AADSTS70008: expired. Trace ID: x"}, "Microsoft rejected the sign-in: invalid_grant."},
		{http.StatusOK, map[string]any{"access_token": "only"}, "Microsoft did not return an identity token."},
		{http.StatusInternalServerError, map[string]any{}, "Microsoft's token endpoint answered 500."},
	} {
		fake.status, fake.body = c.status, c.body
		rec := httptest.NewRecorder()
		MicrosoftOAuthCallback(rec, microsoftOAuthTestRequest(microsoftOAuthCallbackPath, url.Values{"state": {state}, "code": {"c0de"}}))
		q := microsoftOAuthTestLocation(t, rec).Query()
		if q.Get("error") != c.want || q.Has("id_token") {
			t.Errorf("%d %v: query %v, want error %q", c.status, c.body, q, c.want)
		}
	}

	withMicrosoftOAuthTestClient(t, fake, nil)
	rec := httptest.NewRecorder()
	MicrosoftOAuthCallback(rec, microsoftOAuthTestRequest(microsoftOAuthCallbackPath, url.Values{"state": {state}, "code": {"c0de"}}))
	if q := microsoftOAuthTestLocation(t, rec).Query(); q.Get("error") != "not_configured" {
		t.Fatalf("not configured: %v", q)
	}
}
