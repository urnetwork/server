package acceptance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func darwinPeerTCPSummary(socketGeneration int, processName string, processPID int, flowID string, synCounts string, endpoint string) string {
	return fmt.Sprintf(`2026-09-05 13:37:39.000000-0500 kernel[0:0] tcp_connection_summary (tcp_close:0)[%s] interface: en0 (skipped: 0)
so_gencnt: %d t_state: SYN_SENT process: %s:%d Duration: 30.000 sec Conn_Time: 0.000 sec bytes in/out: 0/0 pkts in/out: 0/0 pkt rxmit: 0 ooo pkts: 0 dup bytes in: 0 ACKs delayed: 0 delayed ACKs sent: 0
rtt: 0.000 ms rttvar: 0.000 ms base rtt: 0 ms so_error: 0 svc/tc: 0 flow: %s
2026-09-05 13:37:39.000001-0500 kernel[0:0] tcp_connection_summary [%s] interface: en0 (skipped: 0)
so_gencnt: %d t_state: SYN_SENT process: %s:%d flowctl: 0us (0x0) SYN in/out: %s FIN in/out: 0/0 RST in/out: 0/0 AccECN (client/server): Disabled/Disabled`,
		endpoint,
		socketGeneration,
		processName,
		processPID,
		flowID,
		endpoint,
		socketGeneration,
		processName,
		processPID,
		synCounts,
	)
}

