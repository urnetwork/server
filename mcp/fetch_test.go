package mcp

// Unit coverage for the parts of fetch that do not need the network stack:
// sealed state round trips, static resource discovery, and target validation.
// The end to end path is covered by the stack test.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func TestSealRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		binding := "test-identity"
		state := &cookieJarState{
			Origins: map[string][]*storedCookie{
				"https://example.com": {
					{Name: "session", Value: "abc123"},
				},
			},
		}

		sealed, err := seal(sealLabelCookies, binding, state, fetchSealTtl)
		connect.AssertEqual(t, err, nil)
		// the caller must not be able to read or edit the jar
		connect.AssertEqual(t, sealed == "", false)

		out := &cookieJarState{}
		err = unseal(sealLabelCookies, binding, sealed, out)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(out.Origins), 1)
		connect.AssertEqual(t, out.Origins["https://example.com"][0].Value, "abc123")

		// a jar must not be usable as a continuation
		continuation := &fetchContinuation{}
		err = unseal(sealLabelContinuation, binding, sealed, continuation)
		connect.AssertEqual(t, err != nil, true)

		// state from one token identity cannot be replayed by another
		err = unseal(sealLabelCookies, "other-identity", sealed, out)
		connect.AssertEqual(t, err != nil, true)

		// tampering must not authenticate
		tampered := sealed[:len(sealed)-1] + "A"
		err = unseal(sealLabelCookies, binding, tampered, out)
		connect.AssertEqual(t, err != nil, true)
	})
}

func TestSealExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		binding := "test-identity"
		sealed, err := seal(sealLabelContinuation, binding, &fetchContinuation{PageUrl: "https://example.com"}, -1*time.Second)
		connect.AssertEqual(t, err, nil)

		out := &fetchContinuation{}
		err = unseal(sealLabelContinuation, binding, sealed, out)
		connect.AssertEqual(t, err, errSealExpired)
	})
}

func TestSealedStateRejectsEveryIdentityDimension(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		userId := server.NewId()
		networkId := server.NewId()
		clientId := "oauth-client"
		resource := McpResource
		binding := identityStateBinding(userId.String(), networkId, clientId, resource)

		sealed, err := seal(
			sealLabelCookies,
			binding,
			&cookieJarState{Origins: map[string][]*storedCookie{}},
			fetchSealTtl,
		)
		connect.AssertEqual(t, err, nil)

		otherBindings := map[string]string{
			"subject": identityStateBinding(server.NewId().String(), networkId, clientId, resource),
			"network": identityStateBinding(userId.String(), server.NewId(), clientId, resource),
			"client":  identityStateBinding(userId.String(), networkId, "other-client", resource),
			"audience": identityStateBinding(
				userId.String(),
				networkId,
				clientId,
				"https://other.example/mcp",
			),
		}
		for dimension, otherBinding := range otherBindings {
			out := &cookieJarState{}
			if err := unseal(sealLabelCookies, otherBinding, sealed, out); err == nil {
				t.Fatalf("sealed state replayed across %s", dimension)
			}
		}
	})
}

