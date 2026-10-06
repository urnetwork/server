package acceptance

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// Signal the second read, after io.ReadAll has retained the partial body and
// entered the stalled read. Cancellation then has an exact transport boundary.
type apiBodyReadSignal struct {
	io.ReadCloser
	started chan struct{}
	read    bool
}

func (r *apiBodyReadSignal) Read(p []byte) (int, error) {
	if r.read && r.started != nil {
		close(r.started)
		r.started = nil
	}
	r.read = true
	return r.ReadCloser.Read(p)
}

func TestAPIPostTraceDistinguishesResponseStalls(t *testing.T) {
	for _, bodyStall := range []bool{false, true} {
		name := "headers"
		phase := "waiting_for_response_headers"
		if bodyStall {
			name = "body"
			phase = "reading_response_body"
		}
		t.Run(name, func(t *testing.T) {
			const partialBody = `{"client_id":"unreported-client-secret",`
			received := make(chan struct{})
			bodyRead := make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				if bodyStall {
					w.Header().Set("X-Private-Header", "response-header-secret")
					_, _ = io.WriteString(w, partialBody)
					w.(http.Flusher).Flush()
				}
				close(received)
				<-release
			}))
			defer server.Close()
			defer close(release)
			client := server.Client()
			transport := client.Transport
			client.Transport = runnerRoundTripper(func(request *http.Request) (*http.Response, error) {
				response, err := transport.RoundTrip(request)
				if err == nil && bodyStall {
					response.Body = &apiBodyReadSignal{ReadCloser: response.Body, started: bodyRead}
				}
				return response, err
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				api := &apiClient{baseURL: server.URL, client: client}
				var output provisionResult
				done <- api.post(ctx, "/network/auth-client", map[string]string{"password": "request-body-secret"}, "authorization-secret", &output)
			}()
			boundary := received
			if bodyStall {
				boundary = bodyRead
			}
			select {
			case <-boundary:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not reach the stalled boundary")
			}
			cancel()
			var err error
			select {
			case err = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not stop after cancellation")
			}
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
			detail := err.Error()
			for _, want := range []string{
				"API POST /network/auth-client; request started ", "elapsed ", "phase " + phase,
				"phase_elapsed ", "connection new", "tls_done=", "write_done=", "context canceled",
			} {
				if !strings.Contains(detail, want) {
					t.Errorf("failure detail %q missing %q", detail, want)
				}
			}
			if bodyStall {
				if !strings.Contains(detail, "status 200") || strings.Contains(detail, "headers_received=unobserved") || strings.Contains(detail, "body_bytes 0;") {
					t.Errorf("body stall lost the response boundary: %s", detail)
				}
			} else if !strings.Contains(detail, "headers_received=unobserved") || !strings.Contains(detail, "body_bytes 0;") {
				t.Errorf("header stall incorrectly reports a response body: %s", detail)
			}
			for _, secret := range []string{"unreported-client-secret", "response-header-secret", "request-body-secret", "authorization-secret", server.URL} {
				if strings.Contains(detail, secret) {
					t.Errorf("failure detail leaked %q: %s", secret, detail)
				}
			}
		})
	}
}

func TestAPIPostDialFailureTraceDoesNotExposeTransportMetadata(t *testing.T) {
	const secret = "transport-secret"
	cause := errors.New("dial failed " + secret)
	calls := 0
	api := &apiClient{
		baseURL: "https://url-user:url-password@host-secret.invalid/base-secret?query-secret=",
		client: &http.Client{Transport: runnerRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			if trace := httptrace.ContextClientTrace(request.Context()); trace != nil {
				trace.GetConn("host-secret.invalid")
				trace.DNSStart(httptrace.DNSStartInfo{Host: "dns-secret.invalid"})
				trace.DNSDone(httptrace.DNSDoneInfo{})
				trace.ConnectStart("tcp", "address-secret.invalid:443")
				trace.ConnectDone("tcp", "address-secret.invalid:443", cause)
			}
			return nil, cause
		})},
	}
	var output provisionResult
	err := api.post(context.Background(), "/network/auth-client", map[string]string{"password": "body-secret"}, "jwt-secret", &output)
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want preserved dial cause", err)
	}
	detail := err.Error()
	for _, want := range []string{"API POST /network/auth-client", "phase connecting_api_failed", "connection not_established", "connect_done="} {
		if !strings.Contains(detail, want) {
			t.Errorf("failure detail %q missing %q", detail, want)
		}
	}
	for _, secret := range []string{secret, "url-user", "url-password", "host-secret", "base-secret", "query-secret", "dns-secret", "address-secret", "body-secret", "jwt-secret"} {
		if strings.Contains(detail, secret) {
			t.Errorf("failure detail leaked %q: %s", secret, detail)
		}
	}
	if calls != 1 {
		t.Fatalf("transport calls = %d, want one", calls)
	}
}

