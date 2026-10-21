// Actual adapter and native client controls use memory or a fixture-only TLS
// loopback server. No Main endpoint, credentials, SQL or process is exercised.
package monitor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Track exact bounded body consumption and ownership without a real socket.
type mimirQueryTestBody struct {
	reader    io.Reader
	readError error
	afterRead func()
	readBytes int
	closed    bool
}

func (self *mimirQueryTestBody) Read(buffer []byte) (int, error) {
	if self.readError != nil {
		return 0, self.readError
	}
	n, err := self.reader.Read(buffer)
	self.readBytes += n
	if self.afterRead != nil {
		self.afterRead()
	}
	return n, err
}

func (self *mimirQueryTestBody) Close() error { self.closed = true; return nil }

// Both adapter identities keep their original reducer and source denominator.
func mimirQueryTestAdapter(t testing.TB, selection bool, payload string) (Signal, SignalSettings, string) {
	t.Helper()
	if selection {
		return NewProviderSelectionSignal(), selectionTestSettings(t, payload), "provider-selection-unavailable"
	}
	return NewProviderPickerSignal(), pickerTestSettings(t, payload), "provider-picker-unobservable"
}

// Every rejected HTTP/transport source stays unknown, retains only observed
// finite diagnostics and closes the body even if a client returns body+error.
func TestProviderHttpsFailureBoundariesAndPrivacy(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, testCase := range []struct {
			name, phase, encoding string
			status                int
			err, readError        error
			oversize, nilResponse bool
		}{
			{name: "forbidden", phase: "http-status", status: 403},
			{name: "server-error", phase: "http-status", status: 500},
			{name: "redirect", phase: "http-status", status: 302},
			{name: "encoding", phase: "response-encoding", status: 200, encoding: "gzip"},
			{name: "bound", phase: "response-bound", status: 200, oversize: true},
			{name: "partial-body", phase: "response-body", status: 200, readError: io.ErrUnexpectedEOF},
			{name: "nil-response", phase: "response-body", nilResponse: true},
			{name: "tls", phase: "tls", err: x509.UnknownAuthorityError{Cert: &x509.Certificate{DNSNames: []string{"private-response.example"}}}},
			{name: "dns", phase: "dns", err: &net.DNSError{Name: "private-response.example", Err: "private-cause", IsNotFound: true}},
			{name: "dial", phase: "dial", err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("private-cause")}},
			{name: "timeout", phase: "http", err: context.DeadlineExceeded},
			{name: "opaque", phase: "http", err: errors.New("private-cause query=private-expression password=private-query-password")},
		} {
			signal, settings, class := mimirQueryTestAdapter(t, selection, "")
			payload := "private-response-body"
			if testCase.oversize {
				payload = strings.Repeat("x", providerPickerMaxBytes+512)
			}
			body := &mimirQueryTestBody{reader: strings.NewReader(payload)}
			if testCase.readError != nil {
				payload = pickerTestPayload(t, pickerTestRows(map[string]float64{"initial/nonempty": 20}))
				if selection {
					payload = pickerTestPayload(t, selectionTestRows("returned", 20))
				}
				body.reader = io.MultiReader(strings.NewReader(payload), &mimirQueryTestBody{readError: testCase.readError})
			}
			calls := 0
			settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(request *http.Request) (*http.Response, error) {
				calls++
				providerHttpsTestRequest(t, request, selection)
				if testCase.nilResponse {
					return nil, nil
				}
				return &http.Response{StatusCode: testCase.status, Header: http.Header{"Content-Encoding": []string{testCase.encoding}}, Body: body}, testCase.err
			}}
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || calls != 1 || len(alerts) != 1 || !testCase.nilResponse && !body.closed || body.readBytes > providerPickerMaxBytes+1 {
				t.Fatalf("%s changed request count, unknown result, body bound or ownership", testCase.name)
			}
			alert := requireAlertClass(t, alerts, class)
			if !strings.Contains(alert.Observed, "reason=bounded-source-unavailable") || !strings.Contains(alert.Observed, "mimir_query_phase="+testCase.phase) {
				t.Errorf("%s discarded its finite native boundary", testCase.name)
			}
			if testCase.err != nil || testCase.nilResponse {
				if strings.Contains(alert.Observed, "http_status=") {
					t.Errorf("%s invented a received status", testCase.name)
				}
			}
			if testCase.status == 403 && !strings.Contains(alert.Observed, "error_class="+observationErrorClassAccessDenied) {
				t.Error("authenticated endpoint denial lost its finite class")
			}
			requireAlertOmits(t, alert, "private-response", "private-cause", "private-expression", "private-query-password", "query-synthetic", "metrics.example", "Authorization", "Basic ")
		}
	}
}