func TestRunReportsEachProtocolAndAlwaysRemovesClient(t *testing.T) {
	const (
		networkJWT = "network-jwt-secret"
		clientID   = "client-id-secret"
		proxyToken = "proxy-token-secret"
	)
	var mu sync.Mutex
	paths := []string{}
	progress := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		paths = append(paths, request.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode login: %v", err)
			}
			if body["user_auth"] != "person@example.invalid" || body["password"] != "password-secret" {
				t.Errorf("unexpected login body: %#v", body)
			}
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"` + networkJWT + `"}}`))
		case "/network/auth-client":
			if request.Header.Get("Authorization") != "Bearer "+networkJWT {
				t.Errorf("provision authorization was not the network JWT")
			}
			var body struct {
				ProxyConfig struct {
					LockCallerIP       bool `json:"lock_caller_ip"`
					EnableWG           bool `json:"enable_wg"`
					InitialDeviceState struct {
						CountryCode string `json:"country_code"`
					} `json:"initial_device_state"`
				} `json:"proxy_config"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode provision: %v", err)
			}
			if body.ProxyConfig.LockCallerIP || !body.ProxyConfig.EnableWG || body.ProxyConfig.InitialDeviceState.CountryCode != "us" {
				t.Errorf("unexpected proxy config: %#v", body.ProxyConfig)
			}
			_, _ = w.Write([]byte(`{
                    "client_id":"` + clientID + `",
                    "proxy_config_result":{
                        "socks_proxy_url":"socks5h://proxy.example:8080",
                        "http_proxy_url":"http://proxy.example:8081",
                        "api_base_url":"https://api.proxy.example:8083",
                        "auth_token":"` + proxyToken + `",
                        "proxy_host":"proxy.example",
                        "block":"g7",
                        "wg_config":{
                            "wg_proxy_port":8084,
                            "client_private_key":"private-key-secret",
                            "client_public_key":"public-key",
                            "proxy_public_key":"proxy-public-key",
                            "client_ipv4":"10.0.0.2",
                            "config":"wireguard-config-secret"
                        }
                    }
                }`))
		case "/network/remove-client":
			if request.Header.Get("Authorization") != "Bearer "+networkJWT {
				t.Errorf("cleanup authorization was not the network JWT")
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode cleanup: %v", err)
			}
			if body["client_id"] != clientID {
				t.Errorf("cleanup client id = %q", body["client_id"])
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	results := runWithDependencies(context.Background(), Options{
		APIURL:          server.URL,
		TargetURL:       server.URL + "/target",
		CredentialsPath: "injected-for-test",
		Repeat:          1,
		ProbeTimeout:    time.Second,
		Progress: func(message string) {
			progress = append(progress, message)
		},
	}, runDependencies{
		credentials: &credentials{user: "person@example.invalid", password: "password-secret"},
		httpClient:  server.Client(),
		probes: func(config *proxyConfigResult) map[string]protocolProbe {
			if config.AuthToken != proxyToken {
				t.Errorf("probe factory received auth token %q", config.AuthToken)
			}
			return map[string]protocolProbe{
				"socks":     func(context.Context) error { return errors.New("rejected " + proxyToken + " for " + clientID) },
				"http":      func(context.Context) error { return nil },
				"wireguard": func(context.Context) error { return nil },
			}
		},
		tracker: func(context.Context, provisionResult) (hostedDeviceTracker, error) {
			return &staticHostedDeviceTracker{diagnostic: "events=[none] state={exit=p1}"}, nil
		},
	})

	assertResult(t, results, "socks", "FAIL")
	assertResult(t, results, "http", "PASS")
	assertResult(t, results, "wireguard", "PASS")
	for _, result := range results {
		if strings.Contains(result.Detail, proxyToken) || strings.Contains(result.Detail, clientID) || strings.Contains(result.Detail, networkJWT) {
			t.Fatalf("result leaked a secret: %q", result.Detail)
		}
	}
	progressText := strings.Join(progress, "\n")
	for _, secret := range []string{proxyToken, clientID, networkJWT} {
		if strings.Contains(progressText, secret) {
			t.Fatalf("progress leaked a secret: %q", progressText)
		}
	}
	for _, milestone := range []string{
		"repetition 1/1 started",
		"temporary client assigned to proxy host proxy.example block g7",
		"temporary client public ports http=8081 socks=8080 wireguard=8084",
		"http campaign started",
		"socks campaign failed",
		"wireguard campaign passed",
		"hosted device final diagnostics: events=[none] state={exit=p1}",
		"temporary client cleanup completed",
		"repetition 1/1 finished",
	} {
		if !strings.Contains(progressText, milestone) {
			t.Fatalf("progress missing %q: %q", milestone, progressText)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	wantPaths := []string{"/auth/login-with-password", "/network/auth-client", "/network/remove-client"}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("request paths = %v, want %v", paths, wantPaths)
	}
}

func TestRunOverlapsAllProtocolsOnTheSameProxyDevice(t *testing.T) {
	removed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			_, _ = w.Write([]byte(`{"client_id":"client","proxy_config_result":{"auth_token":"token"}}`))
		case "/network/remove-client":
			close(removed)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	started := make(chan string, len(protocolNames))
	release := make(chan struct{})
	var countLock sync.Mutex
	counts := map[string]int{}
	resultChannel := make(chan []Result, 1)
	go func() {
		resultChannel <- runWithDependencies(context.Background(), Options{
			APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
			OverlapProtocols: true,
		}, runDependencies{
			credentials: &credentials{user: "user", password: "password"},
			httpClient:  server.Client(),
			probes: func(*proxyConfigResult) map[string]protocolProbe {
				probes := map[string]protocolProbe{}
				for _, protocol := range protocolNames {
					protocol := protocol
					probes[protocol] = func(context.Context) error {
						countLock.Lock()
						counts[protocol]++
						call := counts[protocol]
						countLock.Unlock()
						if call == 1 {
							return nil // isolated baseline
						}
						started <- protocol
						<-release
						return nil
					}
				}
				return probes
			},
		})
	}()

	startedSet := map[string]bool{}
	for len(startedSet) < len(protocolNames) {
		select {
		case protocol := <-started:
			startedSet[protocol] = true
		case <-time.After(2 * time.Second):
			t.Fatalf("only overlapping protocols %v started before timeout", startedSet)
		}
	}
	select {
	case <-removed:
		t.Fatal("temporary client was removed while overlapping campaigns were active")
	default:
	}
	close(release)

	var results []Result
	select {
	case results = <-resultChannel:
	case <-time.After(2 * time.Second):
		t.Fatal("overlapping proxy campaigns did not finish")
	}
	for _, protocol := range protocolNames {
		assertResult(t, results, protocol, "PASS")
		if counts[protocol] != 2 {
			t.Fatalf("%s probe ran %d times, want isolated plus overlapping", protocol, counts[protocol])
		}
	}
	select {
	case <-removed:
	default:
		t.Fatal("temporary client was not removed after overlapping campaigns")
	}
}

func TestRunRemovesPartiallyProvisionedClientAndFailsEveryProtocol(t *testing.T) {
	removed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			_, _ = w.Write([]byte(`{"client_id":"partial-client","error":{"message":"proxy allocation failed"}}`))
		case "/network/remove-client":
			removed = true
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	results := runWithDependencies(context.Background(), Options{
		APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
	}, runDependencies{
		credentials: &credentials{user: "user", password: "password"},
		httpClient:  server.Client(),
		probes: func(*proxyConfigResult) map[string]protocolProbe {
			t.Fatal("protocol probes ran after provisioning failed")
			return nil
		},
	})
	if !removed {
		t.Fatal("partially provisioned client was not removed")
	}
	for _, name := range protocolNames {
		assertResult(t, results, name, "FAIL")
	}
}

func TestCancellationDuringProvisionStillCapturesAndRemovesClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			cancel()
			_, _ = w.Write([]byte(`{"client_id":"interrupted-client","proxy_config_result":{"auth_token":"token"}}`))
		case "/network/remove-client":
			removed = true
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	results := runWithDependencies(ctx, Options{
		APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
	}, runDependencies{
		credentials: &credentials{user: "user", password: "password"},
		httpClient:  server.Client(),
		probes: func(*proxyConfigResult) map[string]protocolProbe {
			t.Fatal("protocol probes ran after cancellation")
			return nil
		},
	})
	if !removed {
		t.Fatal("client created during cancellation was not removed")
	}
	for _, name := range protocolNames {
		result := assertResult(t, results, name, "FAIL")
		if !strings.Contains(result.Detail, "context canceled") {
			t.Fatalf("%s cancellation detail = %q", name, result.Detail)
		}
	}
}

