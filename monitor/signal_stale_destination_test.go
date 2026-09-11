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

func TestStaleDestinationSignalSyntheticLifecycleRejection(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 30, 0, 0, time.UTC)
	alerts := runStaleDestinationFixture(t, now, now, 480.125, 21.5)
	if len(alerts) != 1 || alerts[0].Frame != "lifecycle-rejection" {
		t.Fatalf("alerts=%+v, want one lifecycle-rejection alert", alerts)
	}
	alert := alerts[0]
	if alert.SignalNumber != "2.18" || alert.SignalKey != "stale-destination" || alert.Sustain != 1 {
		t.Fatalf("wrong signal identity: %+v", alert)
	}
	for _, want := range []string{
		"inactive contract destinations at 501.6/min",
		"companion_false_rate=480.125",
		"companion_true_rate=21.500",
		"stale Redis provide advertisement",
		"server commit c8dfe570",
		"Connect commit 5b33c91",
		"Connect commit ec34ce1",
		"Connect commit 55daddb",
		"Connect commit f8b1b60",
		"does not prove either retirement fix",
		"still-installed older client can reconnect",
		"retires only the emitting window channel",
		"not a hardware-capacity signal",
		"detail_status=absent",
		"detail_rate_per_minute=unknown",
		"unavailable capability evidence rather than a measured zero",
		"No customer, client, network, device, contract, destination, artifact version, or API-process identifier",
		"SIGNALS.md §2.18 and §5.9",
	} {
		if !strings.Contains(alert.Markdown(), want) {
			t.Fatalf("stale-destination alert missing %q:\n%s", want, alert.Markdown())
		}
	}
}

func TestStaleDestinationSignalSyntheticCompleteJointSenderCohorts(t *testing.T) {
	now := time.Date(2026, 9, 4, 20, 10, 0, 0, time.UTC)
	details := []staleDestinationDetailFixture{
		{
			requestCompanion: "true", senderRole: "server", resolution: "requested_companion",
			relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
			rate: 450,
		},
		{
			requestCompanion: "false", senderRole: "client", resolution: "rejected",
			relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
			rate: 40,
		},
		{
			requestCompanion: "true", senderRole: "absent", resolution: "requested_companion",
			relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
			rate: 10,
		},
		{
			requestCompanion: "true", senderRole: "unknown", resolution: "requested_companion",
			relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
			rate: 1.625,
		},
	}
	payload := staleDestinationFixtureWithDetailsJSON(t, now, []staleDestinationFixtureRate{
		{companion: "false", rate: 480.125},
		{companion: "true", rate: 21.5},
	}, details)
	alerts, err := NewStaleDestinationSignal().Run(
		context.Background(),
		staleDestinationSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%+v, want one lifecycle-rejection alert", alerts)
	}
	markdown := alerts[0].Markdown()
	for _, want := range []string{
		"detail_status=complete",
		"detail_series=4",
		"detail_rate_per_minute=501.625",
		"sender_client_rate_per_minute=40.000",
		"sender_server_rate_per_minute=450.000",
		"sender_absent_rate_per_minute=10.000",
		"sender_unknown_rate_per_minute=1.625",
		"dominant_request_companion=true",
		"dominant_sender_role=server",
		"dominant_resolution=requested_companion",
		"dominant_relationship=public",
		"dominant_source_lifecycle=active_top",
		"dominant_destination_lifecycle=inactive_derived",
		"presence of the additive capability",
		"absent is unavailable capability",
		"unknown is an explicit malformed or future value",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("complete detail alert missing %q:\n%s", want, markdown)
		}
	}
}

