package monitor

// The native engine executes the exact owning query on independent raw samples.
// Its actual JSON vectors then enter the actual probe, including healthy findings.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Raw points use seconds since the epoch; every identity is synthetic.
type accountMessagesEnginePoint struct {
	Seconds int64   `json:"seconds"`
	Value   float64 `json:"value"`
}

type accountMessagesEngineSeries struct {
	Labels map[string]string            `json:"labels"`
	Points []accountMessagesEnginePoint `json:"points"`
}

type accountMessagesEngineCase struct {
	Name       string                        `json:"name"`
	Evaluation int64                         `json:"evaluation"`
	Series     []accountMessagesEngineSeries `json:"series"`
}

// Only raw observations are supplied; no derived sum or source clock is faked.
func accountMessagesEngineCases() []accountMessagesEngineCase {
	series := func(channel, result, instance string, points ...accountMessagesEnginePoint) accountMessagesEngineSeries {
		return accountMessagesEngineSeries{
			Labels: map[string]string{
				"__name__": "urnetwork_account_message_sends_total", "env": "synthetic",
				"job": "api", "channel": channel, "result": result,
				"host": "synthetic-private-host.example", "block": "synthetic-private-block", "instance": instance,
			},
			Points: points,
		}
	}
	stale := []accountMessagesEnginePoint{{Seconds: 6540, Value: 100}, {Seconds: 6600, Value: 110}}
	fresh := []accountMessagesEnginePoint{{Seconds: 7050, Value: 100}, {Seconds: 7110, Value: 110}}
	return []accountMessagesEngineCase{
		{Name: "stale-success", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", stale...),
			series("sms", "sent", "synthetic-private-sms", stale...),
		}},
		{Name: "fresh-success", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", fresh...),
			series("sms", "sent", "synthetic-private-sms", fresh...),
		}},
		{Name: "stale-failure", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", fresh...),
			series("sms", "failed", "synthetic-private-sms", stale...),
		}},
		{Name: "stale-degraded", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", fresh...),
			series("sms", "sent", "synthetic-private-sms", accountMessagesEnginePoint{Seconds: 6540, Value: 100}, accountMessagesEnginePoint{Seconds: 6600, Value: 130}),
			series("sms", "failed", "synthetic-private-sms", stale...),
		}},
		{Name: "mixed-same-job", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", fresh...),
			series("sms", "sent", "synthetic-private-sms-old", stale...),
			series("sms", "sent", "synthetic-private-sms-fresh", fresh...),
		}},
		{Name: "sparse-channel", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "failed", "synthetic-private-email", fresh...),
			series("sms", "sent", "synthetic-private-sms", accountMessagesEnginePoint{Seconds: 7110, Value: 110}),
		}},
		{Name: "missing-channel", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", fresh...),
		}},
		{Name: "window-left-boundary", Evaluation: 86400, Series: []accountMessagesEngineSeries{
			series("email", "failed", "synthetic-private-email", accountMessagesEnginePoint{Seconds: 0, Value: 100}, accountMessagesEnginePoint{Seconds: 60, Value: 110}),
			series("sms", "sent", "synthetic-private-sms", accountMessagesEnginePoint{Seconds: 86250, Value: 100}, accountMessagesEnginePoint{Seconds: 86310, Value: 110}),
		}},
		{Name: "window-contributor-expired", Evaluation: 86460, Series: []accountMessagesEngineSeries{
			series("email", "failed", "synthetic-private-email", accountMessagesEnginePoint{Seconds: 0, Value: 100}, accountMessagesEnginePoint{Seconds: 60, Value: 110}),
			series("sms", "sent", "synthetic-private-sms", accountMessagesEnginePoint{Seconds: 86250, Value: 100}, accountMessagesEnginePoint{Seconds: 86310, Value: 110}),
		}},
		{Name: "source-inside-boundary", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", fresh...),
			series("sms", "sent", "synthetic-private-sms", accountMessagesEnginePoint{Seconds: 6841, Value: 100}, accountMessagesEnginePoint{Seconds: 6901, Value: 110}),
		}},
		{Name: "source-outside-boundary", Evaluation: 7200, Series: []accountMessagesEngineSeries{
			series("email", "sent", "synthetic-private-email", fresh...),
			series("sms", "sent", "synthetic-private-sms", accountMessagesEnginePoint{Seconds: 6839, Value: 100}, accountMessagesEnginePoint{Seconds: 6899, Value: 110}),
		}},
	}
}