func TestCleanupFailureFailsOtherwisePassingProtocols(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			_, _ = w.Write([]byte(`{"client_id":"client","proxy_config_result":{"auth_token":"token"}}`))
		case "/network/remove-client":
			_, _ = w.Write([]byte(`{"error":{"message":"cleanup failed"}}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	passing := func(context.Context) error { return nil }
	results := runWithDependencies(context.Background(), Options{
		APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
	}, runDependencies{
		credentials: &credentials{user: "user", password: "password"},
		httpClient:  server.Client(),
		probes: func(*proxyConfigResult) map[string]protocolProbe {
			return map[string]protocolProbe{"socks": passing, "http": passing, "wireguard": passing}
		},
	})
	for _, name := range protocolNames {
		result := assertResult(t, results, name, "FAIL")
		if !strings.Contains(result.Detail, "cleanup failed") {
			t.Fatalf("%s detail did not report cleanup: %q", name, result.Detail)
		}
	}
}

func TestRunExecutesSustainedProtocolCampaignsSequentiallyBeforeCleanup(t *testing.T) {
	removed := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			_, _ = w.Write([]byte(`{"client_id":"client","proxy_config_result":{"auth_token":"token"}}`))
		case "/network/remove-client":
			close(removed)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	order := []string{}
	results := runWithDependencies(context.Background(), Options{
		APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
	}, runDependencies{
		credentials: &credentials{user: "user", password: "password"},
		httpClient:  server.Client(),
		probes: func(*proxyConfigResult) map[string]protocolProbe {
			probes := map[string]protocolProbe{}
			for _, protocol := range protocolNames {
				protocol := protocol
				probes[protocol] = func(context.Context) error {
					select {
					case <-removed:
						t.Fatalf("temporary client was removed before %s campaign", protocol)
					default:
					}
					order = append(order, protocol)
					return nil
				}
			}
			return probes
		},
	})
	wantOrder := []string{"http", "socks", "wireguard"}
	if !slices.Equal(order, wantOrder) {
		t.Fatalf("campaign order = %v, want %v", order, wantOrder)
	}
	for _, protocol := range protocolNames {
		assertResult(t, results, protocol, "PASS")
	}
	select {
	case <-removed:
	default:
		t.Fatal("temporary client was not removed after all protocol campaigns completed")
	}
}

func TestProbeHTTPSRunsConfiguredSustainedCampaign(t *testing.T) {
	requestCount := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	successfulRequests, err := probeHTTPSCampaign(
		context.Background(),
		"HTTP CONNECT",
		target.URL,
		target.Client().Transport,
		time.Second,
		30*time.Second,
		10*time.Second,
		func(context.Context, time.Duration) error { return nil },
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if successfulRequests != 4 {
		t.Fatalf("successful requests = %d, want 4 (one readiness plus three sustained)", successfulRequests)
	}
	if requestCount != successfulRequests {
		t.Fatalf("target requests = %d, want %d", requestCount, successfulRequests)
	}
}

// Adapts a deterministic function to net/http's transport boundary.
type runnerRoundTripper func(*http.Request) (*http.Response, error)

// Delegates one request without adding transport behavior.
func (self runnerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}

// A failed sustained request is terminal: collect its local boundary once,
// retain the failure, and never manufacture a retry or success.
func TestProbeHTTPSAnnotatesTerminalTransportFailureWithoutRetry(t *testing.T) {
	requestCount := 0
	sentinel := errors.New("socket is not connected")
	transport := runnerRoundTripper(func(request *http.Request) (*http.Response, error) {
		requestCount++
		if requestCount == 1 {
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		}
		return nil, sentinel
	})
	collectorCount := 0

	successfulRequests, err := probeHTTPSCampaign(
		context.Background(),
		"HTTP CONNECT",
		"https://validation.example/generate_204",
		transport,
		time.Second,
		10*time.Second,
		10*time.Second,
		func(context.Context, time.Duration) error { return nil },
		nil,
		func(started time.Time, finished time.Time) string {
			collectorCount++
			if finished.Before(started) {
				t.Errorf("collector interval = %s/%s", started, finished)
			}
			return "local_host{classification=local-kernel-buffer-pressure}"
		},
	)
	if successfulRequests != 1 {
		t.Errorf("successful requests = %d, want 1", successfulRequests)
	}
	if requestCount != 2 {
		t.Errorf("transport requests = %d, want 2", requestCount)
	}
	if collectorCount != 1 {
		t.Errorf("collector calls = %d, want 1", collectorCount)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("campaign error no longer wraps transport cause: %v", err)
	}
	if !strings.Contains(err.Error(), "sustained request 1/1 failed after 1 successful requests") ||
		!strings.Contains(err.Error(), "local_host{classification=local-kernel-buffer-pressure}") {
		t.Fatalf("campaign error lost failure/provenance detail: %v", err)
	}
}

func TestDefaultProxyTargetIsDedicatedConnectivityValidation(t *testing.T) {
	target, err := url.Parse(DefaultTargetURL)
	if err != nil {
		t.Fatal(err)
	}
	if target.Scheme != "https" || target.Host == "" || target.Path != "/generate_204" {
		t.Fatalf("default proxy target = %q, want an HTTPS generate_204 endpoint", DefaultTargetURL)
	}
	if strings.HasSuffix(target.Hostname(), "bringyour.com") ||
		target.Hostname() == "ur.io" ||
		strings.HasSuffix(target.Hostname(), ".ur.io") {
		t.Fatalf("default proxy target %q uses a product/rate-limited origin", DefaultTargetURL)
	}
}

func TestProbeHTTPSFailsImmediatelyOnRateLimitDuringSustainedCampaign(t *testing.T) {
	requestCount := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requestCount++
		if requestCount == 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	successfulRequests, err := probeHTTPSCampaign(
		context.Background(),
		"SOCKS5",
		target.URL,
		target.Client().Transport,
		time.Second,
		50*time.Second,
		10*time.Second,
		func(context.Context, time.Duration) error { return nil },
		nil,
		func(time.Time, time.Time) string {
			t.Fatal("target response triggered local-host collection")
			return ""
		},
	)
	if err == nil ||
		!strings.Contains(err.Error(), "HTTP 429") ||
		!strings.Contains(err.Error(), "sustained request 2/5") ||
		!strings.Contains(err.Error(), "request started ") ||
		!strings.Contains(err.Error(), "phase reading_response_body") {
		t.Fatalf("rate-limit error = %v", err)
	}
	if successfulRequests != 2 {
		t.Fatalf("successful requests = %d, want 2", successfulRequests)
	}
	if requestCount != 3 {
		t.Fatalf("target requests = %d, want 3; a 429 must not be retried", requestCount)
	}
}

// A timeout after WroteRequest means the proxy tunnel, TLS handshake, and
// request write all completed; preserve that distinction in the artifact.
func TestHTTPSRequestTraceIdentifiesResponseHeaderStall(t *testing.T) {
	started := time.Date(2026, time.August, 29, 4, 3, 0, 0, time.UTC)
	requestTrace := &httpsRequestTrace{started: started, phase: "starting_request"}
	clientTrace := requestTrace.clientTrace()
	clientTrace.GotConn(httptrace.GotConnInfo{Reused: false})
	clientTrace.WroteRequest(httptrace.WroteRequestInfo{})

	err := requestTrace.wrap(context.DeadlineExceeded, started.Add(30*time.Second))
	detail := err.Error()
	for _, evidence := range []string{
		"request started 2026-08-29T04:03:00Z",
		"elapsed 30s",
		"phase waiting_for_response_headers",
		"connection new",
		"context deadline exceeded",
	} {
		if !strings.Contains(detail, evidence) {
			t.Errorf("trace detail %q does not contain %q", detail, evidence)
		}
	}
}

// An error before GotConn must not be mislabeled as an established data-plane
// tunnel; this is the adjacent failure class needed to interpret a timeout.
func TestHTTPSRequestTraceIdentifiesTunnelConnectFailure(t *testing.T) {
	started := time.Date(2026, time.August, 29, 4, 3, 0, 0, time.UTC)
	requestTrace := &httpsRequestTrace{started: started, phase: "starting_request"}
	clientTrace := requestTrace.clientTrace()
	clientTrace.ConnectStart("tcp", "example.test:443")
	clientTrace.ConnectDone("tcp", "example.test:443", errors.New("dial failed"))

	detail := requestTrace.wrap(errors.New("dial failed"), started.Add(time.Second)).Error()
	if !strings.Contains(detail, "phase connecting_tunnel_failed") {
		t.Errorf("trace detail %q does not identify the connect failure", detail)
	}
	if !strings.Contains(detail, "connection not_established") {
		t.Errorf("trace detail %q incorrectly claims an established connection", detail)
	}
	if !strings.Contains(detail, "dial example.test:443") {
		t.Errorf("trace detail %q lost the dial endpoint needed to correlate a failed LB path", detail)
	}
}

// The live proxy failure supplied only "unknown authority" even though the
// parsed peer chain was available. Preserve a bounded identity for every
// certificate so an invalid origin chain and an intercepting exit diverge.
func TestHTTPSRequestTraceRetainsRejectedPeerCertificateChain(t *testing.T) {
	started := time.Date(2026, time.September, 1, 6, 20, 12, 0, time.UTC)
	requestTrace := &httpsRequestTrace{started: started, phase: "starting_request"}
	clientTrace := requestTrace.clientTrace()
	leaf := &x509.Certificate{
		Raw:     []byte("leaf certificate"),
		Subject: pkix.Name{CommonName: "validation.example"},
		Issuer:  pkix.Name{CommonName: "unexpected proxy root"},
	}
	intermediate := &x509.Certificate{
		Raw:     []byte("intermediate certificate"),
		Subject: pkix.Name{CommonName: "unexpected proxy root"},
		Issuer:  pkix.Name{CommonName: "unexpected proxy root"},
	}
	clientTrace.TLSHandshakeDone(tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{leaf, intermediate},
	}, x509.UnknownAuthorityError{Cert: leaf})

	detail := requestTrace.wrap(x509.UnknownAuthorityError{Cert: leaf}, started.Add(2*time.Second)).Error()
	for _, evidence := range []string{
		"phase tls_handshake_failed",
		"peer_certs=2",
		"verified_chains=0",
		"validation.example>unexpected_proxy_root/",
		"unexpected_proxy_root>unexpected_proxy_root/",
		"certificate signed by unknown authority",
	} {
		if !strings.Contains(detail, evidence) {
			t.Errorf("trace detail %q does not contain %q", detail, evidence)
		}
	}
}

// On the live failure, TLSHandshakeDone carried an empty ConnectionState even
// though x509 returned UnknownAuthorityError. The rejected leaf inside that
// error is the last normal-verifier evidence available and must not be lost.
func TestHTTPSRequestTraceFallsBackToRejectedUnknownAuthorityLeaf(t *testing.T) {
	started := time.Date(2026, time.September, 1, 7, 46, 47, 0, time.UTC)
	requestTrace := &httpsRequestTrace{started: started, phase: "starting_request"}
	clientTrace := requestTrace.clientTrace()
	leaf := &x509.Certificate{
		Raw:     []byte("rejected leaf certificate"),
		Subject: pkix.Name{CommonName: "connectivitycheck.gstatic.com"},
		Issuer:  pkix.Name{CommonName: "unexpected edge issuer"},
	}
	verificationErr := x509.UnknownAuthorityError{Cert: leaf}
	clientTrace.TLSHandshakeDone(tls.ConnectionState{}, verificationErr)

	detail := requestTrace.wrap(verificationErr, started.Add(1800*time.Millisecond)).Error()
	for _, evidence := range []string{
		"phase tls_handshake_failed",
		"peer_certs=unavailable",
		"verified_chains=0",
		"rejected_leaf=connectivitycheck.gstatic.com>unexpected_edge_issuer/",
		"certificate signed by unknown authority",
	} {
		if !strings.Contains(detail, evidence) {
			t.Errorf("fallback trace detail %q does not contain %q", detail, evidence)
		}
	}
	if strings.Contains(detail, string(leaf.Raw)) {
		t.Fatalf("fallback trace leaked certificate bytes: %q", detail)
	}
}

// A resolved address and the actual connected peer distinguish a bad target
// block from the proxy listener itself. Keep both in the bounded failure text;
// relying on the URL hostname erased this boundary in the main proxy incident.
func TestHTTPSRequestTraceRetainsResolvedAndConnectedEndpoints(t *testing.T) {
	started := time.Date(2026, time.August, 30, 23, 46, 21, 0, time.UTC)
	requestTrace := &httpsRequestTrace{started: started, phase: "starting_request"}
	clientTrace := requestTrace.clientTrace()
	clientTrace.DNSDone(httptrace.DNSDoneInfo{Addrs: []net.IPAddr{
		{IP: net.ParseIP("65.49.70.82")},
		{IP: net.ParseIP("2001:db8::82")},
	}})
	clientTrace.ConnectStart("tcp", "api.bringyour.com:443")
	clientTrace.ConnectDone("tcp", "api.bringyour.com:443", errors.New("dial failed"))

	detail := requestTrace.wrap(errors.New("dial failed"), started.Add(time.Second)).Error()
	for _, evidence := range []string{
		"dial api.bringyour.com:443",
		"resolved 65.49.70.82,2001:db8::82",
	} {
		if !strings.Contains(detail, evidence) {
			t.Errorf("trace detail %q does not contain %q", detail, evidence)
		}
	}
}

// Exact kernel signatures matter: nearby informational lines and a zero stall
// score must not assign a local-host cause to a remote transport failure.
func TestParseLocalNetworkSignalsSeparatesExactFailureSignatures(t *testing.T) {
	output := []byte(strings.Join([]string{
		`kernel: skmem_slab_alloc_locked "skc.buf_def.AppleBCMWLANSkywalkPool": failed to allocate slab (non-sleeping mode)`,
		`kernel: skmem_slab_alloc_locked inventory succeeded`,
		`kernel: netif_gso_tcp_segment_mbuf failed to alloc segment mbuf`,
		`kernel: netif_gso_tcp_segment_mbuf completed`,
		`kernel: DPS Symptoms StallScore:50 NetScore:50`,
		`kernel: DPS Symptoms StallScore:0 NetScore:100`,
		`kernel: DPS Symptoms StallScore:not-a-number`,
		`kernel: DPS Symptoms StallScore:7 StallScore:12`,
	}, "\n"))

	signals := parseLocalNetworkSignals(output, 78800)
	if signals.skywalkSlabFailures != 1 {
		t.Errorf("Skywalk slab failures = %d, want 1", signals.skywalkSlabFailures)
	}
	if signals.gsoFailures != 1 {
		t.Errorf("GSO failures = %d, want 1", signals.gsoFailures)
	}
	if signals.wifiStalls != 3 || signals.maxWifiStallScore != 50 {
		t.Errorf("Wi-Fi stalls = %d max %d, want 3 max 50", signals.wifiStalls, signals.maxWifiStallScore)
	}
	if signals.ipv6RouterLifetimeZeros != 0 || signals.ipMonitorNetworkChanges != 0 || signals.peerTCPStallFlows != 0 || signals.peerTCPStallProcesses != 0 {
		t.Errorf("unrelated signals = %#v, want zero route and peer TCP counts", signals)
	}
}

// Darwin can repeat a TCP summary for one socket. Count its opaque flow once,
// require at least two unanswered outbound SYNs, and exclude the acceptance
// runner itself before aggregating distinct peer processes.
func TestParseLocalNetworkSignalsAggregatesOnlyRepeatedPeerTCPStalls(t *testing.T) {
	const runnerPID = 78800
	peerFlowOne := darwinPeerTCPSummary(47037777, "browser-helper", 41001, "0xaaa1", "0/2", "IPv4-redacted:0<->IPv4-redacted:0")
	output := []byte(strings.Join([]string{
		`configd: RTADV en0: router lifetime became zero router=fe80::private`,
		`configd: RTADV en0: router lifetime refreshed`,
		`configd[123:456] [com.apple.SystemConfiguration:IPMonitor] network changed: IPv6 absent address=2001:db8::private`,
		peerFlowOne,
		peerFlowOne,
		darwinPeerTCPSummary(47037778, "sync-agent", 41002, "0xbbb2", "0/9", "IPv6-redacted:0<->IPv6-redacted:0"),
		darwinPeerTCPSummary(47037779, "proxy-main", runnerPID, "0xccc3", "0/9", "IPv4-redacted:0<->IPv4-redacted:0"),
		darwinPeerTCPSummary(47037780, "one-shot", 41003, "0xddd4", "0/1", "IPv4-redacted:0<->IPv4-redacted:0"),
		darwinPeerTCPSummary(47037781, "answered", 41004, "0xeee5", "1/9", "IPv4-redacted:0<->IPv4-redacted:0"),
	}, "\n"))

	signals := parseLocalNetworkSignals(output, runnerPID)
	if signals.ipv6RouterLifetimeZeros != 1 || signals.ipMonitorNetworkChanges != 1 {
		t.Errorf("route signals = %d/%d, want 1/1", signals.ipv6RouterLifetimeZeros, signals.ipMonitorNetworkChanges)
	}
	if signals.peerTCPStallFlows != 2 || signals.peerTCPStallProcesses != 2 {
		t.Errorf("peer TCP stalls = %d flows/%d processes, want 2/2", signals.peerTCPStallFlows, signals.peerTCPStallProcesses)
	}
	if classification := signals.classification(); classification != "local-network-path-churn+peer-tcp-stall" {
		t.Errorf("classification = %q, want local-network-path-churn+peer-tcp-stall", classification)
	}
}

// The socket join key is record-scoped. An incomplete counter record followed
// by an unrelated record containing a flow field must not fabricate a stalled
// peer flow by joining across the compact-log timestamp boundary.
func TestParseLocalNetworkSignalsDoesNotJoinPeerTCPFieldsAcrossRecords(t *testing.T) {
	output := []byte(strings.Join([]string{
		`2026-09-05 13:37:39.000000-0500 kernel[0:0] tcp_connection_summary [redacted] interface: en0 (skipped: 0)`,
		`so_gencnt: 88037777 t_state: SYN_SENT process: orphan-peer:50001 flowctl: 0us (0x0) SYN in/out: 0/9 FIN in/out: 0/0`,
		`2026-09-05 13:37:40.000000-0500 kernel[0:0] unrelated network record`,
		`metadata flow: 0xfabricated`,
	}, "\n"))

	signals := parseLocalNetworkSignals(output, 78800)
	if signals.peerTCPStallFlows != 0 || signals.peerTCPStallProcesses != 0 {
		t.Fatalf("cross-record TCP fields produced stalls: %#v", signals)
	}
}

// The production query is padded around the exact request but the emitted
// evidence retains the unpadded UTC request interval and no raw log content.
func TestCollectLocalNetworkFailureDiagnosticClassifiesDarwinSignals(t *testing.T) {
	started := time.Date(2026, time.September, 5, 3, 20, 18, 209657000, time.UTC)
	finished := started.Add(2 * time.Millisecond)
	commandCount := 0
	commandName := ""
	commandArgs := []string{}
	diagnostic := collectLocalNetworkFailureDiagnosticWith(
		started,
		finished,
		"darwin",
		"/private/tmp/proxy-main",
		78800,
		func(ctx context.Context, name string, args ...string) ([]byte, error) {
			commandCount++
			commandName = name
			commandArgs = append(commandArgs, args...)
			if _, ok := ctx.Deadline(); !ok {
				t.Error("local log query has no deadline")
			}
			return []byte(strings.Join([]string{
				`kernel: skmem_slab_alloc_locked "skc.buf_def.AppleBCMWLANSkywalkPool": failed to allocate slab (non-sleeping mode)`,
				`kernel: netif_gso_tcp_segment_mbuf failed to alloc segment mbuf`,
				`kernel: DPS Symptoms StallScore:50 NetScore:50`,
			}, "\n")), nil
		},
	)
	if commandCount != 1 || commandName != "/usr/bin/log" {
		t.Fatalf("log command = %q count %d, want /usr/bin/log once", commandName, commandCount)
	}
	joinedArgs := strings.Join(commandArgs, "\x00")
	for _, expected := range []string{
		"show\x00--style\x00compact\x00--info\x00--debug",
		"--start\x00" + started.Add(-2*time.Second).Truncate(time.Second).Local().Format("2006-01-02 15:04:05"),
		"--end\x00" + finished.Add(2*time.Second).Truncate(time.Second).Add(time.Second).Local().Format("2006-01-02 15:04:05"),
		`process == "kernel"`,
		`process == "configd"`,
		`eventMessage CONTAINS "tcp_connection_summary"`,
		`eventMessage CONTAINS "router lifetime became zero"`,
		`category == "IPMonitor"`,
		`eventMessage CONTAINS "network changed:"`,
	} {
		if !strings.Contains(joinedArgs, expected) {
			t.Errorf("log arguments %q do not contain %q", joinedArgs, expected)
		}
	}
	for _, expected := range []string{
		"local_host{os=darwin executable=proxy-main pid=78800",
		"request_interval=2026-09-05T03:20:18.209657Z/2026-09-05T03:20:18.211657Z",
		"classification=local-kernel-buffer-pressure+wifi-stall",
		"skywalk_slab_failures=1",
		"gso_allocation_failures=1",
		"wifi_stalls=1",
		"max_wifi_stall_score=50",
		"ipv6_router_lifetime_zeros=0",
		"ip_monitor_network_changes=0",
		"peer_tcp_stall_flows=0",
		"peer_tcp_stall_processes=0",
	} {
		if !strings.Contains(diagnostic, expected) {
			t.Errorf("diagnostic %q does not contain %q", diagnostic, expected)
		}
	}
	if strings.Contains(diagnostic, "AppleBCMWLANSkywalkPool") {
		t.Fatalf("diagnostic retained raw kernel output: %q", diagnostic)
	}
}

// The fixed-schema diagnostic may retain only aggregate route and peer-flow
// counts. Process names, endpoints, router addresses, and opaque flow IDs from
// unified logging are private raw evidence and must not be emitted.
func TestCollectLocalNetworkFailureDiagnosticRedactsRouteAndPeerTCPDetails(t *testing.T) {
	started := time.Date(2026, time.September, 5, 18, 37, 39, 0, time.UTC)
	output := strings.Join([]string{
		`configd: RTADV en0: router lifetime became zero router=fe80::private-router`,
		`configd[123:456] [com.apple.SystemConfiguration:IPMonitor] network changed: IPv6 absent address=2001:db8::private-address`,
		darwinPeerTCPSummary(57037777, "private-browser", 51001, "0xface01", "0/9", "198.51.100.70:443<->192.0.2.1:52000"),
		darwinPeerTCPSummary(57037778, "private-sync", 51002, "0xface02", "0/7", "203.0.113.71:443<->192.0.2.1:52001"),
	}, "\n")
	diagnostic := collectLocalNetworkFailureDiagnosticWith(
		started,
		started.Add(30*time.Second),
		"darwin",
		"proxy-main",
		78800,
		func(context.Context, string, ...string) ([]byte, error) {
			return []byte(output), nil
		},
	)
	for _, expected := range []string{
		"classification=local-network-path-churn+peer-tcp-stall",
		"ipv6_router_lifetime_zeros=1",
		"ip_monitor_network_changes=1",
		"peer_tcp_stall_flows=2",
		"peer_tcp_stall_processes=2",
	} {
		if !strings.Contains(diagnostic, expected) {
			t.Errorf("diagnostic %q does not contain %q", diagnostic, expected)
		}
	}
	for _, privateValue := range []string{
		"private-browser",
		"private-sync",
		"private-router",
		"private-address",
		"198.51.100.70",
		"203.0.113.71",
		"0xface01",
		"0xface02",
	} {
		if strings.Contains(diagnostic, privateValue) {
			t.Errorf("diagnostic %q retained private raw value %q", diagnostic, privateValue)
		}
	}
}

// Wi-Fi stalls, clean intervals, and an unavailable log query are different
// boundaries; none may be guessed from a generic transport timeout.
func TestCollectLocalNetworkFailureDiagnosticSeparatesAdjacentClassifications(t *testing.T) {
	started := time.Date(2026, time.September, 5, 3, 49, 11, 132992000, time.UTC)
	cases := []struct {
		output             string
		commandErr         error
		wantClassification string
		wantQueryStatus    string
		wantAggregate      string
	}{
		{
			output:             `kernel: DPS Symptoms StallScore:50 NetScore:50`,
			wantClassification: "local-wifi-stall",
		},
		{
			output:             `kernel: ordinary Wi-Fi telemetry StallScore:0`,
			wantClassification: "no-local-kernel-signal",
		},
		{
			output:             `configd[123:456] [com.apple.SystemConfiguration:IPMonitor] network changed: IPv6 present`,
			wantClassification: "no-local-kernel-signal",
			wantAggregate:      "ip_monitor_network_changes=1",
		},
		{
			output:             `configd: RTADV en0: router lifetime became zero`,
			wantClassification: "local-network-path-churn",
			wantAggregate:      "ipv6_router_lifetime_zeros=1",
		},
		{
			output: strings.Join([]string{
				darwinPeerTCPSummary(67037777, "one-peer", 50001, "0xaaa1", "0/2", "IPv4-redacted:0<->IPv4-redacted:0"),
				darwinPeerTCPSummary(67037778, "one-peer", 50001, "0xaaa2", "0/8", "IPv4-redacted:0<->IPv4-redacted:0"),
				darwinPeerTCPSummary(67037779, "second-peer", 50002, "0xaaa3", "0/1", "IPv4-redacted:0<->IPv4-redacted:0"),
			}, "\n"),
			wantClassification: "no-local-kernel-signal",
			wantAggregate:      "peer_tcp_stall_flows=2 peer_tcp_stall_processes=1",
		},
		{
			output: strings.Join([]string{
				darwinPeerTCPSummary(77037777, "first-peer", 50001, "0xbbb1", "0/2", "IPv4-redacted:0<->IPv4-redacted:0"),
				darwinPeerTCPSummary(77037778, "second-peer", 50002, "0xbbb2", "0/3", "IPv4-redacted:0<->IPv4-redacted:0"),
			}, "\n"),
			wantClassification: "local-peer-tcp-stall",
			wantAggregate:      "peer_tcp_stall_flows=2 peer_tcp_stall_processes=2",
		},
		{
			commandErr:         errors.New("query unavailable"),
			wantClassification: "query-unavailable",
			wantQueryStatus:    "query_status=failed",
		},
	}
	for _, testCase := range cases {
		diagnostic := collectLocalNetworkFailureDiagnosticWith(
			started,
			started.Add(30*time.Second),
			"darwin",
			"proxy-main",
			42,
			func(context.Context, string, ...string) ([]byte, error) {
				return []byte(testCase.output), testCase.commandErr
			},
		)
		if !strings.Contains(diagnostic, "classification="+testCase.wantClassification) {
			t.Errorf("diagnostic %q does not classify %q", diagnostic, testCase.wantClassification)
		}
		if testCase.wantQueryStatus != "" && !strings.Contains(diagnostic, testCase.wantQueryStatus) {
			t.Errorf("diagnostic %q does not contain %q", diagnostic, testCase.wantQueryStatus)
		}
		if testCase.wantAggregate != "" && !strings.Contains(diagnostic, testCase.wantAggregate) {
			t.Errorf("diagnostic %q does not contain aggregate %q", diagnostic, testCase.wantAggregate)
		}
	}
}

// A terminal annotation is idempotent, preserves errors.Is, and cannot turn a
// failed request into a successful campaign result.
func TestAppendLocalNetworkFailureDiagnosticPreservesTransportFailure(t *testing.T) {
	started := time.Date(2026, time.September, 5, 3, 49, 11, 132992000, time.UTC)
	sentinel := errors.New("socket is not connected")
	requestTrace := &httpsRequestTrace{started: started, phase: "sending_request_failed"}
	requestErr := requestTrace.wrap(sentinel, started.Add(2*time.Millisecond))
	collectorCount := 0
	collector := func(gotStarted time.Time, gotFinished time.Time) string {
		collectorCount++
		if !gotStarted.Equal(started) || !gotFinished.Equal(started.Add(2*time.Millisecond)) {
			t.Errorf("collector interval = %s/%s", gotStarted, gotFinished)
		}
		return "local_host{classification=local-kernel-buffer-pressure}"
	}

	annotated := appendLocalNetworkFailureDiagnostic(requestErr, collector)
	annotated = appendLocalNetworkFailureDiagnostic(annotated, collector)
	if collectorCount != 1 {
		t.Fatalf("collector calls = %d, want 1", collectorCount)
	}
	if !errors.Is(annotated, sentinel) {
		t.Fatalf("annotated failure no longer wraps transport cause: %v", annotated)
	}
	if !strings.Contains(annotated.Error(), "local_host{classification=local-kernel-buffer-pressure}") {
		t.Fatalf("annotated failure lacks local-host evidence: %v", annotated)
	}
}

// Once a target returned HTTP, the tunnel reached a server. Do not run an
// expensive local-kernel query or confuse target policy with local transport.
func TestAppendLocalNetworkFailureDiagnosticSkipsTargetResponse(t *testing.T) {
	started := time.Date(2026, time.September, 5, 3, 20, 18, 0, time.UTC)
	requestTrace := &httpsRequestTrace{started: started, phase: "reading_response_body"}
	requestErr := requestTrace.wrap(&targetHTTPStatusError{statusCode: http.StatusTooManyRequests}, started.Add(time.Second))
	collectorCalled := false
	annotated := appendLocalNetworkFailureDiagnostic(requestErr, func(time.Time, time.Time) string {
		collectorCalled = true
		return "unexpected"
	})
	if collectorCalled {
		t.Fatal("target response triggered a local-host query")
	}
	var statusErr *targetHTTPStatusError
	if !errors.As(annotated, &statusErr) || statusErr.statusCode != http.StatusTooManyRequests {
		t.Fatalf("target error changed: %v", annotated)
	}
}

// Unsupported hosts are explicit and do not run a guessed logging command.
func TestCollectLocalNetworkFailureDiagnosticFailsClosedOffDarwin(t *testing.T) {
	commandCalled := false
	started := time.Date(2026, time.September, 5, 3, 20, 18, 0, time.UTC)
	diagnostic := collectLocalNetworkFailureDiagnosticWith(
		started,
		started.Add(time.Second),
		"linux",
		"proxy-main",
		42,
		func(context.Context, string, ...string) ([]byte, error) {
			commandCalled = true
			return nil, nil
		},
	)
	if commandCalled {
		t.Fatal("unsupported host executed a logging command")
	}
	if !strings.Contains(diagnostic, "source=unsupported") || !strings.Contains(diagnostic, "classification=query-unavailable") {
		t.Fatalf("unsupported-host diagnostic = %q", diagnostic)
	}
}

func TestReadCredentialsRequiresPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "credentials")
	if err := os.WriteFile(path, []byte("user\npassword\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.user != "user" || got.password != "password" {
		t.Fatalf("credentials = %#v", got)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentials(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("public credentials error = %v", err)
	}
	link := filepath.Join(directory, "credentials-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentials(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink credentials error = %v", err)
	}
}

func TestWriteResultsIsPrivateAtomicTSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "results.tsv")
	results := []Result{
		{Case: "socks", Status: "PASS", Detail: "one\tline\nonly"},
		{Case: "http", Status: "FAIL", Detail: "failed safely"},
		{Case: "wireguard", Status: "PASS", Detail: "complete"},
	}
	if err := WriteResults(path, results); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "server/proxy\tsocks\tPASS\tone line only\n" +
		"server/proxy\thttp\tFAIL\tfailed safely\n" +
		"server/proxy\twireguard\tPASS\tcomplete\n"
	if string(data) != want {
		t.Fatalf("result TSV = %q, want %q", data, want)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".proxy-results-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary result files remained: %v", matches)
	}
}

// A live WireGuard overlap failure exceeded the ordinary progress-line budget,
// which silently removed the encrypted boundary, hosted-device route, and the
// terminal request error from results.tsv. The private result artifact is the
// root-cause record and must retain all three bounded diagnostic boundaries.
func TestWriteResultsRetainsLongWireGuardFailureBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.tsv")
	detail := "WireGuard inner packet trace " + strings.Repeat("tcp-event ", 140) +
		"; WireGuard outer UDP trace out{sent=19} in{packets=8}" +
		"; hosted device timeline: route={142.251.210.195->p41(1)}" +
		"; net/http: timeout awaiting response headers"
	if len(detail) <= 1000 {
		t.Fatalf("test detail is only %d bytes", len(detail))
	}
	if err := WriteResults(path, []Result{{Case: "wireguard", Status: "FAIL", Detail: detail}}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	result := string(data)
	for _, want := range []string{
		"WireGuard inner packet trace",
		"WireGuard outer UDP trace out{sent=19} in{packets=8}",
		"hosted device timeline: route={142.251.210.195->p41(1)}",
		"net/http: timeout awaiting response headers",
	} {
		if !strings.Contains(result, want) {
			t.Fatalf("private result truncated %q: %q", want, result)
		}
	}
}

func assertResult(t *testing.T, results []Result, name, status string) Result {
	t.Helper()
	for _, result := range results {
		if result.Case == name {
			if result.Status != status {
				t.Fatalf("%s status = %s, want %s (%s)", name, result.Status, status, result.Detail)
			}
			return result
		}
	}
	t.Fatalf("no result for %s: %#v", name, results)
	return Result{}
}