func TestStaleDestinationSignalSyntheticPartialDetailCannotAttribute(t *testing.T) {
	now := time.Date(2026, 9, 4, 20, 11, 0, 0, time.UTC)
	payload := staleDestinationFixtureWithDetailsJSON(t, now, []staleDestinationFixtureRate{
		{companion: "false", rate: 500},
		{companion: "true", rate: 20},
	}, []staleDestinationDetailFixture{{
		requestCompanion: "true", senderRole: "server", resolution: "requested_companion",
		relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
		rate: 400,
	}})
	alerts, err := NewStaleDestinationSignal().Run(
		context.Background(),
		staleDestinationSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("alerts=%+v, want one lifecycle-rejection alert", alerts)
	}
	markdown := alerts[0].Markdown()
	for _, want := range []string{
		"detail_status=partial",
		"detail_rate_per_minute=400.000",
		"detail_error=detail_rate_below_aggregate",
		"must not be read as zero",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("partial detail alert missing %q:\n%s", want, markdown)
		}
	}
	for _, forbidden := range []string{"dominant_sender_role=", "dominant_relationship="} {
		if strings.Contains(markdown, forbidden) {
			t.Fatalf("partial detail disclosed attribution %q:\n%s", forbidden, markdown)
		}
	}
}

func TestStaleDestinationSignalSyntheticDetailToleranceEdges(t *testing.T) {
	now := time.Date(2026, 9, 4, 20, 12, 0, 0, time.UTC)
	for _, test := range []struct {
		name       string
		detailRate float64
		wantStatus string
	}{
		{name: "inside two percent", detailRate: 510.4, wantStatus: "complete"},
		{name: "outside two percent below", detailRate: 509.5, wantStatus: "partial"},
		{name: "outside two percent above", detailRate: 530.5, wantStatus: "ambiguous"},
	} {
		payload := staleDestinationFixtureWithDetailsJSON(t, now, []staleDestinationFixtureRate{
			{companion: "false", rate: 500},
			{companion: "true", rate: 20},
		}, []staleDestinationDetailFixture{{
			requestCompanion: "true", senderRole: "server", resolution: "requested_companion",
			relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
			rate: test.detailRate,
		}})
		alerts, err := NewStaleDestinationSignal().Run(
			context.Background(),
			staleDestinationSyntheticSettings(t, now, payload),
		)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "detail_status="+test.wantStatus) {
			t.Fatalf("%s: alerts=%+v, want detail_status=%s", test.name, alerts, test.wantStatus)
		}
	}
}

func TestStaleDestinationSignalSyntheticDetailAmbiguityIsRedacted(t *testing.T) {
	now := time.Date(2026, 9, 4, 20, 13, 0, 0, time.UTC)
	secret := "customer-01J-private-role"
	tests := []struct {
		name    string
		details []staleDestinationDetailFixture
		reason  string
	}{
		{
			name: "invalid fixed label",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: secret, resolution: "rejected",
				relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
				rate: 520,
			}},
			reason: "invalid_detail_labels",
		},
		{
			name: "invalid request companion",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "maybe", senderRole: "client", resolution: "rejected",
				relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
				rate: 520,
			}},
			reason: "invalid_detail_labels",
		},
		{
			name: "invalid resolution",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: "client", resolution: "customer-route",
				relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
				rate: 520,
			}},
			reason: "invalid_detail_labels",
		},
		{
			name: "invalid relationship",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: "client", resolution: "rejected",
				relationship: "private-customer", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
				rate: 520,
			}},
			reason: "invalid_detail_labels",
		},
		{
			name: "invalid source lifecycle",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: "client", resolution: "rejected",
				relationship: "public", sourceLifecycle: "customer-source", destinationLifecycle: "inactive_derived",
				rate: 520,
			}},
			reason: "invalid_detail_labels",
		},
		{
			name: "invalid destination lifecycle",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: "client", resolution: "rejected",
				relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "customer-destination",
				rate: 520,
			}},
			reason: "invalid_detail_labels",
		},
		{
			name: "unexpected extra label",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: "client", resolution: "rejected",
				relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
				rate: 520, extraLabel: secret,
			}},
			reason: "unexpected_detail_label_set",
		},
		{
			name: "duplicate cohort",
			details: []staleDestinationDetailFixture{
				{
					requestCompanion: "false", senderRole: "client", resolution: "rejected",
					relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
					rate: 260,
				},
				{
					requestCompanion: "false", senderRole: "client", resolution: "rejected",
					relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
					rate: 260,
				},
			},
			reason: "duplicate_detail_series",
		},
		{
			name: "negative detail rate",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: "client", resolution: "rejected",
				relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
				rate: -1,
			}},
			reason: "invalid_detail_rate",
		},
		{
			name: "malformed detail sample",
			details: []staleDestinationDetailFixture{{
				requestCompanion: "false", senderRole: "client", resolution: "rejected",
				relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
				rawValue: []any{"malformed"},
			}},
			reason: "malformed_detail_sample",
		},
	}
	for _, test := range tests {
		payload := staleDestinationFixtureWithDetailsJSON(t, now, []staleDestinationFixtureRate{
			{companion: "false", rate: 500},
			{companion: "true", rate: 20},
		}, test.details)
		alerts, err := NewStaleDestinationSignal().Run(
			context.Background(),
			staleDestinationSyntheticSettings(t, now, payload),
		)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "detail_error="+test.reason) {
			t.Fatalf("%s: alerts=%+v, want %s", test.name, alerts, test.reason)
		}
		markdown := alerts[0].Markdown()
		if strings.Contains(markdown, secret) || strings.Contains(markdown, "dominant_sender_role=") {
			t.Fatalf("%s: ambiguous detail escaped redaction/attribution:\n%s", test.name, markdown)
		}
	}
}