// A received warning/info/empty/malformed envelope remains a parser unknown,
// not an invented transport failure or a zero-valued healthy provider cohort.
func TestProviderHttpsEnvelopeAuthorityUnchanged(t *testing.T) {
	for _, selection := range []bool{false, true} {
		for _, payload := range []string{
			`{"status":"success","data":{"resultType":"vector","result":[]}}`,
			`{"status":"success","warnings":["private-warning"],"data":{"resultType":"vector","result":[]}}`,
			`{"status":"success","infos":["private-info"],"data":{"resultType":"vector","result":[]}}`,
			`{"status":"error","error":"private-backend-error"}`,
			`private-malformed`,
		} {
			signal, settings, class := mimirQueryTestAdapter(t, selection, payload)
			alerts, err := signal.Run(context.Background(), settings)
			if err != nil || len(alerts) != 1 {
				t.Fatal("received invalid or absent data became health")
			}
			alert := requireAlertClass(t, alerts, class)
			requireAlertOmits(t, alert, "mimir_query_phase=", "error_class=", "private-warning", "private-info", "private-backend-error", "private-malformed")
		}
	}
}

// HTTPS does not need a services-role SSH gateway when the API source cohort
// is otherwise authoritative. This also prevents reintroducing the old gate.
func TestProviderHttpsApiOnlyInventoryNeedsNoGateway(t *testing.T) {
	settings := pickerTestSettings(t, pickerTestPayload(t, pickerTestRows(map[string]float64{"initial/nonempty": 20})))
	settings.Hosts[0].Roles = []string{"api"}
	alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
	if err != nil || len(alerts) != 0 {
		t.Fatal("logical fleet query retained the SSH gateway prerequisite")
	}
}

// Exact form decoding retains regex escaping, selected cohort and fixed time.
// It sends neither query parameters in the URL nor an invented tenant header.
func TestProviderHttpsExactQueryAndParentDeadline(t *testing.T) {
	for _, selection := range []bool{false, true} {
		signal, settings, _ := mimirQueryTestAdapter(t, selection, "{}")
		settings.LogServiceBlocks["api"] = []string{"blue+test", "green.test"}
		env, err := newProbeEnv(settings)
		if err != nil {
			t.Fatal(err)
		}
		scope := pickerScope(env)
		want := providerPickerQuery(settings.Environment, scope)
		if selection {
			want = providerSelectionQuery(settings.Environment, scope)
		}
		parent, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		parentDeadline, _ := parent.Deadline()
		calls := 0
		settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(request *http.Request) (*http.Response, error) {
			calls++
			providerHttpsTestRequest(t, request, selection)
			deadline, ok := request.Context().Deadline()
			if request.PostForm.Get("query") != want || request.URL.RawQuery != "" || request.GetBody != nil || !ok || !deadline.Equal(parentDeadline) {
				t.Fatal("native form, replay policy or parent deadline changed")
			}
			return providerHttpsTestResponse("{}"), nil
		}}
		if _, err := signal.Run(parent, settings); err != nil || calls != 1 {
			t.Fatal("exact native request was not attempted once")
		}
	}
}

// A changed or unreadable effective generation refuses before client.Do.
func TestProviderHttpsStaleSettingsRefuseBeforeSend(t *testing.T) {
	for _, unavailable := range []bool{false, true} {
		settings := pickerTestSettings(t, "")
		current := settings
		current.Grafana.QueryPassword = "private-rotated"
		settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) {
			if unavailable {
				return SignalSettings{}, errors.New("private-loader-error")
			}
			return current, nil
		})
		calls := 0
		settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(*http.Request) (*http.Response, error) { calls++; return providerHttpsTestResponse("{}"), nil }}
		alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
		if err != nil || calls != 0 {
			t.Fatal("stale or unreadable configuration sent credentials")
		}
		alert := requireAlertClass(t, alerts, "provider-picker-unobservable")
		phase := "settings-stale"
		if unavailable {
			phase = "settings-unavailable"
		}
		if !strings.Contains(alert.Observed, "mimir_query_phase="+phase) {
			t.Fatal("configuration refusal lost its finite reason")
		}
		requireAlertOmits(t, alert, "private-rotated", "private-loader-error", "private-query-password")
	}
}

