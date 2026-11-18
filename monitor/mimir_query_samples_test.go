// The exported instant query on an in-memory HTTP source: no endpoint, credential or process of a real env.
package monitor

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Settings whose source answers the query's request with payload, after holding the request to the owned endpoint,
// the query role's credential and the form.
func mimirQuerySamplesTestSettings(t *testing.T, at time.Time, query string, respond func() (*http.Response, error)) SignalSettings {
	t.Helper()
	source := &providerHttpsTestSource{syntheticSource: &syntheticSource{}, do: func(request *http.Request) (*http.Response, error) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "query-synthetic" || password != "private-query-password" || request.Method != http.MethodPost ||
			request.URL.String() != "https://synthetic-grafana.metrics.example/prometheus/api/v1/query" {
			t.Fatal("the request left the owned endpoint or the query role's credential")
		}
		if err := request.ParseForm(); err != nil || len(request.PostForm) != 2 || request.PostForm.Get("query") != query ||
			request.PostForm.Get("time") != strconv.FormatInt(at.Unix(), 10) {
			t.Fatalf("the request's form is %v", request.PostForm)
		}
		return respond()
	}}
	settings := syntheticSettings(source)
	providerHttpsTestCredentials(t, &settings)
	return settings
}

func TestReadMimirQuerySamplesParsesAVector(t *testing.T) {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	query := `max_over_time(example_total{env="synthetic"}[86400s])`
	payload := `{"status":"success","data":{"resultType":"vector","result":[` +
		`{"metric":{"__name__":"example_total","instance":"one","result":"clean"},"value":[1793577600,"41"]},` +
		`{"metric":{"instance":"one","result":"retained"},"value":[1793577600,"0"]}]}}`
	settings := mimirQuerySamplesTestSettings(t, at, query, func() (*http.Response, error) { return providerHttpsTestResponse(payload), nil })
	samples, err := ReadMimirQuerySamples(context.Background(), settings, query, at, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) != 2 || samples[0].Value != 41 || samples[0].Labels["result"] != "clean" || samples[0].Labels["instance"] != "one" ||
		samples[1].Value != 0 || samples[1].Labels["result"] != "retained" {
		t.Fatalf("samples %+v", samples)
	}
	empty := mimirQuerySamplesTestSettings(t, at, query, func() (*http.Response, error) {
		return providerHttpsTestResponse(`{"status":"success","data":{"resultType":"vector","result":[]}}`), nil
	})
	if samples, err := ReadMimirQuerySamples(context.Background(), empty, query, at, 64*1024); err != nil || len(samples) != 0 {
		t.Fatalf("an empty vector read %d samples: %v", len(samples), err)
	}
}

// Every answer that is no vector is an error whose text carries nothing of the query, the credential or the body, and
// a source that owns no HTTP is refused rather than replaced by a real client.
func TestReadMimirQuerySamplesRefusesWhatIsNoVector(t *testing.T) {
	at := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	query := `private_expression_total{env="synthetic"}`
	for _, testCase := range []struct {
		name    string
		respond func() (*http.Response, error)
		want    string
	}{
		{name: "forbidden", respond: func() (*http.Response, error) {
			response := providerHttpsTestResponse("private-response-body")
			response.StatusCode = http.StatusForbidden
			return response, nil
		}, want: "Mimir query HTTP 403"},
		{name: "not json", respond: func() (*http.Response, error) { return providerHttpsTestResponse("private-response-body"), nil }, want: "Mimir query invalid response"},
		{name: "a matrix", respond: func() (*http.Response, error) {
			return providerHttpsTestResponse(`{"status":"success","data":{"resultType":"matrix","result":[]}}`), nil
		}, want: "Mimir query invalid response"},
		{name: "an error status", respond: func() (*http.Response, error) {
			return providerHttpsTestResponse(`{"status":"error","error":"private-response-body"}`), nil
		}, want: "Mimir query invalid response"},
		{name: "a value that is no number", respond: func() (*http.Response, error) {
			return providerHttpsTestResponse(`{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[1,"private-response-body"]}]}}`), nil
		}, want: "Mimir query invalid response"},
		{name: "transport", respond: func() (*http.Response, error) {
			return nil, errors.New("private-cause password=private-query-password")
		}, want: "Mimir query transport unavailable"},
	} {
		settings := mimirQuerySamplesTestSettings(t, at, query, testCase.respond)
		samples, err := ReadMimirQuerySamples(context.Background(), settings, query, at, 64*1024)
		if err == nil || samples != nil || err.Error() != testCase.want {
			t.Fatalf("%s: samples %v, error %v", testCase.name, samples, err)
		}
		for _, private := range []string{"private-response-body", "private-query-password", "private_expression", "private-cause"} {
			if strings.Contains(err.Error(), private) {
				t.Fatalf("%s: the error carries %q", testCase.name, private)
			}
		}
	}
	// a source with no Do, and settings with neither a source nor a generation check: nothing is sent
	settings := syntheticSettings(&syntheticSource{})
	providerHttpsTestCredentials(t, &settings)
	if _, err := ReadMimirQuerySamples(context.Background(), settings, query, at, 64*1024); err == nil || err.Error() != "Mimir query source contract is unavailable" {
		t.Fatalf("a source that owns no HTTP: %v", err)
	}
	settings.Source = nil
	if _, err := ReadMimirQuerySamples(context.Background(), settings, query, at, 64*1024); err == nil || err.Error() != "Mimir query source contract is unavailable" {
		t.Fatalf("settings with no generation check: %v", err)
	}
	settings = mimirQuerySamplesTestSettings(t, at, query, func() (*http.Response, error) { return providerHttpsTestResponse("{}"), nil })
	settings.Grafana.QueryPassword = ""
	if _, err := ReadMimirQuerySamples(context.Background(), settings, query, at, 64*1024); err == nil {
		t.Fatal("settings with no query credential sent a request")
	}
}
