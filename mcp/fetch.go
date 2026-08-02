package mcp

// The fetch tool: an http request issued from a chosen egress location, with
// the page's static resources optionally returned alongside it.
//
// Everything that must survive between calls travels through the caller,
// because the transport is stateless: the signed proxy id names the egress,
// the sealed cookie jar carries the session, and a sealed continuation carries
// resources that did not fit in the call budget. Each result restates exactly
// what to pass back, since a caller that has to infer the protocol from a
// schema gets it wrong.
//
// One call is bounded by fetchCallBudget, which sits inside the http write
// timeout, so a page with many resources finishes across several calls instead
// of being cut off mid-response.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/oauth"
)

// Tunables. The proxy and target knobs are variables rather than constants so
// the integration test can point at a loopback web server through a plain http
// proxy; production leaves them at their defaults.
var (
	fetchUsePlainHttpProxy   = false
	fetchAllowPrivateTargets = false
	// when set, the proxy ingress is reached at this host:port instead of the
	// one recorded on the proxy client row. The row names the configured proxy
	// host, which in an integration test is not the ephemeral local ingress.
	fetchProxyAddrOverride = ""
)

const (
	// wall clock for one call, comfortably inside the server write timeout so
	// the response is always delivered rather than cut
	fetchCallBudget = 20 * time.Second

	fetchMaxRedirects        = 5
	fetchMaxIdleConns        = 32
	fetchIdleConnTimeout     = 30 * time.Second
	fetchTlsHandshakeTimeout = 10 * time.Second
	fetchProxyDialTimeout    = 10 * time.Second
	fetchProxyKeepAlive      = 30 * time.Second

	fetchDefaultMaxResources     = 8
	fetchMaxResources            = 32
	fetchDefaultMaxResourceBytes = 512 * 1024
	fetchMaxBodyBytes            = 4 * 1024 * 1024

	// sealed state outlives a normal agent session without being indefinite
	fetchSealTtl = 6 * time.Hour

	includeResourcesNone  = "none"
	includeResourcesLinks = "links"
	includeResourcesEmbed = "embed"
)

const (
	MsgFetchUnauthenticated = "This tool requires an authorized access token carrying the mcp:fetch scope."
	MsgFetchUpgradeRequired = "The network is at its plan's concurrent client limit and cannot open another egress proxy."
)

// Input of the fetch tool. Every threaded field is optional on the first call
// and is meant to be echoed back from the previous result afterwards.
type FetchArgs struct {
	Url    string `json:"url" jsonschema:"The absolute http or https URL to load, e.g. 'https://example.com/page'."`
	Method string `json:"method,omitempty" jsonschema:"HTTP method. Defaults to GET."`

	Location string `json:"location,omitempty" jsonschema:"Where the request should egress from, as a place name such as 'Japan', 'Germany', or 'New York'. Ignored when signed_proxy_id is given and still valid. Keep passing the same location alongside signed_proxy_id so the egress can be re-established if it expires."`

	SignedProxyId string `json:"signed_proxy_id,omitempty" jsonschema:"Pass back the signed_proxy_id from a previous fetch result to reuse the same egress location for follow-on loads. Omit on the first call."`
	Cookies       string `json:"cookies,omitempty" jsonschema:"Pass back the cookies value from a previous fetch result to keep the session (logins, consent) across loads. Opaque; do not modify or interpret it."`
	Continuation  string `json:"continuation,omitempty" jsonschema:"Pass back the continuation value from a previous fetch result to collect the resources that did not fit in that call. When set, url is ignored."`

	Headers map[string]string `json:"headers,omitempty" jsonschema:"Extra request headers to send."`
	Body    string            `json:"body,omitempty" jsonschema:"Request body, for methods that take one."`

	IncludeResources string `json:"include_resources,omitempty" jsonschema:"How to return resources the page references (images, stylesheets, scripts, media): 'none', 'links' to list them without content, or 'embed' to also return their bytes. Defaults to 'links'."`
	MaxResources     int    `json:"max_resources,omitempty" jsonschema:"Maximum number of referenced resources to return. Defaults to 8."`
	MaxResourceBytes int    `json:"max_resource_bytes,omitempty" jsonschema:"Maximum bytes for any single embedded resource; larger ones are returned as links instead. Defaults to 524288."`

	Payment string `json:"payment,omitempty" jsonschema:"A signed x402 payment. Send this only when a previous fetch result returned payment_required, retrying the same call with the signed payment to settle the upgrade and continue."`
}