// No native-engine dependency enters Server. A skip is an unmet qualification
// prerequisite, not a passing semantic gate. Sol supplies the pinned checkout.
func TestAccountMessagesPromqlEngine(t *testing.T) {
	engineSource := os.Getenv("MONITOR_PROMQL_ENGINE_SOURCE")
	if engineSource == "" {
		t.Skip("set MONITOR_PROMQL_ENGINE_SOURCE for the required exact-query account-message qualification")
	}
	engineSource, err := filepath.Abs(engineSource)
	if err != nil {
		t.Fatal(err)
	}
	query := accountMessagesQuery("synthetic")
	cases := accountMessagesEngineCases()
	request, err := json.Marshal(struct {
		Query string                      `json:"query"`
		Cases []accountMessagesEngineCase `json:"cases"`
	}{Query: query, Cases: cases})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	requestPath, outputPath := filepath.Join(directory, "request.json"), filepath.Join(directory, "vectors.json")
	if err := os.WriteFile(requestPath, request, 0600); err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile("testdata/account_messages_promql_engine_test.go.txt")
	if err != nil {
		t.Fatal(err)
	}
	engineTest := filepath.Join(directory, "account_messages_promql_engine_test.go")
	if err := os.WriteFile(engineTest, fixture, 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, "go", "test", "-mod=vendor", engineTest, "-run", "^TestAccountMessagesNativeVectors$", "-count=1")
	command.Dir = engineSource
	command.Env = append(os.Environ(), "GOWORK=off", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off",
		"MONITOR_ACCOUNT_ENGINE_REQUEST="+requestPath, "MONITOR_ACCOUNT_ENGINE_OUTPUT="+outputPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("native account-message query qualification failed: %v\n%s", err, output)
	}
	encoded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	vectors := map[string]json.RawMessage{}
	if err := json.Unmarshal(encoded, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors) != len(cases) {
		t.Fatalf("native cases = %d, want %d", len(vectors), len(cases))
	}
	for _, c := range cases {
		payload, exists := vectors[c.Name]
		if !exists {
			t.Fatalf("native case %s is absent", c.Name)
		}
		var response mimirInstantResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			t.Fatal(err)
		}
		if c.Name == "stale-success" || c.Name == "fresh-success" {
			increases := 0
			for _, series := range response.Data.Result {
				part := series.Metric["monitor_account_part"]
				if part != "" && part != "increase" {
					continue
				}
				evaluatedAt, value, err := mimirInstantValue(series.Value)
				if err != nil || evaluatedAt.Unix() != c.Evaluation || value != 20 {
					t.Fatalf("%s: causal setup must derive increase=20 at evaluation time", c.Name)
				}
				increases++
			}
			if increases != 2 {
				t.Fatalf("%s: causal setup needs two derived channel increases", c.Name)
			}
		}
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if !strings.Contains(command, url.QueryEscape(query)) {
				return "", fmt.Errorf("probe did not use the engine-qualified query")
			}
			return string(payload), nil
		}}
		settings := syntheticSettings(source)
		settings.Now = func() time.Time { return time.Unix(c.Evaluation, 0).UTC() }
		settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (accountMessagesProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatalf("%s: probe rejected native vector: %v", c.Name, err)
		}
		unknown, page, degraded := false, false, false
		unknownChannel := "channel=sms"
		if c.Name == "window-left-boundary" || c.Name == "window-contributor-expired" {
			unknownChannel = "channel=email"
		}
		fresh := c.Name == "fresh-success" || c.Name == "source-inside-boundary"
		alerts := Alerts{}
		for _, finding := range findings {
			if finding.healthy {
				if !fresh && (finding.frame == unknownChannel || finding.class == "account-message-sends-unobservable" ||
					c.Name == "stale-success" && finding.frame != "") {
					t.Fatalf("%s: unknown source or range emitted healthy finding %s", c.Name, finding.class)
				}
				continue
			}
			unknown = unknown || finding.class == "account-message-sends-unobservable"
			if finding.class == "account-message-channel-degraded" {
				degraded = true
				if finding.frame != "channel=sms" || !strings.Contains(finding.observed, "source_freshness=unknown") {
					t.Fatalf("%s: historical degraded outcome lost its channel or unknown freshness", c.Name)
				}
			}
			if finding.class == "account-message-channel-down" {
				page = true
				wantFrame := "channel=sms"
				if c.Name == "sparse-channel" {
					wantFrame = "channel=email"
				}
				if finding.frame != wantFrame {
					t.Fatalf("%s: wrong independent historical page frame %s", c.Name, finding.frame)
				}
				if c.Name == "stale-failure" && !strings.Contains(finding.observed, "source_freshness=unknown") {
					t.Fatal("historical failure was attributed to a fresh current source")
				}
			}
			alerts = append(alerts, alertFromFinding(settings, "19.3", "account-messages", "Account message delivery", finding))
		}
		if fresh && len(alerts) != 0 {
			t.Fatalf("%s: fresh healthy control alerted", c.Name)
		}
		if !fresh && !unknown {
			t.Fatalf("%s: historical outcomes hid unknown source or range coverage", c.Name)
		}
		wantPage := c.Name == "stale-failure" || c.Name == "sparse-channel"
		if page != wantPage {
			t.Fatalf("%s: historical failure retained = %t, want %t", c.Name, page, wantPage)
		}
		if degraded != (c.Name == "stale-degraded") {
			t.Fatalf("%s: historical degraded outcome retained = %t", c.Name, degraded)
		}
		serialized, err := json.Marshal(alerts)
		if err != nil {
			t.Fatal(err)
		}
		var markdown strings.Builder
		if err := WriteAlertsMarkdown(&markdown, alerts); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(serialized)+markdown.String(), "synthetic-private-") {
			t.Fatalf("%s: private raw process identity escaped into the alert", c.Name)
		}
	}
}
