package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestMissingOriginSignalSyntheticFallbackFromNormalBeforeDetailRollout(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 36, 20, 0, time.UTC)
	alerts := runMissingOriginFixture(t, now, 1411.325)
	if len(alerts) != 1 || alerts[0].Frame != "fallback-from-normal" {
		t.Fatalf("alerts=%+v, want one fallback-from-normal alert", alerts)
	}
	fallback := &alerts[0]
	if fallback.SignalNumber != "2.17" || fallback.SignalKey != "missing-origin" || fallback.Sustain != 1 {
		t.Fatalf("wrong signal identity: %+v", *fallback)
	}
	for _, want := range []string{
		"non-companion requests are 1411.3/min",
		"fell back to Stream/companion settlement",
		"original wire bit",
		"active top-level providers",
		"maximum client-window lifetime",
		"Connect commit ec34ce1",
		"Connect commit 55daddb",
		"source_owner=other excludes that singleton but does not identify an application",
		"still-installed older client can reconnect",
		"provider return paths and same-network peers",
		"§2.20 reports zero successful contracts",
		"server commit c8dfe570",
		"do not infer endpoint roles",
		"edit Redis blobs",
		"detail_status=absent",
		"detail_rate_per_minute=unknown",
		"missing detail is unavailable instrumentation rather than a measured zero",
		"predating c8dfe570",
		"No customer, client, network, device, contract, destination, or API-process identifier",
		"bounded metrics-gateway name",
		"SIGNALS.md §2.17 and §5.9",
	} {
		if !strings.Contains(fallback.Markdown(), want) {
			t.Fatalf("fallback alert missing %q:\n%s", want, fallback.Markdown())
		}
	}
}

func TestMissingOriginSignalDocumentationContract(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	sectionStart := strings.Index(catalog, "### 2.17 Missing companion-origin contract rate")
	sectionEnd := strings.Index(catalog, "### 2.18 Stale contract destination rejection")
	if sectionStart < 0 || sectionEnd <= sectionStart {
		t.Fatal("SIGNALS.md does not contain a bounded §2.17 section")
	}
	section := strings.Join(strings.Fields(catalog[sectionStart:sectionEnd]), " ")
	for _, want := range []string{
		"Selected/discovery windows additionally require `ec34ce1`",
		"Provider-return source owners additionally require `55daddb`",
		"`source_owner=egress_prober` selects only the server-owned singleton",
		"`source_owner=other` excludes that singleton but does not identify a product, application, device, or artifact",
		"A complete older-schema cohort is retained as `unattributed`, never folded into `other`",
		"A still-installed older client can reconnect and create another legacy window indefinitely",
		"elapsed time alone is not artifact convergence",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("SIGNALS.md §2.17 missing %q:\n%s", want, section)
		}
	}
}