func TestStaleDestinationSignalSyntheticRejectsStaleAndSkewedDetail(t *testing.T) {
	now := time.Date(2026, 9, 4, 20, 14, 0, 0, time.UTC)
	tests := []struct {
		name       string
		sampleTime time.Time
		reason     string
	}{
		{name: "stale", sampleTime: now.Add(-2 * time.Minute), reason: "stale_detail_sample"},
		{name: "timestamp mismatch", sampleTime: now.Add(-time.Second), reason: "detail_sample_time_skew"},
	}
	for _, test := range tests {
		payload := staleDestinationFixtureWithDetailsJSON(t, now, []staleDestinationFixtureRate{
			{companion: "false", rate: 500},
			{companion: "true", rate: 20},
		}, []staleDestinationDetailFixture{{
			requestCompanion: "false", senderRole: "client", resolution: "rejected",
			relationship: "public", sourceLifecycle: "active_top", destinationLifecycle: "inactive_derived",
			rate: 520, sampleTime: test.sampleTime,
		}})
		alerts, err := NewStaleDestinationSignal().Run(
			context.Background(),
			staleDestinationSyntheticSettings(t, now, payload),
		)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		if len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "detail_error="+test.reason) {
			t.Fatalf("%s: alerts=%+v, want %s", test.name, alerts, test.reason)
		}
	}
}

func TestStaleDestinationSignalDocumentationContract(t *testing.T) {
	catalogBytes, err := os.ReadFile("SIGNALS.md")
	if err != nil {
		t.Fatal(err)
	}
	catalog := string(catalogBytes)
	sectionStart := strings.Index(catalog, "### 2.18 Stale contract destination rejection")
	sectionEnd := strings.Index(catalog, "### 2.19 Provider egress probe coverage")
	if sectionStart < 0 || sectionEnd <= sectionStart {
		t.Fatal("SIGNALS.md does not contain a bounded §2.18 section")
	}
	section := strings.Join(strings.Fields(catalog[sectionStart:sectionEnd]), " ")
	for _, want := range []string{
		"urnetwork_connect_inactive_destination_details_total",
		"request_companion,sender_role,resolution,relationship, source_lifecycle,destination_lifecycle",
		"Connect producer in `f8b1b60`",
		"proves only that reported sequence lane",
		"Selected/discovery-window requesters also require Connect commit `ec34ce1`",
		"Provider-return source owners also require Connect commit `55daddb`",
		"it does not prove `ec34ce1`, `55daddb`, an application, or an artifact version",
		"a still-installed older client can reconnect and create another legacy window indefinitely",
		"`absent` means the wire field was absent and must not be called an old client",
		"A deficit is partial rollout or ingestion, not a zero",
		"Partial or ambiguous detail never renders a dominant cohort",
		"deployed `f860da1d` predates `f8b1b60`",
	} {
		if !strings.Contains(section, want) {
			t.Fatalf("SIGNALS.md §2.18 missing %q:\n%s", want, section)
		}
	}
}

