package router

// A conditional JSON answer without a database: the entity tag of its exact
// body, scoped to the caller's network, the route and the scope; a 304
// without a body when If-None-Match names the tag; the full answer otherwise.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type conditionalTestResult struct {
	Providers []string `json:"providers"`
}

// A strong tag: 32 lowercase hex digits in double quotes.
var conditionalTestTag = regexp.MustCompile(`^"[0-9a-f]{32}"$`)

// A session of networkId, as the auth wrappers leave it.
func conditionalTestSession(networkId server.Id) *session.ClientSession {
	clientId := server.NewId()
	byJwt := session.NewByJwt(networkId, server.NewId(), "conditional", false, false).Client(server.NewId(), clientId)
	return session.Testing_CreateClientSession(context.Background(), byJwt)
}

// Answers GET path with result for clientSession and scope, with the given
// If-None-Match headers.
func conditionalTestServe(
	t *testing.T,
	clientSession *session.ClientSession,
	path string,
	scope string,
	result *conditionalTestResult,
	ifNoneMatch ...string,
) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for _, value := range ifNoneMatch {
		req.Header.Add("If-None-Match", value)
	}
	w := httptest.NewRecorder()
	if !ConditionalJsonFormatter[*conditionalTestResult](w, req, scope)(clientSession, result) {
		t.Fatal("the formatter left the answer to another formatter")
	}
	return w
}

