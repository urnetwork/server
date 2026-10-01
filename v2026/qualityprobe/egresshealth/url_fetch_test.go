// End-to-end final-response timing and bounded redirect behavior.
package egresshealth

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type urlTimedBody struct {
	reader      io.Reader
	clock       *fakeClock
	duration    time.Duration
	totalBytes  int
	initialWait time.Duration
}

func (self *urlTimedBody) Read(buffer []byte) (int, error) {
	if self.totalBytes == 0 {
		self.totalBytes = self.reader.(interface{ Len() int }).Len()
		_ = self.clock.Sleep(context.Background(), self.initialWait)
	}
	n, err := self.reader.Read(buffer)
	if n > 0 {
		_ = self.clock.Sleep(context.Background(), self.duration*time.Duration(n)/time.Duration(self.totalBytes))
	}
	return n, err
}
func (*urlTimedBody) Close() error { return nil }

type urlRateBody struct {
	ctx       context.Context
	remaining int
	bits      int64
	closed    bool
}

func (self *urlRateBody) Read(buffer []byte) (int, error) {
	if self.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(buffer), self.remaining, 4096)
	select {
	case <-self.ctx.Done():
		return 0, self.ctx.Err()
	case <-time.After(time.Duration(n) * 8 * time.Second / time.Duration(self.bits)):
	}
	for index := range n {
		buffer[index] = 'x'
	}
	self.remaining -= n
	return n, nil
}

func (self *urlRateBody) Close() error { self.closed = true; return nil }

// The 1MiB ceiling is not a required transfer. A large real page at exactly
// 100kbps can produce sufficient measured content before the 10s deadline.
func TestUrlProbeLargeContentAtThresholdDoesNotRequireOneMiB(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var body *urlRateBody
		client := &http.Client{Transport: echoStageRoundTripper(func(request *http.Request) (*http.Response, error) {
			trace := httptrace.ContextClientTrace(request.Context())
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			time.Sleep(time.Second)
			trace.GotFirstResponseByte()
			body = &urlRateBody{ctx: request.Context(), remaining: 1024 * 1024, bits: 100000}
			return &http.Response{StatusCode: 200, ContentLength: 1024 * 1024, Header: http.Header{}, Body: body}, nil
		})}
		start := time.Now()
		result := fetchUrlProbe(t.Context(), client, Destination{Name: "synthetic-large", Url: "https://large.example/"}, 10*time.Second, DefaultRequestProfile(), Options{})
		if !result.Ok || result.BodyComplete || !result.BodySampled || result.ByteCount < 64*1024 || result.ByteCount >= 1024*1024 || math.Abs(result.BodyBytesPerSecond-12500) > 1e-6 || time.Since(start) >= 10*time.Second || !body.closed {
			t.Fatalf("read ceiling became mandatory transfer: elapsed=%s result=%+v", time.Since(start), result)
		}
	})
}