func TestAPIPostTraceIdentifiesPendingTransportPhases(t *testing.T) {
	for _, test := range []struct {
		phase   string
		advance func(*httptrace.ClientTrace)
	}{
		{"waiting_for_connection", func(trace *httptrace.ClientTrace) { trace.GetConn("host-secret") }},
		{"resolving_api", func(trace *httptrace.ClientTrace) { trace.DNSStart(httptrace.DNSStartInfo{Host: "dns-secret"}) }},
		{"resolving_api_failed", func(trace *httptrace.ClientTrace) {
			trace.DNSDone(httptrace.DNSDoneInfo{Err: errors.New("dns-secret")})
		}},
		{"connecting_api", func(trace *httptrace.ClientTrace) { trace.ConnectStart("tcp", "peer-secret") }},
		{"tls_handshake", func(trace *httptrace.ClientTrace) { trace.TLSHandshakeStart() }},
		{"tls_handshake_failed", func(trace *httptrace.ClientTrace) {
			trace.TLSHandshakeDone(tls.ConnectionState{}, errors.New("certificate-secret"))
		}},
		{"writing_request", func(trace *httptrace.ClientTrace) { trace.GotConn(httptrace.GotConnInfo{}) }},
		{"writing_request_failed", func(trace *httptrace.ClientTrace) {
			trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("request-secret")})
		}},
		{"reading_response_headers", func(trace *httptrace.ClientTrace) { trace.GotFirstResponseByte() }},
	} {
		t.Run(test.phase, func(t *testing.T) {
			api := &apiClient{baseURL: "https://api.invalid", client: &http.Client{Transport: runnerRoundTripper(func(request *http.Request) (*http.Response, error) {
				test.advance(httptrace.ContextClientTrace(request.Context()))
				return nil, context.DeadlineExceeded
			})}}
			var output provisionResult
			err := api.post(context.Background(), "/network/auth-client", nil, "", &output)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "phase "+test.phase+";") {
				t.Fatalf("transport boundary = %v, want %s", err, test.phase)
			}
			if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "tunnel") {
				t.Fatalf("API diagnostic contains transport inputs or a proxy tunnel label: %v", err)
			}
		})
	}
}

