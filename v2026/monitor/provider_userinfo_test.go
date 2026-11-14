package monitor

// Synthetic regressions for shared provider evidence and its public callers.
// All provider responses are in-memory; no socket, credential or provider is used.
import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The numeric documentation host isolates userinfo from email/opaque-id heuristics.
func TestProviderEvidenceUserinfoCausal(t *testing.T) {
	value := "frame Main.start https://testuserq:testpassz@192.0.2.33/trace"
	got := providerEvidence(value)
	for _, useful := range []string{"frame Main.start", "192.0.2.33/trace"} {
		if !strings.Contains(got, useful) {
			t.Fatal("causal fixture lost useful frame/host/path before privacy assertion")
		}
	}
	for _, secret := range []string{"testuserq", "testpassz"} {
		if strings.Contains(got, secret) {
			t.Fatal("provider-userinfo-causal: no-query numeric-host credentials survived shared evidence sanitizer")
		}
	}
}

// Authority removal is lexical, so malformed escapes cannot select a raw fallback.
func TestProviderEvidenceUserinfoGrammarAndUsefulContext(t *testing.T) {
	cases := []struct {
		input     string
		useful    string
		forbidden []string
	}{
		{input: "https://testuserq:testpassz@192.0.2.33/trace?session=testqueryz.", useful: "https://192.0.2.33/trace?redacted.", forbidden: []string{"testuserq", "testpassz", "testqueryz"}},
		{input: "http://testuserq@reports.example/trace", useful: "http://reports.example/trace", forbidden: []string{"testuserq"}},
		{input: "HTTPS://testuserq:testpassz@reports.example/trace", useful: "HTTPS://reports.example/trace", forbidden: []string{"testuserq", "testpassz"}},
		{input: "https://test%75serq:test%70assz@reports.example/trace", useful: "https://reports.example/trace", forbidden: []string{"test%75serq", "test%70assz"}},
		{input: "https://testuserq:testpassz@[2001:db8::1]/trace", useful: "https://\\[2001:db8::1\\]/trace", forbidden: []string{"testuserq", "testpassz"}},
		{input: "https://testuserq:test@passz@reports.example/trace", useful: "https://reports.example/trace", forbidden: []string{"testuserq", "test@passz"}},
		{input: "https://test%zzuserq:testpassz@reports.example/trace", useful: "https://reports.example/trace", forbidden: []string{"test%zzuserq", "testpassz"}},
		{input: "https://test(userq):test(passz)@reports.example/trace", useful: "https://reports.example/trace", forbidden: []string{"test(userq)", "test(passz)"}},
		{input: "https://testuserq:test!$&'()*+,;=passz@reports.example/trace", useful: "https://reports.example/trace", forbidden: []string{"testuserq", "passz"}},
		{input: "https://testuserq:testpassz@/trace", useful: "https:///trace", forbidden: []string{"testuserq", "testpassz"}},
	}
	for index, c := range cases {
		got := providerEvidence("frame Main.start " + c.input)
		if !strings.Contains(got, "frame Main.start") || !strings.Contains(got, c.useful) {
			t.Errorf("case %d lost useful context", index)
		}
		for _, secret := range c.forbidden {
			if strings.Contains(got, secret) {
				t.Errorf("case %d retained synthetic userinfo/query", index)
			}
		}
	}
}

// Preserve credential-free URLs and actual crash text rather than dropping all URLs.
func TestProviderEvidenceUserinfoHealthyContext(t *testing.T) {
	for _, value := range []string{
		"frame Main.start https://reports.example/trace",
		"frame Main.start http://192.0.2.33/source.go:7.",
		"frame Main.start https://reports.example/trace#frame",
	} {
		if providerEvidence(value) != value {
			t.Fatal("healthy evidence changed")
		}
	}
	got := providerEvidence("frame Main.start https://reports.example/trace?session=testqueryz")
	if !strings.Contains(got, "frame Main.start https://reports.example/trace?redacted") || strings.Contains(got, "testqueryz") {
		t.Fatal("query-only context/redaction changed")
	}
}

// Independently protected console/error paths retain their existing policy.
func TestProviderEvidenceUserinfoProtectedBranches(t *testing.T) {
	if safeGooglePlayIssueURI("https://testuserq:testpassz@play.google.com/console/issues/synthetic") != "" {
		t.Fatal("console URL admitted userinfo")
	}
	text := providerErrorText(&urlUserinfoSyntheticError{})
	for _, secret := range []string{"testuserq", "testpassz", "reports.example", "testqueryz"} {
		if strings.Contains(text, secret) {
			t.Fatal("provider error retained a synthetic URL component")
		}
	}
	if !strings.Contains(text, "redacted-provider-url") {
		t.Fatal("provider error lost full-URL redaction")
	}
}

// Deliberately synthetic error text stays out of assertion diagnostics.
type urlUserinfoSyntheticError struct{}

func (self *urlUserinfoSyntheticError) Error() string {
	return "failure https://testuserq:testpassz@reports.example/trace?session=testqueryz"
}

