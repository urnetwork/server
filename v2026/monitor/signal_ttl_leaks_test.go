package monitor

import (
	"context"
	"strings"
	"testing"
)

func TestTTLLeaksSignalSyntheticDurationAsSecondsResidue(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "INFO keyspace") && strings.Contains(command, "avg=$7") {
			return "6380 6574864 4938147 508792685310180\n6381 6450000 4820000 908000000000000\n6382 100 90 86400000", nil
		}
		if strings.Contains(command, "EVAL_RO") && strings.Contains(command, "base64 -d") && strings.Contains(command, "-p 6381") {
			return "4914\n99\n47\n52\n0\n0\n0\n28799985310940036\nlegacy-contracts", nil
		}
		t.Fatalf("unexpected TTL command: %s", command)
		return "", nil
	}}
	alerts, err := NewTTLLeaksSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "ttl-leaks")
	if len(alerts) != 1 ||
		alert.Target != "redis-1" ||
		!strings.Contains(alert.Symptom, "2 of 3 Redis nodes") ||
		!strings.Contains(alert.Observed, "affected_ports=6380,6381") ||
		!strings.Contains(alert.Observed, "max_avg_ttl_ms=908000000000000") ||
		!strings.Contains(alert.Observed, "sample_legacy_contracts=47") ||
		!strings.Contains(alert.Observed, "sample_legacy_ids=52") ||
		!strings.Contains(alert.Mechanism, "legacy s_sk suffixes") ||
		!strings.Contains(alert.Action, "binary-safe expire-leaked-ttls") {
		t.Fatalf("fleet TTL residue alert lost aggregation, attribution, or remediation: %+v", alert)
	}
}

func TestTTLLeaksSignalSyntheticUnknownFamilyDoesNotPrescribeStreamCleanup(t *testing.T) {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "INFO keyspace") {
			return "6380 10000 9000 508792685310180", nil
		}
		if strings.Contains(command, "EVAL_RO") {
			return "5000 10 0 0 0 0 10 28799985312000000 other", nil
		}
		t.Fatalf("unexpected TTL command: %s", command)
		return "", nil
	}}
	alerts, err := NewTTLLeaksSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "ttl-leaks")
	if !strings.Contains(alert.Observed, "sample_other=10") ||
		strings.Contains(alert.Action, "expire-leaked-ttls") ||
		!strings.Contains(alert.Action, "attribution") {
		t.Fatalf("unknown TTL family received stream-specific remediation: %+v", alert)
	}
}

func TestTTLLeaksSignalSyntheticAnnualEscrowTTLIsHealthy(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) {
		return "6380 1000 900 25920000000", nil // 300 days
	}}
	alerts, err := NewTTLLeaksSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 0 {
		t.Fatalf("intentional annual-balance TTL produced alerts: %+v", alerts)
	}
}
