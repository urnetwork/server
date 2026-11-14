package egresshealth

import (
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// Tests of the request profile: the default browser navigation, the headers
// never sent, transparent gzip and the byte cap without Range.

// The default is what a
// mainstream desktop browser sends with a top-level navigation -- its agent,
// Accept, Accept-Language, the Sec-Fetch family and Upgrade-Insecure-Requests
// -- and deliberately not Accept-Encoding (the transport's own gzip is what
// gets decoded transparently) or Range (which bot managers refuse).
func TestDefaultRequestProfileIsABrowserNavigation(t *testing.T) {
	profile := DefaultRequestProfile()
	if !strings.HasPrefix(profile.UserAgent, "Mozilla/5.0 (") || !strings.Contains(profile.UserAgent, "Firefox/") {
		t.Errorf("UserAgent %q is not a mainstream desktop browser's", profile.UserAgent)
	}
	if strings.Contains(strings.ToLower(profile.UserAgent), "urnetwork") || strings.Contains(profile.UserAgent, "Go-http-client") {
		t.Errorf("UserAgent %q still names the probe", profile.UserAgent)
	}
	for name, want := range map[string]string{
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            "none",
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	} {
		if got := profile.Headers[name]; got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if !strings.HasPrefix(profile.Headers["Accept"], "text/html") {
		t.Errorf("Accept = %q, want a document navigation's", profile.Headers["Accept"])
	}
	if profile.Headers["Accept-Language"] == "" {
		t.Error("no Accept-Language; every browser sends one")
	}
	for name := range profile.Headers {
		if neverSent(name) {
			t.Errorf("the default profile carries %s, which is never sent", name)
		}
	}

	// A fresh value each time: editing one must not change the next probe's.
	profile.Headers["Accept"] = "mutated"
	if DefaultRequestProfile().Headers["Accept"] == "mutated" {
		t.Error("mutating a returned profile changed the default")
	}
}

// The pool and its profile are data from the
// server, so the rule has to hold however the header is spelled.
func TestNeverSentIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"Range", "range", "RANGE", "Accept-Encoding", "accept-encoding"} {
		if !neverSent(name) {
			t.Errorf("neverSent(%q) = false", name)
		}
	}
	for _, name := range []string{"Accept", "User-Agent", "Sec-Fetch-Mode", "Accept-Language"} {
		if neverSent(name) {
			t.Errorf("neverSent(%q) = true", name)
		}
	}
}

// Drives a whole sampled run:
// every request -- whatever class, whatever the destination asked for -- goes
// out with the browser profile and without a Range header.
func TestEveryRequestCarriesTheProfileAndNeverARange(t *testing.T) {
	var stateLock sync.Mutex
	var seen []http.Header
	record := func(body string) handler {
		return func(w http.ResponseWriter, r *http.Request) {
			stateLock.Lock()
			seen = append(seen, r.Header.Clone())
			stateLock.Unlock()
			_, _ = w.Write([]byte(body))
		}
	}
	dests := stubDestinations(t, []spec{
		{name: "dns-a", class: ClassDns, h: record(`{"Answer":[{"data":"192.0.2.1"}]}`), verify: BodyCheck{Kind: BodyCheckDnsJson}},
		{name: "conn-a", class: ClassConnectivity, h: record("success\n"), verify: BodyCheck{Kind: BodyCheckContains, Text: "success"}},
		{name: "cdn-a", class: ClassCdn, h: record("/* css */")},
		{name: "site-a", class: ClassSite, h: record("<html>")},
	})
	// What a pooled destination could still ask for.
	dests[0].Headers = map[string]string{"Accept": acceptDnsJson}
	dests[2].Headers = map[string]string{"Range": "bytes=0-1023"}
	dests[3].Headers = map[string]string{"range": "bytes=0-99", "Accept-Encoding": "br, zstd"}

	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	if res.OkCount != res.Total {
		t.Fatalf("Summary = %s; every stub answers correctly", res.Summary())
	}
	stateLock.Lock()
	defer stateLock.Unlock()
	if len(seen) != len(dests) {
		t.Fatalf("saw %d requests, want %d", len(seen), len(dests))
	}
	profile := DefaultRequestProfile()
	for i, h := range seen {
		if _, present := h["Range"]; present {
			t.Errorf("request %d carried Range %q", i, h.Get("Range"))
		}
		if h.Get("User-Agent") != profile.UserAgent {
			t.Errorf("request %d User-Agent = %q", i, h.Get("User-Agent"))
		}
		if h.Get("Sec-Fetch-Mode") != "navigate" || h.Get("Upgrade-Insecure-Requests") != "1" {
			t.Errorf("request %d is missing the navigation headers: %v", i, h)
		}
		if h.Get("Accept-Encoding") != "gzip" {
			t.Errorf("request %d Accept-Encoding = %q, want the transport's own gzip", i, h.Get("Accept-Encoding"))
		}
	}
}

