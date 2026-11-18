package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// T2 for the breakers' measures' clients (ABUSE-M9-03) on testdata/mail_measures: each client reads its source's
// shape, and answers unknown, never zero, when the source gives it nothing. No request leaves the process: the
// transport is a test's.

func testMailMeasuresFixture(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "mail_measures", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// A transport answering from a function, keeping every request it saw.
type testMailMeasuresTransport struct {
	stateLock sync.Mutex
	requests  []*http.Request
	answer    func(request *http.Request) (*http.Response, error)
}

func (self *testMailMeasuresTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	self.stateLock.Lock()
	self.requests = append(self.requests, request)
	self.stateLock.Unlock()
	return self.answer(request)
}

func (self *testMailMeasuresTransport) seen() []*http.Request {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]*http.Request{}, self.requests...)
}

func testMailMeasuresResponse(status int, body []byte) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(string(body))),
	}
}

const testPostmasterToken = "test-token-not-a-credential"

func testPostmasterTokenSource(ctx context.Context) (string, error) {
	return testPostmasterToken, nil
}

var testPostmasterDay = time.Date(2026, 11, 1, 15, 30, 0, 0, time.UTC)

// The client's settings on a .example origin.
func testPostmasterSettings() *PostmasterToolsClientSettings {
	settings := DefaultPostmasterToolsClientSettings()
	settings.BaseUrl = "https://postmaster.example"
	return settings
}

// The client reads the day's userReportedSpamRatio from the API's own shape, asking for one domain and one day
// with the token in the header.
func TestPostmasterToolsClientReadsADaysSpamRate(t *testing.T) {
	transport := &testMailMeasuresTransport{answer: func(request *http.Request) (*http.Response, error) {
		return testMailMeasuresResponse(http.StatusOK, testMailMeasuresFixture(t, "postmaster_traffic_stats.json")), nil
	}}
	client := NewPostmasterToolsClient(&PostmasterToolsClientArgs{TokenSource: testPostmasterTokenSource, Transport: transport, Settings: testPostmasterSettings()})
	partsPerMillion, known := client.SpamRate(context.Background(), "Root.Example.", testPostmasterDay)
	if !known || partsPerMillion != 1200 {
		t.Fatalf("the rate reads %d, known %t, want 0.12%%", partsPerMillion, known)
	}
	requests := transport.seen()
	if len(requests) != 1 {
		t.Fatalf("%d requests", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodGet || request.URL.Scheme != "https" || request.URL.Host != "postmaster.example" ||
		request.URL.Path != "/v1/domains/root.example/trafficStats/20261101" || request.URL.RawQuery != "" {
		t.Errorf("asked %s %s", request.Method, request.URL)
	}
	if request.Header.Get("Authorization") != "Bearer "+testPostmasterToken || strings.Contains(request.URL.String(), testPostmasterToken) {
		t.Error("the token rides somewhere but the Authorization header")
	}
}

// A missing day, an API error and an answer that is not the day's read unknown, never zero, and no error names the
// token or the url.
func TestPostmasterToolsClientAnswersUnknownNotZero(t *testing.T) {
	fixture := testMailMeasuresFixture(t, "postmaster_traffic_stats.json")
	cases := []struct {
		name   string
		answer func(request *http.Request) (*http.Response, error)
	}{
		{"a day not reported yet", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusNotFound, []byte(`{"error":{"code":404,"status":"NOT_FOUND"}}`)), nil
		}},
		{"an API error", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusInternalServerError, []byte(`{"error":{"code":500}}`)), nil
		}},
		{"a refused token", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusUnauthorized, nil), nil
		}},
		{"an unreachable API", func(request *http.Request) (*http.Response, error) {
			return nil, errors.New("dial " + request.URL.String() + " with " + request.Header.Get("Authorization"))
		}},
		{"a redirect", func(request *http.Request) (*http.Response, error) {
			response := testMailMeasuresResponse(http.StatusFound, nil)
			response.Header.Set("Location", "https://elsewhere.example/")
			return response, nil
		}},
		{"no ratio in the answer", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, testMailMeasuresFixture(t, "postmaster_traffic_stats_no_ratio.json")), nil
		}},
		{"another day's answer", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte(strings.Replace(string(fixture), "20261101", "20261031", 1))), nil
		}},
		{"a ratio out of range", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte(strings.Replace(string(fixture), "0.0012", "1.5", 1))), nil
		}},
		{"an answer that is not JSON", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte("<html>sign in</html>")), nil
		}},
		{"an answer past its bound", func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte(strings.Repeat(" ", 2*1024*1024))), nil
		}},
	}
	for _, c := range cases {
		transport := &testMailMeasuresTransport{answer: c.answer}
		client := NewPostmasterToolsClient(&PostmasterToolsClientArgs{TokenSource: testPostmasterTokenSource, Transport: transport, Settings: testPostmasterSettings()})
		if partsPerMillion, known := client.SpamRate(context.Background(), "root.example", testPostmasterDay); known || partsPerMillion != 0 {
			t.Errorf("%s: %d, known %t", c.name, partsPerMillion, known)
		}
		_, err := client.spamRate(context.Background(), "root.example", testPostmasterDay)
		if !errors.Is(err, ErrPostmasterToolsSource) {
			t.Errorf("%s: %v", c.name, err)
		} else if strings.Contains(err.Error(), testPostmasterToken) || strings.Contains(err.Error(), "postmaster.example") || strings.Contains(err.Error(), "root.example") {
			t.Errorf("%s: the error names the token, the url or the domain: %v", c.name, err)
		}
	}
}

