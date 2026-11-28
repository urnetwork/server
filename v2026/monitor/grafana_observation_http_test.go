package monitor

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"testing"
	"time"
)

type grafanaRoundTripFunc func(*http.Request) (*http.Response, error)

func (f grafanaRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type grafanaHTTPClientFunc func(*http.Request) (*http.Response, error)

func (f grafanaHTTPClientFunc) Do(request *http.Request) (*http.Response, error) {
	return f(request)
}

func grafanaFixtureResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestGrafanaObservationHTTPClientReplaysOverIPv4(t *testing.T) {
	const (
		requestBody = `{"queries":[{"expr":"vector(1)"}]}`
		username    = "fixture-query-user"
		password    = "synthetic-query-credential"
	)
	primaryCalls := 0
	ipv4Calls := 0
	primary := grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		primaryCalls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != requestBody {
			t.Fatalf("primary request body = %q", body)
		}
		return nil, syscall.EHOSTUNREACH
	})
	ipv4 := grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		ipv4Calls++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != requestBody {
			t.Fatalf("IPv4 replay body = %q", body)
		}
		gotUsername, gotPassword, ok := request.BasicAuth()
		if !ok || gotUsername != username || gotPassword != password {
			t.Fatalf("IPv4 replay auth = %q/%q/%t", gotUsername, gotPassword, ok)
		}
		if request.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("IPv4 replay content type = %q", request.Header.Get("Content-Type"))
		}
		return grafanaFixtureResponse(http.StatusOK, `{"status":"success"}`), nil
	})
	client := newGrafanaObservationHTTPClientWithTransports(time.Second, primary, ipv4)
	request, err := http.NewRequest(
		http.MethodPost,
		"https://grafana.fixture.example/api/ds/query",
		strings.NewReader(requestBody),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.SetBasicAuth(username, password)

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("response status = %d", response.StatusCode)
	}
	if primaryCalls != 1 || ipv4Calls != 1 {
		t.Fatalf("transport calls = primary:%d IPv4:%d, want 1/1", primaryCalls, ipv4Calls)
	}
}

func TestGrafanaObservationHTTPClientDoesNotRetryHTTPResult(t *testing.T) {
	primaryCalls := 0
	ipv4Calls := 0
	primary := grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
		primaryCalls++
		return grafanaFixtureResponse(http.StatusServiceUnavailable, "synthetic outage"), nil
	})
	ipv4 := grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
		ipv4Calls++
		return grafanaFixtureResponse(http.StatusOK, "unexpected"), nil
	})
	client := newGrafanaObservationHTTPClientWithTransports(time.Second, primary, ipv4)
	request, err := http.NewRequest(
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("response status = %d", response.StatusCode)
	}
	if primaryCalls != 1 || ipv4Calls != 0 {
		t.Fatalf("transport calls = primary:%d IPv4:%d, want 1/0", primaryCalls, ipv4Calls)
	}
}

func TestGrafanaObservationHTTPClientKeepsSuccessContextThroughBodyRead(t *testing.T) {
	primary := grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body: &grafanaContextBody{
				ctx:     request.Context(),
				content: "synthetic delayed body",
			},
			Header: make(http.Header),
		}, nil
	})
	client := newGrafanaObservationHTTPClientWithTransports(
		time.Second,
		primary,
		grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return grafanaFixtureResponse(http.StatusOK, "unexpected"), nil
		}),
	)
	request, err := http.NewRequest(
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read successful response after Do returned: %v", err)
	}
	if string(body) != "synthetic delayed body" {
		t.Fatalf("successful response body = %q", body)
	}
}

func TestGrafanaObservationHTTPClientKeepsFallbackContextThroughBodyRead(t *testing.T) {
	client := newGrafanaObservationHTTPClientWithTransports(
		time.Second,
		grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, syscall.EHOSTUNREACH
		}),
		grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body: &grafanaContextBody{
					ctx:     request.Context(),
					content: "synthetic fallback body",
				},
				Header: make(http.Header),
			}, nil
		}),
	)
	request, err := http.NewRequest(
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read successful fallback response after Do returned: %v", err)
	}
	if string(body) != "synthetic fallback body" {
		t.Fatalf("successful fallback response body = %q", body)
	}
}

