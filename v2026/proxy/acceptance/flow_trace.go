package acceptance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026/proxy/flowtrace"
)

type flowTraceClient struct {
	url, token, session string
	client              *http.Client
	dnsEnabled          bool
}

func startFlowTrace(ctx context.Context, config *proxyConfigResult, target string) (*flowTraceClient, error) {
	parsed, err := url.Parse(target)
	if err != nil || parsed.Scheme != "https" {
		return nil, errors.New("flow tracing requires an HTTPS target")
	}
	port, err := strconv.ParseUint(urlPort(target), 10, 16)
	if err != nil || port == 0 || config.APIBaseURL == "" || config.AuthToken == "" {
		return nil, errors.New("flow tracing requires proxy API credentials and target port")
	}
	apiURL, err := url.Parse(config.APIBaseURL)
	if err != nil || apiURL.Scheme != "https" || apiURL.Host == "" || apiURL.User != nil {
		return nil, errors.New("flow tracing requires an authenticated HTTPS API")
	}
	client := &flowTraceClient{
		url: strings.TrimRight(config.APIBaseURL, "/") + "/diagnostics/flow-trace", token: config.AuthToken,
		client: &http.Client{
			Transport: &http.Transport{}, Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if err := client.start(ctx, uint16(port)); err != nil {
		client.client.CloseIdleConnections()
		return nil, err
	}
	return client, nil
}

func (client *flowTraceClient) start(ctx context.Context, port uint16) error {
	body, _ := json.Marshal(flowtrace.StartRequest{Port: port, IncludeDns: true})
	snapshot, err := client.call(ctx, http.MethodPost, "", body)
	if err != nil {
		return err
	}
	if !snapshot.DnsEnabled {
		return errors.New("deployed server does not support DNS flow diagnostics")
	}
	client.session = snapshot.Session
	client.dnsEnabled = true
	return nil
}

// Disarm this exact generation even when the probe's request context has
// expired. A failed disarm is reported by the caller; server expiry remains a
// bounded fallback, not the intended cleanup path.
func (c *flowTraceClient) stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := c.call(ctx, http.MethodDelete, "", nil)
	c.client.CloseIdleConnections()
	return err
}

func (c *flowTraceClient) call(ctx context.Context, method, query string, body []byte) (flowtrace.Snapshot, error) {
	var snapshot flowtrace.Snapshot
	request, err := http.NewRequestWithContext(ctx, method, c.url+query, bytes.NewReader(body))
	if err != nil {
		return snapshot, errors.New("invalid diagnostic API URL")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-UR-Flow-Trace", c.session)
	response, err := c.client.Do(request)
	if err != nil {
		return snapshot, errors.New("diagnostic API request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return snapshot, fmt.Errorf("diagnostic API status %d", response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseBytes+1))
	if err != nil || len(data) > maxAPIResponseBytes || json.Unmarshal(data, &snapshot) != nil {
		return snapshot, errors.New("invalid diagnostic API response")
	}
	if snapshot.Session == "" || (c.session != "" && snapshot.Session != c.session) {
		return snapshot, errors.New("diagnostic generation changed")
	}
	return snapshot, nil
}

type flowTraceTransport struct {
	protocol  string
	transport http.RoundTripper
	trace     *flowTraceClient
}

type flowTraceDnsContextKey struct{}

func (t flowTraceTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.trace.dnsEnabled && t.protocol == "wireguard" {
		request = request.WithContext(context.WithValue(request.Context(), flowTraceDnsContextKey{}, true))
	}
	return t.transport.RoundTrip(request)
}

// probeHTTPSRequest calls these diagnostics outside http.Client.Do, keeping
// the target's timeout and normal TLS error wrapping unchanged. Missing
// diagnostics never substitute a failure for a successful target response.
func (t flowTraceTransport) failure(ctx context.Context, trace *httpsRequestTrace, before flowtrace.Snapshot, beforeErr, requestErr error) error {
	if beforeErr != nil {
		return fmt.Errorf("%w; flow_provenance={unavailable: %s}", requestErr, beforeErr)
	}
	trace.stateLock.Lock()
	origin, localPort := trace.originAddress, trace.originLocalPort
	dnsQueries := append([]flowtrace.DnsQuery(nil), trace.dnsQueries...)
	trace.stateLock.Unlock()
	// Keep target timing and its error intact while obtaining the already
	// recorded metadata. This read never retries the target request.
	diagnosticCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	after, err := t.trace.call(diagnosticCtx, http.MethodGet, fmt.Sprintf("?after=%d&after_dropped=%d", before.Cursor, before.Dropped), nil)
	if err != nil {
		return fmt.Errorf("%w; flow_provenance={unavailable: %s}", requestErr, err)
	}
	if t.trace.dnsEnabled && t.protocol == "wireguard" {
		requestErr = withDnsFlowEvidence(requestErr, after, dnsQueries)
	}
	flow, returns, attributionErr := flowtrace.Attribute(after, t.protocol, origin, localPort)
	if attributionErr != nil {
		return fmt.Errorf("%w; flow_provenance={unavailable: %s}", requestErr, attributionErr)
	}
	trace.stateLock.Lock()
	trace.originAddress = flow.Origin
	trace.stateLock.Unlock()
	providers := map[string]bool{}
	for _, event := range returns {
		providers[event.Provider] = true
	}
	providerNames := make([]string, 0, len(providers))
	for provider := range providers {
		providerNames = append(providerNames, provider)
	}
	sort.Strings(providerNames)
	events := []string{}
	for _, event := range returns[:min(8, len(returns))] {
		events = append(events, fmt.Sprintf("%s provider=%s seq=%d bytes=%d", event.At.UTC().Format(time.RFC3339Nano), event.Provider, event.Sequence, event.Bytes))
	}
	return fmt.Errorf("%w; flow_provenance={scope=actual-return-flow origin=%s local=%s provider_count=%d first_providers=[%s] payload_events=%d first_events=[%s]}",
		requestErr, flow.Origin, flow.Local, len(providerNames), strings.Join(providerNames[:min(8, len(providerNames))], ","), len(returns), strings.Join(events, " | "))
}

func withDnsFlowEvidence(cause error, snapshot flowtrace.Snapshot, queries []flowtrace.DnsQuery) error {
	evidence, err := flowtrace.AttributeDns(snapshot, queries)
	if err != nil {
		return fmt.Errorf("%w; dns_flow_provenance={unavailable: %s}", cause, err)
	}
	items := make([]string, 0, len(evidence))
	for index, item := range evidence {
		if item.Unavailable != "" {
			items = append(items, fmt.Sprintf("q%d unavailable=%s", index, item.Unavailable))
			continue
		}
		returns := make([]string, 0, min(4, len(item.Replies)))
		for _, event := range item.Replies[:min(4, len(item.Replies))] {
			returns = append(returns, fmt.Sprintf("%s/provider=%s/bytes=%d", event.At.UTC().Format(time.RFC3339Nano), event.Provider, event.Dns.PacketBytes))
		}
		items = append(items, fmt.Sprintf("q%d local=%s resolver=%s socket_active_at_freeze=%t admission=%s replies=%d first_returns=[%s] other_id_replies=%d tuple_only_icmp_generation_unproven=%d",
			index, item.Flow.Local, item.Flow.Origin, item.Query.Active, item.Admission, len(item.Replies), strings.Join(returns, ","), item.OtherIdReplies, item.TupleOnlyIcmp))
	}
	return fmt.Errorf("%w; dns_flow_provenance={scope=hosted-submission-and-authenticated-return correlation=tuple+dns-id-not-authenticated checksums=unchecked admission_not_upstream_write=true queries=[%s]}", cause, strings.Join(items, " | "))
}

func withFlowTrace(config *proxyConfigResult, protocol string, transport http.RoundTripper) http.RoundTripper {
	if config.flowTrace == nil {
		return transport
	}
	return flowTraceTransport{protocol: protocol, transport: transport, trace: config.flowTrace}
}
