package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestReliabilityPipelineSignalSyntheticReliabilityBacklog(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "6380\t9\t0.1 20.0 14.2 98\n6381\t9\t0.1 1.0 0.4 98", nil
	}}
	alerts, err := NewReliabilityPipelineSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 2 {
		t.Fatalf("reliability alerts = %d, want cluster backlog plus one slow node: %+v", len(alerts), alerts)
	}
	byTarget := map[string]Alert{}
	for _, alert := range alerts {
		byTarget[alert.Target] = alert
	}
	if alert, ok := byTarget["redis-1"]; !ok || !strings.Contains(alert.Symptom, "pending discovery blocks") {
		t.Fatalf("cluster-scoped backlog alert = %+v", alert)
	}
	if alert, ok := byTarget["redis-1:6380"]; !ok || !strings.Contains(alert.Symptom, "average local latency") {
		t.Fatalf("node-scoped latency alert = %+v", alert)
	}
}

func TestReliabilityPipelineSignalClusterBacklogIsNotDuplicatedPerNode(t *testing.T) {
	var output strings.Builder
	for port := 6380; port < 6380+32; port++ {
		fmt.Fprintf(&output, "%d\t7\t0.1 1.0 0.3 98\n", port)
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if count := strings.Count(command, "SCARD client_reliability_stats_blocks"); count != 1 {
			t.Fatalf("SCARD collection count = %d, want 1; command:\n%s", count, command)
		}
		return output.String(), nil
	}}
	alerts, err := NewReliabilityPipelineSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 {
		t.Fatalf("backlog alerts = %d, want one cluster-scoped alert: %+v", len(alerts), alerts)
	}
	if alerts[0].Class != "reliability-pipeline-degraded" || alerts[0].Target != "redis-1" {
		t.Fatalf("cluster backlog alert = %+v", alerts[0])
	}
	if !strings.Contains(alerts[0].Observed, "blocks=7 nodes_sampled=32") {
		t.Fatalf("cluster backlog observations = %q", alerts[0].Observed)
	}
}

func TestReliabilityPipelineSignalRejectsMovedSlotAsMetric(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "6380\tMOVED 9508 192.0.2.10:6382\t0.145 1.250 0.275 98", nil
	}}
	alerts, err := NewReliabilityPipelineSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	requireAlertClass(t, alerts, "cannot-observe")
	for _, alert := range alerts {
		if alert.Class == "reliability-pipeline-degraded" {
			t.Fatalf("MOVED hash slot became a latency alert: %+v", alert)
		}
	}
}

func TestParseRedisLatencyFormats(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
		want   float64
	}{
		{name: "redis cli 8", output: "0.145 1.250 0.275 98", want: 0.275},
		{name: "legacy labelled", output: "min: 0, max: 2, avg: 0.31 (98 samples)", want: 0.31},
		{name: "carriage return updates", output: "0.1 2.0 0.5 20\r0.1 1.0 0.2 98\r", want: 0.2},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseRedisLatency(test.output)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("latency = %v, want %v", got, test.want)
			}
		})
	}
}
