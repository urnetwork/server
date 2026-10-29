// Valid stream TTL aggregates carry no writer or cleanup provenance. These
// public controls retain the measured breach without inventing its mechanism.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// All producer data is synthetic and remains behind the existing public API.
func ttlAttributionProvenanceSettings(t *testing.T, average int64, detail string, detailErr error) SignalSettings {
	t.Helper()
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "INFO keyspace") {
			return fmt.Sprintf("6380 10000 9000 %d\n6381 0 0 0\n6382 0 0 0", average), nil
		}
		if strings.Contains(command, "EVAL_RO") {
			return detail, detailErr
		}
		t.Fatal("unexpected synthetic TTL observation")
		return "", nil
	}}
	return syntheticSettings(source)
}

// Read each serialization rather than relying on a private reducer field.
func ttlAttributionProvenancePublic(t *testing.T, alert Alert) []string {
	t.Helper()
	encoded, err := json.Marshal(alert)
	if err != nil {
		t.Fatal("synthetic TTL alert encoding failed")
	}
	var jsonl strings.Builder
	if err := (Alerts{alert}).WriteJSONL(&jsonl); err != nil {
		t.Fatal("synthetic TTL JSONL failed")
	}
	return []string{string(encoded), alert.Markdown(), jsonl.String()}
}

// Moderate and historical-looking stream expiries have the same absent writer
// authority. Even the old fingerprint cannot authenticate an emitting artifact.
func TestTTLAttributionProvenanceValidStreamsStayUnattributed(t *testing.T) {
	for _, detail := range []string{
		"5000 1 1 0 0 0 0 200 200 10368000001 legacy-contracts",
		"5000 1 0 1 0 0 0 200 200 10368000001 legacy-ids",
		"5000 1 0 0 1 0 0 200 200 10368000001 current-contracts",
		"5000 1 0 0 0 1 0 200 200 10368000001 current-ids",
		"5000 10 10 0 0 0 0 2000 300 28799985312000000 legacy-contracts",
		"5000 1 1 0 0 0 0 200 200 9223372036854775807 legacy-contracts",
	} {
		settings := ttlAttributionProvenanceSettings(t, 63072000001, detail, nil)
		alerts, err := NewTTLLeaksSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("valid stream detail lost the independent INFO breach")
		}
		breach := requireAlertClass(t, alerts, "ttl-leaks")
		if breach.Severity != SeverityWarn || breach.Sustain != 1 || breach.Frame != "duration-as-seconds" || breach.Target != "redis-1" || strings.Contains(breach.Observed, "sample_status=unavailable") {
			t.Fatal("provenance correction changed the existing breach or rejected valid detail")
		}
		if !strings.Contains(breach.Observed, "max_avg_ttl_ms=63072000001") || !strings.Contains(breach.Observed, "sample_suspect_bytes=") {
			t.Fatal("numeric TTL and sampled byte facts were discarded")
		}
		for _, output := range ttlAttributionProvenancePublic(t, breach) {
			if !strings.Contains(output, "writer and cleanup history are unverified") {
				t.Error("valid family membership manufactured writer or cleanup authority")
			}
			for _, unsupported := range []string{
				"A pre-fix raw time.Duration reached Redis Lua as nanoseconds",
				"An intermediate cleanup then read PTTL through go-redis",
				"The cleanup artifact is attributable",
				"d8e34003",
				"expire-leaked-ttls",
			} {
				if strings.Contains(output, unsupported) {
					t.Error("public TTL finding asserted a historical cause or selected its artifact without provenance")
				}
			}
		}
	}
}

// Missing or mixed detail cannot erase INFO's independent positive or acquire
// the stream-specific explanation. A current detail failure is never zero.
func TestTTLAttributionProvenanceUnknownMixedPreserveBreach(t *testing.T) {
	for _, test := range []struct {
		detail      string
		cause       error
		unavailable bool
	}{
		{detail: "TTL-PROVENANCE-PRIVATE-CANARY", unavailable: true},
		{detail: "5000 1 1 0 0 0 0 200 200 10368000001 legacy-contracts", cause: errors.New("TTL-PROVENANCE-PRIVATE-CANARY"), unavailable: true},
		{detail: "5000 10 9 0 0 0 1 2000 300 28799985312000000 legacy-contracts"},
		{detail: "5000 1 0 0 0 0 1 200 200 10368000001 other"},
		{detail: "5000 0 0 0 0 0 0 0 0 -1 none"},
	} {
		settings := ttlAttributionProvenanceSettings(t, 63072000001, test.detail, test.cause)
		alerts, err := NewTTLLeaksSignal().Run(context.Background(), settings)
		if err != nil || len(alerts) != 1 {
			t.Fatal("unknown or mixed detail erased the INFO breach")
		}
		breach := requireAlertClass(t, alerts, "ttl-leaks")
		if strings.Contains(breach.Observed, "sample_status=unavailable") != test.unavailable || strings.Contains(breach.Action, "current stream TTL contract") {
			t.Error("unknown or mixed detail acquired stream-only authority or lost unavailability")
		}
		for _, output := range ttlAttributionProvenancePublic(t, breach) {
			if strings.Contains(output, "TTL-PROVENANCE-PRIVATE-CANARY") || strings.Contains(output, "d8e34003") {
				t.Error("private detail or historical artifact entered public output")
			}
		}
	}
}

// INFO's healthy boundary does not require an optional attribution request.
func TestTTLAttributionProvenanceHealthyBoundarySkipsDetail(t *testing.T) {
	for _, average := range []int64{0, 25920000000, 63072000000} {
		detailCalls := 0
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "EVAL_RO") {
				detailCalls++
				return "TTL-PROVENANCE-PRIVATE-CANARY", nil
			}
			return fmt.Sprintf("6380 10000 9000 %d\n6381 0 0 0\n6382 0 0 0", average), nil
		}}
		alerts, err := NewTTLLeaksSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 0 || detailCalls != 0 {
			t.Fatal("healthy annual/exact-threshold control acquired a detail dependency or false breach")
		}
	}
}
