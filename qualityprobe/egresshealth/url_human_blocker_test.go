// Human-gate fixtures are synthetic documents; no live pages or tokens are kept.
package egresshealth

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Inert, hidden, example and auxiliary controls do not replace a document.
func TestUrlProbeHumanGateIgnoresInactiveAndUnrelatedControls(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "template widget", body: `<title>Security check</title><template><div class="g-recaptcha"></div></template><p>A browser-security tutorial.</p>`},
		{name: "template script", body: `<title>Just a moment</title><template><script src="/cdn-cgi/challenge-platform/start.js"></script></template>`},
		{name: "example widget", body: `<title>Security check</title><pre><code><div class="h-captcha"></div></code></pre>`},
		{name: "hidden widget", body: `<title>Security check</title><section hidden><div class="cf-turnstile"></div></section>`},
		{name: "inert widget", body: `<title>Security check</title><section inert><div class="g-recaptcha"></div></section>`},
		{name: "aria hidden widget", body: `<title>Security check</title><section aria-hidden="true"><div class="g-recaptcha"></div></section>`},
		{name: "style hidden widget", body: `<title>Security check</title><section style="display: none !important"><div class="g-recaptcha"></div></section>`},
		{name: "nonexecuting script", body: `<title>Just a moment</title><script type="application/json" src="/cdn-cgi/challenge-platform/start.js"></script>`},
		{name: "query script lookalike", body: `<title>Just a moment</title><script src="/app.js?example=/cdn-cgi/challenge-platform/start.js"></script>`},
		{name: "query form lookalike", body: `<form action="/search?next=/sorry/index"></form><p>Unusual traffic from your computer network.</p>`},
		{name: "foreign sorry form", body: `<form action="https://forms.example/sorry/index"></form><p>Unusual traffic from your computer network.</p>`},
		{name: "template portal form", body: `<title>Wi-Fi sign in</title><template><form></form></template>`},
		{name: "article and contact widget", body: `<title>Security check</title><article><h1>Are you a human?</h1><p>A tutorial about verification questions.</p></article><footer><div class="g-recaptcha"></div></footer>`},
		{name: "outside article heading", body: `<title>Are you a human?</title><h1>Are you a human?</h1><article><p>A tutorial about verification questions.</p></article><div class="g-recaptcha"></div>`},
		{name: "footer widget", body: `<title>Security check</title><p>A browser-security tutorial.</p><footer><div class="g-recaptcha"></div></footer>`},
		{name: "article prompt", body: `<title>Publisher</title><article><h1>Are you a human?</h1><p>A tutorial.</p></article><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "passive detection", body: `<title>Just a moment</title><script src="/cdn-cgi/challenge-platform/h/g/scripts/jsd/main.js"></script><noscript>Enable JavaScript and cookies to continue.</noscript>`},
		{name: "mount without challenge script", body: `<title>Publisher</title><h1>Are you a human?</h1><div id="px-captcha"></div><script src="https://sensor.vendor.example/sensor.js"></script>`},
		{name: "challenge script in query", body: `<title>Publisher</title><h1>Are you a human?</h1><div id="px-captcha"></div><script src="https://sensor.vendor.example/sensor.js?next=/captcha.js"></script>`},
		{name: "hidden heading", body: `<title>Publisher</title><h1 hidden>Are you a human?</h1><div class="h-captcha"></div>`},
		{name: "inert heading", body: `<title>Publisher</title><template><h1>Are you a human?</h1></template><div class="h-captcha"></div>`},
		{name: "contact widget", body: `<title>Contact us</title><form><div class="g-recaptcha"></div></form>`},
		{name: "consent", body: `<title>Privacy preferences</title><h1>We value your privacy</h1><button>Accept cookies</button>`},
	} {
		classification, err := judgeUrlProbeContent(Destination{}, &http.Response{StatusCode: 200, Header: http.Header{}}, []byte(test.body))
		if err != nil || classification != "content" {
			t.Errorf("%s: classified ordinary content as %q: %v", test.name, classification, err)
		}
	}
}

// Explicit response decisions remain authoritative even with article markup.
func TestUrlProbeHumanGateArticleDoesNotOverrideDecisionHeaders(t *testing.T) {
	for _, header := range []http.Header{
		{"Cf-Mitigated": {"challenge"}},
		{"X-Amzn-Waf-Action": {"captcha"}},
		{"X-Amzn-Waf-Action": {"challenge"}},
	} {
		classification, err := judgeUrlProbeContent(Destination{}, &http.Response{StatusCode: 200, Header: header}, []byte(`<title>Publisher</title><article><p>Synthetic article content.</p></article>`))
		if err == nil || classification != "captcha" {
			t.Errorf("article structure overrode an explicit decision: classification=%s error=%v", classification, err)
		}
	}
}

// A heading or a custom title needs live challenge structure for corroboration.
func TestUrlProbeHumanGateRejectsCorroboratedDocumentPrompts(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "human title", body: `<title>Are you a human?</title><div class="g-recaptcha"></div>`},
		{name: "human heading", body: `<title>Publisher</title><h1>Are you a human?</h1><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "split human heading", body: `<title>Publisher</title><h1>Are <span>you&nbsp;a</span> human?</h1><div class="h-captcha"></div>`},
		{name: "custom browser gate", body: `<title>Example title</title><script src="/cdn-cgi/challenge-platform/start.js"></script><noscript>Enable JavaScript and cookies to continue.</noscript>`},
		{name: "existing title gate", body: `<title>Just a moment</title><script src="https://challenge.vendor.example/cdn-cgi/challenge-platform/start.js?generated=synthetic"></script>`},
		{name: "existing script gate with article wrapper", body: `<title>Just a moment</title><script src="/cdn-cgi/challenge-platform/start.js"></script><article><p>Enable JavaScript and cookies to continue.</p></article>`},
		{name: "existing sorry gate", body: `<form action="/sorry/index"><div class="g-recaptcha"></div></form><p>Unusual traffic from your computer network.</p>`},
	} {
		client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})}
		result, err := Check(context.Background(), client, Options{
			UrlProbe: true, Destinations: []Destination{{Name: "synthetic-gate", Class: ClassSite, Url: "https://page.example/"}},
		})
		if err != nil || result == nil || len(result.Checks) != 1 {
			t.Fatalf("%s: no URL result: result=%+v error=%v", test.name, result, err)
		}
		check := result.Checks[0]
		if result.Total != 1 || result.OkCount != 0 || result.NotMeasured != 0 || result.TlsAuthenticationFailure ||
			check.ContentClassification != "captcha" || check.FailureStage != "response_content" {
			t.Errorf("%s: corroborated gate was accepted or lost measured evidence: %+v", test.name, check)
		}
		if err := result.UrlProbeEvidence.ValidateOutcome(0, 1, false); err != nil {
			t.Errorf("%s: producer and evidence validator disagree: %v", test.name, err)
		}
	}
}

// Mixed fleets retain v1 receipts while new producers identify changed semantics.
func TestUrlProbeHumanGateEvidenceVersions(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3} {
		evidence := urlEvidenceTestSuccess()
		evidence.ContentMatcherVersion = version
		want := version == 1 || version == 2
		if err := evidence.ValidateOutcome(1, 1, false); (err == nil) != want {
			t.Errorf("matcher version %d: accepted=%t want=%t: %v", version, err == nil, want, err)
		}
		evidence.ContentClassification, evidence.FailureStage = "captcha", "response_content"
		if err := evidence.ValidateOutcome(0, 1, false); (err == nil) != want {
			t.Errorf("failed matcher version %d: accepted=%t want=%t: %v", version, err == nil, want, err)
		}
	}
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic content"))}, nil
	})}
	result, err := Check(context.Background(), client, Options{
		UrlProbe: true, Destinations: []Destination{{Name: "synthetic-content", Class: ClassSite, Url: "https://page.example/"}},
	})
	if err != nil || result == nil || result.UrlProbeEvidence == nil || result.UrlProbeEvidence.ContentMatcherVersion != 2 {
		t.Fatalf("producer did not stamp matcher v2: result=%+v error=%v", result, err)
	}
}
