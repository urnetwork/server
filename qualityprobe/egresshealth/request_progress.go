package egresshealth

import (
	"context"
	"crypto/tls"
	"net/http/httptrace"
	"sync/atomic"
)

// requestProgress is local diagnostic evidence for a generic net/http
// deadline. It cannot identify DNS versus socket versus TLS while obtaining a
// connection, but it does distinguish that phase from request write and
// response wait. Typed provider-tunnel stage errors remain authoritative.
type requestProgress struct{ phase atomic.Uint32 }

const (
	requestPhaseConnect uint32 = iota + 1
	requestPhaseDNS
	requestPhaseAfterDNS
	requestPhaseDial
	requestPhaseAfterDial
	requestPhaseTLS
	requestPhaseAfterTLS
	requestPhaseConnected
	requestPhaseWritten
	requestPhaseResponseByte
)

func (self *requestProgress) advance(phase uint32) {
	for previous := self.phase.Load(); previous < phase; previous = self.phase.Load() {
		if self.phase.CompareAndSwap(previous, phase) {
			return
		}
	}
}

func traceRequestProgress(ctx context.Context) (context.Context, *requestProgress) {
	progress := &requestProgress{}
	trace := &httptrace.ClientTrace{
		GetConn:      func(string) { progress.advance(requestPhaseConnect) },
		DNSStart:     func(httptrace.DNSStartInfo) { progress.advance(requestPhaseDNS) },
		DNSDone:      func(httptrace.DNSDoneInfo) { progress.advance(requestPhaseAfterDNS) },
		ConnectStart: func(string, string) { progress.advance(requestPhaseDial) },
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				progress.advance(requestPhaseAfterDial)
			}
		},
		TLSHandshakeStart: func() { progress.advance(requestPhaseTLS) },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if err == nil {
				progress.advance(requestPhaseAfterTLS)
			}
		},
		GotConn:              func(httptrace.GotConnInfo) { progress.advance(requestPhaseConnected) },
		WroteRequest:         func(httptrace.WroteRequestInfo) { progress.advance(requestPhaseWritten) },
		GotFirstResponseByte: func() { progress.advance(requestPhaseResponseByte) },
	}
	return httptrace.WithClientTrace(ctx, trace), progress
}

func (self *requestProgress) timeoutStage() string {
	switch self.phase.Load() {
	case requestPhaseDNS:
		return "request_dns_timeout"
	case requestPhaseDial:
		return "request_dial_timeout"
	case requestPhaseTLS:
		return "request_tls_timeout"
	case requestPhaseConnect:
		fallthrough
	case requestPhaseAfterDNS, requestPhaseAfterDial, requestPhaseAfterTLS:
		return "request_connect_timeout"
	case requestPhaseConnected:
		return "request_write_timeout"
	case requestPhaseWritten:
		return "request_response_timeout"
	default:
		// The transport did not publish a phase (or a first response byte was
		// seen). Preserve the generic class rather than inventing a cause.
		return "request_timeout"
	}
}
