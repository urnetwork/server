package mcp

// Unit coverage for the parts of fetch that do not need the network stack:
// sealed state round trips, static resource discovery, and target validation.
// The end to end path is covered by the stack test.

import (
	"net/url"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

func TestSealRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		state := &cookieJarState{
			Origins: map[string][]*storedCookie{
				"https://example.com": {
					{Name: "session", Value: "abc123"},
				},
			},
		}

		sealed, err := seal(sealLabelCookies, state, fetchSealTtl)
		connect.AssertEqual(t, err, nil)
		// the caller must not be able to read or edit the jar
		connect.AssertEqual(t, sealed == "", false)

		out := &cookieJarState{}
		err = unseal(sealLabelCookies, sealed, out)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(out.Origins), 1)
		connect.AssertEqual(t, out.Origins["https://example.com"][0].Value, "abc123")

		// a jar must not be usable as a continuation
		continuation := &fetchContinuation{}
		err = unseal(sealLabelContinuation, sealed, continuation)
		connect.AssertEqual(t, err != nil, true)

		// tampering must not authenticate
		tampered := sealed[:len(sealed)-1] + "A"
		err = unseal(sealLabelCookies, tampered, out)
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestSealExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		sealed, err := seal(sealLabelContinuation, &fetchContinuation{PageUrl: "https://example.com"}, -1*time.Second)
		connect.AssertEqual(t, err, nil)

		out := &fetchContinuation{}
		err = unseal(sealLabelContinuation, sealed, out)
		connect.AssertEqual(t, err, errSealExpired)
	})
}

func TestExtractResourceRefs(t *testing.T) {
	base, err := url.Parse("https://example.com/page")
	connect.AssertEqual(t, err, nil)

	body := []byte(`
	<html><head>
		<link rel="stylesheet" href="/style.css">
		<link rel="icon" href="/favicon.ico">
		<meta property="og:image" content="https://cdn.example.com/og.png">
	</head><body>
		<img src="/a.png">
		<img srcset="/b-1x.png 1x, /b-2x.png 2x">
		<img src="data:image/png;base64,AAAA">
		<video src="/v.mp4" poster="/poster.jpg"></video>
		<script src="/app.js"></script>
		<img src="/a.png">
	</body></html>
	`)

	refs := extractResourceRefs(base, body, 32)

	byUrl := map[string]string{}
	for _, ref := range refs {
		byUrl[ref.url.String()] = ref.kind
	}

	connect.AssertEqual(t, byUrl["https://example.com/style.css"], resourceKindStylesheet)
	connect.AssertEqual(t, byUrl["https://example.com/a.png"], resourceKindImage)
	connect.AssertEqual(t, byUrl["https://example.com/b-1x.png"], resourceKindImage)
	connect.AssertEqual(t, byUrl["https://cdn.example.com/og.png"], resourceKindImage)
	connect.AssertEqual(t, byUrl["https://example.com/v.mp4"], resourceKindMedia)
	connect.AssertEqual(t, byUrl["https://example.com/app.js"], resourceKindScript)

	// data urls carry their own bytes and are never fetched
	for _, ref := range refs {
		connect.AssertEqual(t, ref.url.Scheme != "data", true)
	}

	// the duplicate img is not returned twice
	aCount := 0
	for _, ref := range refs {
		if ref.url.String() == "https://example.com/a.png" {
			aCount += 1
		}
	}
	connect.AssertEqual(t, aCount, 1)
}

func TestExtractResourceRefsRespectsMax(t *testing.T) {
	base, err := url.Parse("https://example.com/")
	connect.AssertEqual(t, err, nil)

	body := []byte(`<img src="/1.png"><img src="/2.png"><img src="/3.png"><img src="/4.png">`)

	refs := extractResourceRefs(base, body, 2)
	connect.AssertEqual(t, len(refs), 2)

	refs = extractResourceRefs(base, body, 0)
	connect.AssertEqual(t, len(refs), 0)
}

func TestValidateFetchUrl(t *testing.T) {
	allowed := []string{
		"https://example.com/page",
		"http://example.com",
		"https://93.184.216.34/",
	}
	refused := []string{
		"ftp://example.com/f",
		"file:///etc/passwd",
		"http://127.0.0.1:8080/",
		"https://10.1.2.3/",
		"http://192.168.1.1/",
		"http://169.254.169.254/latest/meta-data/",
		"https://[::1]/",
	}

	for _, rawUrl := range allowed {
		fetchUrl, err := url.Parse(rawUrl)
		connect.AssertEqual(t, err, nil)
		if err := validateFetchUrl(fetchUrl); err != nil {
			t.Errorf("expected %s to be allowed, got %s", rawUrl, err)
		}
	}

	for _, rawUrl := range refused {
		fetchUrl, err := url.Parse(rawUrl)
		connect.AssertEqual(t, err, nil)
		if err := validateFetchUrl(fetchUrl); err == nil {
			t.Errorf("expected %s to be refused", rawUrl)
		}
	}
}

func TestValidateFetchUrlAllowsPrivateWhenEnabled(t *testing.T) {
	// the stack test runs its web server on loopback, so the check is
	// switchable; confirm the switch actually governs it
	fetchAllowPrivateTargets = true
	defer func() {
		fetchAllowPrivateTargets = false
	}()

	fetchUrl, err := url.Parse("http://127.0.0.1:8080/")
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, validateFetchUrl(fetchUrl), nil)
}

func TestFetchNextStepDescribesThreading(t *testing.T) {
	// the caller is told what to carry forward, per value present
	out := &FetchResult{
		SignedProxyId: "SIGNEDID",
		Location:      "Japan",
		Cookies:       "SEALEDCOOKIES",
		Continuation:  "SEALEDCONT",
	}
	nextStep := fetchNextStep(out)

	for _, expected := range []string{"signed_proxy_id", "SIGNEDID", "cookies", "continuation"} {
		if !containsString(nextStep, expected) {
			t.Errorf("next step is missing %q: %s", expected, nextStep)
		}
	}

	// with nothing to carry, the caller is told that too
	empty := fetchNextStep(&FetchResult{})
	connect.AssertEqual(t, empty != "", true)
}

func containsString(s string, sub string) bool {
	return 0 <= indexString(s, sub)
}

func indexString(s string, sub string) int {
	for i := 0; i+len(sub) <= len(s); i += 1 {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