func TestGrafanaObservationHTTPClientRedactsTransportDetails(t *testing.T) {
	const (
		primaryPrivate = "primary-private-marker.fixture.example"
		ipv4Private    = "ipv4-private-marker.fixture.example"
	)
	primary := grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, &net.OpError{
			Op:   "read",
			Net:  "tcp6",
			Addr: syntheticNetworkAddress(primaryPrivate),
			Err:  syscall.ENETUNREACH,
		}
	})
	ipv4 := grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(ipv4Private)
	})
	client := newGrafanaObservationHTTPClientWithTransports(time.Second, primary, ipv4)
	request, err := http.NewRequest(
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(request)
	if err == nil {
		t.Fatal("dual transport failure returned nil error")
	}
	want := "Grafana observation transport failed: primary=network-unreachable ipv4_fallback=transport-error"
	if err.Error() != want {
		t.Fatalf("transport error = %q, want %q", err, want)
	}
	for _, private := range []string{primaryPrivate, ipv4Private, "tcp6"} {
		if strings.Contains(err.Error(), private) {
			t.Fatalf("transport error leaked %q: %s", private, err)
		}
	}
}

func TestGrafanaObservationHTTPClientDoesNotRetryUnknownTransportError(t *testing.T) {
	const privateMarker = "private-certificate-detail.fixture.example"
	ipv4Calls := 0
	client := newGrafanaObservationHTTPClientWithTransports(
		time.Second,
		grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New(privateMarker)
		}),
		grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			ipv4Calls++
			return grafanaFixtureResponse(http.StatusOK, "unexpected"), nil
		}),
	)
	request, err := http.NewRequest(
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(request)
	if err == nil || err.Error() != "Grafana observation transport failed: primary=transport-error" {
		t.Fatalf("unknown transport error = %v", err)
	}
	if strings.Contains(err.Error(), privateMarker) {
		t.Fatalf("unknown transport error leaked private detail: %s", err)
	}
	if ipv4Calls != 0 {
		t.Fatalf("IPv4 fallback calls = %d, want 0", ipv4Calls)
	}
}

func TestGrafanaObservationHTTPClientsDoNotShareDefaultTransport(t *testing.T) {
	first := newGrafanaObservationHTTPClient(time.Second)
	second := newGrafanaObservationHTTPClient(time.Second)
	firstPrimary := first.primary.(*http.Client).Transport
	firstIPv4 := first.ipv4.(*http.Client).Transport
	secondPrimary := second.primary.(*http.Client).Transport
	secondIPv4 := second.ipv4.(*http.Client).Transport
	if firstPrimary == http.DefaultTransport || firstIPv4 == http.DefaultTransport {
		t.Fatal("Grafana observer retained the process-wide default transport")
	}
	if firstPrimary == secondPrimary || firstIPv4 == secondIPv4 {
		t.Fatal("independent Grafana observers share a connection pool")
	}
}

