package egresshealth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// requestProgress is local diagnostic evidence for a generic net/http
// deadline. The provider tunnel reports its target DNS/TCP/TLS boundaries;
// uninstrumented clients retain the generic connection class. Typed provider
// stage errors remain authoritative.
type requestProgress struct {
	phase     atomic.Uint32
	stateLock sync.Mutex
	dnsStart  time.Time
	dnsEnd    time.Time
	firstByte time.Time
	tcpStart  time.Time
	tcpEnd    time.Time
	tlsStart  time.Time
	tlsEnd    time.Time
	written   time.Time
}

// Fixed-cardinality last-attempt aggregates keep timing visible without URLs,
// addresses or provider identifiers. Failed no-byte attempts have DNS time only.
func requestTimingSummary(checks []CheckResult) string {
	dnsCount, byteCount := 0, 0
	var dns, connect, first time.Duration
	for _, check := range checks {
		if check.DnsMeasured {
			dnsCount++
			dns += check.DnsLookupLatency
		}
		if check.FirstByteReceived {
			byteCount++
			connect += check.ConnectFirstByteLatency
			first += check.TimeToFirstByte
		}
	}
	parts := []string{}
	if dnsCount > 0 {
		parts = append(parts, fmt.Sprintf("dns_timed=%d dns_ms=%.1f", dnsCount, float64(dns)/float64(time.Millisecond)/float64(dnsCount)))
	}
	if byteCount > 0 {
		parts = append(parts, fmt.Sprintf("first_byte=%d connect_ttfb_ms=%.1f ttfb_ms=%.1f", byteCount, float64(connect)/float64(time.Millisecond)/float64(byteCount), float64(first)/float64(time.Millisecond)/float64(byteCount)))
	}
	return strings.Join(parts, " ")
}

// Bounded timing diagnostics are separate from the stable score summary.
func (self *Result) TimingSummary() string {
	if self == nil {
		return ""
	}
	return requestTimingSummary(self.Checks)
}

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
	return traceRequestProgressAt(ctx, time.Now)
}

// Uses the run's clock so latency and retry accounting share one time domain.
func traceRequestProgressAt(ctx context.Context, now func() time.Time) (context.Context, *requestProgress) {
	progress := &requestProgress{}
	trace := &httptrace.ClientTrace{
		GetConn: func(string) { progress.advance(requestPhaseConnect) },
		DNSStart: func(httptrace.DNSStartInfo) {
			progress.advance(requestPhaseDNS)
			at := now()
			progress.stateLock.Lock()
			defer progress.stateLock.Unlock()
			if progress.dnsStart.IsZero() {
				progress.dnsStart = at
			}
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			at := now()
			func() {
				progress.stateLock.Lock()
				defer progress.stateLock.Unlock()
				progress.dnsEnd = at
			}()
			if info.Err == nil {
				progress.advance(requestPhaseAfterDNS)
			}
		},
		ConnectStart: func(string, string) {
			progress.advance(requestPhaseDial)
			progress.stateLock.Lock()
			defer progress.stateLock.Unlock()
			if progress.tcpStart.IsZero() {
				progress.tcpStart = now()
			}
		},
		ConnectDone: func(_, _ string, err error) {
			progress.stateLock.Lock()
			progress.tcpEnd = now()
			progress.stateLock.Unlock()
			if err == nil {
				progress.advance(requestPhaseAfterDial)
			}
		},
		// The provider's DialTLSContext reports its owned handshake before
		// net/http verifies the returned *tls.Conn with a second no-op
		// handshake. Preserve the first interval across those duplicate events.
		TLSHandshakeStart: func() {
			progress.advance(requestPhaseTLS)
			progress.stateLock.Lock()
			if progress.tlsStart.IsZero() {
				progress.tlsStart = now()
			}
			progress.stateLock.Unlock()
		},
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			progress.stateLock.Lock()
			if progress.tlsEnd.IsZero() {
				progress.tlsEnd = now()
			}
			progress.stateLock.Unlock()
			if err == nil {
				progress.advance(requestPhaseAfterTLS)
			}
		},
		GotConn: func(httptrace.GotConnInfo) { progress.advance(requestPhaseConnected) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				progress.advance(requestPhaseWritten)
				progress.stateLock.Lock()
				progress.written = now()
				progress.stateLock.Unlock()
			}
		},
		GotFirstResponseByte: func() {
			progress.advance(requestPhaseResponseByte)
			progress.recordFirstByte(now())
		},
	}
	return httptrace.WithClientTrace(ctx, trace), progress
}

// The first response byte is immutable even if a transport emits it twice.
func (self *requestProgress) recordFirstByte(at time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.firstByte.IsZero() {
		self.firstByte = at
	}
}

// Records independent setup and transfer clocks; no first byte means no speed.
func (self *requestProgress) recordTiming(result *CheckResult, start, end time.Time) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	result.Latency = end.Sub(start)
	if !self.tcpStart.IsZero() && !self.tcpEnd.IsZero() {
		result.TcpConnectLatency = max(0, self.tcpEnd.Sub(self.tcpStart))
	}
	if !self.tlsStart.IsZero() && !self.tlsEnd.IsZero() {
		result.TlsHandshakeLatency = max(0, self.tlsEnd.Sub(self.tlsStart))
	}
	if !self.dnsStart.IsZero() {
		dnsEnd := self.dnsEnd
		if dnsEnd.IsZero() {
			dnsEnd = end
		}
		result.DnsLookupLatency = max(0, dnsEnd.Sub(self.dnsStart))
		result.DnsMeasured = true
	}
	if !self.firstByte.IsZero() {
		result.FirstByteReceived = true
		if !self.written.IsZero() {
			result.RequestWritten = true
			result.RequestTimeToFirstByte = max(0, self.firstByte.Sub(self.written))
		}
		result.TimeToFirstByte = max(0, self.firstByte.Sub(start))
		result.ConnectFirstByteLatency = max(0, result.TimeToFirstByte-result.DnsLookupLatency)
		result.BodyDuration = max(0, end.Sub(self.firstByte))
		if 0 < result.BodyDuration {
			result.BodyBytesPerSecond = float64(result.ByteCount) / result.BodyDuration.Seconds()
		}
	}
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