func TestMissingOriginSignalSyntheticCompleteDetailedCohorts(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 37, 0, 0, time.UTC)
	payload := missingOriginFixtureWithDetailsJSON(t, now, 1411.325, []missingOriginDetailFixture{
		{
			senderRole: "server", sourceOwner: "egress_prober",
			resolution: "stream_fallback", relationship: "network",
			sourceLifecycle: "active_top", destinationLifecycle: "active_derived",
			rate: 1200,
		},
		{
			senderRole: "client", sourceOwner: "other",
			resolution: "stream_fallback", relationship: "public",
			sourceLifecycle: "active_top", destinationLifecycle: "active_top",
			rate: 211.325,
		},
	})
	alerts, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "missing-origin-rate")
	for _, want := range []string{
		"detail_status=complete",
		"detail_series=2",
		"detail_rate_per_minute=1411.325",
		"sender_server_rate_per_minute=1200.000",
		"sender_client_rate_per_minute=211.325",
		"source_owner_egress_prober_rate_per_minute=1200.000",
		"source_owner_other_rate_per_minute=211.325",
		"dominant_sender_role=server",
		"dominant_source_owner=egress_prober",
		"dominant_resolution=stream_fallback",
		"dominant_relationship=network",
		"dominant_source_lifecycle=active_top",
		"dominant_destination_lifecycle=active_derived",
		"dominant_rate_per_minute=1200.000",
		"fixed vocabulary",
		"summed detail rate reconciles",
		"not an endpoint identity",
		"not accepted from the request",
		"source_owner=egress_prober identifies only the server-owned singleton",
		"No metric label proves client artifact adoption",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("complete-detail alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestMissingOriginSignalSyntheticLegacyDetailStaysUnattributed(t *testing.T) {
	now := time.Date(2026, 9, 13, 19, 40, 0, 0, time.UTC)
	payload := missingOriginFixtureWithDetailsJSON(t, now, 900, []missingOriginDetailFixture{{
		omitAttribution: true,
		resolution:      "stream_fallback", relationship: "public",
		sourceLifecycle: "active_top", destinationLifecycle: "active_derived",
		rate: 900,
	}})
	alerts, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "missing-origin-rate").Markdown()
	for _, want := range []string{
		"detail_status=complete",
		"sender_unattributed_rate_per_minute=900.000",
		"source_owner_unattributed_rate_per_minute=900.000",
		"dominant_sender_role=unattributed",
		"dominant_source_owner=unattributed",
		"unattributed is an older API schema rather than a cause",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("legacy-detail alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "dominant_source_owner=other") ||
		strings.Contains(markdown, "dominant_source_owner=egress_prober") {
		t.Fatalf("legacy detail was falsely attributed:\n%s", markdown)
	}
}

func TestMissingOriginSignalSyntheticPartialDetailDoesNotAttribute(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 38, 0, 0, time.UTC)
	payload := missingOriginFixtureWithDetailsJSON(t, now, 1400, []missingOriginDetailFixture{
		{
			resolution: "stream_fallback", relationship: "network",
			sourceLifecycle: "active_top", destinationLifecycle: "active_derived",
			rate: 700,
		},
	})
	alerts, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	markdown := requireAlertClass(t, alerts, "missing-origin-rate").Markdown()
	for _, want := range []string{
		"detail_status=partial",
		"detail_rate_per_minute=700.000",
		"detail_error=detail_rate_below_aggregate",
		"mixed API generations or incomplete metric ingestion",
		"before using any detail cohort for causal attribution",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("partial-detail alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(markdown, "dominant_resolution=") {
		t.Fatalf("partial detail was used for dominant attribution:\n%s", markdown)
	}
}

func TestMissingOriginSignalSyntheticAmbiguousDetailsStayRedacted(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 39, 0, 0, time.UTC)
	secret := "customer-identity-must-not-escape"
	tests := []struct {
		name    string
		details []missingOriginDetailFixture
		want    string
	}{
		{
			name: "unknown label value",
			details: []missingOriginDetailFixture{{
				resolution: "stream_fallback", relationship: secret,
				sourceLifecycle: "active_top", destinationLifecycle: "active_derived", rate: 1400,
			}},
			want: "invalid_detail_labels",
		},
		{
			name: "unexpected label set",
			details: []missingOriginDetailFixture{{
				resolution: "stream_fallback", relationship: "network",
				sourceLifecycle: "active_top", destinationLifecycle: "active_derived", rate: 1400,
				extraLabels: map[string]string{"customer_id": secret},
			}},
			want: "unexpected_detail_label_set",
		},
		{
			name: "half-upgraded attribution schema",
			details: []missingOriginDetailFixture{{
				omitAttribution: true,
				resolution:      "stream_fallback", relationship: "network",
				sourceLifecycle: "active_top", destinationLifecycle: "active_derived", rate: 1400,
				extraLabels: map[string]string{"sender_role": "client"},
			}},
			want: "unexpected_detail_label_set",
		},
		{
			name: "producer cannot emit monitor rollout class",
			details: []missingOriginDetailFixture{{
				senderRole: "client", sourceOwner: "unattributed",
				resolution: "stream_fallback", relationship: "network",
				sourceLifecycle: "active_top", destinationLifecycle: "active_derived", rate: 1400,
			}},
			want: "invalid_detail_labels",
		},
		{
			name: "duplicate cohort",
			details: []missingOriginDetailFixture{
				{
					resolution: "stream_fallback", relationship: "network",
					sourceLifecycle: "active_top", destinationLifecycle: "active_derived", rate: 700,
				},
				{
					resolution: "stream_fallback", relationship: "network",
					sourceLifecycle: "active_top", destinationLifecycle: "active_derived", rate: 700,
				},
			},
			want: "duplicate_detail_series",
		},
		{
			name: "sample time skew",
			details: []missingOriginDetailFixture{{
				resolution: "stream_fallback", relationship: "network",
				sourceLifecycle: "active_top", destinationLifecycle: "active_derived", rate: 1400,
				sampleTime: now.Add(-time.Second),
			}},
			want: "detail_sample_time_skew",
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			payload := missingOriginFixtureWithDetailsJSON(t, now, 1400, testCase.details)
			alerts, err := NewMissingOriginSignal().Run(
				context.Background(),
				missingOriginSyntheticSettings(t, now, payload),
			)
			if err != nil {
				t.Fatal(err)
			}
			markdown := requireAlertClass(t, alerts, "missing-origin-rate").Markdown()
			for _, want := range []string{
				"detail_status=ambiguous",
				"detail_rate_per_minute=unknown",
				"detail_error=" + testCase.want,
				"raw labels or samples are discarded",
				"independently valid aggregate rate remains actionable",
			} {
				if !strings.Contains(markdown, want) {
					t.Fatalf("ambiguous-detail alert missing %q:\n%s", want, markdown)
				}
			}
			if strings.Contains(markdown, secret) {
				t.Fatalf("ambiguous detail leaked protected label data:\n%s", markdown)
			}
		})
	}
}

func TestMissingOriginSignalSyntheticHealthy(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 41, 0, 0, time.UTC)
	if alerts := runMissingOriginFixture(t, now, 499.999); len(alerts) != 0 {
		t.Fatalf("rates below boundary alerted: %+v", alerts)
	}
	if alerts := runMissingOriginFixture(t, now, 500); len(alerts) != 0 {
		t.Fatalf("rate at boundary alerted: %+v", alerts)
	}
}

