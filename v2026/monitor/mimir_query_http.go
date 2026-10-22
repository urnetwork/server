// Two logical provider-fleet queries use one authenticated native HTTPS read.
// Physical/node probes retain their existing SSH ownership and source scope.
package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const mimirQueryTimeout = 15 * time.Second

// A supplied synthetic source must explicitly own HTTP. Missing Do support
// cannot fall through to a real network client or an SSH fallback.
type unavailableMimirQuerySource struct{}

func (unavailableMimirQuerySource) Do(*http.Request) (*http.Response, error) {
	return nil, &mimirQueryError{phase: "source-contract"}
}

// Underlying errors remain private and retain typed context/admission identity.
// Only closed phases and an actually received numeric status reach alerts.
type mimirQueryError struct {
	phase  string
	status int
	err    error
}

func (self *mimirQueryError) Error() string {
	var timedOut net.Error
	if errors.As(self.err, &timedOut) && timedOut.Timeout() {
		return "Mimir query timeout"
	}
	switch self.phase {
	case "http-status":
		return fmt.Sprintf("Mimir query HTTP %d", self.status)
	case "source-contract", "configuration-unavailable", "settings-stale", "settings-unavailable":
		return "Mimir query source contract is unavailable"
	case "response-bound", "response-body", "response-encoding":
		return "Mimir query invalid response"
	default:
		return "Mimir query transport unavailable"
	}
}

func (self *mimirQueryError) Unwrap() error { return self.err }

// Keep the shared lifecycle/admission details intact; these additive fields
// belong only to this native request and never claim backend or host identity.
func mimirQueryFailureDetails(err error) string {
	var failure *mimirQueryError
	if !errors.As(err, &failure) {
		return ""
	}
	phase := failure.phase
	switch phase {
	case "source-contract", "configuration-unavailable", "settings-stale", "settings-unavailable", "dns", "dial", "tls", "http", "http-status", "response-bound", "response-body", "response-encoding":
	default:
		phase = "unknown"
	}
	details := " mimir_query_phase=" + phase
	if 100 <= failure.status && failure.status <= 599 {
		details += " http_status=" + strconv.Itoa(failure.status)
	}
	return details
}

// Match the qualified single-request native transport: verified TLS, no
// environment proxy, redirect, pooling, HTTP2 replay or IPv4 fallback request.
func newMimirQueryTransport() *http.Transport {
	dialer := &net.Dialer{Timeout: mimirQueryTimeout, Resolver: &net.Resolver{PreferGo: true}}
	return &http.Transport{
		DialContext:            dialer.DialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		ForceAttemptHTTP2:      false,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
		TLSHandshakeTimeout:    mimirQueryTimeout,
		ResponseHeaderTimeout:  mimirQueryTimeout,
		MaxResponseHeaderBytes: 16 * 1024,
		MaxConnsPerHost:        1,
	}
}

// The owned client cannot turn a redirect into another credentialed request.
func newMimirQueryClient(transport *http.Transport) *http.Client {
	return &http.Client{Transport: transport, Timeout: mimirQueryTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

// Both adapters pass their unchanged POST query, fixed Unix time and body cap.
// The existing full-settings seam fences the credential/scope before sending
// and again before accepting data. It exposes no secret-derived fingerprint.
func readProviderMimirQuery(parent context.Context, env *probeEnv, query string, at time.Time, maxBytes int) (string, error) {
	ctx, cancel := context.WithTimeout(parent, mimirQueryTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hostname := env.cfg.env + "-grafana." + env.cfg.mimirQueryDomain
	if !validMimirQueryHostname(hostname) || net.ParseIP(env.cfg.mimirQueryDomain) != nil || env.cfg.mimirQueryUsername == "" || env.cfg.mimirQueryPassword == "" || strings.ContainsAny(env.cfg.mimirQueryUsername, ":\r\n\x00") || strings.ContainsAny(env.cfg.mimirQueryPassword, "\r\n\x00") || maxBytes <= 0 {
		return "", &mimirQueryError{phase: "configuration-unavailable"}
	}
	checkGeneration := func() error {
		// This legacy-named seam compares all effective SignalSettings, including
		// the new role credential and domain; it is not a router-only comparison.
		if env.cfg.routerGenerationCheck == nil {
			if env.mimirQueryClient == nil {
				return &mimirQueryError{phase: "settings-unavailable"}
			}
			return ctx.Err()
		}
		current, err := env.cfg.routerGenerationCheck(ctx)
		if err != nil {
			return &mimirQueryError{phase: "settings-unavailable", err: err}
		}
		if !current {
			return &mimirQueryError{phase: "settings-stale"}
		}
		return ctx.Err()
	}
	if err := checkGeneration(); err != nil {
		return "", err
	}
	form := url.Values{"query": []string{query}, "time": []string{strconv.FormatInt(at.Unix(), 10)}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+hostname+"/prometheus/api/v1/query", strings.NewReader(form.Encode()))
	if err != nil {
		return "", &mimirQueryError{phase: "configuration-unavailable"}
	}
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.SetBasicAuth(env.cfg.mimirQueryUsername, env.cfg.mimirQueryPassword)
	client := env.mimirQueryClient
	if client == nil {
		transport := newMimirQueryTransport()
		defer transport.CloseIdleConnections()
		client = newMimirQueryClient(transport)
	}
	response, err := doScopedGrafanaRequest(client, request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	if err != nil {
		var sourceError *mimirQueryError
		if errors.As(err, &sourceError) {
			return "", sourceError
		}
		phase := "http"
		var dnsError *net.DNSError
		var operationError *net.OpError
		var certificateError *tls.CertificateVerificationError
		var unknownAuthority x509.UnknownAuthorityError
		var hostnameError x509.HostnameError
		var invalidCertificate x509.CertificateInvalidError
		switch {
		case errors.As(err, &certificateError), errors.As(err, &unknownAuthority), errors.As(err, &hostnameError), errors.As(err, &invalidCertificate):
			phase = "tls"
		case errors.As(err, &dnsError):
			phase = "dns"
		case errors.As(err, &operationError) && operationError.Op == "dial":
			phase = "dial"
		}
		return "", &mimirQueryError{phase: phase, err: err}
	}
	if response == nil {
		return "", &mimirQueryError{phase: "response-body"}
	}
	if response.StatusCode != http.StatusOK {
		return "", &mimirQueryError{phase: "http-status", status: response.StatusCode}
	}
	if response.Body == nil {
		return "", &mimirQueryError{phase: "response-body", status: response.StatusCode}
	}
	if encoding := response.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return "", &mimirQueryError{phase: "response-encoding", status: response.StatusCode}
	}
	if response.ContentLength > int64(maxBytes) {
		return "", &mimirQueryError{phase: "response-bound", status: response.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return "", &mimirQueryError{phase: "response-body", status: response.StatusCode, err: err}
	}
	if len(body) > maxBytes {
		return "", &mimirQueryError{phase: "response-bound", status: response.StatusCode}
	}
	if err := checkGeneration(); err != nil {
		return "", err
	}
	return string(body), nil
}
