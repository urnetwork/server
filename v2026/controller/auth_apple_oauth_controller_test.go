package controller

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func appleOAuthTestState(platform string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(`{"platform":"` + platform + `","token":"abc123"}`))
}

func TestOAuthPlatformForState(t *testing.T) {
	cases := map[string]string{
		appleOAuthTestState("windows"):                                    "windows",
		appleOAuthTestState("linux"):                                      "linux",
		appleOAuthTestState("android"):                                    "android",
		appleOAuthTestState("Windows"):                                    "windows",
		base64.URLEncoding.EncodeToString([]byte(`{"platform":"linux"}`)): "linux",
		base64.StdEncoding.EncodeToString([]byte(`{"platform":"linux"}`)): "linux",
		"plainrandomstate":                                                "",
		base64.RawURLEncoding.EncodeToString([]byte(`not json`)):          "",
		base64.RawURLEncoding.EncodeToString([]byte(`{"token":"x"}`)):     "",
		"": "",
	}
	for state, platform := range cases {
		if got := oauthPlatformForState(state); got != platform {
			t.Errorf("state %q: platform %q, want %q", state, got, platform)
		}
	}
}

func TestOAuthSchemeForState(t *testing.T) {
	cases := map[string]string{
		appleOAuthTestState("android"): "ur",
		appleOAuthTestState("windows"): "urnetwork",
		appleOAuthTestState("linux"):   "urnetwork",
		appleOAuthTestState("ios"):     "ur",
		"opaque":                       "ur",
	}
	for state, scheme := range cases {
		if got := oauthSchemeForState(state); got != scheme {
			t.Errorf("state %q: scheme %q, want %q", state, got, scheme)
		}
	}
}

func TestAppleOAuthReturnLocation(t *testing.T) {
	state := appleOAuthTestState("windows")
	location, ok := AppleOAuthReturnLocation(url.Values{
		"state":    {state},
		"id_token": {"h.p.s"},
		"code":     {"c0de"},
		"user":     {`{"name":{"firstName":"Ada"},"email":"ada@example.com"}`},
	})
	if !ok {
		t.Fatal("expected a location")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Scheme != "urnetwork" || parsed.Host != "oauth" || parsed.Path != "/apple" {
		t.Errorf("unexpected redirect target %q", location)
	}
	q := parsed.Query()
	if q.Get("state") != state || q.Get("id_token") != "h.p.s" || q.Get("code") != "c0de" {
		t.Errorf("unexpected query %v", q)
	}
	if !strings.Contains(q.Get("user"), "Ada") {
		t.Errorf("user was not passed through: %v", q)
	}
	if q.Has("error") {
		t.Errorf("no error expected: %v", q)
	}

	// an error from apple is handed back as the error, nothing else
	location, ok = AppleOAuthReturnLocation(url.Values{
		"state": {state},
		"error": {"user_cancelled_authorize"},
		"code":  {"ignored"},
	})
	if !ok {
		t.Fatal("expected a location")
	}
	parsed, _ = url.Parse(location)
	q = parsed.Query()
	if q.Get("error") != "user_cancelled_authorize" || q.Has("code") || q.Has("id_token") {
		t.Errorf("unexpected error query %v", q)
	}

	// no token and no error: the app gets an error rather than a bare state
	location, _ = AppleOAuthReturnLocation(url.Values{"state": {state}})
	parsed, _ = url.Parse(location)
	if parsed.Query().Get("error") == "" {
		t.Errorf("expected an error for a return without a token: %q", location)
	}

	// no state: nothing to hand back
	if _, ok := AppleOAuthReturnLocation(url.Values{"id_token": {"h.p.s"}}); ok {
		t.Error("expected no location without a state")
	}
}

func TestAppleOAuthCallbackHandler(t *testing.T) {
	// apple posts a form (response_mode=form_post)
	form := url.Values{
		"state":    {appleOAuthTestState("linux")},
		"id_token": {"h.p.s"},
		"code":     {"c0de"},
	}
	req := httptest.NewRequest(http.MethodPost, "/auth/apple/callback", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	AppleOAuthCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	location := rec.Header().Get("Location")
	if !strings.HasPrefix(location, "urnetwork://oauth/apple?") {
		t.Errorf("unexpected location %q", location)
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("expected no-store, got %q", rec.Header().Get("Cache-Control"))
	}

	// a GET with the same parameters works the same way (manual testing)
	req = httptest.NewRequest(http.MethodGet, "/auth/apple/callback?"+url.Values{
		"state": {appleOAuthTestState("android")},
		"error": {"user_cancelled_authorize"},
	}.Encode(), nil)
	rec = httptest.NewRecorder()
	AppleOAuthCallback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status %d, want 302", rec.Code)
	}
	if location := rec.Header().Get("Location"); !strings.HasPrefix(location, "ur://oauth/apple?") || !strings.Contains(location, "error=user_cancelled_authorize") {
		t.Errorf("unexpected location %q", location)
	}

	// no state is a bad request
	req = httptest.NewRequest(http.MethodPost, "/auth/apple/callback", strings.NewReader("id_token=h.p.s"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	AppleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", rec.Code)
	}

	// an oversized body is refused
	big := "state=" + strings.Repeat("a", appleOAuthCallbackMaxBodyBytes+1)
	req = httptest.NewRequest(http.MethodPost, "/auth/apple/callback", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	AppleOAuthCallback(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400 for an oversized body", rec.Code)
	}

	// other methods are refused
	req = httptest.NewRequest(http.MethodPut, "/auth/apple/callback", nil)
	rec = httptest.NewRecorder()
	AppleOAuthCallback(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status %d, want 405", rec.Code)
	}
}