// Fifty seconds before the redirect and ten seconds of final setup do not
// become final request-written TTFB. Each phase keeps its own diagnostic.
func TestUrlProbeFinalResponseClockExcludesRedirectAndSetup(t *testing.T) {
	clock := newFakeClock()
	client := &http.Client{Transport: echoStageRoundTripper(func(request *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(request.Context())
		if request.URL.Hostname() == "first.example" {
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			_ = clock.Sleep(request.Context(), 50*time.Second)
			trace.GotFirstResponseByte()
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://final.example/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		trace.DNSStart(httptrace.DNSStartInfo{})
		_ = clock.Sleep(request.Context(), 2*time.Second)
		trace.DNSDone(httptrace.DNSDoneInfo{})
		trace.ConnectStart("tcp", "final.example:443")
		_ = clock.Sleep(request.Context(), 3*time.Second)
		trace.ConnectDone("tcp", "final.example:443", nil)
		trace.TLSHandshakeStart()
		_ = clock.Sleep(request.Context(), 5*time.Second)
		trace.TLSHandshakeDone(tls.ConnectionState{}, nil)
		trace.WroteRequest(httptrace.WroteRequestInfo{})
		_ = clock.Sleep(request.Context(), 2*time.Second)
		trace.GotFirstResponseByte()
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &urlTimedBody{reader: strings.NewReader(strings.Repeat("content ", 3125)), clock: clock, duration: 2 * time.Second}}, nil
	})}
	result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Class: ClassSite, Url: "https://first.example/"}, time.Minute, DefaultRequestProfile(), Options{Now: clock.Now})
	if !result.Ok || result.RedirectCount != 1 || result.RequestTimeToFirstByte != 2*time.Second || result.DnsLookupLatency != 2*time.Second || result.TcpConnectLatency != 3*time.Second || result.TlsHandshakeLatency != 5*time.Second || result.Latency != 14*time.Second || math.Abs(result.BodyBytesPerSecond-12500) > 1e-6 {
		t.Fatalf("redirect/setup leaked into final response clocks: %+v", result)
	}
}

// Headers and the first actual body-byte wait are separate clocks. Forcing a
// one-byte initial read prevents an already-returned chunk inflating bandwidth.
func TestUrlProbeWireBodyClockStartsAtFirstActualByte(t *testing.T) {
	clock := newFakeClock()
	client := &http.Client{Transport: echoStageRoundTripper(func(request *http.Request) (*http.Response, error) {
		trace := httptrace.ContextClientTrace(request.Context())
		trace.WroteRequest(httptrace.WroteRequestInfo{})
		_ = clock.Sleep(request.Context(), time.Second)
		trace.GotFirstResponseByte()
		_ = clock.Sleep(request.Context(), 3*time.Second)
		return &http.Response{StatusCode: 200, Header: http.Header{}, Body: &urlTimedBody{
			reader: strings.NewReader(strings.Repeat("content ", 3125)), clock: clock, duration: 2 * time.Second, initialWait: 2 * time.Second,
		}}, nil
	})}
	result := fetchUrlProbe(t.Context(), client, Destination{Name: "synthetic", Url: "https://headers.example/"}, 10*time.Second, DefaultRequestProfile(), Options{Now: clock.Now})
	if !result.Ok || result.RequestTimeToFirstByte != time.Second || result.BodyDuration != 2*time.Second-80*time.Microsecond || math.Abs(result.BodyBytesPerSecond-12500) > 1e-6 || result.Latency != 8*time.Second || result.BodyFirstByteWait != 2*time.Second+80*time.Microsecond || result.WireSampleByteCount != 24999 {
		t.Fatalf("response headers or an untimed first chunk corrupted body throughput: %+v", result)
	}
}

// A sixth redirect, downgrade, credential-bearing URL, private literal, or
// unsupported port cannot cause an extra network request.
func TestUrlProbeRedirectBoundaryAndCredentialIsolation(t *testing.T) {
	for _, target := range []string{"http://clear.example/", "https://user:secret@public.example/", "https://127.0.0.1/", "https://public.example:444/"} {
		requests := 0
		client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {target}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://start.example/"}, time.Second, DefaultRequestProfile(), Options{})
		if result.Ok || requests != 1 || result.TlsAuthenticationFailure || result.FailureStage != "redirect_policy" {
			t.Errorf("unsafe redirect %q result=%+v requests=%d", target, result, requests)
		}
	}
	requests := 0
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		if requests > 1 && (request.Header.Get("Authorization") != "" || request.Header.Get("Cookie") != "" || request.Header.Get("Proxy-Authorization") != "") {
			t.Error("cross-origin credential leaked")
		}
		return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://redirect.example/again"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://start.example/", Headers: map[string]string{"Authorization": "synthetic-secret", "Cookie": "synthetic-cookie", "Proxy-Authorization": "synthetic-proxy"}}, time.Second, DefaultRequestProfile(), Options{})
	if requests != 6 || result.RedirectCount != 5 || result.FailureStage != "redirect_limit" || result.Ok {
		t.Fatalf("redirect cap escaped: requests=%d result=%+v", requests, result)
	}
}

