package monitor

import (
	"context"
	"strings"
	"testing"
)

const syntheticRedisByteSample = "1000 1000 1409966 " +
	"score 73 1318490 " +
	"provide 508 48456 " +
	"client-key 240 24480 " +
	"connect 54 7774 " +
	"stream 123 10442 " +
	"other 2 324"

func TestRedisBytesSignalSyntheticScorePayloadDominance(t *testing.T) {
	sawBoundedReadOnlySample := false
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, "INFO memory"):
			return "6380 12700000000 12884901888\n6381 8000000000 12884901888", nil
		case strings.Contains(command, "EVAL_RO"):
			sawBoundedReadOnlySample = strings.Contains(command, "timeout 45") &&
				strings.Contains(command, "base64 -d") &&
				strings.Contains(command, " 0 1000 20 250")
			return syntheticRedisByteSample, nil
		default:
			t.Fatalf("unexpected Redis byte command: %s", command)
			return "", nil
		}
	}}

	alerts, err := NewRedisBytesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "score-byte-dominance")
	if !sawBoundedReadOnlySample ||
		alert.Target != "redis-1:6380" ||
		!strings.Contains(alert.Symptom, "93.5%") ||
		!strings.Contains(alert.Observed, "score_keys=73") ||
		!strings.Contains(alert.Observed, "score_bytes=1318490") ||
		!strings.Contains(alert.Mechanism, "caller location and target") ||
		!strings.Contains(alert.Action, "one zero-caller baseline") ||
		!strings.Contains(alert.Verify, "five-hour legacy TTL") {
		t.Fatalf("score byte-dominance alert lost bounded attribution or remediation: %+v", alert)
	}
}

func TestRedisBytesSignalSyntheticLowUtilizationIsHealthy(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "INFO memory") {
			return "6380 5000000000 12884901888", nil
		}
		if strings.Contains(command, "EVAL_RO") {
			return syntheticRedisByteSample, nil
		}
		t.Fatalf("unexpected Redis byte command: %s", command)
		return "", nil
	}}

	alerts, err := NewRedisBytesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("low-utilization node produced byte-dominance alert: %+v", alerts)
	}
}