func TestStaleDestinationSignalSyntheticHealthyBoundary(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 31, 0, 0, time.UTC)
	if alerts := runStaleDestinationFixture(t, now, now, 49.5, 0.5); len(alerts) != 0 {
		t.Fatalf("boundary rate alerted: %+v", alerts)
	}
}

func TestStaleDestinationSignalSyntheticReportsMissingInitializedPartitions(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 32, 0, 0, time.UTC)
	for _, test := range []struct {
		name    string
		rates   []staleDestinationFixtureRate
		missing string
		present string
	}{
		{name: "partial", rates: []staleDestinationFixtureRate{{"false", 0}}, missing: "true", present: "false"},
		{name: "empty", missing: "false,true", present: "none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := staleDestinationFixtureJSON(t, now, test.rates)
			alerts, err := NewStaleDestinationSignal().Run(
				context.Background(),
				staleDestinationSyntheticSettings(t, now, payload),
			)
			if err != nil {
				t.Fatal(err)
			}
			if len(alerts) != 1 {
				t.Fatalf("alerts=%+v, want one instrumentation alert", alerts)
			}
			alert := alerts[0]
			if alert.Class != "stale-destination-instrumentation" ||
				alert.Target != "api-fleet" || alert.Frame != "initialized-partitions" || alert.Sustain != 2 {
				t.Fatalf("wrong instrumentation identity: %+v", alert)
			}
			for _, want := range []string{
				"Mimir query completed",
				"missing_companion_partitions=" + test.missing,
				"present_companion_partitions=" + test.present,
				"server commit c8dfe570",
				"one complete five-minute rate window",
				"UNKNOWN, not a healthy zero",
				"No client, network, contract, or destination identifier",
				"SIGNALS.md §2.18 and §8.12",
			} {
				if !strings.Contains(alert.Markdown(), want) {
					t.Fatalf("instrumentation alert missing %q:\n%s", want, alert.Markdown())
				}
			}
			if strings.Contains(alert.Action, "Restore access") {
				t.Fatalf("instrumentation alert retained generic access guidance: %s", alert.Action)
			}
		})
	}
}