// With no credential the client answers unknown and asks nothing; so it does when the token source fails, and for
// a domain that could change the path.
func TestPostmasterToolsClientWithNoCredentialAsksNothing(t *testing.T) {
	transport := &testMailMeasuresTransport{answer: func(request *http.Request) (*http.Response, error) {
		return testMailMeasuresResponse(http.StatusOK, testMailMeasuresFixture(t, "postmaster_traffic_stats.json")), nil
	}}
	none := NewPostmasterToolsClient(&PostmasterToolsClientArgs{Transport: transport, Settings: testPostmasterSettings()})
	if _, known := none.SpamRate(context.Background(), "root.example", testPostmasterDay); known {
		t.Error("a client with no credential answered")
	}
	failing := NewPostmasterToolsClient(&PostmasterToolsClientArgs{
		TokenSource: func(ctx context.Context) (string, error) {
			return "", errors.New("no token today")
		},
		Transport: transport,
		Settings:  testPostmasterSettings(),
	})
	if _, known := failing.SpamRate(context.Background(), "root.example", testPostmasterDay); known {
		t.Error("a client whose token source failed answered")
	}
	client := NewPostmasterToolsClient(&PostmasterToolsClientArgs{TokenSource: testPostmasterTokenSource, Transport: transport, Settings: testPostmasterSettings()})
	for _, domain := range []string{"", "root.example/../other.example", "root.example?x=1", "root example"} {
		if _, known := client.SpamRate(context.Background(), domain, testPostmasterDay); known {
			t.Errorf("domain %q answered", domain)
		}
	}
	if requests := transport.seen(); len(requests) != 0 {
		t.Errorf("%d requests left", len(requests))
	}
}

// abuse-postmaster-tools.yml reads as its three fields, and a file lacking one or not YAML is refused with no value
// in the error.
func TestParsePostmasterToolsCredential(t *testing.T) {
	credential, err := ParsePostmasterToolsCredential([]byte("client_id: \" test-client \"\nclient_secret: test-secret-value\nrefresh_token: test-refresh-value\n"))
	if err != nil || credential.ClientId != "test-client" || credential.ClientSecret != "test-secret-value" || credential.RefreshToken != "test-refresh-value" {
		t.Fatalf("%+v, %v", credential, err)
	}
	for _, data := range []string{
		"",
		"client_id: test-client\nclient_secret: test-secret-value\n",
		"client_id: test-client\nrefresh_token: test-refresh-value\n",
		"client_secret: [test-secret-value\n",
	} {
		_, err := ParsePostmasterToolsCredential([]byte(data))
		if !errors.Is(err, ErrPostmasterToolsCredential) || strings.Contains(err.Error(), "test-secret-value") || strings.Contains(err.Error(), "test-refresh-value") {
			t.Errorf("%q: %v", data, err)
		}
	}
}

