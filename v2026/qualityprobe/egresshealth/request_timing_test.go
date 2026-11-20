// Request timing separates resolver latency from connection/first-byte work.
package egresshealth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// Advances the injected clock while returning the small sampled body.
type timedSampleBody struct {
	io.Reader
	clock    *fakeClock
	advanced bool
}

// Models body transfer after the first response byte without sleeping.
func (self *timedSampleBody) Read(p []byte) (int, error) {
	if !self.advanced {
		self.advanced = true
		_ = self.clock.Sleep(context.Background(), 5*time.Second)
	}
	return self.Reader.Read(p)
}

// No resource is acquired by the synthetic body.
func (self *timedSampleBody) Close() error { return nil }

// DNS and TCP/TLS work count in first-byte latency, body transfer does not.
func TestSampleLatencyEndsAtFirstResponseByte(t *testing.T) {
	clock := newFakeClock()
	client := &http.Client{Transport: echoStageRoundTripper(func(req *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(req.Context())
		trace.DNSStart(httptrace.DNSStartInfo{})
		_ = clock.Sleep(req.Context(), 2*time.Second)
		trace.DNSDone(httptrace.DNSDoneInfo{})
		_ = clock.Sleep(req.Context(), 3*time.Second)
		trace.GotFirstResponseByte()
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &timedSampleBody{Reader: strings.NewReader("User-agent: *"), clock: clock}}, nil
	})}
	result := fetch(context.Background(), client, Destination{Name: "sample", Class: ClassSite, Url: "https://sample.example/robots.txt"}, time.Minute, DefaultRequestProfile(), clock.Now)
	if !result.Ok || result.Latency != 10*time.Second {
		t.Fatalf("total fetch latency was lost: %+v", result)
	}
	if !result.DnsMeasured || result.DnsLookupLatency != 2*time.Second || result.TimeToFirstByte != 5*time.Second || result.ConnectFirstByteLatency != 3*time.Second || result.BodyDuration != 5*time.Second || result.BodyBytesPerSecond != 2.6 {
		t.Fatalf("DNS/connect/transfer clocks were combined: %+v", result)
	}
}

// A silent response cannot start a throughput clock or renew its hard deadline.
func TestSampleNoFirstByteStopsAtHardDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &http.Client{Transport: echoStageRoundTripper(func(req *http.Request) (*http.Response, error) {
			<-req.Context().Done()
			return nil, req.Context().Err()
		})}
		result := fetch(context.Background(), client, Destination{Name: "silent-sample", Class: ClassSite, Url: "https://silent.example/robots.txt"}, 10*time.Second, DefaultRequestProfile(), time.Now)
		if result.Ok || result.Latency != 10*time.Second || result.FirstByteReceived || result.BodyDuration != 0 || result.BodyBytesPerSecond != 0 {
			t.Fatalf("silent response escaped timing boundary: %+v", result)
		}
	})
}
