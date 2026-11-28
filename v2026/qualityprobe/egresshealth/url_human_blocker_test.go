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
		{name: "contact verification paragraph", body: `<title>Contact us</title><form><p>Please verify you are a human to submit this message.</p><div class="h-captcha"></div></form>`},
		{name: "contact verification button", body: `<title>Contact us</title><form><button>Confirm you are a human</button><div class="g-recaptcha"></div></form>`},
		{name: "contact widget with outside paragraph", body: `<title>Contact us</title><p>Verify that you are human.</p><form><textarea name="message"></textarea><div class="h-captcha"></div></form>`},
		{name: "article quoted paragraph", body: `<title>Publisher</title><article><p>Press &amp; Hold to confirm you are<br />a human (and not a bot).</p></article><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "article with outside paragraph", body: `<title>Publisher</title><p>Confirm you are a human.</p><article><p>A tutorial about verification.</p></article><div class="h-captcha"></div>`},
		{name: "quoted article prose", body: `<title>Are you a human?</title><article><blockquote><p>A quoted passage explaining verification prompts.</p></blockquote></article><div class="h-captcha"></div>`},
		{name: "quoted article role", body: `<title>Are you a human?</title><blockquote role="article"><p>A quoted passage explaining verification prompts.</p></blockquote><div class="h-captcha"></div>`},
		{name: "article inside quote", body: `<title>Are you a human?</title><blockquote><article><p>A quoted article explaining verification prompts.</p></article></blockquote><div class="h-captcha"></div>`},
		{name: "article role after quoted introduction", body: `<title>Are you a human?</title><blockquote><p>A quoted introduction.</p><section role="article"><p>A quoted article explaining verification prompts.</p></section></blockquote><div class="h-captcha"></div>`},
		{name: "article inside inline quote", body: `<title>Are you a human?</title><q><span role="article">A quoted article explaining verification prompts.</span></q><div class="h-captcha"></div>`},
		{name: "blockquote paragraph", body: `<title>Publisher</title><blockquote><p>Please verify you are a human.</p></blockquote><div class="h-captcha"></div>`},
		{name: "inline quoted paragraph", body: `<title>Publisher</title><p><q>Confirm you are a human</q></p><div class="h-captcha"></div>`},
		{name: "quoted prose", body: `<title>Publisher</title><main><p>The prompt says: verify you are a human.</p></main><div class="h-captcha"></div>`},
		{name: "prompt description", body: `<title>Publisher</title><p>Verify you are human prompts are common.</p><div class="h-captcha"></div>`},
		{name: "quoted literal", body: `<title>Publisher</title><p>"Confirm you are a human."</p><div class="h-captcha"></div>`},
		{name: "template paragraph", body: `<title>Publisher</title><template><p>Confirm you are a human.</p></template><div class="h-captcha"></div>`},
		{name: "hidden paragraph", body: `<title>Publisher</title><div hidden><p>Confirm you are a human.</p></div><div class="h-captcha"></div>`},
		{name: "inert paragraph", body: `<title>Publisher</title><div inert><p>Confirm you are a human.</p></div><div class="h-captcha"></div>`},
		{name: "style hidden paragraph", body: `<title>Publisher</title><p style="visibility: hidden">Confirm you are a human.</p><div class="h-captcha"></div>`},
		{name: "auxiliary paragraph", body: `<title>Publisher</title><aside><p>Confirm you are a human.</p></aside><div class="h-captcha"></div>`},
		{name: "paragraph without widget", body: `<title>Publisher</title><p>Confirm you are a human.</p>`},
		{name: "paragraph without live script", body: `<title>Publisher</title><p>Confirm you are a human.</p><div id="px-captcha"></div><template><script src="https://captcha.vendor.example/generated/captcha.js"></script></template>`},
		{name: "paragraph with passive script", body: `<title>Publisher</title><p>Confirm you are a human.</p><script src="/cdn-cgi/challenge-platform/h/g/scripts/jsd/main.js"></script>`},
		{name: "script-only prompt", body: `<title>Publisher</title><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script><script>const message = "Press & Hold to confirm you are a human (and not a bot).";</script>`},
		{name: "benign press and hold", body: `<title>Player</title><button>Press &amp; Hold to preview audio</button><div class="h-captcha"></div>`},
		{name: "press and hold only", body: `<title>Publisher</title><button>Press &amp; Hold</button><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "generic paragraph", body: `<title>Publisher</title><p>Security check</p><div class="h-captcha"></div>`},
		{name: "human word boundary", body: `<title>Publisher</title><h1>Verify you are a humanist</h1><div class="h-captcha"></div>`},
		{name: "consent", body: `<title>Privacy preferences</title><h1>We value your privacy</h1><button>Accept cookies</button>`},
	} {
		classification, err := judgeUrlProbeContent(Destination{}, &http.Response{StatusCode: 200, Header: http.Header{}}, []byte(test.body))
		if err != nil || classification != "content" {
			t.Errorf("%s: classified ordinary content as %q: %v", test.name, classification, err)
		}
	}
}

