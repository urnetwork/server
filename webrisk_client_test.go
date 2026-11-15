package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The Web Risk client on a scripted transport (webrisk_client.go): lists brought current by raw diffs and checked by
// their checksum, a cold start fetching in full, only a local prefix match asking hashes.search, and that for the
// prefix alone, the key in a header and never in a url or an error. Every name is .example and the key a test
// string.

const testWebRiskKey = "test-web-risk-key"

// One request the fake saw.
type testWebRiskRequest struct {
	path   string
	query  url.Values
	apiKey string
	rawUrl string
}

// Answers each request with the next scripted answer for its path, and records it.
type testWebRiskTransport struct {
	stateLock sync.Mutex
	requests  []*testWebRiskRequest
	diffs     []func(request *testWebRiskRequest) (int, string)
	searches  []func(request *testWebRiskRequest) (int, string)
}

func (self *testWebRiskTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	seen := &testWebRiskRequest{
		path:   request.URL.Path,
		query:  request.URL.Query(),
		apiKey: request.Header.Get("X-Goog-Api-Key"),
		rawUrl: request.URL.String(),
	}
	self.requests = append(self.requests, seen)
	var answers *[]func(request *testWebRiskRequest) (int, string)
	switch request.URL.Path {
	case "/v1/threatLists:computeDiff":
		answers = &self.diffs
	case "/v1/hashes:search":
		answers = &self.searches
	default:
		return nil, errors.New("an unknown path")
	}
	if len(*answers) == 0 {
		return nil, errors.New("no answer scripted")
	}
	answer := (*answers)[0]
	*answers = (*answers)[1:]
	status, body := answer(seen)
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     http.Header{},
		Request:    request,
	}, nil
}

func (self *testWebRiskTransport) script(path string, answers ...func(request *testWebRiskRequest) (int, string)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch path {
	case "diff":
		self.diffs = append(self.diffs, answers...)
	case "search":
		self.searches = append(self.searches, answers...)
	}
}

func (self *testWebRiskTransport) seen() []*testWebRiskRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

func (self *testWebRiskTransport) seenOf(path string) []*testWebRiskRequest {
	requests := []*testWebRiskRequest{}
	for _, request := range self.seen() {
		if request.path == path {
			requests = append(requests, request)
		}
	}
	return requests
}

// Settings on a .example origin, one list unless more are given.
func testWebRiskSettings(threatTypes ...WebRiskThreatType) *WebRiskClientSettings {
	settings := DefaultWebRiskClientSettings()
	settings.BaseUrl = "https://webrisk.example"
	settings.ThreatTypes = []WebRiskThreatType{WebRiskThreatTypeSocialEngineering}
	if 0 < len(threatTypes) {
		settings.ThreatTypes = threatTypes
	}
	return settings
}