// Rotation at the response boundary cannot certify data with stale credentials,
// even when the body contains a complete healthy process cohort.
func TestProviderHttpsRotationAfterReadRejectsHealthyBody(t *testing.T) {
	for _, selection := range []bool{false, true} {
		payload := pickerTestPayload(t, pickerTestRows(map[string]float64{"initial/nonempty": 20}))
		if selection {
			payload = pickerTestPayload(t, selectionTestRows("returned", 20))
		}
		signal, settings, class := mimirQueryTestAdapter(t, selection, payload)
		current, checks := settings, 0
		settings.SettingsGenerationCheck = NewSettingsGenerationCheck(func() (SignalSettings, error) { checks++; return current, nil })
		body := &mimirQueryTestBody{reader: strings.NewReader(payload), afterRead: func() { current.Grafana.QueryPassword = "private-rotated" }}
		settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200, Body: body}, nil }}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || checks != 2 || !body.closed || len(alerts) != 1 || !strings.Contains(requireAlertClass(t, alerts, class).Observed, "mimir_query_phase=settings-stale") {
			t.Fatal("mid-request settings rotation accepted a healthy response")
		}
	}
}

// Cancellation is forced during Read without sleeps; no healthy reduction or
// new request survives, and the owned body is joined by its synchronous close.
func TestProviderHttpsCancellationDuringBodyClosesWithoutVerdict(t *testing.T) {
	settings := pickerTestSettings(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &mimirQueryTestBody{reader: strings.NewReader(pickerTestPayload(t, pickerTestRows(map[string]float64{"initial/nonempty": 20}))), afterRead: cancel}
	settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(*http.Request) (*http.Response, error) { return &http.Response{StatusCode: 200, Body: body}, nil }}
	alerts, err := NewProviderPickerSignal().Run(ctx, settings)
	if !errors.Is(err, context.Canceled) || len(alerts) != 0 || !body.closed {
		t.Fatal("cancellation escaped body ownership or emitted a verdict")
	}
}

// Missing synthetic Do and missing production generation authority both fail
// before native I/O. A programmatic source cannot silently enable networking.
func TestProviderHttpsMissingAuthorityCannotFallThrough(t *testing.T) {
	settings := pickerTestSettings(t, "")
	settings.Source = &syntheticSource{}
	alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
	if err != nil || !strings.Contains(requireAlertClass(t, alerts, "provider-picker-unobservable").Observed, "mimir_query_phase=source-contract") {
		t.Fatal("missing synthetic HTTP source did not fail closed")
	}
	env, err := newProbeEnv(settings)
	if err != nil {
		t.Fatal(err)
	}
	env.mimirQueryClient = nil
	if raw, err := readProviderMimirQuery(context.Background(), env, "synthetic-query", pickerTestNow(), providerPickerMaxBytes); raw != "" || !strings.Contains(mimirQueryFailureDetails(err), "settings-unavailable") {
		t.Fatal("missing production generation authority reached native transport")
	}
}

// Invalid endpoint/credential input cannot send a password or manufacture a
// different origin. These controls use only synthetic configuration values.
func TestProviderHttpsInvalidConfigurationRefusesBeforeSend(t *testing.T) {
	for _, defect := range []string{"missing-user", "missing-password", "url-domain", "numeric-domain", "colon-user", "newline-password"} {
		settings := pickerTestSettings(t, "")
		switch defect {
		case "missing-user":
			settings.Grafana.QueryUsername = ""
		case "missing-password":
			settings.Grafana.QueryPassword = ""
		case "url-domain":
			settings.Grafana.QueryDomain = "private.example/other?secret=private-query-password"
		case "numeric-domain":
			settings.Grafana.QueryDomain = "192.0.2.1"
		case "colon-user":
			settings.Grafana.QueryUsername = "private:user"
		case "newline-password":
			settings.Grafana.QueryPassword = "private\npassword"
		}
		calls := 0
		settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(*http.Request) (*http.Response, error) { calls++; return providerHttpsTestResponse("{}"), nil }}
		alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
		if err != nil || calls != 0 {
			t.Errorf("%s admitted a credentialed request", defect)
		}
		alert := requireAlertClass(t, alerts, "provider-picker-unobservable")
		if !strings.Contains(alert.Observed, "mimir_query_phase=configuration-unavailable") {
			t.Fatal("invalid native authority lost its closed boundary")
		}
		requireAlertOmits(t, alert, "private-query-password", "private.example", "private:user", "192.0.2.1")
	}
}