// Every answer is the JSON body with a strong tag of it and a Cache-Control
// that has a cache revalidate before reuse; the same body answers the same
// tag every time.
func TestConditionalJsonAnswerCarriesItsTag(t *testing.T) {
	clientSession := conditionalTestSession(server.NewId())
	result := &conditionalTestResult{Providers: []string{"a", "b"}}

	w := conditionalTestServe(t, clientSession, "/network/provider-status", "format=full", result)
	if w.Code != http.StatusOK || w.Body.String() != `{"providers":["a","b"]}` {
		t.Fatalf("answer %d %q", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	if !conditionalTestTag.MatchString(etag) {
		t.Fatalf("ETag %q, want a strong tag of 32 hex digits", etag)
	}
	if contentType := w.Header().Get("Content-Type"); contentType != "application/json" {
		t.Fatalf("Content-Type %q", contentType)
	}
	if cacheControl := w.Header().Get("Cache-Control"); cacheControl != "private, no-cache" {
		t.Fatalf("Cache-Control %q", cacheControl)
	}

	again := conditionalTestServe(t, clientSession, "/network/provider-status", "format=full", &conditionalTestResult{Providers: []string{"a", "b"}})
	if again.Header().Get("ETag") != etag {
		t.Fatalf("the same body answered %s, then %s", etag, again.Header().Get("ETag"))
	}
}

// An If-None-Match that names the answer's tag, weakly or in a list of tags,
// or "*", is answered 304 with the tag and Cache-Control and no body. One that
// names no current tag, or is malformed, is answered in full.
func TestConditionalJsonAnswerNotModified(t *testing.T) {
	clientSession := conditionalTestSession(server.NewId())
	result := &conditionalTestResult{Providers: []string{"a"}}
	etag := conditionalTestServe(t, clientSession, "/p", "", result).Header().Get("ETag")

	for name, ifNoneMatch := range map[string][]string{
		"the tag":                        {etag},
		"the tag, weak":                  {"W/" + etag},
		"a list holding the tag":         {`"0123", ` + etag + ` , W/"4567"`},
		"one of several headers":         {`"0123"`, etag},
		"any":                            {"*"},
		"a list without spaces":          {`"0123",` + etag},
		"the tag with surrounding space": {" \t" + etag + " "},
	} {
		w := conditionalTestServe(t, clientSession, "/p", "", result, ifNoneMatch...)
		if w.Code != http.StatusNotModified || w.Body.Len() != 0 {
			t.Fatalf("%s: %d %q, want 304 and no body", name, w.Code, w.Body.String())
		}
		if w.Header().Get("ETag") != etag || w.Header().Get("Cache-Control") != "private, no-cache" || w.Header().Get("Content-Type") != "" {
			t.Fatalf("%s: headers %v", name, w.Header())
		}
	}

	for name, ifNoneMatch := range map[string][]string{
		"another tag":                {`"00000000000000000000000000000000"`},
		"the tag unquoted":           {etag[1 : len(etag)-1]},
		"a malformed list":           {`"0123" junk, ` + etag},
		"an empty header":            {""},
		"the tag with its quote cut": {etag[:len(etag)-1]},
	} {
		w := conditionalTestServe(t, clientSession, "/p", "", result, ifNoneMatch...)
		if w.Code != http.StatusOK || w.Body.String() != `{"providers":["a"]}` || w.Header().Get("ETag") != etag {
			t.Fatalf("%s: %d %q %v, want the full answer", name, w.Code, w.Body.String(), w.Header())
		}
	}
}

// The tag changes exactly when the body does: a changed body answers a new
// tag, and the old tag is answered in full with it.
func TestConditionalJsonTagFollowsTheBody(t *testing.T) {
	clientSession := conditionalTestSession(server.NewId())
	before := conditionalTestServe(t, clientSession, "/p", "", &conditionalTestResult{Providers: []string{"a"}})
	beforeTag := before.Header().Get("ETag")

	changed := &conditionalTestResult{Providers: []string{"a", "c"}}
	after := conditionalTestServe(t, clientSession, "/p", "", changed, beforeTag)
	afterTag := after.Header().Get("ETag")
	if after.Code != http.StatusOK || after.Body.String() != `{"providers":["a","c"]}` {
		t.Fatalf("a changed body against the old tag: %d %q", after.Code, after.Body.String())
	}
	if afterTag == beforeTag || !conditionalTestTag.MatchString(afterTag) {
		t.Fatalf("the tag %s did not follow the body (was %s)", afterTag, beforeTag)
	}
	if w := conditionalTestServe(t, clientSession, "/p", "", changed, afterTag); w.Code != http.StatusNotModified {
		t.Fatalf("the new tag: %d", w.Code)
	}
}

// Equal bodies have different tags for different networks, routes and
// scopes, so a tag never stands for another network's answer or another
// query's, and one network's tag is answered in full for another network.
// Two callers of one network with one body share a tag.
func TestConditionalJsonTagIsScoped(t *testing.T) {
	networkId := server.NewId()
	clientSession := conditionalTestSession(networkId)
	result := &conditionalTestResult{Providers: []string{}}
	tag := func(clientSession *session.ClientSession, path string, scope string) string {
		return conditionalTestServe(t, clientSession, path, scope, result).Header().Get("ETag")
	}

	base := tag(clientSession, "/network/provider-status", "client_id=&format=full")
	otherNetworkSession := conditionalTestSession(server.NewId())
	tags := map[string]string{
		"another network": tag(otherNetworkSession, "/network/provider-status", "client_id=&format=full"),
		"another scope":   tag(clientSession, "/network/provider-status", "client_id=&format=compact"),
		"another route":   tag(clientSession, "/network/other", "client_id=&format=full"),
		"no network":      tag(session.Testing_CreateClientSession(context.Background(), nil), "/network/provider-status", "client_id=&format=full"),
	}
	seen := map[string]string{base: "base"}
	for name, value := range tags {
		if other, ok := seen[value]; ok {
			t.Fatalf("%s shares the tag %s with %s", name, value, other)
		}
		seen[value] = name
	}

	if w := conditionalTestServe(t, otherNetworkSession, "/network/provider-status", "client_id=&format=full", result, base); w.Code != http.StatusOK || w.Body.String() != `{"providers":[]}` {
		t.Fatalf("another network's tag: %d %q", w.Code, w.Body.String())
	}

	sameNetworkSession := conditionalTestSession(networkId)
	if sameNetwork := tag(sameNetworkSession, "/network/provider-status", "client_id=&format=full"); sameNetwork != base {
		t.Fatalf("another caller of the network: %s, want %s", sameNetwork, base)
	}
}

// The weak comparison of If-None-Match, header by header and tag by tag.
func TestIfNoneMatch(t *testing.T) {
	const etag = `"abc"`
	for _, testCase := range []struct {
		ifNoneMatch []string
		match       bool
	}{
		{nil, false},
		{[]string{`"abc"`}, true},
		{[]string{`W/"abc"`}, true},
		{[]string{`"ab"`}, false},
		{[]string{`"abcd"`}, false},
		{[]string{`abc`}, false},
		{[]string{`"x", "abc"`}, true},
		{[]string{`"x",,"abc"`}, true},
		{[]string{`"x"`, `W/"abc"`}, true},
		{[]string{`*`}, true},
		{[]string{` * `}, true},
		{[]string{`"x" "abc"`}, true},
		{[]string{`"x", bad, "abc"`}, false},
		{[]string{`"a"bc"`}, false},
		{[]string{`W/abc`}, false},
		{[]string{`"`}, false},
		{[]string{`W/`}, false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/p", nil)
		for _, value := range testCase.ifNoneMatch {
			req.Header.Add("If-None-Match", value)
		}
		if match := IfNoneMatch(req, etag); match != testCase.match {
			t.Fatalf("If-None-Match %q: %t, want %t", testCase.ifNoneMatch, match, testCase.match)
		}
	}
}
