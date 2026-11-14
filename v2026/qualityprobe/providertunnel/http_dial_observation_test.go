// Request-owned trace callbacks are bounded, monotonic, and joined before the
// HTTP owner returns; unknown dialers retain unknown diagnostic evidence.
package providertunnel

import (
	"context"
	"net/http/httptrace"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/urnetwork/connect/v2026"
)

// A failed resolution cannot be mislabeled as a target socket timeout.
func TestProviderDialObservationHeldDns(t *testing.T) {
	var dnsStart, dnsFailure, tcpStart atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart.Add(1) },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if info.Err == context.DeadlineExceeded {
				dnsFailure.Add(1)
			}
		},
		ConnectStart: func(string, string) { tcpStart.Add(1) },
	})
	_, trace := traceProviderHttpDial(ctx, "tcp", "target.example:443")
	trace.observe(connect.TunDialDnsStarted)
	if stage := trace.finish(context.DeadlineExceeded); stage != "dial_dns" || dnsStart.Load() != 1 || dnsFailure.Load() != 1 || tcpStart.Load() != 0 {
		t.Errorf("DNS boundary lost: stage=%s dns=%d/%d tcp=%d", stage, dnsStart.Load(), dnsFailure.Load(), tcpStart.Load())
	}
}

// Later losing family events cannot overwrite the first target connection.
func TestProviderDialObservationKeepsPositiveTcpProgress(t *testing.T) {
	var dnsStart, dnsAnswer, tcpStart, tcpSuccess atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) { dnsStart.Add(1) },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			if info.Err == nil {
				dnsAnswer.Add(1)
			}
		},
		ConnectStart: func(string, string) { tcpStart.Add(1) },
		ConnectDone: func(_, _ string, err error) {
			if err == nil {
				tcpSuccess.Add(1)
			}
		},
	})
	_, trace := traceProviderHttpDial(ctx, "tcp", "target.example:443")
	for _, event := range []connect.TunDialEvent{connect.TunDialDnsStarted, connect.TunDialDnsAnswered, connect.TunDialTcpStarted, connect.TunDialTcpConnected, connect.TunDialDnsStarted, connect.TunDialDnsAnswered} {
		trace.observe(event)
	}
	if stage := trace.finish(nil); stage != "dial_tcp" || dnsStart.Load() != 1 || dnsAnswer.Load() != 1 || tcpStart.Load() != 1 || tcpSuccess.Load() != 1 {
		t.Errorf("late family changed target progress: stage=%s events=%d/%d/%d/%d", stage, dnsStart.Load(), dnsAnswer.Load(), tcpStart.Load(), tcpSuccess.Load())
	}
}

// An uninstrumented custom dialer is not evidence that TCP was ever started.
func TestProviderDialObservationKeepsUnknownDialAmbiguous(t *testing.T) {
	var tcpStart atomic.Int32
	ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{ConnectStart: func(string, string) { tcpStart.Add(1) }})
	_, trace := traceProviderHttpDial(ctx, "tcp", "target.example:443")
	if stage := trace.finish(context.DeadlineExceeded); stage != "dial_dns_or_socket" || tcpStart.Load() != 0 {
		t.Errorf("unknown dial invented socket evidence: stage=%s tcp=%d", stage, tcpStart.Load())
	}
}

// Closing one request joins its admitted callback and refuses every late one.
func TestProviderDialObservationCloseJoinsAdmittedCallbacks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		var tcpStart atomic.Int32
		ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
			DNSStart:     func(httptrace.DNSStartInfo) { close(entered); <-release },
			ConnectStart: func(string, string) { tcpStart.Add(1) },
		})
		_, trace := traceProviderHttpDial(ctx, "tcp", "target.example:443")
		go trace.observe(connect.TunDialDnsStarted)
		<-entered
		done := make(chan struct{})
		go func() { trace.finish(context.Canceled); close(done) }()
		synctest.Wait()
		select {
		case <-done:
			t.Error("request retired before its callback joined")
		default:
		}
		close(release)
		<-done
		trace.observe(connect.TunDialTcpStarted)
		if tcpStart.Load() != 0 {
			t.Error("late callback changed a retired request")
		}
	})
}