// Native source access keeps the existing endpoint guard ahead of injected Do.
func TestProviderHttpsHostScopeGuardsBeforeCredentialsLeave(t *testing.T) {
	settings := pickerTestSettings(t, "")
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "synthetic-grafana.metrics.example", Roles: []string{"services"}})
	settings.ExcludedHosts = []string{"synthetic-grafana.metrics.example"}
	calls := 0
	settings.Source = &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(*http.Request) (*http.Response, error) { calls++; return providerHttpsTestResponse("{}"), nil }}
	alerts, err := NewProviderPickerSignal().Run(context.Background(), settings)
	if err != nil || calls != 0 {
		t.Fatal("excluded owned endpoint reached the HTTP source")
	}
	requireAlertClass(t, alerts, "monitor-host-scope-partial")
	for _, alert := range alerts {
		requireAlertOmits(t, alert, "private-query-password", "synthetic-grafana.metrics.example")
	}
}

// Malformed internal diagnostics never expand the finite public vocabulary.
func TestMimirQueryFailureProjectionIsClosed(t *testing.T) {
	err := &mimirQueryError{phase: "private-phase-value", status: 999, err: errors.New("private-cause")}
	if mimirQueryFailureDetails(err) != " mimir_query_phase=unknown" || strings.Contains(err.Error(), "private-") {
		t.Fatal("native query diagnostics exposed a raw field or unproved status")
	}
}

// Keep the accepted native transport policy explicit: one fresh HTTP/1 client,
// verified TLS, unchanged 15-second budget and no proxy/replay/redirect path.
func TestMimirQueryNativeTransportPolicy(t *testing.T) {
	transport := newMimirQueryTransport()
	defer transport.CloseIdleConnections()
	client := newMimirQueryClient(transport)
	if transport.Proxy != nil || !transport.DisableKeepAlives || !transport.DisableCompression || transport.ForceAttemptHTTP2 || transport.TLSNextProto == nil || len(transport.TLSNextProto) != 0 || transport.TLSClientConfig.InsecureSkipVerify || transport.TLSClientConfig.MinVersion != tls.VersionTLS12 || len(transport.TLSClientConfig.NextProtos) != 1 || transport.TLSClientConfig.NextProtos[0] != "http/1.1" || transport.MaxConnsPerHost != 1 || transport.TLSHandshakeTimeout != 15*time.Second || transport.ResponseHeaderTimeout != 15*time.Second || client.Timeout != 15*time.Second || client.CheckRedirect(nil, nil) != http.ErrUseLastResponse {
		t.Fatal("native request policy broadened or changed its budget")
	}
}

// Actual native TLS rejects an untrusted local fixture, then accepts its
// explicitly supplied test root. A redirect still cannot issue a second POST.
func TestMimirQueryNativeTlsAndRedirect(t *testing.T) {
	var calls atomic.Int32
	fixture := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls.Add(1)
		writer.Header().Set("Location", "/synthetic-second-request")
		writer.WriteHeader(http.StatusFound)
	}))
	fixture.Config.ErrorLog = log.New(io.Discard, "", 0)
	fixture.StartTLS()
	defer fixture.Close()
	transport := newMimirQueryTransport()
	defer transport.CloseIdleConnections()
	client := newMimirQueryClient(transport)
	request, err := http.NewRequest(http.MethodPost, fixture.URL, strings.NewReader("query=synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	if response, err := client.Do(request); err == nil || calls.Load() != 0 {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		t.Fatal("native TLS accepted an untrusted fixture certificate")
	}
	trusted := newMimirQueryTransport()
	defer trusted.CloseIdleConnections()
	trusted.TLSClientConfig.RootCAs = x509.NewCertPool()
	trusted.TLSClientConfig.RootCAs.AddCert(fixture.Certificate())
	request, err = http.NewRequest(http.MethodPost, fixture.URL, strings.NewReader("query=synthetic"))
	if err != nil {
		t.Fatal(err)
	}
	response, err := newMimirQueryClient(trusted).Do(request)
	if err != nil {
		t.Fatal("trusted local fixture did not provide the TLS healthy control")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusFound || calls.Load() != 1 {
		t.Fatal("native client followed a redirect or lost the received status")
	}
}