func TestReuseProxyRequiresCurrentNetworkOwner(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ownerNetworkId := server.NewId()
		ownerUserId := server.NewId()
		ownerDeviceId := server.NewId()
		ownerClientId := server.NewId()
		ownerNetworkName := fmt.Sprintf("proxy-owner-%s", ownerNetworkId)
		model.Testing_CreateNetwork(ctx, ownerNetworkId, ownerNetworkName, ownerUserId)
		model.Testing_CreateDevice(
			ctx,
			ownerNetworkId,
			ownerDeviceId,
			ownerClientId,
			"proxy owner",
			"mcp",
		)

		proxyDeviceConfig := &model.ProxyDeviceConfig{
			ProxyDeviceConnection: model.ProxyDeviceConnection{ClientId: ownerClientId},
			ProxyDeviceMode:       model.ProxyDeviceModeDevice,
		}
		if err := model.CreateProxyDeviceConfig(ctx, proxyDeviceConfig); err != nil {
			t.Fatalf("create proxy device config: %v", err)
		}
		proxyClient, err := model.CreateProxyClient(
			ctx,
			proxyDeviceConfig.ProxyId,
			ownerClientId,
			proxyDeviceConfig.InstanceId,
			model.CreateProxyClientOptions{},
		)
		if err != nil {
			t.Fatalf("create proxy client: %v", err)
		}

		ownerSession := session.NewLocalClientSession(
			ctx,
			"",
			jwt.NewByJwt(ownerNetworkId, ownerUserId, ownerNetworkName, false, false),
		)
		defer ownerSession.Cancel()
		ownerBinding := identityStateBinding(
			ownerUserId.String(),
			ownerNetworkId,
			"oauth-client",
			McpResource,
		)
		ownerHandle, err := seal(
			sealLabelProxy,
			ownerBinding,
			&sealedProxyHandle{SignedProxyId: proxyClient.AuthToken},
			fetchSealTtl,
		)
		connect.AssertEqual(t, err, nil)

		acquired, err := reuseProxy(ownerSession, ownerBinding, ownerHandle)
		connect.AssertEqual(t, err, nil)
		if acquired == nil {
			t.Fatal("owner could not reuse its proxy handle")
		}
		refreshedHandle := &sealedProxyHandle{}
		if err := unseal(sealLabelProxy, ownerBinding, acquired.signedProxyId, refreshedHandle); err != nil {
			t.Fatalf("unseal refreshed proxy handle: %v", err)
		}
		connect.AssertEqual(t, refreshedHandle.SignedProxyId, proxyClient.AuthToken)

		attackerNetworkId := server.NewId()
		attackerUserId := server.NewId()
		attackerNetworkName := fmt.Sprintf("proxy-attacker-%s", attackerNetworkId)
		model.Testing_CreateNetwork(ctx, attackerNetworkId, attackerNetworkName, attackerUserId)
		attackerSession := session.NewLocalClientSession(
			ctx,
			"",
			jwt.NewByJwt(attackerNetworkId, attackerUserId, attackerNetworkName, false, false),
		)
		defer attackerSession.Cancel()
		attackerBinding := identityStateBinding(
			attackerUserId.String(),
			attackerNetworkId,
			"oauth-client",
			McpResource,
		)
		// Sealing the owner's raw proxy credential with the attacker's binding
		// isolates the ownership check. A real caller cannot perform this seal.
		attackerHandle, err := seal(
			sealLabelProxy,
			attackerBinding,
			&sealedProxyHandle{SignedProxyId: proxyClient.AuthToken},
			fetchSealTtl,
		)
		connect.AssertEqual(t, err, nil)
		if _, err := reuseProxy(attackerSession, attackerBinding, attackerHandle); err == nil {
			t.Fatal("proxy handle reused by a different network")
		}

		removeResult, err := model.RemoveNetworkClient(
			&model.RemoveNetworkClientArgs{ClientId: ownerClientId},
			ownerSession,
		)
		connect.AssertEqual(t, err, nil)
		if removeResult.Error != nil {
			t.Fatalf("deactivate proxy owner: %s", removeResult.Error.Message)
		}
		if _, err := reuseProxy(ownerSession, ownerBinding, acquired.signedProxyId); err == nil {
			t.Fatal("proxy handle reused after its owner client was deactivated")
		}
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
		"https://user:password@example.com/",
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

func TestValidateFetchUrlLeavesAddressPolicyToConnect(t *testing.T) {
	for _, rawUrl := range []string{
		"http://127.0.0.1:8080/",
		"https://10.1.2.3/",
		"http://169.254.169.254/latest/meta-data/",
		"https://[::1]/",
	} {
		fetchUrl, err := url.Parse(rawUrl)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, validateFetchUrl(fetchUrl), nil)
	}
}

func TestFetchRequestMetadataIsBounded(t *testing.T) {
	connect.AssertEqual(t, validateFetchRequestMetadata(FetchArgs{
		Body: strings.Repeat("a", fetchMaxRequestBodyBytes+1),
	}) != nil, true)
	connect.AssertEqual(t, validateFetchRequestMetadata(FetchArgs{
		Headers: map[string]string{"Connection": "keep-alive"},
	}) != nil, true)
	connect.AssertEqual(t, validateFetchRequestMetadata(FetchArgs{
		Headers: map[string]string{"X-Test": "one\r\ntwo"},
	}) != nil, true)
}

func TestFetchHeadersStayOnCredentialOrigin(t *testing.T) {
	credentialOrigin, _ := url.Parse("https://example.com/start")
	sameOrigin, _ := url.Parse("https://EXAMPLE.com:443/resource")
	otherOrigin, _ := url.Parse("https://cdn.example.com/resource")
	headers := map[string]string{
		"Authorization": "Bearer secret",
		"Cookie":        "session=secret",
		"X-Api-Key":     "secret",
		"Accept":        "image/png",
		"User-Agent":    "test-agent",
	}

	sameOriginHeaders := scopedFetchHeaders(credentialOrigin, sameOrigin, headers)
	connect.AssertEqual(t, sameOriginHeaders.Get("Authorization"), "Bearer secret")
	connect.AssertEqual(t, sameOriginHeaders.Get("X-Api-Key"), "secret")

	otherOriginHeaders := scopedFetchHeaders(credentialOrigin, otherOrigin, headers)
	connect.AssertEqual(t, otherOriginHeaders.Get("Authorization"), "")
	connect.AssertEqual(t, otherOriginHeaders.Get("Cookie"), "")
	connect.AssertEqual(t, otherOriginHeaders.Get("X-Api-Key"), "")
	connect.AssertEqual(t, otherOriginHeaders.Get("Accept"), "image/png")
	connect.AssertEqual(t, otherOriginHeaders.Get("User-Agent"), "test-agent")
}