func TestAPIRequestTraceIgnoresLateCallbacksAndPreservesTypedCause(t *testing.T) {
	trace := newAPIRequestTrace()
	callbacks := trace.clientTrace()
	callbacks.GotConn(httptrace.GotConnInfo{Reused: true})
	trace.responseReceived(http.StatusOK)
	cause := &net.DNSError{Name: "host-secret", Err: "error-secret"}
	var callbacksDone sync.WaitGroup
	callbacksDone.Add(1)
	go func() {
		defer callbacksDone.Done()
		for range 100 {
			callbacks.ConnectDone("tcp", "peer-secret", cause)
			callbacks.WroteRequest(httptrace.WroteRequestInfo{})
		}
	}()
	err := trace.failure("/arbitrary/path-secret?query-secret", cause, 7)
	before := err.Error()
	callbacksDone.Wait()
	callbacks.GotConn(httptrace.GotConnInfo{})
	trace.responseReceived(http.StatusBadGateway)
	var dnsError *net.DNSError
	if !errors.As(err, &dnsError) || dnsError != cause {
		t.Fatalf("typed cause lost: %v", err)
	}
	for _, want := range []string{"API POST other;", "phase reading_response_body;", "connection reused;", "status 200;", "body_bytes 7;", "DNS lookup failed"} {
		if !strings.Contains(before, want) {
			t.Errorf("failure snapshot %q missing %q", before, want)
		}
	}
	if before != err.Error() || strings.Contains(before, "secret") {
		t.Fatalf("failure snapshot changed or contains secrets: %v", err)
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.statusCode != http.StatusOK || trace.connection != "reused" {
		t.Fatal("late callbacks changed a frozen trace")
	}
}

type apiFailingBody struct {
	reader io.Reader
	err    error
}

func (b *apiFailingBody) Read(p []byte) (int, error) {
	if n, err := b.reader.Read(p); err != io.EOF {
		return n, err
	}
	return 0, b.err
}

func (*apiFailingBody) Close() error { return nil }

func TestRunProvisionTransportFailureDoesNotRetryProbeOrRemoveWithoutID(t *testing.T) {
	for _, phase := range []string{"connecting_api_failed", "waiting_for_response_headers", "reading_response_body"} {
		t.Run(phase, func(t *testing.T) {
			paths := []string{}
			progress := []string{}
			client := &http.Client{Transport: runnerRoundTripper(func(request *http.Request) (*http.Response, error) {
				paths = append(paths, request.URL.Path)
				if request.URL.Path == "/auth/login-with-password" {
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"network":{"by_jwt":"network-jwt-secret"}}`)), Header: make(http.Header)}, nil
				}
				if request.URL.Path != "/network/auth-client" {
					t.Errorf("unexpected request %q", request.URL.Path)
					return nil, errors.New("unexpected request")
				}
				if trace := httptrace.ContextClientTrace(request.Context()); trace != nil {
					trace.ConnectStart("tcp", "api.invalid:443")
					if phase == "connecting_api_failed" {
						trace.ConnectDone("tcp", "api.invalid:443", context.DeadlineExceeded)
					} else {
						trace.ConnectDone("tcp", "api.invalid:443", nil)
						trace.GotConn(httptrace.GotConnInfo{Reused: true})
						trace.WroteRequest(httptrace.WroteRequestInfo{})
					}
				}
				if phase == "reading_response_body" {
					return &http.Response{
						StatusCode: http.StatusOK, Header: http.Header{"X-Private": []string{"response-header-secret"}},
						Body: &apiFailingBody{reader: strings.NewReader(`{"client_id":"partial-client-secret",`), err: context.DeadlineExceeded},
					}, nil
				}
				return nil, context.DeadlineExceeded
			})}
			results := runWithDependencies(context.Background(), Options{
				APIURL: "https://api.invalid", TargetURL: "https://target.invalid", CredentialsPath: "injected", Repeat: 1,
				Progress: func(message string) { progress = append(progress, message) },
			}, runDependencies{
				credentials: &credentials{user: "user-secret", password: "password-secret"}, httpClient: client,
				probes: func(*proxyConfigResult) map[string]protocolProbe {
					t.Error("probes started after provisioning failed")
					return nil
				},
				tracker: func(context.Context, provisionResult) (hostedDeviceTracker, error) {
					t.Error("tracking started after provisioning failed")
					return nil, errors.New("unexpected tracker")
				},
			})
			if want := []string{"/auth/login-with-password", "/network/auth-client"}; !slices.Equal(paths, want) {
				t.Errorf("API requests = %v, want exactly %v", paths, want)
			}
			for _, protocol := range protocolNames {
				result := assertResult(t, results, protocol, "FAIL")
				if !strings.Contains(result.Detail, "phase "+phase) || !strings.Contains(result.Detail, "context deadline exceeded") {
					t.Errorf("%s lost provisioning boundary: %s", protocol, result.Detail)
				}
				if phase != "connecting_api_failed" && (!strings.Contains(result.Detail, "connection reused;") || !strings.Contains(result.Detail, "tls_done=unobserved")) {
					t.Errorf("%s lost reused-connection evidence: %s", protocol, result.Detail)
				}
				progress = append(progress, result.Detail)
			}
			for _, secret := range []string{"user-secret", "password-secret", "network-jwt-secret", "partial-client-secret", "response-header-secret"} {
				if strings.Contains(strings.Join(progress, "\n"), secret) {
					t.Errorf("result or progress leaked %q", secret)
				}
			}
		})
	}
}