// The token source trades the refresh token at the token endpoint once, keeps the access token until the margin
// before its expiry, then trades again; the API takes the token it gave. A trade that fails answers an error, never
// a stale token, and its error holds no credential or token.
func TestPostmasterToolsTokenSourceTradesAndKeeps(t *testing.T) {
	credential := &PostmasterToolsCredential{ClientId: "test-client", ClientSecret: "test-secret-value", RefreshToken: "test-refresh-value"}
	settings := testPostmasterSettings()
	settings.TokenUrl = "https://token.example/token"
	now := testPostmasterDay
	tokenCount := 0
	failToken := false
	transport := &testMailMeasuresTransport{answer: func(request *http.Request) (*http.Response, error) {
		if request.URL.Host == "token.example" {
			if err := request.ParseForm(); err != nil {
				return nil, err
			}
			if request.Method != http.MethodPost || request.PostForm.Get("grant_type") != "refresh_token" || request.PostForm.Get("client_id") != "test-client" ||
				request.PostForm.Get("client_secret") != "test-secret-value" || request.PostForm.Get("refresh_token") != "test-refresh-value" {
				return testMailMeasuresResponse(http.StatusBadRequest, nil), nil
			}
			if failToken {
				return testMailMeasuresResponse(http.StatusBadRequest, []byte(`{"error":"invalid_grant"}`)), nil
			}
			tokenCount += 1
			return testMailMeasuresResponse(http.StatusOK, []byte(fmt.Sprintf(`{"access_token":"test-access-%d","expires_in":3600,"token_type":"Bearer"}`, tokenCount))), nil
		}
		if request.Header.Get("Authorization") != fmt.Sprintf("Bearer test-access-%d", tokenCount) {
			return testMailMeasuresResponse(http.StatusUnauthorized, nil), nil
		}
		return testMailMeasuresResponse(http.StatusOK, testMailMeasuresFixture(t, "postmaster_traffic_stats.json")), nil
	}}
	tokenSource := NewPostmasterToolsTokenSource(credential, transport, settings, func() time.Time { return now })
	client := NewPostmasterToolsClient(&PostmasterToolsClientArgs{TokenSource: tokenSource, Transport: transport, Settings: settings})

	if rate, known := client.SpamRate(context.Background(), "root.example", testPostmasterDay); !known || rate != 1200 {
		t.Fatalf("the first read: %d, %t", rate, known)
	}
	now = now.Add(time.Hour - settings.TokenExpiryMargin - time.Second)
	if rate, known := client.SpamRate(context.Background(), "root.example", testPostmasterDay); !known || rate != 1200 || tokenCount != 1 {
		t.Fatalf("inside the token's life: %d, %t, %d trades", rate, known, tokenCount)
	}
	now = now.Add(time.Second)
	if rate, known := client.SpamRate(context.Background(), "root.example", testPostmasterDay); !known || rate != 1200 || tokenCount != 2 {
		t.Fatalf("at the margin: %d, %t, %d trades", rate, known, tokenCount)
	}

	// a refused trade: no token, and the read is unknown
	failToken = true
	now = now.Add(2 * time.Hour)
	token, err := tokenSource(context.Background())
	if token != "" || !errors.Is(err, ErrPostmasterToolsSource) {
		t.Fatalf("a refused trade: %q, %v", token, err)
	}
	for _, secret := range []string{"test-secret-value", "test-refresh-value", "test-access-"} {
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error holds %q: %v", secret, err)
		}
	}
	if _, known := client.SpamRate(context.Background(), "root.example", testPostmasterDay); known {
		t.Error("a read with no token answered")
	}
	// an answer that is not a bearer token, and a credential that is none
	for name, answer := range map[string]string{
		"no token":     `{"expires_in":3600,"token_type":"Bearer"}`,
		"no expiry":    `{"access_token":"test-access-x","token_type":"Bearer"}`,
		"another type": `{"access_token":"test-access-x","expires_in":3600,"token_type":"mac"}`,
		"not json":     `<html></html>`,
	} {
		source := NewPostmasterToolsTokenSource(credential, &testMailMeasuresTransport{answer: func(request *http.Request) (*http.Response, error) {
			return testMailMeasuresResponse(http.StatusOK, []byte(answer)), nil
		}}, settings, nil)
		if token, err := source(context.Background()); token != "" || !errors.Is(err, ErrPostmasterToolsSource) {
			t.Errorf("%s: %q, %v", name, token, err)
		}
	}
	if token, err := NewPostmasterToolsTokenSource(nil, transport, settings, nil)(context.Background()); token != "" || !errors.Is(err, ErrPostmasterToolsCredential) {
		t.Errorf("no credential: %q, %v", token, err)
	}
}
