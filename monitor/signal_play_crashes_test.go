package monitor

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	gojwt "github.com/golang-jwt/jwt/v5"
)

func TestPlayCrashesMissingCredentialNoops(t *testing.T) {
	created := 0
	signal := newPlayCrashesSignal("http://invalid", func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) {
		created++
		return nil, fmt.Errorf("must not run")
	})
	alerts, err := signal.Run(context.Background(), SignalSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 || created != 0 {
		t.Fatalf("missing credential ran provider: alerts=%v clients=%d", alerts, created)
	}
}

func TestSafeGooglePlayIssueURI(t *testing.T) {
	if got := safeGooglePlayIssueURI("https://play.google.com/console/issues/7?account=secret"); got != "https://play.google.com/console/issues/7?redacted" {
		t.Fatalf("safe URI = %q", got)
	}
	for _, raw := range []string{
		"https://evil.example/issues/7",
		"https://play.google.com.evil.example/issues/7",
		"https://user:secret@play.google.com/issues/7",
		"https://play.google.com/issues/7#secret",
		"http://play.google.com/issues/7",
	} {
		if got := safeGooglePlayIssueURI(raw); got != "" {
			t.Fatalf("unsafe URI %q rendered as %q", raw, got)
		}
	}
}

func TestPlayCrashesPresentIncompleteCredentialIsVisible(t *testing.T) {
	created := 0
	signal := newPlayCrashesSignal("http://invalid", func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) {
		created++
		return nil, nil
	})
	_, err := signal.Run(context.Background(), SignalSettings{
		GooglePlay: GooglePlayReportingSettings{Enabled: true, PackageName: "com.example.app"},
	})
	if err == nil || !strings.Contains(err.Error(), "credential is incomplete") {
		t.Fatalf("incomplete credential error = %v", err)
	}
	if created != 0 {
		t.Fatalf("client created for incomplete credential: %d", created)
	}
}