// Why Accept-Encoding is left to the
// transport: a server that takes the transport's gzip offer is decoded before
// the capped read and the body check see it, so a DoH answer still parses and
// ByteCount is the decoded bytes.
func TestGzipBodyIsDecodedTransparently(t *testing.T) {
	const answer = `{"Status":0,"Answer":[{"name":"example.com.","type":1,"data":"192.0.2.1"}]}`
	dests := stubDestinations(t, []spec{{name: "doh", class: ClassDns, verify: BodyCheck{Kind: BodyCheckDnsJson}, h: func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			http.Error(w, "this stub only speaks gzip", http.StatusNotAcceptable)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		_, _ = zw.Write([]byte(answer))
		_ = zw.Close()
	}}})
	dests[0].Headers = map[string]string{"Accept": acceptDnsJson}

	res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
	if err != nil {
		t.Fatalf("check err = %v", err)
	}
	c := res.Checks[0]
	if !c.Ok {
		t.Fatalf("a gzip-encoded DoH answer failed: %+v -- the body check saw compressed bytes", c)
	}
	if c.ByteCount != int64(len(answer)) {
		t.Errorf("ByteCount = %d, want the %d decoded bytes", c.ByteCount, len(answer))
	}
}

// With no byte-range request to lean on, the
// capped read is the whole bound on what a load keeps, and it must hold
// against a server that streams far more than the cap -- with the transport's
// gzip in play or not.
func TestByteCapHoldsWithoutRange(t *testing.T) {
	const streamed = 64 * MaxBodyBytes
	chunk := strings.Repeat("x", 4096)
	for name, gzipped := range map[string]bool{"identity": false, "gzip": true} {
		var sawRange atomic.Bool
		dests := stubDestinations(t, []spec{{name: "flood", class: ClassCdn, h: func(w http.ResponseWriter, r *http.Request) {
			_, present := r.Header["Range"]
			sawRange.Store(present)
			var out io.Writer = w
			if gzipped {
				w.Header().Set("Content-Encoding", "gzip")
				zw := gzip.NewWriter(w)
				defer zw.Close()
				out = zw
			}
			for written := 0; written < streamed; written += len(chunk) {
				if _, err := out.Write([]byte(chunk)); err != nil {
					return
				}
			}
		}}})
		dests[0].MaxBytes = maxAssetBytes

		res, err := check(context.Background(), http.DefaultClient, dests, fastOptions())
		if err != nil {
			t.Fatalf("%s: check err = %v", name, err)
		}
		c := res.Checks[0]
		if sawRange.Load() {
			t.Errorf("%s: the load carried a Range header", name)
		}
		if c.ByteCount != maxAssetBytes {
			t.Fatalf("%s: ByteCount = %d from a %d-byte stream, want exactly the cap %d", name, c.ByteCount, streamed, maxAssetBytes)
		}
		if !c.Ok {
			t.Fatalf("%s: a large healthy response must still be OK: %+v", name, c)
		}
	}
}