func TestGrafanaSignalsOwnIndependentObservationClients(t *testing.T) {
	datasourceAdapter := NewGrafanaDatasourcesSignal().(*signalAdapter)
	datasourceProbe := datasourceAdapter.probe.(grafanaDatasourcesProbe)
	datasourceClient := datasourceProbe.client.(*grafanaObservationHTTPClient)
	redisAdapter := NewRedisRatesSignal().(*signalAdapter)
	redisProbe := redisAdapter.probe.(redisRatesProbe)
	redisClient := redisProbe.client.(*grafanaObservationHTTPClient)
	subscriptionAdapter := NewSubscriptionMetricsSignal().(*signalAdapter)
	subscriptionProbe := subscriptionAdapter.probe.(subscriptionMetricsProbe)
	subscriptionClient := subscriptionProbe.client.(*grafanaObservationHTTPClient)
	clients := []*grafanaObservationHTTPClient{datasourceClient, redisClient, subscriptionClient}
	for i, client := range clients {
		for j := i + 1; j < len(clients); j++ {
			other := clients[j]
			if client == other {
				t.Fatalf("Grafana signal clients %d and %d share an HTTP client", i, j)
			}
			primary := client.primary.(*http.Client).Transport
			otherPrimary := other.primary.(*http.Client).Transport
			ipv4 := client.ipv4.(*http.Client).Transport
			otherIPv4 := other.ipv4.(*http.Client).Transport
			if primary == otherPrimary || ipv4 == otherIPv4 {
				t.Fatalf("Grafana signal clients %d and %d share a connection pool", i, j)
			}
		}
	}
	for index, client := range clients {
		if client.primary.(*http.Client).Transport == http.DefaultTransport ||
			client.ipv4.(*http.Client).Transport == http.DefaultTransport {
			t.Fatalf("Grafana signal client %d uses the process-wide default transport", index)
		}
	}
}

func TestGrafanaObservationHTTPClientTimeoutFallsBackToIPv4(t *testing.T) {
	primaryCalls := 0
	ipv4Calls := 0
	client := newGrafanaObservationHTTPClientWithTransports(
		100*time.Millisecond,
		grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			primaryCalls++
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
		grafanaRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			ipv4Calls++
			if err := request.Context().Err(); err != nil {
				t.Fatalf("IPv4 fallback received spent context: %v", err)
			}
			return grafanaFixtureResponse(http.StatusOK, "synthetic success"), nil
		}),
	)
	request, err := http.NewRequest(
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if primaryCalls != 1 || ipv4Calls != 1 {
		t.Fatalf("transport calls = primary:%d IPv4:%d, want 1/1", primaryCalls, ipv4Calls)
	}
}

func TestGrafanaObservationHTTPClientParentCancellationStopsFallback(t *testing.T) {
	primaryCalls := 0
	ipv4Calls := 0
	client := newGrafanaObservationHTTPClientWithClients(
		grafanaHTTPClientFunc(func(request *http.Request) (*http.Response, error) {
			primaryCalls++
			<-request.Context().Done()
			return nil, request.Context().Err()
		}),
		grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			ipv4Calls++
			return grafanaFixtureResponse(http.StatusOK, "unexpected"), nil
		}),
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(request)
	if err == nil || err.Error() != "Grafana observation transport failed: primary=context-canceled" {
		t.Fatalf("canceled observation error = %v", err)
	}
	if primaryCalls != 1 || ipv4Calls != 0 {
		t.Fatalf("transport calls = primary:%d IPv4:%d, want 1/0", primaryCalls, ipv4Calls)
	}
}

func TestGrafanaObservationHTTPClientClosesErrorResponses(t *testing.T) {
	primaryBody := &grafanaCloseRecorder{}
	ipv4Body := &grafanaCloseRecorder{}
	client := newGrafanaObservationHTTPClientWithClients(
		grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: primaryBody}, syscall.ECONNRESET
		}),
		grafanaHTTPClientFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusBadGateway, Body: ipv4Body}, errors.New("synthetic fallback failure")
		}),
	)
	request, err := http.NewRequest(
		http.MethodGet,
		"https://grafana.fixture.example/api/health",
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(request)
	if err == nil {
		t.Fatal("dual transport error returned nil")
	}
	if !primaryBody.closed || !ipv4Body.closed {
		t.Fatalf("error response bodies closed = primary:%t IPv4:%t", primaryBody.closed, ipv4Body.closed)
	}
}