// One resource the page referenced.
type FetchResourceEntry struct {
	Url         string `json:"url"`
	Kind        string `json:"kind"`
	ContentType string `json:"content_type,omitempty"`
	SizeBytes   int    `json:"size_bytes,omitempty"`
	Embedded    bool   `json:"embedded"`
	Error       string `json:"error,omitempty"`
}

// Structured output of the fetch tool.
type FetchResult struct {
	SignedProxyId string `json:"signed_proxy_id,omitempty"`
	Cookies       string `json:"cookies,omitempty"`
	Continuation  string `json:"continuation,omitempty"`

	Location    string `json:"location,omitempty"`
	Status      int    `json:"status,omitempty"`
	FinalUrl    string `json:"final_url,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	DurationMs  int64  `json:"duration_ms"`
	Truncated   bool   `json:"truncated,omitempty"`

	Resources []*FetchResourceEntry `json:"resources"`

	PaymentRequired *controller.X402PaymentRequired `json:"payment_required,omitempty"`

	// what the caller should do next, restated in the structured output as
	// well as in the text content
	NextStep string `json:"next_step,omitempty"`
}

// Sealed state carrying resources that did not fit in the call budget.
type fetchContinuation struct {
	SignedProxyId string        `json:"p"`
	PageUrl       string        `json:"u"`
	Pending       []*pendingRef `json:"r"`
	Include       string        `json:"i"`
	MaxBytes      int           `json:"b"`
}

type pendingRef struct {
	Url  string `json:"u"`
	Kind string `json:"k"`
}

// Sealed cookie jar, stored per origin so it can be rehydrated into a
// net/http jar on the next call.
type cookieJarState struct {
	Origins map[string][]*storedCookie `json:"o"`
}

type storedCookie struct {
	Name  string `json:"n"`
	Value string `json:"v"`
}

// Handles the fetch tool call.
func fetchTool(
	ctx context.Context,
	req *mcpsdk.CallToolRequest,
	args FetchArgs,
) (*mcpsdk.CallToolResult, *FetchResult, error) {
	startTime := time.Now()

	// this tool provisions a billed egress client, so it needs its own scope
	// beyond the read scope the transport already required
	if !tokenHasScope(ctx, oauth.ScopeMcpFetch) {
		return insufficientScopeResult(oauth.ScopeMcpFetch), nil, nil
	}

	clientSession, err := clientSessionFromToken(ctx, req)
	if err != nil {
		return fetchErrorResult(MsgFetchUnauthenticated), nil, nil
	}
	defer clientSession.Cancel()

	if args.Payment != "" {
		if err := settleFetchPayment(clientSession, args.Payment); err != nil {
			glog.Infof("[mcp]fetch payment settle error = %s\n", err)
			return fetchErrorResult(fmt.Sprintf("The payment could not be settled: %s", err)), nil, nil
		}
	}

	budgetCtx, cancelBudget := context.WithTimeout(clientSession.Ctx, fetchCallBudget)
	defer cancelBudget()

	var continuation *fetchContinuation
	if args.Continuation != "" {
		continuation = &fetchContinuation{}
		if err := unseal(sealLabelContinuation, args.Continuation, continuation); err != nil {
			return fetchErrorResult(
				"The continuation is expired or invalid. Start the page again with url.",
			), nil, nil
		}
		// the continuation names the egress it was produced against
		args.SignedProxyId = continuation.SignedProxyId
	}

	// validate the target before provisioning anything: acquiring an egress
	// opens a billed client, which a request that is going to be refused
	// should never pay for
	var pageUrl *url.URL
	if continuation == nil {
		pageUrl, err = url.Parse(strings.TrimSpace(args.Url))
		if err != nil {
			return fetchErrorResult("The url could not be parsed."), nil, nil
		}
		if err := validateFetchUrl(pageUrl); err != nil {
			return fetchErrorResult(err.Error()), nil, nil
		}
	}

	proxy, upgrade, err := acquireProxy(clientSession, args.SignedProxyId, args.Location)
	if err != nil {
		glog.Infof("[mcp]fetch acquire proxy error = %s\n", err)
		return fetchErrorResult(err.Error()), nil, nil
	}
	if upgrade != nil {
		return fetchUpgradeResult(upgrade)
	}

	jar, err := newCookieJar(args.Cookies)
	if err != nil {
		return fetchErrorResult("The cookies value is expired or invalid. Retry without it."), nil, nil
	}

	httpClient, err := newProxyHttpClient(proxy, fetchCallBudget)
	if err != nil {
		return fetchErrorResult(err.Error()), nil, nil
	}
	httpClient.Jar = jar

	out := &FetchResult{
		SignedProxyId: proxy.signedProxyId,
		Location:      proxy.location,
		Resources:     []*FetchResourceEntry{},
	}
	contents := []mcpsdk.Content{}
	origins := map[string]*url.URL{}

	includeResources := args.IncludeResources
	maxResourceBytes := args.MaxResourceBytes
	var pending []*pendingRef

	if continuation != nil {
		// resuming: the page is already delivered, only resources remain
		out.FinalUrl = continuation.PageUrl
		includeResources = continuation.Include
		maxResourceBytes = continuation.MaxBytes
		pending = continuation.Pending
	} else {
		pageUrl, err := url.Parse(strings.TrimSpace(args.Url))
		if err != nil {
			return fetchErrorResult("The url could not be parsed."), nil, nil
		}
		if err := validateFetchUrl(pageUrl); err != nil {
			return fetchErrorResult(err.Error()), nil, nil
		}

		page, err := fetchOne(budgetCtx, httpClient, args.Method, pageUrl, args.Headers, args.Body, fetchMaxBodyBytes)
		if err != nil {
			glog.Infof("[mcp]fetch %s error = %s\n", displayHost(pageUrl), err)
			return fetchErrorResult(fmt.Sprintf("The request failed: %s", err)), nil, nil
		}

		origins[originKey(page.finalUrl)] = page.finalUrl
		out.Status = page.status
		out.FinalUrl = page.finalUrl.String()
		out.ContentType = page.contentType
		out.Truncated = page.truncated

		if isTextContentType(page.contentType) {
			contents = append(contents, &mcpsdk.TextContent{Text: string(page.body)})
		} else {
			contents = append(contents, resourceContent(page.finalUrl, page.contentType, page.body))
		}

		if includeResources != includeResourcesNone && isHtmlContentType(page.contentType) {
			maxResources := args.MaxResources
			if maxResources <= 0 {
				maxResources = fetchDefaultMaxResources
			}
			if fetchMaxResources < maxResources {
				maxResources = fetchMaxResources
			}
			for _, ref := range extractResourceRefs(page.finalUrl, page.body, maxResources) {
				pending = append(pending, &pendingRef{Url: ref.url.String(), Kind: ref.kind})
			}
		}
	}

	if includeResources == "" {
		includeResources = includeResourcesLinks
	}
	if maxResourceBytes <= 0 {
		maxResourceBytes = fetchDefaultMaxResourceBytes
	}

	// resources are embedded until the budget runs out; whatever is left is
	// handed back as a continuation rather than dropped silently
	remaining := []*pendingRef{}
	for i, ref := range pending {
		refUrl, err := url.Parse(ref.Url)
		if err != nil {
			continue
		}

		if includeResources != includeResourcesEmbed || budgetSpent(budgetCtx) {
			remaining = append(remaining, pending[i:]...)
			break
		}

		resource, err := fetchOne(budgetCtx, httpClient, http.MethodGet, refUrl, args.Headers, "", maxResourceBytes+1)
		if err != nil {
			out.Resources = append(out.Resources, &FetchResourceEntry{
				Url:      ref.Url,
				Kind:     ref.Kind,
				Embedded: false,
				Error:    err.Error(),
			})
			continue
		}
		origins[originKey(resource.finalUrl)] = resource.finalUrl

		if maxResourceBytes < len(resource.body) {
			// too large to embed; the caller can still load it directly
			out.Resources = append(out.Resources, &FetchResourceEntry{
				Url:         ref.Url,
				Kind:        ref.Kind,
				ContentType: resource.contentType,
				SizeBytes:   len(resource.body),
				Embedded:    false,
			})
			contents = append(contents, resourceLink(refUrl, ref.Kind, resource.contentType, len(resource.body)))
			continue
		}

		out.Resources = append(out.Resources, &FetchResourceEntry{
			Url:         ref.Url,
			Kind:        ref.Kind,
			ContentType: resource.contentType,
			SizeBytes:   len(resource.body),
			Embedded:    true,
		})
		contents = append(contents, resourceContent(resource.finalUrl, resource.contentType, resource.body))
	}

	// anything not embedded is still reported, as a link
	for _, ref := range remaining {
		refUrl, err := url.Parse(ref.Url)
		if err != nil {
			continue
		}
		out.Resources = append(out.Resources, &FetchResourceEntry{
			Url:      ref.Url,
			Kind:     ref.Kind,
			Embedded: false,
		})
		contents = append(contents, resourceLink(refUrl, ref.Kind, "", 0))
	}

	if includeResources == includeResourcesEmbed && 0 < len(remaining) {
		sealedContinuation, err := seal(sealLabelContinuation, &fetchContinuation{
			SignedProxyId: proxy.signedProxyId,
			PageUrl:       out.FinalUrl,
			Pending:       remaining,
			Include:       includeResources,
			MaxBytes:      maxResourceBytes,
		}, fetchSealTtl)
		if err == nil {
			out.Continuation = sealedContinuation
		}
	}

	if sealedCookies, err := sealCookieJar(jar, origins); err == nil {
		out.Cookies = sealedCookies
	}

	out.DurationMs = time.Since(startTime).Milliseconds()
	out.NextStep = fetchNextStep(out)

	contents = append(contents, &mcpsdk.TextContent{Text: out.NextStep})

	return &mcpsdk.CallToolResult{Content: contents}, out, nil
}

// One loaded url.
type fetchedResponse struct {
	status      int
	finalUrl    *url.URL
	contentType string
	body        []byte
	truncated   bool
}

func fetchOne(
	ctx context.Context,
	httpClient *http.Client,
	method string,
	fetchUrl *url.URL,
	headers map[string]string,
	body string,
	maxBytes int,
) (*fetchedResponse, error) {
	if method == "" {
		method = http.MethodGet
	}

	var bodyReader io.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	}

	request, err := http.NewRequestWithContext(ctx, strings.ToUpper(method), fetchUrl.String(), bodyReader)
	if err != nil {
		return nil, err
	}
	for name, value := range headers {
		// the egress identity is ours to set, not the caller's
		if strings.EqualFold(name, "Proxy-Authorization") || strings.EqualFold(name, "Host") {
			continue
		}
		request.Header.Set(name, value)
	}
	if request.Header.Get("User-Agent") == "" {
		request.Header.Set("User-Agent", fetchUserAgent)
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	// read one byte past the cap so truncation is detectable
	limited, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, err
	}
	truncated := false
	if maxBytes < len(limited) {
		limited = limited[:maxBytes]
		truncated = true
	}

	finalUrl := response.Request.URL
	if finalUrl == nil {
		finalUrl = fetchUrl
	}

	return &fetchedResponse{
		status:      response.StatusCode,
		finalUrl:    finalUrl,
		contentType: response.Header.Get("Content-Type"),
		body:        limited,
		truncated:   truncated,
	}, nil
}

const fetchUserAgent = "Mozilla/5.0 (compatible; URnetwork-MCP/1.0)"

func budgetSpent(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
		return false
	}
}

// Renders a loaded resource as the content block that best matches its type,
// so hosts render images and audio natively instead of showing base64.
func resourceContent(resourceUrl *url.URL, contentType string, body []byte) mcpsdk.Content {
	switch mediaContentKind(contentType) {
	case resourceKindImage:
		return &mcpsdk.ImageContent{Data: body, MIMEType: mediaType(contentType)}
	case "audio":
		return &mcpsdk.AudioContent{Data: body, MIMEType: mediaType(contentType)}
	}

	if isTextContentType(contentType) {
		return &mcpsdk.EmbeddedResource{
			Resource: &mcpsdk.ResourceContents{
				URI:      resourceUrl.String(),
				MIMEType: mediaType(contentType),
				Text:     string(body),
			},
		}
	}

	return &mcpsdk.EmbeddedResource{
		Resource: &mcpsdk.ResourceContents{
			URI:      resourceUrl.String(),
			MIMEType: mediaType(contentType),
			Blob:     body,
		},
	}
}

func resourceLink(resourceUrl *url.URL, kind string, contentType string, sizeBytes int) mcpsdk.Content {
	link := &mcpsdk.ResourceLink{
		URI:         resourceUrl.String(),
		Name:        resourceUrl.Path,
		Description: fmt.Sprintf("%s referenced by the page", kind),
		MIMEType:    mediaType(contentType),
	}
	if 0 < sizeBytes {
		size := int64(sizeBytes)
		link.Size = &size
	}
	return link
}

func mediaType(contentType string) string {
	if i := strings.Index(contentType, ";"); 0 <= i {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

func originKey(u *url.URL) string {
	return fmt.Sprintf("%s://%s", u.Scheme, u.Host)
}

// Rehydrates the sealed jar into a net/http cookie jar.
func newCookieJar(sealedCookies string) (http.CookieJar, error) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if sealedCookies == "" {
		return jar, nil
	}

	state := &cookieJarState{}
	if err := unseal(sealLabelCookies, sealedCookies, state); err != nil {
		return nil, err
	}

	for origin, cookies := range state.Origins {
		originUrl, err := url.Parse(origin)
		if err != nil {
			continue
		}
		httpCookies := []*http.Cookie{}
		for _, cookie := range cookies {
			httpCookies = append(httpCookies, &http.Cookie{
				Name:  cookie.Name,
				Value: cookie.Value,
				Path:  "/",
			})
		}
		jar.SetCookies(originUrl, httpCookies)
	}

	return jar, nil
}

// Captures the jar for every origin the call touched. The jar is keyed by
// origin rather than replayed verbatim, which loses cross-subdomain scoping
// but keeps the sealed blob small and predictable.
func sealCookieJar(jar http.CookieJar, origins map[string]*url.URL) (string, error) {
	state := &cookieJarState{Origins: map[string][]*storedCookie{}}

	for origin, originUrl := range origins {
		cookies := jar.Cookies(originUrl)
		if len(cookies) == 0 {
			continue
		}
		stored := []*storedCookie{}
		for _, cookie := range cookies {
			stored = append(stored, &storedCookie{Name: cookie.Name, Value: cookie.Value})
		}
		sort.Slice(stored, func(i int, j int) bool {
			return stored[i].Name < stored[j].Name
		})
		state.Origins[origin] = stored
	}

	if len(state.Origins) == 0 {
		return "", nil
	}
	return seal(sealLabelCookies, state, fetchSealTtl)
}

// The threading protocol, restated against the values actually produced. This
// is the copy callers follow most reliably, because it sits in the result they
// just read rather than in a schema they saw earlier.
func fetchNextStep(out *FetchResult) string {
	steps := []string{}

	if out.SignedProxyId != "" {
		steps = append(steps, fmt.Sprintf(
			"To load another page from the same location (%s), call fetch again and pass signed_proxy_id=%q. Keep passing location as well so the egress can be re-established if it expires.",
			out.Location,
			out.SignedProxyId,
		))
	}
	if out.Cookies != "" {
		steps = append(steps, "Pass cookies back unchanged on the next call to keep the site session (logins, consent banners). It is opaque; do not edit it.")
	}
	if out.Continuation != "" {
		steps = append(steps, "Some referenced resources did not fit in this call. Call fetch again passing continuation to collect the rest; url is not needed.")
	}
	if len(steps) == 0 {
		return "No follow-on state to carry."
	}

	return "Next steps: " + strings.Join(steps, " ")
}

func fetchErrorResult(message string) *mcpsdk.CallToolResult {
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: message}},
	}
}

// A plan limit is not a failure the caller can retry blindly: it either needs
// an upgrade or, with x402 enabled, a payment it can sign and resend on the
// same call.
func fetchUpgradeResult(upgrade *upgradeRequired) (*mcpsdk.CallToolResult, *FetchResult, error) {
	message := upgrade.message
	if message == "" {
		message = MsgFetchUpgradeRequired
	}

	out := &FetchResult{
		Resources:       []*FetchResourceEntry{},
		PaymentRequired: upgrade.terms,
	}

	if upgrade.terms != nil {
		out.NextStep = "Payment is required to open another egress proxy. Sign the payment described in payment_required and call fetch again with the same arguments plus payment set to the signed payment. The upgrade is settled inline and the fetch proceeds."
	} else {
		out.NextStep = "The network is at its plan's client limit. Reuse an existing signed_proxy_id, or upgrade the network's plan, then retry."
	}

	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: message},
			&mcpsdk.TextContent{Text: out.NextStep},
		},
	}, out, nil
}
