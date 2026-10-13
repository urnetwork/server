// Cookie tests pin the no-cookies rule (cookies.go): every cookie header is
// dropped, in any letter case, and nothing else is.
package session

import (
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
)

func TestDropCookiesRemovesEveryCookieHeader(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.Header.Add("Cookie", "session=secret")
	req.Header.Add("Cookie", "other=value")
	req.Header["cookie"] = []string{"lower=case"}
	req.Header["COOKIE2"] = []string{`$Version="1"`}
	req.Header.Set("Authorization", "Bearer token")
	req.Header.Set("X-Cookie", "kept")

	DropCookies(req)

	if cookies := req.Cookies(); len(cookies) != 0 {
		t.Fatalf("cookies survived the drop: %v", cookies)
	}
	requireHeaderKeys(t, req.Header, "Authorization", "X-Cookie")
}

func TestDropSetCookiesRemovesEveryCookieHeader(t *testing.T) {
	header := http.Header{}
	header.Add("Set-Cookie", (&http.Cookie{Name: "session", Value: "secret", Domain: "bringyour.com"}).String())
	header["set-cookie"] = []string{"lower=case"}
	header["Set-Cookie2"] = []string{"old=1"}
	header.Set("Content-Type", "application/json")
	header.Set("Set-Cookies", "kept")

	DropSetCookies(header)

	requireHeaderKeys(t, header, "Content-Type", "Set-Cookies")
}

// The session is where handlers read headers, so it must hold none.
func TestNewClientSessionFromRequestDropsCookies(t *testing.T) {
	req := httptest.NewRequest("GET", "/hello", nil)
	req.Header.Set(urForwardedForHeader, "173.25.160.143:41001")
	req.Header.Set("Cookie", "session=secret")

	clientSession, err := NewClientSessionFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Cancel()

	requireHeaderKeys(t, clientSession.Header, http.CanonicalHeaderKey(urForwardedForHeader))
	requireHeaderKeys(t, req.Header, http.CanonicalHeaderKey(urForwardedForHeader))
}

// Fails unless header holds exactly the keys named.
func requireHeaderKeys(t *testing.T, header http.Header, keys ...string) {
	t.Helper()
	got := slices.Sorted(maps.Keys(header))
	want := slices.Sorted(slices.Values(keys))
	if !slices.Equal(got, want) {
		t.Fatalf("header keys = %v, want %v", got, want)
	}
}