func TestGrafanaObservationHTTPClientFailsClosedWithoutReplayableBody(t *testing.T) {
	ipv4Calls := 0
	client := newGrafanaObservationHTTPClientWithTransports(
		time.Second,
		grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, syscall.ECONNRESET
		}),
		grafanaRoundTripFunc(func(*http.Request) (*http.Response, error) {
			ipv4Calls++
			return grafanaFixtureResponse(http.StatusOK, "unexpected"), nil
		}),
	)
	request, err := http.NewRequest(
		http.MethodPost,
		"https://grafana.fixture.example/api/ds/query",
		io.NopCloser(strings.NewReader("synthetic non-replayable body")),
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.Do(request)
	if err == nil || err.Error() != "Grafana observation transport failed: primary=connection-reset ipv4_fallback=request-replay-unavailable" {
		t.Fatalf("non-replayable request error = %v", err)
	}
	if ipv4Calls != 0 {
		t.Fatalf("IPv4 fallback calls = %d, want 0", ipv4Calls)
	}
}

func TestClassifyGrafanaTransportError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		wantClass string
		wantRetry bool
	}{
		{name: "host route", err: syscall.EHOSTUNREACH, wantClass: "network-unreachable", wantRetry: true},
		{name: "network route", err: syscall.ENETUNREACH, wantClass: "network-unreachable", wantRetry: true},
		{name: "connection reset", err: syscall.ECONNRESET, wantClass: "connection-reset", wantRetry: true},
		{name: "deadline", err: context.DeadlineExceeded, wantClass: "timeout", wantRetry: true},
		{name: "network timeout", err: syntheticGrafanaTimeoutError{}, wantClass: "timeout", wantRetry: true},
		{name: "canceled", err: context.Canceled, wantClass: "context-canceled", wantRetry: false},
		{name: "unknown", err: errors.New("synthetic transport detail"), wantClass: "transport-error", wantRetry: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotClass, gotRetry := classifyGrafanaTransportError(context.Background(), test.err)
			if gotClass != test.wantClass || gotRetry != test.wantRetry {
				t.Fatalf("classification = %s/%t, want %s/%t", gotClass, gotRetry, test.wantClass, test.wantRetry)
			}
		})
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	gotClass, gotRetry := classifyGrafanaTransportError(canceledCtx, syscall.EHOSTUNREACH)
	if gotClass != "context-canceled" || gotRetry {
		t.Fatalf("canceled request classification = %s/%t", gotClass, gotRetry)
	}
}

func TestForceDialNetworkUsesIPv4AndPreservesAddress(t *testing.T) {
	gotNetwork := ""
	gotAddress := ""
	dial := forceDialNetwork(
		"tcp4",
		func(_ context.Context, network string, address string) (net.Conn, error) {
			gotNetwork = network
			gotAddress = address
			return nil, errors.New("synthetic dial stop")
		},
	)
	_, _ = dial(context.Background(), "tcp", "grafana.fixture.example:443")
	if gotNetwork != "tcp4" || gotAddress != "grafana.fixture.example:443" {
		t.Fatalf("forced dial = %s/%s", gotNetwork, gotAddress)
	}
}

type syntheticNetworkAddress string

func (a syntheticNetworkAddress) Network() string { return "tcp6" }
func (a syntheticNetworkAddress) String() string  { return string(a) }

type syntheticGrafanaTimeoutError struct{}

func (syntheticGrafanaTimeoutError) Error() string { return "synthetic timeout" }
func (syntheticGrafanaTimeoutError) Timeout() bool { return true }
func (syntheticGrafanaTimeoutError) Temporary() bool {
	return true
}

type grafanaCloseRecorder struct {
	closed bool
}

func (*grafanaCloseRecorder) Read([]byte) (int, error) { return 0, io.EOF }
func (r *grafanaCloseRecorder) Close() error {
	r.closed = true
	return nil
}

type grafanaContextBody struct {
	ctx     context.Context
	content string
	read    bool
}

func (b *grafanaContextBody) Read(target []byte) (int, error) {
	if b.read {
		return 0, io.EOF
	}
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	default:
	}
	b.read = true
	return copy(target, b.content), nil
}

func (*grafanaContextBody) Close() error { return nil }