func TestMissingOriginSignalSyntheticMissingSeriesIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 42, 0, 0, time.UTC)
	payload := missingOriginFixtureJSON(t, now, nil)
	_, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err == nil || !strings.Contains(err.Error(), "returned 0 fallback aggregate series, want 1") {
		t.Fatalf("missing counter series error=%v", err)
	}
}

func TestMissingOriginSignalSyntheticDuplicateSeriesIsUnknown(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 42, 0, 0, time.UTC)
	payload := missingOriginFixtureJSON(t, now, []float64{10, 20})
	_, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err == nil || !strings.Contains(err.Error(), "returned 2 fallback aggregate series, want 1") {
		t.Fatalf("duplicate counter series error=%v", err)
	}
}

func TestMissingOriginSignalSyntheticRejectsStaleAndInvalidRates(t *testing.T) {
	now := time.Date(2026, 9, 2, 15, 43, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		sampleTime time.Time
		falseRate  float64
		want       string
	}{
		{name: "stale", sampleTime: now.Add(-2 * time.Minute), falseRate: 10, want: "stale fallback sample"},
		{name: "negative", sampleTime: now, falseRate: -1, want: "invalid fallback rate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := missingOriginFixtureJSON(t, test.sampleTime, []float64{test.falseRate})
			_, err := NewMissingOriginSignal().Run(
				context.Background(),
				missingOriginSyntheticSettings(t, now, payload),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func runMissingOriginFixture(t testing.TB, now time.Time, rate float64) Alerts {
	t.Helper()
	payload := missingOriginFixtureJSON(t, now, []float64{rate})
	alerts, err := NewMissingOriginSignal().Run(
		context.Background(),
		missingOriginSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func missingOriginSyntheticSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_connect_contract_failures_total") ||
			!strings.Contains(command, "urnetwork_connect_missing_origin_details_total") ||
			!strings.Contains(command, "missing_companion_origin") ||
			!strings.Contains(command, "companion%3D%22false%22") ||
			!strings.Contains(command, "request_companion%3D%22false%22") ||
			!strings.Contains(command, "sum+by+%28sender_role%2Csource_owner%2Cresolution%2Crelationship%2Csource_lifecycle%2Cdestination_lifecycle%29") ||
			!strings.Contains(command, "monitor_metric") ||
			!strings.Contains(command, "%5B5m%5D") ||
			!strings.Contains(command, "%22synthetic%22") {
			return "", fmt.Errorf("unexpected Mimir command on %s: %s", host.Name, command)
		}
		return payload, nil
	}}
	settings := syntheticSettings(source)
	settings.Environment = "synthetic"
	settings.Now = func() time.Time { return now }
	settings.Hosts = append(settings.Hosts, HostSettings{Name: "metrics-1", Roles: []string{"services"}})
	return settings
}

func missingOriginFixtureJSON(t testing.TB, sampleTime time.Time, rates []float64) string {
	t.Helper()
	result := []map[string]any{}
	for _, rate := range rates {
		result = append(result, map[string]any{
			"metric": map[string]string{"monitor_metric": missingOriginAggregateMetric},
			"value":  []any{float64(sampleTime.Unix()), fmt.Sprintf("%.6f", rate)},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

type missingOriginDetailFixture struct {
	senderRole           string
	sourceOwner          string
	omitAttribution      bool
	resolution           string
	relationship         string
	sourceLifecycle      string
	destinationLifecycle string
	rate                 float64
	sampleTime           time.Time
	extraLabels          map[string]string
}

func missingOriginFixtureWithDetailsJSON(
	t testing.TB,
	aggregateSampleTime time.Time,
	aggregateRate float64,
	details []missingOriginDetailFixture,
) string {
	t.Helper()
	result := []map[string]any{{
		"metric": map[string]string{"monitor_metric": missingOriginAggregateMetric},
		"value":  []any{float64(aggregateSampleTime.Unix()), fmt.Sprintf("%.6f", aggregateRate)},
	}}
	for _, detail := range details {
		sampleTime := detail.sampleTime
		if sampleTime.IsZero() {
			sampleTime = aggregateSampleTime
		}
		labels := map[string]string{
			"monitor_metric":        missingOriginDetailMetric,
			"resolution":            detail.resolution,
			"relationship":          detail.relationship,
			"source_lifecycle":      detail.sourceLifecycle,
			"destination_lifecycle": detail.destinationLifecycle,
		}
		if !detail.omitAttribution {
			senderRole := detail.senderRole
			if senderRole == "" {
				senderRole = "client"
			}
			sourceOwner := detail.sourceOwner
			if sourceOwner == "" {
				sourceOwner = "other"
			}
			labels["sender_role"] = senderRole
			labels["source_owner"] = sourceOwner
		}
		for key, value := range detail.extraLabels {
			labels[key] = value
		}
		result = append(result, map[string]any{
			"metric": labels,
			"value":  []any{float64(sampleTime.Unix()), fmt.Sprintf("%.6f", detail.rate)},
		})
	}
	payload, err := json.Marshal(map[string]any{
		"status": "success",
		"data":   map[string]any{"resultType": "vector", "result": result},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}