// The actual failing redirect URL is retained; a content error with an
// authenticated response records a clean event independently of URL success.
func TestUrlProbeRetainsExactHopTlsEvidence(t *testing.T) {
	client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Hostname() == "start.example" {
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://affected.example/path"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return nil, x509.UnknownAuthorityError{}
	})}
	result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://start.example/"}, time.Second, DefaultRequestProfile(), Options{})
	if !result.TlsAuthenticationFailure || len(result.UrlProbeSecurity) != 2 || result.UrlProbeSecurity[0].TlsFailure || !result.UrlProbeSecurity[1].TlsFailure || result.UrlProbeSecurity[1].Destination.Url != "https://affected.example/path" {
		t.Fatalf("wrong TLS URL evidence: %+v", result)
	}
	client.Transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic service unavailable"))}, nil
	})
	result = fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://affected.example/path"}, time.Second, DefaultRequestProfile(), Options{})
	if result.Ok || len(result.UrlProbeSecurity) != 1 || result.UrlProbeSecurity[0].TlsFailure || result.TlsAuthenticationFailure {
		t.Fatalf("ordinary quality failure lost authenticated TLS evidence: %+v", result)
	}
}

// Neither partial HTTP responses nor truncated body errors use the tiny-page
// exemption; exact completed pages may, without a fictional throughput value.
func TestUrlProbeTinyPartialAndTruncatedBodiesCannotPass(t *testing.T) {
	for _, test := range []struct {
		status   int
		length   int64
		reader   io.Reader
		wantPass bool
	}{
		{200, 5, strings.NewReader("small"), true},
		{206, 5, strings.NewReader("small"), false},
		{200, 10, strings.NewReader("small"), false},
		{200, -1, io.MultiReader(strings.NewReader("small"), urlErrorReader{}), false},
	} {
		client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: test.status, ContentLength: test.length, Header: http.Header{}, Body: io.NopCloser(test.reader)}, nil
		})}
		result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://small.example/"}, time.Second, DefaultRequestProfile(), Options{})
		if result.Ok != test.wantPass {
			t.Errorf("status=%d length=%d result=%+v", test.status, test.length, result)
		}
	}
}

type urlErrorReader struct{}

func (urlErrorReader) Read([]byte) (int, error) { return 0, errors.New("synthetic truncated body") }

// The security coin is independent of the normal general/country coin. A
// seeded sequence proves the exact draws and each branch remains reachable.
func TestUrlProbeSecurityPoolGetsEqualProbability(t *testing.T) {
	counts := map[string]int{}
	for seed := int64(0); seed < 200; seed++ {
		expected := "security_recheck"
		rng := rand.New(rand.NewSource(seed))
		if rng.Intn(2) != 0 {
			expected = "general"
			if rng.Intn(2) == 1 {
				expected = "country"
			}
		}
		client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("synthetic failure"))}, nil
		})}
		result, err := Check(context.Background(), client, Options{UrlProbe: true, Rand: rand.New(rand.NewSource(seed)), Destinations: []Destination{{Name: "general", Class: ClassSite, Url: "https://general.example/"}}, CountryDestinations: []Destination{{Name: "country", Class: ClassSite, Url: "https://country.example/"}}, SecurityDestinations: []Destination{{Name: "security", Class: ClassSite, Url: "https://affected.example/"}}})
		if err != nil || result.UrlSource != expected {
			t.Fatalf("seed=%d source=%s want=%s err=%v", seed, result.UrlSource, expected, err)
		}
		counts[result.UrlSource]++
	}
	for _, source := range []string{"security_recheck", "general", "country"} {
		if counts[source] == 0 {
			t.Errorf("source %s starved", source)
		}
	}
}