// An untagged tutorial title cannot turn its contact widget into a document gate.
func TestUrlProbeHumanGateKeepsFormEvidenceLocal(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "untagged tutorial", body: `<title>Are you a human?</title><main><h1>Are you a human?</h1><p>A tutorial about verification questions.</p></main><form><div class="h-captcha"></div></form>`},
		{name: "contact textarea", body: `<title>Are you a human?</title><main><h1>Are you a human?</h1><p>A tutorial about verification questions.</p><form><label>Contact us<textarea name="message"></textarea></label><div class="h-captcha"></div></form></main>`},
		{name: "form-owned human mount", body: `<title>Are you a human?</title><main><h1>Are you a human?</h1><p>A tutorial about verification questions.</p><form><div id="px-captcha"></div><button>Send message</button></form></main><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "form-owned heading", body: `<title>Publisher</title><main><p>A tutorial about verification questions.</p></main><form><h1>Are you a human?</h1><textarea name="message"></textarea></form><div class="h-captcha"></div>`},
		{name: "unlabeled contact input", body: `<title>Verify you are human</title><form><input name="contact-message"><div class="h-captcha"></div></form>`},
		{name: "contact input inside prompt", body: `<title>Verify you are human</title><form><p>Please verify you are human<input name="contact-message"></p><div class="h-captcha"></div></form>`},
		{name: "quoted tutorial", body: `<title>Are you a human?</title><main><blockquote><p>A quoted passage explaining verification prompts.</p></blockquote></main><form><div class="h-captcha"></div></form>`},
	} {
		client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/html"}}, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})}
		result, err := Check(context.Background(), client, Options{
			UrlProbe: true, Destinations: []Destination{{Name: "synthetic-tutorial", Class: ClassSite, Url: "https://page.example/"}},
		})
		if err != nil || result == nil || len(result.Checks) != 1 {
			t.Fatalf("%s: no tutorial result: result=%+v error=%v", test.name, result, err)
		}
		check := result.Checks[0]
		if result.Total != 1 || result.OkCount != 1 || result.NotMeasured != 0 || result.TlsAuthenticationFailure ||
			check.ContentClassification != "content" || check.FailureStage != "" {
			t.Errorf("%s: contact widget overrode tutorial content: %+v", test.name, check)
		}
		if err := result.UrlProbeEvidence.ValidateOutcome(1, 1, false); err != nil {
			t.Errorf("%s: producer and evidence validator disagree: %v", test.name, err)
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

// Human prompts need live challenge structure and retain measured-failure evidence.
func TestUrlProbeHumanGateRejectsCorroboratedDocumentPrompts(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{name: "human title", body: `<title>Are you a human?</title><div class="g-recaptcha"></div>`},
		{name: "captcha-only form", body: `<title>Verify you are human</title><form><div class="g-recaptcha"></div></form>`},
		{name: "captcha form controls", body: `<title>Verify you are human</title><form><input type="hidden" name="nonce" value="synthetic"><div class="g-recaptcha"><input type="checkbox"></div><input type="submit" value="Continue"></form>`},
		{name: "hidden quoted article", body: `<title>Are you a human?</title><article><blockquote><p hidden>A hidden passage.</p></blockquote></article><div class="h-captcha"></div>`},
		{name: "hidden article inside quote", body: `<title>Are you a human?</title><blockquote><article hidden><p>A hidden passage.</p></article></blockquote><div class="h-captcha"></div>`},
		{name: "script-only article inside quote", body: `<title>Are you a human?</title><blockquote><article><script>const example = "A quoted article explaining verification prompts.";</script></article></blockquote><div class="h-captcha"></div>`},
		{name: "human heading", body: `<title>Publisher</title><h1>Are you a human?</h1><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "split human heading", body: `<title>Publisher</title><h1>Are <span>you&nbsp;a</span> human?</h1><div class="h-captcha"></div>`},
		{name: "verify a human title", body: `<title>Please verify you are a human</title><div class="h-captcha"></div>`},
		{name: "confirm a human heading", body: `<title>Publisher</title><h1>Confirm you are a human</h1><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "verify that a human heading", body: `<title>Publisher</title><h1>Verify that you are<br>a human</h1><div class="h-captcha"></div>`},
		{name: "confirm that a human heading", body: `<title>Publisher</title><h1>Please confirm that you are a human.</h1><div class="g-recaptcha"></div>`},
		{name: "human paragraph", body: `<title>Publisher</title><p>Verify that you are human.</p><div class="h-captcha"></div>`},
		{name: "inline split paragraph", body: `<title>Publisher</title><p>Please ver<span>ify</span> that you are <strong>a human</strong>.</p><div class="h-captcha"></div>`},
		{name: "default hold paragraph", body: `<title>Publisher</title><h1>Before we continue...</h1><p>Press &amp; Hold to confirm you are<br />a human (and not a bot).</p><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "human button", body: `<title>Publisher</title><button>Confirm you are a human</button><div class="cf-turnstile"></div>`},
		{name: "hold button", body: `<title>Publisher</title><button>Press and Hold to confirm you are a human.</button><div id="px-captcha"></div><script src="https://captcha.vendor.example/generated/captcha.js"></script>`},
		{name: "paragraph with challenge script", body: `<title>Publisher</title><p>Please confirm you are a human to continue.</p><script src="/cdn-cgi/challenge-platform/start.js"></script>`},
		{name: "form heading with challenge script", body: `<title>Publisher</title><form><h1>Confirm you are a human</h1><div class="h-captcha"></div></form><script src="/cdn-cgi/challenge-platform/start.js"></script>`},
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

// Mixed fleets retain v1/v2 receipts while new producers identify changed semantics.
func TestUrlProbeHumanGateEvidenceVersions(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3, 4} {
		evidence := urlEvidenceTestSuccess()
		evidence.ContentMatcherVersion = version
		want := version == 1 || version == 2 || version == 3
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
	if err != nil || result == nil || result.UrlProbeEvidence == nil || result.UrlProbeEvidence.ContentMatcherVersion != 3 {
		t.Fatalf("producer did not stamp matcher v3: result=%+v error=%v", result, err)
	}
}
