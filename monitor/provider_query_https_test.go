// Synthetic native query controls: no resolver, socket, process or Main access.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// The optional Do seam exercises the actual adapters without a network client.
type providerHttpsTestSource struct {
	*syntheticSource
	do func(*http.Request) (*http.Response, error)
}

func (self *providerHttpsTestSource) Do(request *http.Request) (*http.Response, error) {
	return self.do(request)
}

// A complete in-memory HTTP response preserves the original parser boundary.
func providerHttpsTestResponse(payload string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload))}
}

// Decoding keeps the causal control buildable on the previous settings type;
// that baseline ignores these new fields and still requires the SSH source.
func providerHttpsTestCredentials(t testing.TB, settings *SignalSettings) {
	t.Helper()
	if err := json.Unmarshal([]byte(`{"QueryDomain":"metrics.example","QueryUsername":"query-synthetic","QueryPassword":"private-query-password"}`), &settings.Grafana); err != nil {
		t.Fatal(err)
	}
}

// Assert the owned endpoint, role credential, form and original evaluation time.
func providerHttpsTestRequest(t testing.TB, request *http.Request, selection bool) {
	t.Helper()
	username, password, ok := request.BasicAuth()
	if !ok || username != "query-synthetic" || password != "private-query-password" || request.Method != http.MethodPost || request.URL.String() != "https://synthetic-grafana.metrics.example/prometheus/api/v1/query" || request.Header.Get("Content-Type") != "application/x-www-form-urlencoded" || request.Header.Get("X-Scope-OrgID") != "" {
		t.Fatal("native request changed endpoint, credential role or form authority")
	}
	if err := request.ParseForm(); err != nil || len(request.PostForm) != 2 || request.PostForm.Get("time") != strconv.FormatInt(pickerTestNow().Unix(), 10) {
		t.Fatal("native request changed its fixed evaluation time")
	}
	query := request.PostForm.Get("query")
	metric := "urnetwork_provider_picker_outcomes_total"
	if selection {
		metric = "urnetwork_findproviders2_selection_schema_version"
	}
	if !strings.Contains(query, metric) || !strings.Contains(query, "offset 5m") || !strings.Contains(query, "timestamp(") || !strings.Contains(query, "resets(") || !strings.Contains(query, `env="synthetic",job="api"`) {
		t.Fatal("native request changed bounded query semantics")
	}
}

// Authenticated fleet metrics must not require a usable SSH gateway. The
// existing path deterministically returns unavailable despite a valid source.
func TestProviderHttpsDoesNotRequireSshGateway(t *testing.T) {
	for _, selection := range []bool{false, true} {
		signal := NewProviderPickerSignal()
		payload := pickerTestPayload(t, pickerTestRows(map[string]float64{"initial/nonempty": 20}))
		settings := pickerTestSettings(t, payload)
		if selection {
			signal = NewProviderSelectionSignal()
			payload = pickerTestPayload(t, selectionTestRows("returned", 20))
			settings = selectionTestSettings(t, payload)
		}
		providerHttpsTestCredentials(t, &settings)
		sshCalls, queryCalls := 0, 0
		settings.Source = &providerHttpsTestSource{
			syntheticSource: &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
				sshCalls++
				return "", errors.New("private-synthetic-ssh-unavailable")
			}},
			do: func(request *http.Request) (*http.Response, error) {
				queryCalls++
				providerHttpsTestRequest(t, request, selection)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(payload))}, nil
			},
		}
		alerts, err := signal.Run(context.Background(), settings)
		if err != nil || len(alerts) != 0 || queryCalls != 1 || sshCalls != 0 {
			t.Errorf("%s: provider metric read still required SSH despite an available authenticated native source (alerts=%d native=%d ssh=%d)", signal.Key(), len(alerts), queryCalls, sshCalls)
		}
	}
}