func newTestWebRiskClient(t *testing.T, transport *testWebRiskTransport, settings *WebRiskClientSettings) *WebRiskClient {
	t.Helper()
	client, err := NewWebRiskClient(&WebRiskClientArgs{ApiKey: testWebRiskKey, Transport: transport, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func testWebRiskHash(expression string) [sha256.Size]byte {
	return sha256.Sum256([]byte(expression))
}

// The checksum the API gives over a list's sorted prefixes.
func testWebRiskChecksum(prefixes ...string) string {
	sorted := slices.Sorted(slices.Values(prefixes))
	digest := sha256.New()
	for _, prefix := range sorted {
		io.WriteString(digest, prefix)
	}
	return base64.StdEncoding.EncodeToString(digest.Sum(nil))
}

// A computeDiff answer of responseType adding additions (all of one size) and removing at indices, checked against
// the list it leaves.
func testWebRiskDiff(responseType string, additions []string, indices []int, version string, leaves []string, next time.Time) func(*testWebRiskRequest) (int, string) {
	return func(request *testWebRiskRequest) (int, string) {
		answer := map[string]any{
			"responseType":    responseType,
			"newVersionToken": base64.StdEncoding.EncodeToString([]byte(version)),
			"checksum":        map[string]any{"sha256": testWebRiskChecksum(leaves...)},
		}
		if 0 < len(additions) {
			answer["additions"] = map[string]any{"rawHashes": []any{map[string]any{
				"prefixSize": len(additions[0]),
				"rawHashes":  base64.StdEncoding.EncodeToString([]byte(strings.Join(additions, ""))),
			}}}
		}
		if 0 < len(indices) {
			answer["removals"] = map[string]any{"rawIndices": map[string]any{"indices": indices}}
		}
		if !next.IsZero() {
			answer["recommendedNextDiff"] = next.Format(time.RFC3339Nano)
		}
		body, _ := json.Marshal(answer)
		return http.StatusOK, string(body)
	}
}

// A hashes.search answer listing hashes under threatType until expire, absent ones until negativeExpire.
func testWebRiskSearch(threatType WebRiskThreatType, expire time.Time, negativeExpire time.Time, hashes ...[sha256.Size]byte) func(*testWebRiskRequest) (int, string) {
	return func(request *testWebRiskRequest) (int, string) {
		threats := []any{}
		for _, hash := range hashes {
			threats = append(threats, map[string]any{
				"threatTypes": []string{string(threatType)},
				"hash":        base64.StdEncoding.EncodeToString(hash[:]),
				"expireTime":  expire.Format(time.RFC3339Nano),
			})
		}
		body, _ := json.Marshal(map[string]any{"threats": threats, "negativeExpireTime": negativeExpire.Format(time.RFC3339Nano)})
		return http.StatusOK, string(body)
	}
}

func testWebRiskPrefix(expression string) string {
	hash := testWebRiskHash(expression)
	return string(hash[:4])
}

func TestWebRiskHostExpressions(t *testing.T) {
	settings := DefaultWebRiskClientSettings()
	cases := []struct {
		host string
		want []string
	}{
		{host: "shop.net.root.example", want: []string{"shop.net.root.example/", "net.root.example/", "root.example/"}},
		{host: "root.example", want: []string{"root.example/"}},
		{host: "example", want: []string{"example/"}},
		{host: "a.b.c.d.e", want: []string{"a.b.c.d.e/", "b.c.d.e/", "c.d.e/", "d.e/"}},
		// at most four suffixes, from the last five labels
		{host: "a.b.c.d.e.f.g", want: []string{"a.b.c.d.e.f.g/", "c.d.e.f.g/", "d.e.f.g/", "e.f.g/", "f.g/"}},
	}
	for _, c := range cases {
		if got := WebRiskHostExpressions(c.host, settings); !slices.Equal(got, c.want) {
			t.Errorf("%s: %v, want %v", c.host, got, c.want)
		}
	}
}

// A reset fills a cold list, the next update sends its version and applies a diff's removals and additions, and a
// new client, a cold start, fetches in full again.
func TestWebRiskClientAppliesADiffAndAColdStartRefetches(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	transport := &testWebRiskTransport{}
	client := newTestWebRiskClient(t, transport, testWebRiskSettings())

	listed := testWebRiskPrefix("shop.net.root.example/")
	other := testWebRiskPrefix("other.root.example/")
	third := testWebRiskPrefix("third.root.example/")
	first := []string{listed, other}
	transport.script("diff", testWebRiskDiff("RESET", first, nil, "v1", first, time.Time{}))
	if err := client.Update(ctx, now); err != nil {
		t.Fatal(err)
	}
	if !client.Ready() {
		t.Fatal("a list fetched in full is not held")
	}
	requests := transport.seenOf("/v1/threatLists:computeDiff")
	if len(requests) != 1 || requests[0].query.Get("versionToken") != "" || requests[0].query.Get("threatType") != "SOCIAL_ENGINEERING" ||
		requests[0].query.Get("constraints.supportedCompressions") != "RAW" {
		t.Fatalf("the cold fetch: %+v", requests)
	}

	// remove the sorted list's entry for listed, add third
	sorted := slices.Sorted(slices.Values(first))
	removeIndex := slices.Index(sorted, listed)
	transport.script("diff", testWebRiskDiff("DIFF", []string{third}, []int{removeIndex}, "v2", []string{other, third}, time.Time{}))
	if err := client.Update(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requests = transport.seenOf("/v1/threatLists:computeDiff")
	if len(requests) != 2 || requests[1].query.Get("versionToken") != base64.StdEncoding.EncodeToString([]byte("v1")) {
		t.Fatalf("the diff did not send the list's version: %+v", requests[len(requests)-1].query)
	}
	list := client.currentList(WebRiskThreatTypeSocialEngineering)
	if !slices.Equal(list.prefixes, slices.Sorted(slices.Values([]string{other, third}))) || string(list.versionToken) != "v2" {
		t.Fatalf("the list after the diff: %q at %q", list.prefixes, list.versionToken)
	}
	// the removed prefix no longer matches, so its lookup asks nothing
	lookup, err := client.Lookup(ctx, testWebRiskHash("shop.net.root.example/"), now)
	if err != nil || lookup.PrefixMatch {
		t.Fatalf("a removed prefix matched: %+v, %v", lookup, err)
	}

	cold := newTestWebRiskClient(t, transport, testWebRiskSettings())
	transport.script("diff", testWebRiskDiff("RESET", []string{other, third}, nil, "v2", []string{other, third}, time.Time{}))
	if err := cold.Update(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requests = transport.seenOf("/v1/threatLists:computeDiff")
	if requests[len(requests)-1].query.Get("versionToken") != "" {
		t.Fatal("a cold start sent a version")
	}
}

// A diff that leaves a list off its checksum drops the list and fetches it in full; a full fetch off its checksum
// fails the update and keeps no list, so nothing is looked up against a list nobody vouches for.
func TestWebRiskClientChecksumMismatchRefetchesInFull(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	transport := &testWebRiskTransport{}
	client := newTestWebRiskClient(t, transport, testWebRiskSettings())
	listed := testWebRiskPrefix("shop.net.root.example/")
	other := testWebRiskPrefix("other.root.example/")
	transport.script("diff", testWebRiskDiff("RESET", []string{listed}, nil, "v1", []string{listed}, time.Time{}))
	if err := client.Update(ctx, now); err != nil {
		t.Fatal(err)
	}

	// a diff whose checksum names a list the diff does not leave, then the full list
	transport.script("diff",
		testWebRiskDiff("DIFF", []string{other}, nil, "v2", []string{other}, time.Time{}),
		testWebRiskDiff("RESET", []string{listed, other}, nil, "v2", []string{listed, other}, time.Time{}),
	)
	if err := client.Update(ctx, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	requests := transport.seenOf("/v1/threatLists:computeDiff")
	if len(requests) != 3 || requests[1].query.Get("versionToken") == "" || requests[2].query.Get("versionToken") != "" {
		t.Fatalf("the mismatch was not fetched again in full: %d requests", len(requests))
	}
	if list := client.currentList(WebRiskThreatTypeSocialEngineering); len(list.prefixes) != 2 {
		t.Fatalf("the list after the full fetch: %q", list.prefixes)
	}

	transport.script("diff",
		testWebRiskDiff("DIFF", nil, []int{0}, "v3", []string{listed, other}, time.Time{}),
		testWebRiskDiff("RESET", []string{listed}, nil, "v3", []string{other}, time.Time{}),
	)
	if err := client.Update(ctx, now.Add(2*time.Hour)); !errors.Is(err, ErrWebRiskChecksum) {
		t.Fatalf("two mismatches answered %v", err)
	}
	if client.Ready() {
		t.Fatal("a list off its checksum is still held")
	}
}

// Only raw diffs are taken, a removal must index an entry, a prefix size must be the protocol's, and a list waits for
// the time the API recommends. A refused answer leaves the list as it was.
func TestWebRiskClientRefusesWhatIsNotARawDiff(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	transport := &testWebRiskTransport{}
	client := newTestWebRiskClient(t, transport, testWebRiskSettings())
	listed := testWebRiskPrefix("shop.net.root.example/")
	transport.script("diff", testWebRiskDiff("RESET", []string{listed}, nil, "v1", []string{listed}, now.Add(30*time.Minute)))
	if err := client.Update(ctx, now); err != nil {
		t.Fatal(err)
	}
	// before the recommended time, no request at all
	if err := client.Update(ctx, now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if count := len(transport.seenOf("/v1/threatLists:computeDiff")); count != 1 {
		t.Fatalf("%d diffs asked before the recommended time", count)
	}

	refusals := []func(*testWebRiskRequest) (int, string){
		func(*testWebRiskRequest) (int, string) {
			return http.StatusOK, `{"responseType":"DIFF","additions":{"riceHashes":{"firstValue":"1"}},"checksum":{"sha256":"` + testWebRiskChecksum(listed) + `"}}`
		},
		func(*testWebRiskRequest) (int, string) {
			return http.StatusOK, `{"responseType":"DIFF","removals":{"rawIndices":{"indices":[5]}},"checksum":{"sha256":"` + testWebRiskChecksum(listed) + `"}}`
		},
		func(*testWebRiskRequest) (int, string) {
			return http.StatusOK, `{"responseType":"DIFF","additions":{"rawHashes":[{"prefixSize":3,"rawHashes":"YWJj"}]},"checksum":{"sha256":"` + testWebRiskChecksum(listed) + `"}}`
		},
		func(*testWebRiskRequest) (int, string) {
			return http.StatusOK, `{"responseType":"RESPONSE_TYPE_UNSPECIFIED"}`
		},
		func(*testWebRiskRequest) (int, string) {
			return http.StatusInternalServerError, `{}`
		},
		func(*testWebRiskRequest) (int, string) {
			return http.StatusOK, `not json`
		},
	}
	for index, refusal := range refusals {
		transport.script("diff", refusal)
		if err := client.Update(ctx, now.Add(time.Duration(index+1)*time.Hour)); !errors.Is(err, ErrWebRiskSource) {
			t.Fatalf("refusal %d answered %v", index, err)
		}
		if list := client.currentList(WebRiskThreatTypeSocialEngineering); list == nil || !slices.Equal(list.prefixes, []string{listed}) {
			t.Fatalf("refusal %d changed the list", index)
		}
	}

	settings := testWebRiskSettings()
	settings.DiffResponseMaxByteCount = 64
	bounded := newTestWebRiskClient(t, transport, settings)
	transport.script("diff", testWebRiskDiff("RESET", []string{listed}, nil, "v1", []string{listed}, time.Time{}))
	if err := bounded.Update(ctx, now); !errors.Is(err, ErrWebRiskSource) {
		t.Fatalf("an answer past the bound answered %v", err)
	}
}

// A hash no local list holds a prefix of sends nothing; a match sends its prefix alone; a full hash that is not
// among the answer's acts on nothing, and the prefix's absence is cached until its negative expire time; a listed
// full hash is answered with its list, from the cache until it expires.
func TestWebRiskLookupSendsOnlyAMatchedPrefix(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	transport := &testWebRiskTransport{}
	client := newTestWebRiskClient(t, transport, testWebRiskSettings(WebRiskThreatTypeSocialEngineering, WebRiskThreatTypeMalware))
	listedHash := testWebRiskHash("shop.net.root.example/")
	matchedHash := testWebRiskHash("blog.net.root.example/")
	listed := string(listedHash[:4])
	matched := string(matchedHash[:4])
	transport.script("diff",
		testWebRiskDiff("RESET", []string{listed, matched}, nil, "v1", []string{listed, matched}, time.Time{}),
		testWebRiskDiff("RESET", nil, nil, "v1", nil, time.Time{}),
	)
	if err := client.Update(ctx, now); err != nil {
		t.Fatal(err)
	}

	lookup, err := client.Lookup(ctx, testWebRiskHash("quiet.root.example/"), now)
	if err != nil || lookup.PrefixMatch || 0 < len(lookup.ThreatTypes) {
		t.Fatalf("a miss: %+v, %v", lookup, err)
	}
	if count := len(transport.seenOf("/v1/hashes:search")); count != 0 {
		t.Fatalf("a prefix miss sent %d searches", count)
	}

	// the match's prefix holds another full hash only
	unrelated := matchedHash
	unrelated[31] ^= 0xff
	transport.script("search", testWebRiskSearch(WebRiskThreatTypeSocialEngineering, now.Add(time.Hour), now.Add(10*time.Minute), unrelated))
	lookup, err = client.Lookup(ctx, matchedHash, now)
	if err != nil || !lookup.PrefixMatch || 0 < len(lookup.ThreatTypes) {
		t.Fatalf("a prefix match with no full-hash match: %+v, %v", lookup, err)
	}
	searches := transport.seenOf("/v1/hashes:search")
	if len(searches) != 1 || searches[0].query.Get("hashPrefix") != base64.StdEncoding.EncodeToString([]byte(matched)) ||
		!slices.Equal(searches[0].query["threatTypes"], []string{"SOCIAL_ENGINEERING", "MALWARE"}) {
		t.Fatalf("the search sent: %+v", searches)
	}
	// cached until the negative expire time
	if lookup, err := client.Lookup(ctx, matchedHash, now.Add(5*time.Minute)); err != nil || 0 < len(lookup.ThreatTypes) {
		t.Fatalf("the cached absence: %+v, %v", lookup, err)
	}
	if count := len(transport.seenOf("/v1/hashes:search")); count != 1 {
		t.Fatalf("a cached absence searched again: %d", count)
	}

	transport.script("search", testWebRiskSearch(WebRiskThreatTypeSocialEngineering, now.Add(time.Hour), now.Add(10*time.Minute), listedHash))
	lookup, err = client.Lookup(ctx, listedHash, now)
	if err != nil || !lookup.PrefixMatch || !slices.Equal(lookup.ThreatTypes, []WebRiskThreatType{WebRiskThreatTypeSocialEngineering}) {
		t.Fatalf("a confirmed match: %+v, %v", lookup, err)
	}
	if lookup, err := client.Lookup(ctx, listedHash, now.Add(30*time.Minute)); err != nil || len(lookup.ThreatTypes) != 1 {
		t.Fatalf("the cached listing: %+v, %v", lookup, err)
	}
	if count := len(transport.seenOf("/v1/hashes:search")); count != 2 {
		t.Fatalf("a cached listing searched again: %d", count)
	}
	// past its expiry it is asked again
	transport.script("search", testWebRiskSearch(WebRiskThreatTypeSocialEngineering, now.Add(3*time.Hour), now.Add(3*time.Hour)))
	if lookup, err := client.Lookup(ctx, listedHash, now.Add(2*time.Hour)); err != nil || 0 < len(lookup.ThreatTypes) {
		t.Fatalf("an expired listing: %+v, %v", lookup, err)
	}

	// a search that fails is an error and is not cached
	failingHash := testWebRiskHash("shop.net.root.example/")
	transport.script("search", func(*testWebRiskRequest) (int, string) { return http.StatusServiceUnavailable, "" })
	if _, err := client.Lookup(ctx, failingHash, now.Add(4*time.Hour)); !errors.Is(err, ErrWebRiskSource) {
		t.Fatalf("a failed search answered %v", err)
	}
}

// The key rides a header on every request and never the url, and no error names the key, a prefix or a host.
func TestWebRiskClientKeyRidesOnlyTheHeader(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	transport := &testWebRiskTransport{}
	client := newTestWebRiskClient(t, transport, testWebRiskSettings())
	hash := testWebRiskHash("shop.net.root.example/")
	prefix := string(hash[:4])
	transport.script("diff", testWebRiskDiff("RESET", []string{prefix}, nil, "v1", []string{prefix}, time.Time{}))
	if err := client.Update(ctx, now); err != nil {
		t.Fatal(err)
	}
	transport.script("search", func(*testWebRiskRequest) (int, string) { return http.StatusOK, `{"threats":[{"hash":"short"}]}` })
	_, err := client.Lookup(ctx, hash, now)
	if !errors.Is(err, ErrWebRiskSource) {
		t.Fatalf("a malformed search answered %v", err)
	}
	for _, text := range []string{testWebRiskKey, base64.StdEncoding.EncodeToString([]byte(prefix)), "root.example"} {
		if strings.Contains(err.Error(), text) {
			t.Errorf("an error names %q: %v", text, err)
		}
	}
	for _, request := range transport.seen() {
		if request.apiKey != testWebRiskKey {
			t.Errorf("a request without the key: %s", request.path)
		}
		if strings.Contains(request.rawUrl, testWebRiskKey) || request.query.Has("key") {
			t.Errorf("a url carries the key: %s", request.path)
		}
	}
}

func TestNewWebRiskClientRefusesWhatCannotRun(t *testing.T) {
	if _, err := NewWebRiskClient(&WebRiskClientArgs{Settings: testWebRiskSettings()}); !errors.Is(err, ErrWebRiskClientRefused) {
		t.Fatalf("no key: %v", err)
	}
	plain := testWebRiskSettings()
	plain.BaseUrl = "http://webrisk.example"
	pathed := testWebRiskSettings()
	pathed.BaseUrl = "https://webrisk.example/v1"
	none := testWebRiskSettings()
	none.ThreatTypes = nil
	unbounded := testWebRiskSettings()
	unbounded.RequestTimeout = 0
	longPrefix := testWebRiskSettings()
	longPrefix.HashPrefixMaxByteCount = 33
	for index, settings := range []*WebRiskClientSettings{plain, pathed, none, unbounded, longPrefix} {
		if _, err := NewWebRiskClient(&WebRiskClientArgs{ApiKey: testWebRiskKey, Settings: settings}); !errors.Is(err, ErrWebRiskClientRefused) {
			t.Errorf("settings %d: %v", index, err)
		}
	}
}

// The key from vault's file; a missing or keyless file is an error that names no text of it.
func TestLoadWebRiskApiKey(t *testing.T) {
	pop := Vault.PushSimpleResource(WebRiskKeyResourceName, []byte("api_key: "+testWebRiskKey+"\n"))
	key, err := LoadWebRiskApiKey()
	pop()
	if err != nil || key != testWebRiskKey {
		t.Fatalf("%q, %v", key, err)
	}
	pop = Vault.PushSimpleResource(WebRiskKeyResourceName, []byte("other: "+testWebRiskKey+"\n"))
	_, err = LoadWebRiskApiKey()
	pop()
	if !errors.Is(err, ErrWebRiskClientRefused) || strings.Contains(err.Error(), testWebRiskKey) {
		t.Fatalf("a file with no key: %v", err)
	}
	pop = Vault.PushSimpleResource(WebRiskKeyResourceName, []byte("api_key: ["+testWebRiskKey+"\n"))
	_, err = LoadWebRiskApiKey()
	pop()
	if !errors.Is(err, ErrWebRiskClientRefused) || strings.Contains(err.Error(), testWebRiskKey) {
		t.Fatalf("a file that does not parse: %v", err)
	}
}