func TestFetchRedirectStripsCrossOriginCredentials(t *testing.T) {
	original, _ := http.NewRequest(http.MethodGet, "https://example.com/start", nil)
	redirected, _ := http.NewRequest(http.MethodGet, "https://other.example/path", nil)
	redirected.Header.Set("Authorization", "Bearer secret")
	redirected.Header.Set("X-Api-Key", "secret")
	redirected.Header.Set("Accept", "application/json")

	connect.AssertEqual(t, validateFetchRedirect(redirected, []*http.Request{original}), nil)
	connect.AssertEqual(t, redirected.Header.Get("Authorization"), "")
	connect.AssertEqual(t, redirected.Header.Get("X-Api-Key"), "")
	connect.AssertEqual(t, redirected.Header.Get("Accept"), "application/json")

	bodyRedirect, _ := http.NewRequest(http.MethodPost, "https://other.example/path", strings.NewReader("secret"))
	connect.AssertEqual(t, validateFetchRedirect(bodyRedirect, []*http.Request{original}) != nil, true)
}

func TestFetchOneEnforcesCrossOriginCredentialBoundary(t *testing.T) {
	credentialOrigin, err := url.Parse("https://credentials.example/start")
	connect.AssertEqual(t, err, nil)

	receivedHeaders := make(chan http.Header, 1)
	resourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		receivedHeaders <- request.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer resourceServer.Close()
	resourceUrl, err := url.Parse(resourceServer.URL)
	connect.AssertEqual(t, err, nil)

	_, err = fetchOne(
		context.Background(),
		resourceServer.Client(),
		http.MethodGet,
		resourceUrl,
		credentialOrigin,
		map[string]string{
			"Authorization": "Bearer secret",
			"Cookie":        "session=secret",
			"X-Api-Key":     "secret",
			"Accept":        "image/png",
		},
		"",
		1024,
	)
	if err != nil {
		t.Fatalf("fetch cross-origin resource: %v", err)
	}
	resourceHeaders := <-receivedHeaders
	for _, name := range []string{"Authorization", "Cookie", "X-Api-Key"} {
		if value := resourceHeaders.Get(name); value != "" {
			t.Fatalf("cross-origin resource received %s=%q", name, value)
		}
	}
	connect.AssertEqual(t, resourceHeaders.Get("Accept"), "image/png")

	redirectHeaders := make(chan http.Header, 1)
	redirectTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		redirectHeaders <- request.Header.Clone()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer redirectTarget.Close()
	redirectSource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, redirectTarget.URL, http.StatusFound)
	}))
	defer redirectSource.Close()
	redirectUrl, err := url.Parse(redirectSource.URL)
	connect.AssertEqual(t, err, nil)
	redirectClient := redirectSource.Client()
	redirectClient.CheckRedirect = validateFetchRedirect

	_, err = fetchOne(
		context.Background(),
		redirectClient,
		http.MethodGet,
		redirectUrl,
		redirectUrl,
		map[string]string{
			"X-Api-Key": "secret",
			"Accept":    "application/json",
		},
		"",
		1024,
	)
	if err != nil {
		t.Fatalf("follow cross-origin redirect: %v", err)
	}
	redirectedHeaders := <-redirectHeaders
	connect.AssertEqual(t, redirectedHeaders.Get("X-Api-Key"), "")
	connect.AssertEqual(t, redirectedHeaders.Get("Accept"), "application/json")

	bodyTargetReached := make(chan struct{}, 1)
	bodyTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		bodyTargetReached <- struct{}{}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer bodyTarget.Close()
	bodySource := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Redirect(w, request, bodyTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer bodySource.Close()
	bodySourceUrl, err := url.Parse(bodySource.URL)
	connect.AssertEqual(t, err, nil)
	bodyClient := bodySource.Client()
	bodyClient.CheckRedirect = validateFetchRedirect

	_, err = fetchOne(
		context.Background(),
		bodyClient,
		http.MethodPost,
		bodySourceUrl,
		bodySourceUrl,
		nil,
		"secret request body",
		1024,
	)
	if err == nil || !strings.Contains(err.Error(), "refusing to redirect a request body across origins") {
		t.Fatalf("cross-origin body redirect error = %v", err)
	}
	select {
	case <-bodyTargetReached:
		t.Fatal("cross-origin redirect forwarded the request body")
	default:
	}
}

