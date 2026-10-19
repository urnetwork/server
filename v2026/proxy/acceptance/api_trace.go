package acceptance

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

const (
	apiGetConn = iota
	apiDNSStart
	apiDNSDone
	apiConnectStart
	apiConnectDone
	apiTLSStart
	apiTLSDone
	apiGotConn
	apiWriteDone
	apiFirstResponseByte
	apiHeadersReceived
	apiMilestoneCount
)

var apiMilestoneNames = [apiMilestoneCount]string{
	"get_conn", "dns_start", "dns_done", "connect_start", "connect_done",
	"tls_start", "tls_done", "got_conn", "write_done", "first_response_byte", "headers_received",
}

// API diagnostics are separate from proxy tunnel diagnostics: they retain only
// fixed transport milestones, never URL/peer identities, headers, or payloads.
// httptrace callbacks may outlive Do during cancellation or competing dials.
type apiRequestTrace struct {
	mu             sync.Mutex
	started        time.Time
	phase          string
	phaseStarted   time.Time
	phaseMilestone int
	connection     string
	statusCode     int
	milestones     [apiMilestoneCount]time.Time
	frozen         bool
}

func newAPIRequestTrace() *apiRequestTrace {
	started := time.Now()
	return &apiRequestTrace{
		started: started, phase: "starting_request", phaseStarted: started,
		phaseMilestone: -1, connection: "not_established",
	}
}

func (t *apiRequestTrace) recordLocked(milestone int, phase string) {
	if t.frozen {
		return
	}
	now := time.Now()
	if t.milestones[milestone].IsZero() {
		t.milestones[milestone] = now
	}
	// A losing dial or a late write callback must not overwrite evidence that
	// this request already obtained a connection or received response headers.
	if milestone >= t.phaseMilestone {
		t.phase = phase
		t.phaseStarted = now
		t.phaseMilestone = milestone
	}
}

func (t *apiRequestTrace) record(milestone int, phase string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.recordLocked(milestone, phase)
}

func (t *apiRequestTrace) clientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn:  func(string) { t.record(apiGetConn, "waiting_for_connection") },
		DNSStart: func(httptrace.DNSStartInfo) { t.record(apiDNSStart, "resolving_api") },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			phase := "api_resolved"
			if info.Err != nil {
				phase = "resolving_api_failed"
			}
			t.record(apiDNSDone, phase)
		},
		ConnectStart: func(string, string) { t.record(apiConnectStart, "connecting_api") },
		ConnectDone: func(_, _ string, err error) {
			phase := "api_connected"
			if err != nil {
				phase = "connecting_api_failed"
			}
			t.record(apiConnectDone, phase)
		},
		TLSHandshakeStart: func() { t.record(apiTLSStart, "tls_handshake") },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			phase := "tls_complete"
			if err != nil {
				phase = "tls_handshake_failed"
			}
			t.record(apiTLSDone, phase)
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if t.frozen {
				return
			}
			t.connection = "new"
			if info.Reused {
				t.connection = "reused"
			}
			t.recordLocked(apiGotConn, "writing_request")
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			phase := "waiting_for_response_headers"
			if info.Err != nil {
				phase = "writing_request_failed"
			}
			t.record(apiWriteDone, phase)
		},
		GotFirstResponseByte: func() { t.record(apiFirstResponseByte, "reading_response_headers") },
	}
}

func (t *apiRequestTrace) responseReceived(statusCode int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.frozen {
		return
	}
	t.statusCode = statusCode
	t.recordLocked(apiHeadersReceived, "reading_response_body")
}

// Error text is a bounded metadata snapshot. The cause remains available to
// errors.Is/As, but is never formatted: transport errors can contain full URLs,
// proxy credentials, or arbitrary strings returned by a response-body reader.
type apiRequestFailure struct {
	detail string
	cause  error
}

func (e *apiRequestFailure) Error() string { return e.detail }
func (e *apiRequestFailure) Unwrap() error { return e.cause }

func (t *apiRequestTrace) failure(path string, cause error, bodyBytes int) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.frozen = true
	finished := time.Now()
	milestones := make([]string, 0, apiMilestoneCount)
	for index, name := range apiMilestoneNames {
		elapsed := "unobserved"
		if !t.milestones[index].IsZero() {
			elapsed = t.milestones[index].Sub(t.started).Round(time.Millisecond).String()
		}
		milestones = append(milestones, name+"="+elapsed)
	}
	status := "unavailable"
	if t.statusCode != 0 {
		status = fmt.Sprint(t.statusCode)
	}
	// Only these fixed acceptance endpoints may appear in a diagnostic. Never
	// derive a label from request.URL, a redirect, or an arbitrary caller path.
	switch path {
	case "/auth/login-with-password", "/network/auth-client", "/network/remove-client":
	default:
		path = "other"
	}
	return &apiRequestFailure{
		detail: fmt.Sprintf(
			"API POST %s; request started %s; elapsed %s; phase %s; phase_elapsed %s; connection %s; status %s; body_bytes %d; milestones={%s}: %s",
			path, t.started.UTC().Format(time.RFC3339Nano), finished.Sub(t.started).Round(time.Millisecond),
			t.phase, finished.Sub(t.phaseStarted).Round(time.Millisecond), t.connection,
			status, bodyBytes, strings.Join(milestones, " "), apiTransportFailureReason(cause),
		),
		cause: cause,
	}
}

func apiTransportFailureReason(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "context deadline exceeded"
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected EOF"
	case errors.Is(err, io.EOF):
		return "EOF"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return "network timeout"
	}
	var dnsError *net.DNSError
	if errors.As(err, &dnsError) {
		return "DNS lookup failed"
	}
	var certificateError *tls.CertificateVerificationError
	if errors.As(err, &certificateError) {
		return "TLS certificate verification failed"
	}
	return "HTTP transport failed"
}
