// URL success requires final content, not a redirect or a challenge page.
package egresshealth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// Challenge/portal bodies remain measured failures, never certificate findings.
func TestUrlProbeRejectsChallengeAndPortalContent(t *testing.T) {
	for _, body := range []string{
		`<html><title>Verify you are human</title><form><div class="g-recaptcha"></div></form></html>`,
		`<html><title>Wi-Fi sign in</title><form>Please log in to this network to access the Internet</form></html>`,
		`<html><title>Just a moment...</title><script src="/cdn-cgi/challenge-platform/start.js"></script></html>`,
	} {
		client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
		})}
		result, err := Check(context.Background(), client, Options{
			UrlProbe: true, Destinations: []Destination{{Name: "synthetic-page", Class: ClassSite, Url: "https://page.example/"}},
		})
		if err != nil || result.Total != 1 || result.OkCount != 0 || result.TlsAuthenticationFailure {
			t.Fatalf("challenge/portal accepted or mislabeled as security: result=%+v err=%v", result, err)
		}
	}
}

// Inclusive performance boundaries and EOF-only small-body handling are stable.
func TestUrlProbePerformanceThresholdsAndSmallBodyEvidence(t *testing.T) {
	policy := DefaultUrlProbePolicy()
	for _, test := range []struct {
		name           string
		result         CheckResult
		pass           bool
		classification string
	}{
		{"exact thresholds", CheckResult{RequestWritten: true, RequestTimeToFirstByte: 2 * time.Second, ByteCount: 25000, BodyDuration: 2 * time.Second, BodyBytesPerSecond: 12500}, true, "passed"},
		{"ttfb above threshold", CheckResult{RequestWritten: true, RequestTimeToFirstByte: 2*time.Second + time.Nanosecond, ByteCount: 25000, BodyDuration: 2 * time.Second, BodyBytesPerSecond: 12500}, false, "ttfb_slow"},
		{"throughput below threshold", CheckResult{RequestWritten: true, ByteCount: 25000, BodyDuration: 2*time.Second + time.Nanosecond, BodyBytesPerSecond: 12499.999}, false, "throughput_slow"},
		{"small complete", CheckResult{RequestWritten: true, ByteCount: 12, BodyComplete: true}, true, "insufficient_sample"},
		{"small incomplete", CheckResult{RequestWritten: true, ByteCount: 12}, false, "insufficient_sample"},
		{"large zero duration", CheckResult{RequestWritten: true, ByteCount: 25000}, false, "throughput_not_measured"},
		{"missing write timestamp", CheckResult{ByteCount: 12, BodyComplete: true}, false, "ttfb_not_measured"},
	} {
		result := test.result
		result.WireByteCount = result.ByteCount + 1
		result.WireSampleByteCount = result.ByteCount
		err := judgeUrlProbePerformance(&result, policy)
		if (err == nil) != test.pass || result.PerformanceClassification != test.classification {
			t.Errorf("%s: class=%s error=%v", test.name, result.PerformanceClassification, err)
		}
		if test.name == "small complete" && result.BodyBytesPerSecond != 0 {
			t.Error("tiny-page bypass invented a bandwidth estimate")
		}
	}
	policy.MaxBodyBytes = policy.MinThroughputBytes - 1
	if policy.Validate() == nil {
		t.Error("a low read cap could manufacture the tiny-page bypass")
	}
}

// Only the reader's EOF proves completeness; the sampling cap is not EOF.
func TestUrlProbeReadCapDoesNotPretendTheBodyCompleted(t *testing.T) {
	for _, test := range []struct {
		body     string
		limit    int
		complete bool
	}{
		{"short", 16, true}, {"exact", 5, false}, {"truncated", 3, false},
	} {
		body, complete, err := readUrlProbeBody(strings.NewReader(test.body), test.limit)
		if err != nil || complete != test.complete || len(body) != min(len(test.body), test.limit) {
			t.Errorf("body=%q cap=%d bytes=%d complete=%t err=%v", test.body, test.limit, len(body), complete, err)
		}
	}
}