func TestFetchCookiesStayOnPageOrigin(t *testing.T) {
	jar, err := cookiejar.New(nil)
	connect.AssertEqual(t, err, nil)
	pageOrigin, _ := url.Parse("https://example.com/page")
	otherOrigin, _ := url.Parse("https://other.example/page")
	jar.SetCookies(pageOrigin, []*http.Cookie{{Name: "page", Value: "secret"}})
	jar.SetCookies(otherOrigin, []*http.Cookie{{Name: "other", Value: "secret"}})

	scopedJar := &originScopedCookieJar{jar: jar, sendOrigin: pageOrigin}
	connect.AssertEqual(t, len(scopedJar.Cookies(pageOrigin)), 1)
	connect.AssertEqual(t, len(scopedJar.Cookies(otherOrigin)), 0)
}

func TestFetchOneCapsResponseBody(t *testing.T) {
	testServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strings.Repeat("x", 1024)))
	}))
	defer testServer.Close()

	fetchUrl, _ := url.Parse(testServer.URL)
	response, err := fetchOne(
		context.Background(),
		testServer.Client(),
		http.MethodGet,
		fetchUrl,
		fetchUrl,
		nil,
		"",
		32,
	)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(response.body), 32)
	connect.AssertEqual(t, response.truncated, true)
}

func TestFetchResourceByteLimitIsServerOwned(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		requested int
		expected  int
	}{
		{name: "negative", requested: -1, expected: fetchDefaultMaxResourceBytes},
		{name: "zero", requested: 0, expected: fetchDefaultMaxResourceBytes},
		{name: "below ceiling", requested: 1024, expected: 1024},
		{name: "at ceiling", requested: fetchMaxResourceBytes, expected: fetchMaxResourceBytes},
		{name: "above ceiling", requested: fetchMaxResourceBytes + 1, expected: fetchMaxResourceBytes},
	} {
		actual := fetchResourceByteLimit(testCase.requested)
		if actual != testCase.expected {
			t.Errorf("%s: byte limit = %d, want %d", testCase.name, actual, testCase.expected)
		}
	}
}

func TestFetchEmbeddedByteBudgetIsAggregateAndServerOwned(t *testing.T) {
	connect.AssertEqual(
		t,
		fetchEmbeddedResourceByteLimit(fetchMaxResourceBytes+1, 0),
		fetchMaxResourceBytes,
	)
	connect.AssertEqual(
		t,
		fetchEmbeddedResourceByteLimit(fetchMaxResourceBytes, fetchMaxEmbeddedBytes-1024),
		1024,
	)
	connect.AssertEqual(
		t,
		fetchEmbeddedResourceByteLimit(fetchMaxResourceBytes, fetchMaxEmbeddedBytes),
		0,
	)
	connect.AssertEqual(
		t,
		fetchEmbeddedResourceByteLimit(fetchMaxResourceBytes, fetchMaxEmbeddedBytes+1),
		0,
	)
}

func TestFetchConcurrencyIsLimitedPerIdentity(t *testing.T) {
	limiter := newFetchConcurrencyLimiter()
	releaseOne, err := limiter.acquire(context.Background(), "identity-one")
	connect.AssertEqual(t, err, nil)
	defer releaseOne()
	releaseTwo, err := limiter.acquire(context.Background(), "identity-one")
	connect.AssertEqual(t, err, nil)
	defer releaseTwo()

	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	defer cancelWait()
	_, err = limiter.acquire(waitCtx, "identity-one")
	connect.AssertEqual(t, err != nil, true)

	otherRelease, err := limiter.acquire(context.Background(), "identity-two")
	connect.AssertEqual(t, err, nil)
	otherRelease()
}

func TestFetchConcurrencyIsLimitedGlobally(t *testing.T) {
	limiter := newFetchConcurrencyLimiter()
	releases := make([]func(), 0, fetchMaxConcurrentCalls)
	for i := range fetchMaxConcurrentCalls {
		release, err := limiter.acquire(context.Background(), fmt.Sprintf("identity-%d", i))
		connect.AssertEqual(t, err, nil)
		releases = append(releases, release)
	}
	connect.AssertEqual(t, cap(limiter.globalSemaphore), fetchMaxConcurrentCalls)
	connect.AssertEqual(t, len(limiter.globalSemaphore), fetchMaxConcurrentCalls)
	defer func() {
		for _, release := range releases {
			release()
		}
	}()

	waitCtx, cancelWait := context.WithCancel(context.Background())
	cancelWait()
	_, err := limiter.acquire(waitCtx, "overflow")
	connect.AssertEqual(t, err != nil, true)
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
