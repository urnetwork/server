package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urnetwork/server/session"
)

type optionalAuthTestArgs struct {
	Sort string `json:"sort"`
}

type optionalAuthTestResult struct {
	Sort     string `json:"sort"`
	SignedIn bool   `json:"signed_in"`
}

// WrapWithInputOptionalAuth serves a signed-out request like WrapWithInputNoAuth
// (the impl sees a session with no jwt and the decoded body), and refuses a
// present-but-invalid bearer token like WrapWithInputRequireAuth, so a stale
// token surfaces as a 401 instead of a silently signed-out page.
func TestWrapWithInputOptionalAuth(t *testing.T) {
	impl := func(args *optionalAuthTestArgs, clientSession *session.ClientSession) (*optionalAuthTestResult, error) {
		return &optionalAuthTestResult{
			Sort:     args.Sort,
			SignedIn: clientSession.ByJwt != nil,
		}, nil
	}

	// signed out: the impl runs with no jwt
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/", strings.NewReader(`{"sort":"blocks"}`))
	req.RemoteAddr = "127.0.0.1:52344"
	WrapWithInputOptionalAuth(impl, w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("signed out: status %d, want 200: %s", w.Code, w.Body.String())
	}
	if body := strings.TrimSpace(w.Body.String()); body != `{"sort":"blocks","signed_in":false}` {
		t.Fatalf("signed out: body %q", body)
	}

	// a bearer token that does not parse is a 401, and the impl never runs
	called := false
	w = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/", strings.NewReader(`{"sort":"blocks"}`))
	req.RemoteAddr = "127.0.0.1:52344"
	req.Header.Set("Authorization", "Bearer not.a.jwt")
	WrapWithInputOptionalAuth(
		func(args *optionalAuthTestArgs, clientSession *session.ClientSession) (*optionalAuthTestResult, error) {
			called = true
			return impl(args, clientSession)
		},
		w,
		req,
	)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("bad token: status %d, want 401: %s", w.Code, w.Body.String())
	}
	if called {
		t.Fatal("bad token: the impl ran")
	}
}