// Compressed bytes, not their inflated decoded representation, supply the
// throughput gate. A server ignoring identity cannot manufacture fast egress.
func TestUrlProbeThroughputUsesWireBytesEvenWhenServerSendsGzip(t *testing.T) {
	rng := rand.New(rand.NewSource(31))
	content := []byte(strings.Repeat("readable synthetic content ", 10000))
	for range 32768 {
		content = append(content, byte('!'+rng.Intn(90)))
	}
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	_, _ = compressor.Write(content)
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	if encoded.Len() < 16384 || encoded.Len() >= 50000 {
		t.Fatalf("synthetic gzip shape changed: %d", encoded.Len())
	}
	for _, compressed := range []bool{false, true} {
		clock := newFakeClock()
		payload := content
		headers := http.Header{}
		if compressed {
			payload = encoded.Bytes()
			headers.Set("Content-Encoding", "gzip")
		}
		client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			if request.Header.Get("Accept-Encoding") != "identity" {
				t.Error("URL probe allowed implicit transport decompression")
			}
			return &http.Response{StatusCode: 200, ContentLength: int64(len(payload)), Header: headers, Body: &urlTimedBody{reader: bytes.NewReader(payload), clock: clock, duration: 4 * time.Second}}, nil
		})}
		result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://compressed.example/"}, time.Minute, DefaultRequestProfile(), Options{Now: clock.Now})
		if result.Ok == compressed || result.ByteCount < 64*1024 || result.ByteCount > int64(len(content)) || result.WireByteCount > int64(len(payload)) || result.WireSampleByteCount != result.WireByteCount-1 || math.Abs(result.BodyBytesPerSecond*result.BodyDuration.Seconds()-float64(result.WireSampleByteCount)) > 1e-6 {
			t.Fatalf("compressed=%t decoded bytes became bandwidth: %+v", compressed, result)
		}
		if compressed && result.PerformanceClassification != "throughput_slow" {
			t.Fatalf("gzip inflation escaped throughput gate: %+v", result)
		}
	}
}

// A compression bomb cannot use the small-wire-body exception before its
// decoded document has ended. Both encoded and decoded reads are bounded.
func TestUrlProbeCompressedReadCapCannotManufactureSmallCompletePage(t *testing.T) {
	var encoded bytes.Buffer
	compressor := gzip.NewWriter(&encoded)
	_, _ = compressor.Write(bytes.Repeat([]byte("x"), 2*1024*1024))
	_ = compressor.Close()
	client := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Encoding": {"gzip"}}, Body: io.NopCloser(bytes.NewReader(encoded.Bytes()))}, nil
	})}
	result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://compressed.example/"}, time.Second, DefaultRequestProfile(), Options{})
	if result.Ok || result.BodyComplete || result.ByteCount != 1024*1024 || result.WireByteCount > 1024*1024 || result.PerformanceClassification != "insufficient_sample" {
		t.Fatalf("compressed cap pretended to be complete small content: %+v", result)
	}
}

// Each hop shares the original hard deadline; a redirect cannot renew it.
func TestUrlProbeRedirectsCannotRenewAttemptDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		requests := 0
		client := &http.Client{Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			requests++
			select {
			case <-request.Context().Done():
				return nil, request.Context().Err()
			case <-time.After(3 * time.Second):
			}
			return &http.Response{StatusCode: 302, Header: http.Header{"Location": {"https://next.example/"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		})}
		start := time.Now()
		result := fetchUrlProbe(context.Background(), client, Destination{Name: "synthetic", Url: "https://start.example/"}, 10*time.Second, DefaultRequestProfile(), Options{})
		if time.Since(start) != 10*time.Second || requests != 4 || result.Ok || result.TlsAuthenticationFailure {
			t.Fatalf("redirect renewed deadline: requests=%d elapsed=%s result=%+v", requests, time.Since(start), result)
		}
	})
}