func TestPlayCrashesPaginatesRetriesDeduplicatesAndRedacts(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	issueCount := int64(3)
	metricAttempts := 0
	issueAttempts := 0
	metricPages := 0
	issuePages := 0
	sampleCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1beta1/apps/com.example.app/crashRateMetricSet":
			writeTestJSON(t, writer, map[string]any{
				"freshnessInfo": map[string]any{"freshnesses": []any{
					map[string]any{
						"aggregationPeriod": "DAILY",
						"latestEndTime":     map[string]any{"year": 2026, "month": 9, "day": 2, "timeZone": map[string]any{"id": "America/Los_Angeles"}},
					},
				}},
			})
		case "/v1beta1/apps/com.example.app/crashRateMetricSet:query":
			metricAttempts++
			if metricAttempts == 1 {
				writer.Header().Set("Retry-After", "0")
				writer.WriteHeader(http.StatusTooManyRequests)
				return
			}
			var body playMetricRequest
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode metric request: %v", err)
				writer.WriteHeader(http.StatusBadRequest)
				return
			}
			metricPages++
			if body.PageToken == "" {
				writeTestJSON(t, writer, playMetricResponseFixture("2026-08-31", "metric-next"))
				return
			}
			if body.PageToken != "metric-next" {
				t.Errorf("metric page token = %q", body.PageToken)
			}
			writeTestJSON(t, writer, playMetricResponseFixture("2026-09-01", ""))
		case "/v1beta1/apps/com.example.app/errorIssues:search":
			issueAttempts++
			if issueAttempts == 1 {
				writer.Header().Set("Retry-After", "0")
				writer.WriteHeader(http.StatusTooManyRequests)
				return
			}
			assertPlayIntervalQuery(t, request.URL.Query())
			issuePages++
			if request.URL.Query().Get("pageToken") == "" {
				writeTestJSON(t, writer, map[string]any{
					"errorIssues": []any{map[string]any{
						"name": "apps/com.example.app/issue-7", "type": "CRASH",
						"cause": "java.lang.IllegalStateException", "location": "network.ur.Main.start",
						"distinctUsers": "2", "errorReportCount": fmt.Sprintf("%d", issueCount),
						"lastErrorReportTime": "2026-09-02T10:00:00Z",
						"issueUri":            "https://play.google.com/console/issues/7?account=secret",
						"lastAppVersion":      map[string]any{"versionCode": "103"},
						"lastOsVersion":       map[string]any{"apiLevel": "36"},
						"sampleErrorReports":  []string{"apps/com.example.app/report-9"},
					}},
					"nextPageToken": "issue-next",
				})
				return
			}
			if request.URL.Query().Get("pageToken") != "issue-next" {
				t.Errorf("issue page token = %q", request.URL.Query().Get("pageToken"))
			}
			writeTestJSON(t, writer, map[string]any{"errorIssues": []any{}})
		case "/v1beta1/apps/com.example.app/errorReports:search":
			sampleCalls++
			assertPlayIntervalQuery(t, request.URL.Query())
			if request.URL.Query().Get("filter") != "errorReportId = report-9" {
				t.Errorf("sample filter = %q", request.URL.Query().Get("filter"))
			}
			writeTestJSON(t, writer, map[string]any{"errorReports": []any{map[string]any{
				"name": "apps/com.example.app/report-9", "type": "CRASH", "issue": "apps/com.example.app/issue-7",
				"eventTime": "2026-09-02T10:00:00Z",
				"reportText": "Fatal for alice@example.com id 123e4567-e89b-12d3-a456-426614174000\n" +
					"token abcdefghijklmnop.qrstuvwxyzABCDEF.ghijklmnopqrstuv at `Main.start`\n" +
					"https://errors.example/trace?session=private",
				"appVersion":  map[string]any{"versionCode": "103"},
				"osVersion":   map[string]any{"apiLevel": "36"},
				"deviceModel": map[string]any{"marketingName": "Pixel Synthetic"},
			}}})
		default:
			t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := newProviderHTTP(server.Client())
	client.wait = func(context.Context, time.Duration) error { return nil }
	signal := newPlayCrashesSignal(server.URL, func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) {
		return client, nil
	})
	settings := playCrashTestSettings(now, t.TempDir())

	alerts, err := signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "play-crash-issue")
	if alert.Frame != "version=103" || !strings.Contains(alert.Observed, "reports_in_48h=3") {
		t.Fatalf("unexpected grouped alert: %+v", alert)
	}
	markdown := alert.Markdown()
	for _, want := range []string{"java.lang.IllegalStateException", "Pixel Synthetic", "redacted-email", "redacted-id", "redacted-token", "redacted"} {
		if !strings.Contains(markdown, want) {
			t.Errorf("markdown missing %q:\n%s", want, markdown)
		}
	}
	for _, secret := range []string{"alice@example.com", "123e4567-e89b-12d3-a456-426614174000", "abcdefghijklmnop.qrstuvwxyzABCDEF.ghijklmnopqrstuv", "session=private", "account=secret", "`Main.start`"} {
		if strings.Contains(markdown, secret) {
			t.Errorf("markdown leaked %q:\n%s", secret, markdown)
		}
	}
	if metricAttempts != 3 || metricPages != 2 || issueAttempts != 3 || issuePages != 2 || sampleCalls != 1 {
		t.Fatalf("unexpected request counts metric attempts/pages=%d/%d issue=%d/%d samples=%d", metricAttempts, metricPages, issueAttempts, issuePages, sampleCalls)
	}

	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("unchanged provider data emitted duplicate alerts: %+v", alerts)
	}
	if sampleCalls != 1 {
		t.Fatalf("unchanged issue fetched another sample: %d", sampleCalls)
	}

	issueCount = 4
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	corrected := requireAlertClass(t, alerts, "play-crash-issue")
	if !strings.Contains(corrected.Observed, "reports_in_48h=4") || sampleCalls != 2 {
		t.Fatalf("same-hour correction was not observed: alert=%+v samples=%d", corrected, sampleCalls)
	}

	issueCount = 2
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	replacement := requireAlertClass(t, alerts, "play-crash-correction")
	if !strings.Contains(replacement.Observed, "reports_in_48h=2") || sampleCalls != 3 {
		t.Fatalf("provider replacement correction was not observed once: alert=%+v samples=%d", replacement, sampleCalls)
	}
}

func TestPlayCrashesEmptyMetricIsUnobservableNotZero(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/v1beta1/apps/com.example.app/crashRateMetricSet":
			writeTestJSON(t, writer, map[string]any{"freshnessInfo": map[string]any{"freshnesses": []any{map[string]any{
				"aggregationPeriod": "DAILY", "latestEndTime": map[string]any{"year": 2026, "month": 9, "day": 2, "timeZone": map[string]any{"id": "America/Los_Angeles"}},
			}}}})
		case "/v1beta1/apps/com.example.app/crashRateMetricSet:query":
			writeTestJSON(t, writer, map[string]any{"rows": []any{}})
		case "/v1beta1/apps/com.example.app/errorIssues:search":
			writeTestJSON(t, writer, map[string]any{"errorIssues": []any{}})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client := newProviderHTTP(server.Client())
	signal := newPlayCrashesSignal(server.URL, func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) { return client, nil })
	alerts, err := signal.Run(context.Background(), playCrashTestSettings(now, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "play-crash-data-unobservable")
	if !strings.Contains(alert.Mechanism, "not proof of zero") {
		t.Fatalf("empty response was not explicitly qualified: %s", alert.Markdown())
	}
}

func TestPlayCrashesAuthenticationFailureBecomesRedactedVisibilityAlert(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(`{"error":"expired bearer private-token"}`))
	}))
	defer server.Close()
	client := newProviderHTTP(server.Client())
	signal := newPlayCrashesSignal(server.URL, func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) { return client, nil })
	monitor := NewWithSignals(playCrashTestSettings(now, t.TempDir()), signal)
	alerts, err := monitor.Run(context.Background())
	if err == nil {
		t.Fatal("authentication failure did not propagate an error")
	}
	alert := requireAlertClass(t, alerts, "provider-authentication")
	if !strings.Contains(alert.Observed, "HTTP status 401") {
		t.Fatalf("visibility alert lost status: %s", alert.Markdown())
	}
	if strings.Contains(alert.Markdown(), "private-token") || strings.Contains(alert.Markdown(), server.URL) {
		t.Fatalf("visibility alert leaked provider data: %s", alert.Markdown())
	}
}