// Verify actual public serializers, without printing the synthetic sensitive output.
func requireUserinfoPublicPrivacy(t *testing.T, alert Alert, useful []string) {
	t.Helper()
	var serialized bytes.Buffer
	if err := WriteAlertsJSONL(&serialized, []Alert{alert}); err != nil {
		t.Fatal("public JSONL failed")
	}
	for _, output := range []string{serialized.String(), alert.Markdown()} {
		for _, secret := range []string{"testuserq", "testpassz", "testqueryz"} {
			if strings.Contains(output, secret) {
				t.Fatal("public provider alert retained synthetic userinfo/query")
			}
		}
		for _, want := range useful {
			if !strings.Contains(output, want) {
				t.Fatal("public provider alert lost useful crash context")
			}
		}
	}
}

// Exercise sample JSON validation, shared sanitization and public JSONL/Markdown.
func TestPlayCrashesUserinfoPublicSample(t *testing.T) {
	now := time.Date(2026, 9, 2, 12, 34, 56, 0, time.UTC)
	sampleCalls := 0
	client := newProviderHTTP(providerDoerFunc(func(request *http.Request) (*http.Response, error) {
		var value any
		switch request.URL.Path {
		case "/v1beta1/apps/com.example.app/crashRateMetricSet":
			value = map[string]any{"freshnessInfo": map[string]any{"freshnesses": []any{map[string]any{"aggregationPeriod": "DAILY", "latestEndTime": map[string]any{"year": 2026, "month": 9, "day": 2, "timeZone": map[string]any{"id": "America/Los_Angeles"}}}}}}
		case "/v1beta1/apps/com.example.app/crashRateMetricSet:query":
			value = playMetricResponseFixture("2026-09-01", "")
		case "/v1beta1/apps/com.example.app/errorIssues:search":
			value = map[string]any{"errorIssues": []any{map[string]any{
				"name": "apps/com.example.app/issue-synthetic", "type": "CRASH", "cause": "IllegalStateException", "location": "Main.start", "distinctUsers": "2", "errorReportCount": "3", "lastErrorReportTime": "2026-09-02T10:00:00Z", "lastAppVersion": map[string]any{"versionCode": "103"}, "sampleErrorReports": []string{"apps/com.example.app/report-synthetic"},
			}}}
		case "/v1beta1/apps/com.example.app/errorReports:search":
			sampleCalls++
			value = map[string]any{"errorReports": []any{map[string]any{
				"name": "apps/com.example.app/report-synthetic", "type": "CRASH", "issue": "apps/com.example.app/issue-synthetic", "eventTime": "2026-09-02T10:00:00Z", "appVersion": map[string]any{"versionCode": "103"}, "reportText": "frame Main.start https://testuserq:testpassz@192.0.2.33/trace\nhttps://testuserq:testpassz@reports.example/trace?session=testqueryz",
			}}}
		default:
			t.Error("unexpected synthetic provider request")
			value = map[string]any{}
		}
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(bytes.NewReader(body)), Request: request}, nil
	}))
	signal := newPlayCrashesSignal("https://provider.example", func(context.Context, GooglePlayReportingSettings) (*providerHTTP, error) { return client, nil })
	alerts, err := signal.Run(context.Background(), playCrashTestSettings(now, t.TempDir()))
	if err != nil {
		t.Fatal("synthetic Play signal failed")
	}
	alert := requireAlertClass(t, alerts, "play-crash-issue")
	if sampleCalls != 1 || alert.Frame != "version=103" || !strings.Contains(alert.Observed, "reports_in_48h=3") || !strings.HasSuffix(alert.Target, "/issue-synthetic") {
		t.Fatal("synthetic issue/count/identity/frame changed")
	}
	requireUserinfoPublicPrivacy(t, alert, []string{"sanitized_sample", "Main.start", "192.0.2.33/trace", "reports.example/trace?redacted"})
}

// Apple's TSV labels share the sanitizer and then enter public group metadata.
func TestAppleCrashesUserinfoPublicLabels(t *testing.T) {
	appId := "123456789"
	rows := "Date\tApp Apple Identifier\tApp Version\tDevice\tPlatform Version\tCrashes\tUnique Devices\n" +
		"2026-09-01\t" + appId + "\thttps://testuserq:testpassz@192.0.2.33/release\thttps://testuserq:testpassz@reports.example/device\thttps://testuserq:testpassz@reports.example/os?session=testqueryz\t3\t2\n"
	partitions := map[string]appleCrashPartition{}
	count, err := parseAppleCrashTSV([]byte(rows), appId, "2026-09-02", partitions, 10)
	if err != nil || count != 1 || len(partitions["2026-09-01"].Groups) != 1 {
		t.Fatal("synthetic Apple label fixture failed")
	}
	settings := SignalSettings{Environment: "synthetic", Now: func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) }, AppleReporting: AppleReportingSettings{AppID: appId}}
	for version, group := range partitions["2026-09-01"].Groups {
		if group.Crashes != 3 || group.UniqueDevices != 2 {
			t.Fatal("synthetic Apple counts changed")
		}
		alert := appleGroupAlert(settings, "2026-09-01", version, "", "2026-09-02", appleCrashGroup{}, false, group, true)
		requireUserinfoPublicPrivacy(t, alert, []string{"crashes=3", "192.0.2.33/release", "reports.example/device", "reports.example/os?redacted"})
	}
}