// Known bodyless legacy contracts are catalog errors, not provider failures.
func TestUrlProbeRejectsBodylessCatalogBeforeRequest(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) { requests++; return nil, nil })}
	result, err := Check(context.Background(), client, Options{UrlProbe: true, Destinations: []Destination{{Name: "synthetic-204", Class: ClassConnectivity, Url: "https://empty.example/", Expect: ExpectStatus, Status: 204}}})
	if err == nil || result != nil || requests != 0 {
		t.Fatalf("bodyless catalog generated provider evidence: result=%v error=%v requests=%d", result, err, requests)
	}
}

// A real final document is required even if a migrated entry says reachable.
func TestUrlProbeFollowsRedirectToFinalContent(t *testing.T) {
	requests := 0
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			if request.URL.Hostname() == "first.example" {
				return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://regional.example/document"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			}
			if request.URL.Hostname() != "regional.example" {
				t.Errorf("unexpected redirect target %s", request.URL)
			}
			return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic final document"))}, nil
		}),
	}
	result, err := Check(context.Background(), client, Options{
		UrlProbe: true, Destinations: []Destination{{Name: "synthetic-redirect", Class: ClassSite, Url: "https://first.example/", Expect: ExpectReachable}},
	})
	if err != nil || requests != 2 || result.OkCount != 1 || result.Total != 1 {
		t.Fatalf("redirect was treated as content: requests=%d result=%+v error=%v", requests, result, err)
	}
}

// Missing Location cannot turn a bare 3xx into usable content.
func TestUrlProbeRejectsBareRedirect(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 302, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	result, err := Check(context.Background(), client, Options{
		UrlProbe: true, Destinations: []Destination{{Name: "synthetic-empty", Class: ClassSite, Url: "https://page.example/", Expect: ExpectReachable}},
	})
	if err != nil || result.Total != 1 || result.OkCount != 0 || result.TlsAuthenticationFailure {
		t.Fatalf("bare redirect accepted: result=%+v error=%v", result, err)
	}
}

// Cloudflare JavaScript detection and ordinary form widgets are not blocks.
func TestUrlProbeChallengeMatcherRequiresBlockingEvidence(t *testing.T) {
	for _, body := range []string{
		`<html><title>Ordinary article</title><script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script><p>Readable content.</p></html>`,
		`<html><title>Just a moment</title><script src="/cdn-cgi/challenge-platform/h/g/scripts/jsd/main.js"></script><article>A regular article with an ambiguous title.</article></html>`,
		`<html><title>Contact us</title><form><div class="g-recaptcha"></div></form><p>Contact form.</p></html>`,
		`<html><title>Contact us</title><form><div class="h-captcha"></div><div class="cf-turnstile"></div></form></html>`,
		`<html><title>CAPTCHA tutorial</title><article>Verify you are human.</article><script>const example = '<title>Just a moment</title><div class="g-recaptcha"></div>';</script></html>`,
		`<html><title>Networking article</title><!-- <form action="/sorry/"></form> unusual traffic from your computer network --><p>Ordinary content.</p></html>`,
	} {
		classification, err := judgeUrlProbeContent(Destination{}, &http.Response{StatusCode: 200, Header: http.Header{"Server": {"cloudflare"}, "Cf-Ray": {"synthetic"}}}, []byte(body))
		if err != nil || classification != "content" {
			t.Errorf("ordinary HTML mislabeled %q: %v", classification, err)
		}
	}
	for _, header := range []http.Header{{"Cf-Mitigated": {"challenge"}}, {"X-Amzn-Waf-Action": {"captcha"}}, {"X-Amzn-Waf-Action": {"challenge"}}} {
		classification, err := judgeUrlProbeContent(Destination{}, &http.Response{StatusCode: 200, Header: header}, []byte("synthetic challenge response"))
		if err == nil || classification != "captcha" {
			t.Errorf("documented challenge header ignored: %v", header)
		}
	}
	classification, err := judgeUrlProbeContent(Destination{}, &http.Response{StatusCode: 200, Header: http.Header{}}, []byte(`<html><form action="/sorry/index"><div class="g-recaptcha"></div></form><p>Our systems have detected unusual traffic from your computer network.</p></html>`))
	if err == nil || classification != "captcha" {
		t.Error("Google unusual-traffic interstitial was accepted")
	}
}