func TestPlayCrashesAPIAndSchemaFailuresHaveDistinctVisibilityClasses(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)
	for name, response := range map[string]struct {
		status int
		body   string
		class  string
	}{
		"provider API":  {status: http.StatusServiceUnavailable, body: `{"error":"outage"}`, class: "provider-api"},
		"provider data": {status: http.StatusOK, body: `{"freshnessInfo":`, class: "provider-data-invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(response.status)
				_, _ = writer.Write([]byte(response.body))
			}))
			defer server.Close()
			client := newProviderHTTP(server.Client())
			client.wait = func(context.Context, time.Duration) error { return nil }
			signal := newPlayCrashesSignal(server.URL, func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) { return client, nil })
			alerts, err := NewWithSignals(playCrashTestSettings(now, t.TempDir()), signal).Run(context.Background())
			if err == nil {
				t.Fatal("provider failure did not propagate an error")
			}
			requireAlertClass(t, alerts, response.class)
		})
	}
}

func TestGooglePlayOAuthUsesReportingScopeAndBearerToken(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: encoded})

	tokenCalls := 0
	tokenServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		tokenCalls++
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		assertion := request.FormValue("assertion")
		parsed, err := gojwt.Parse(assertion, func(token *gojwt.Token) (any, error) {
			if token.Method != gojwt.SigningMethodRS256 {
				t.Errorf("OAuth assertion method = %v", token.Method)
			}
			return &key.PublicKey, nil
		})
		if err != nil || !parsed.Valid {
			t.Fatalf("parse OAuth assertion: %v", err)
		}
		claims := parsed.Claims.(gojwt.MapClaims)
		if claims["scope"] != playReportingOAuthScope || tokenKeyID(parsed) != "key-7" {
			t.Errorf("OAuth assertion claims/header = %#v %#v", claims, parsed.Header)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"access_token":"synthetic-access","token_type":"Bearer","expires_in":3600}`))
	}))
	defer tokenServer.Close()

	apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer synthetic-access" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	defer apiServer.Close()

	client, err := newGooglePlayReportingClient(context.Background(), GooglePlayReportingSettings{
		ClientEmail: "monitor@example.invalid", PrivateKey: string(privatePEM), PrivateKeyID: "key-7", TokenURL: tokenServer.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		OK bool `json:"ok"`
	}
	if err := client.json(context.Background(), http.MethodGet, apiServer.URL, "synthetic google API", nil, nil, &response); err != nil {
		t.Fatal(err)
	}
	if !response.OK || tokenCalls != 1 {
		t.Fatalf("OAuth response=%+v token calls=%d", response, tokenCalls)
	}
}

func tokenKeyID(token *gojwt.Token) string {
	value, _ := token.Header["kid"].(string)
	return value
}

func playCrashTestSettings(now time.Time, stateDir string) SignalSettings {
	return SignalSettings{
		Environment: "synthetic", StateDir: stateDir, Now: func() time.Time { return now },
		GooglePlay: GooglePlayReportingSettings{
			Enabled: true, PackageName: "com.example.app", ClientEmail: "monitor@example.invalid",
			PrivateKey: "synthetic", PrivateKeyID: "key", TokenURL: "https://oauth.example.invalid/token",
		},
	}
}

func playMetricResponseFixture(date, next string) map[string]any {
	parsed, _ := time.Parse("2006-01-02", date)
	return map[string]any{
		"rows": []any{map[string]any{
			"aggregationPeriod": "DAILY",
			"startTime":         map[string]any{"year": parsed.Year(), "month": int(parsed.Month()), "day": parsed.Day(), "timeZone": map[string]any{"id": "America/Los_Angeles"}},
			"metrics": []any{
				map[string]any{"metric": "crashRate", "decimalValue": map[string]any{"value": "0.02"}},
				map[string]any{"metric": "userPerceivedCrashRate", "decimalValue": map[string]any{"value": "0.01"}},
				map[string]any{"metric": "distinctUsers", "decimalValue": map[string]any{"value": "100"}},
			},
		}},
		"nextPageToken": next,
	}
}

func assertPlayIntervalQuery(t *testing.T, query url.Values) {
	t.Helper()
	for _, key := range []string{"interval.startTime.year", "interval.startTime.timeZone.id", "interval.endTime.year", "interval.endTime.timeZone.id"} {
		if query.Get(key) == "" {
			t.Errorf("missing interval query key %s in %v", key, query)
		}
	}
	if query.Get("interval.startTime.timeZone.id") != "UTC" || query.Get("interval.endTime.timeZone.id") != "UTC" {
		t.Errorf("interval is not UTC: %v", query)
	}
}

func writeTestJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
