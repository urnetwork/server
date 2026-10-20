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
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, "INFO memory"):
				return "6380 12700000000 12884901888\n6381 8000000000 12884901888\n6382 7000000000 12884901888", nil
			case strings.Contains(command, "EVAL_RO"):
				sawBoundedReadOnlySample = strings.Contains(command, "timeout 45") &&
					strings.Contains(command, "base64 -d") &&
					strings.Contains(command, " 0 1000 20 250")
				return syntheticRedisByteSample, nil
			default:
				t.Fatalf("unexpected Redis byte command: %s", command)
				return "", nil
			}
		},
		redisFn: func(_ HostSettings, port int, args ...string) (string, error) {
			if port != 6379 || strings.Join(args, " ") != "-c --json GET client_score_alias_v1_ready" {
				t.Fatalf("unexpected score-alias marker read on %d: %v", port, args)
			}
			return "null", nil
		},
	}

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
		!strings.Contains(alert.Observed, "alias_schema_ready=false") ||
		!strings.Contains(alert.Mechanism, "caller location and target") ||
		!strings.Contains(alert.Action, "one zero-caller baseline") ||
		!strings.Contains(alert.Verify, "five-hour legacy TTL") {
		t.Fatalf("score byte-dominance alert lost bounded attribution or remediation: %+v", alert)
	}
}

func TestRedisBytesSignalReadyAliasSchemaRequiresWriterAndExpiryAuthority(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, "INFO memory"):
				return "6380 12700000000 12884901888\n6381 8000000000 12884901888\n6382 7000000000 12884901888", nil
			case strings.Contains(command, "EVAL_RO"):
				return syntheticRedisByteSample, nil
			default:
				t.Fatalf("unexpected Redis byte command: %s", command)
				return "", nil
			}
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Join(args, " ") != "-c --json GET client_score_alias_v1_ready" {
				t.Fatalf("unexpected score-alias marker read: %v", args)
			}
			return `"1"`, nil
		},
	}

	alerts, err := NewRedisBytesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "score-byte-dominance")
	markdown := alert.Markdown()
	for _, want := range []string{
		"alias_schema_ready=true",
		"some writer previously completed a compatibility export",
		"alias_publication_time=unknown score_exporter_capability=unverified",
		"If the alias-aware writer remains active and publication is less than five hours old",
		"A retained marker can outlive a writer rollback",
		"client score alias schema ready",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("ready-alias drain alert missing %q:\n%s", want, markdown)
		}
	}
	if strings.Contains(alert.Markdown(), "software fix is already active") ||
		strings.Contains(alert.Markdown(), "continuing dominance now measures legacy duplicate payloads") ||
		strings.Contains(alert.Action, "Deploy the alias-aware score cache") {
		t.Fatalf("ready alias schema prescribed an already-deployed fix: %s", alert.Action)
	}
}

func TestRedisBytesSignalSyntheticLowUtilizationIsHealthy(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "INFO memory") {
			return "6380 5000000000 12884901888\n6381 4000000000 12884901888\n6382 3000000000 12884901888", nil
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