func TestStaleDestinationSignalSyntheticRejectsDuplicateAndUnknownPartitions(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 33, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		rates []staleDestinationFixtureRate
		want  string
	}{
		{
			name:  "duplicate",
			rates: []staleDestinationFixtureRate{{"false", 1}, {"false", 2}, {"true", 0}},
			want:  `duplicate companion partition "false"`,
		},
		{
			name:  "unknown",
			rates: []staleDestinationFixtureRate{{"false", 0}, {"unknown", 0}},
			want:  `unexpected companion partition "unknown"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := staleDestinationFixtureJSON(t, now, test.rates)
			_, err := NewStaleDestinationSignal().Run(
				context.Background(),
				staleDestinationSyntheticSettings(t, now, payload),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestStaleDestinationSignalSyntheticRejectsStaleInvalidAndSkewedSamples(t *testing.T) {
	now := time.Date(2026, 9, 2, 18, 34, 0, 0, time.UTC)
	for _, test := range []struct {
		name      string
		falseTime time.Time
		trueTime  time.Time
		falseRate float64
		trueRate  float64
		want      string
	}{
		{name: "stale", falseTime: now.Add(-2 * time.Minute), trueTime: now, want: "stale companion=false sample"},
		{name: "negative", falseTime: now, trueTime: now, falseRate: -1, want: "invalid companion=false rate"},
		{name: "skewed", falseTime: now, trueTime: now.Add(-time.Second), want: "partition sample times differ"},
	} {
		t.Run(test.name, func(t *testing.T) {
			payload := staleDestinationFixtureJSONTimes(t, []staleDestinationFixtureSample{
				{companion: "false", sampleTime: test.falseTime, rate: test.falseRate},
				{companion: "true", sampleTime: test.trueTime, rate: test.trueRate},
			})
			_, err := NewStaleDestinationSignal().Run(
				context.Background(),
				staleDestinationSyntheticSettings(t, now, payload),
			)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want %q", err, test.want)
			}
		})
	}
}

func runStaleDestinationFixture(
	t testing.TB,
	now time.Time,
	sampleTime time.Time,
	falseRate float64,
	trueRate float64,
) Alerts {
	t.Helper()
	payload := staleDestinationFixtureJSON(t, sampleTime, []staleDestinationFixtureRate{
		{"false", falseRate},
		{"true", trueRate},
	})
	alerts, err := NewStaleDestinationSignal().Run(
		context.Background(),
		staleDestinationSyntheticSettings(t, now, payload),
	)
	if err != nil {
		t.Fatal(err)
	}
	return alerts
}

func staleDestinationSyntheticSettings(t testing.TB, now time.Time, payload string) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(host HostSettings, command string) (string, error) {
		if host.Name != "metrics-1" ||
			!strings.Contains(command, "urnetwork_connect_contract_failures_total") ||
			!strings.Contains(command, "urnetwork_connect_inactive_destination_details_total") ||
			!strings.Contains(command, "inactive_destination") ||
			!strings.Contains(command, "sum+by+%28companion%29") ||
			!strings.Contains(command, "sender_role") ||
			!strings.Contains(command, "source_lifecycle") ||
			!strings.Contains(command, "destination_lifecycle") ||
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

type staleDestinationFixtureRate struct {
	companion string
	rate      float64
}

type staleDestinationFixtureSample struct {
	companion  string
	sampleTime time.Time
	rate       float64
}

type staleDestinationDetailFixture struct {
	requestCompanion     string
	senderRole           string
	resolution           string
	relationship         string
	sourceLifecycle      string
	destinationLifecycle string
	rate                 float64
	sampleTime           time.Time
	extraLabel           string
	rawValue             []any
}

func staleDestinationFixtureJSON(t testing.TB, sampleTime time.Time, rates []staleDestinationFixtureRate) string {
	t.Helper()
	samples := make([]staleDestinationFixtureSample, 0, len(rates))
	for _, rate := range rates {
		samples = append(samples, staleDestinationFixtureSample{
			companion:  rate.companion,
			sampleTime: sampleTime,
			rate:       rate.rate,
		})
	}
	return staleDestinationFixtureJSONTimes(t, samples)
}

func staleDestinationFixtureJSONTimes(t testing.TB, samples []staleDestinationFixtureSample) string {
	t.Helper()
	result := []map[string]any{}
	for _, sample := range samples {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"monitor_metric": staleDestinationAggregateMetric,
				"companion":      sample.companion,
			},
			"value": []any{float64(sample.sampleTime.Unix()), fmt.Sprintf("%.6f", sample.rate)},
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

func staleDestinationFixtureWithDetailsJSON(
	t testing.TB,
	aggregateSampleTime time.Time,
	rates []staleDestinationFixtureRate,
	details []staleDestinationDetailFixture,
) string {
	t.Helper()
	result := []map[string]any{}
	for _, rate := range rates {
		result = append(result, map[string]any{
			"metric": map[string]string{
				"monitor_metric": staleDestinationAggregateMetric,
				"companion":      rate.companion,
			},
			"value": []any{float64(aggregateSampleTime.Unix()), fmt.Sprintf("%.6f", rate.rate)},
		})
	}
	for _, detail := range details {
		sampleTime := detail.sampleTime
		if sampleTime.IsZero() {
			sampleTime = aggregateSampleTime
		}
		metric := map[string]string{
			"monitor_metric":        staleDestinationDetailMetric,
			"request_companion":     detail.requestCompanion,
			"sender_role":           detail.senderRole,
			"resolution":            detail.resolution,
			"relationship":          detail.relationship,
			"source_lifecycle":      detail.sourceLifecycle,
			"destination_lifecycle": detail.destinationLifecycle,
		}
		if detail.extraLabel != "" {
			metric["customer_id"] = detail.extraLabel
		}
		value := []any{float64(sampleTime.Unix()), fmt.Sprintf("%.6f", detail.rate)}
		if detail.rawValue != nil {
			value = detail.rawValue
		}
		result = append(result, map[string]any{
			"metric": metric,
			"value":  value,
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
