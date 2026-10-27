// Keep a proven TTL breach independent from unavailable or private detail.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// Supply a complete current INFO breach and one independently controlled detail
// response through the production source adapter. No native service is used.
func redisTtlAttributionAuthoritySettings(reply string, sourceErr error) SignalSettings {
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "EVAL_RO") {
			return reply, sourceErr
		}
		return "6380 100 90 908000000000000\n6381 0 0 0\n6382 0 0 0", nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts[1].Name = "redis-keyspace.example"
	return settings
}

// Invalid detail cannot erase the primary breach or select a stream cleanup.
func redisTtlAttributionAuthorityUnavailable(t *testing.T, settings SignalSettings) Alert {
	t.Helper()
	alerts, err := NewTTLLeaksSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal("TTL detail failure discarded an independently valid INFO breach")
	}
	redisKeyspaceAuthorityPrivate(t, alerts)
	breach := requireAlertClass(t, alerts, "ttl-leaks")
	if len(alerts) != 1 || breach.Identity() != "redis/ttl-leaks|ttl-leaks|redis-keyspace.example|duration-as-seconds" ||
		breach.Severity != SeverityWarn || breach.Sustain != 1 || !strings.Contains(breach.Observed, "sample_status=unavailable") {
		t.Fatal("TTL attribution public authority assertion: malformed detail was treated as valid attribution")
	}
	for _, forbidden := range []string{"sample_legacy_contracts=", "sample_max_family=", "pre-fix raw time.Duration", "expire-leaked-ttls", "d8e34003", "8 hours"} {
		if strings.Contains(breach.Markdown(), forbidden) {
			t.Fatal("TTL attribution public authority assertion: unavailable detail acquired a stream-specific cause or cleanup")
		}
	}
	return breach
}

// A malformed other-family count cannot silently become zero and prove that
// the entire suspect cohort belongs to streams.
func TestRedisTtlAttributionAuthorityPublicMalformedNumbers(t *testing.T) {
	settings := redisTtlAttributionAuthoritySettings(
		"5000 10 10 0 0 0 synthetic-private-sentinel 2000 300 28799985312000000 legacy-contracts", nil,
	)
	redisTtlAttributionAuthorityUnavailable(t, settings)
}

// Unrecognized native labels must never enter public JSON or Markdown.
func TestRedisTtlAttributionAuthorityPublicUnknownFamilyPrivacy(t *testing.T) {
	settings := redisTtlAttributionAuthoritySettings(
		"5000 10 0 0 0 0 10 2000 300 28799985312000000 synthetic-private-sentinel", nil,
	)
	redisTtlAttributionAuthorityUnavailable(t, settings)
}

// A transport error with plausible partial stdout keeps only fixed public
// unavailability text; the original raw error must not enter Context.
func TestRedisTtlAttributionAuthorityPublicTransportPrivacy(t *testing.T) {
	settings := redisTtlAttributionAuthoritySettings(
		"5000 10 10 0 0 0 0 2000 300 28799985312000000 legacy-contracts", errors.New("synthetic-private-sentinel"),
	)
	redisTtlAttributionAuthorityUnavailable(t, settings)
}

// Every number, conservation boundary and family maximum is validated by the
// actual sample function before its result reaches the public finding.
func TestRedisTtlAttributionAuthorityExactAggregate(t *testing.T) {
	valid := "5000 10 10 0 0 0 0 2000 300 28799985312000000 legacy-contracts"
	cases := []string{
		"", "5000 10", valid + " extra",
		"0 0 0 0 0 0 0 0 0 -1 none",
		"5 10 10 0 0 0 0 2000 300 28799985312000000 legacy-contracts",
		"5000 10 9 0 0 0 0 2000 300 28799985312000000 legacy-contracts",
		"5000 10 11 0 0 0 0 2000 300 28799985312000000 legacy-contracts",
		"9223372036854775807 9223372036854775807 9223372036854775807 1 0 0 0 0 0 28799985312000000 legacy-contracts",
		"5000 10 10 0 0 0 0 200 300 28799985312000000 legacy-contracts",
		"5000 10 10 0 0 0 0 2000 0 28799985312000000 legacy-contracts",
		"5000 1 1 0 0 0 0 2000 300 28799985312000000 legacy-contracts",
		"5000 0 0 0 0 0 0 2000 300 -1 none",
		"5000 0 0 0 0 0 0 0 0 28799985312000000 other",
		"5000 10 10 0 0 0 0 2000 300 10368000000 legacy-contracts",
		"5000 10 10 0 0 0 0 2000 300 28799985312000000 other",
		"5000 10 10 0 0 0 0 2000 300 28799985312000000 none",
		"5000 0 0 0 0 0 0 0 0 -1 other",
		"5000 0 0 0 0 0 0 0 0 -2 none",
	}
	for index := 0; index < 10; index++ {
		for _, malformed := range []string{"synthetic-private-sentinel", "9223372036854775808", "1.5", "1e3"} {
			fields := strings.Fields(valid)
			fields[index] = malformed
			cases = append(cases, strings.Join(fields, " "))
		}
		if index < 9 {
			fields := strings.Fields(valid)
			fields[index] = "-1"
			cases = append(cases, strings.Join(fields, " "))
		}
	}
	for index, reply := range cases {
		settings := redisTtlAttributionAuthoritySettings(reply, nil)
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		_, err = sampleRedisTTLAttribution(context.Background(), env, env.cfg.redisEntryHost(), 6380)
		if err == nil {
			t.Fatalf("TTL attribution parser authority assertion: invalid case %d acquired family authority", index)
		}
		if strings.Contains(err.Error(), "synthetic-private-sentinel") {
			t.Fatal("TTL attribution parser privacy assertion: raw input entered its error")
		}
		redisTtlAttributionAuthorityUnavailable(t, settings)
	}
}

// Empty suspect cohorts and missing MEMORY USAGE on an observed suspect are
// valid. Keep multi-century PTTL as int64 milliseconds without duration casts.
func TestRedisTtlAttributionAuthorityValidZeroAndFamilies(t *testing.T) {
	for _, test := range []struct {
		name, reply string
		stream      bool
	}{
		{name: "no expiries", reply: "5000 0 0 0 0 0 0 0 0 -1 none"},
		{name: "no sampled suspect at threshold", reply: "5000 0 0 0 0 0 0 0 0 10368000000 other"},
		{name: "suspect vanished before memory read", reply: "5000 1 0 0 0 0 1 0 0 10368000001 other"},
		{name: "raw int64 maximum", reply: "5000 10 0 0 0 0 10 2000 300 9223372036854775807 other"},
		{name: "legacy contracts", reply: "5000 10 10 0 0 0 0 2000 300 28799985312000000 legacy-contracts", stream: true},
		{name: "legacy ids", reply: "5000 10 0 10 0 0 0 2000 300 28799985312000000 legacy-ids", stream: true},
		{name: "current contracts", reply: "5000 10 0 0 10 0 0 2000 300 28799985312000000 current-contracts", stream: true},
		{name: "current ids", reply: "5000 10 0 0 0 10 0 2000 300 28799985312000000 current-ids", stream: true},
		{name: "mixed families", reply: "5000 10 9 0 0 0 1 2000 300 28799985312000000 legacy-contracts"},
	} {
		settings := redisTtlAttributionAuthoritySettings(test.reply, nil)
		env, err := newProbeEnv(settings.withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		attribution, err := sampleRedisTTLAttribution(context.Background(), env, env.cfg.redisEntryHost(), 6380)
		if err != nil || attribution.seen != 5000 {
			t.Fatalf("TTL attribution healthy control assertion: %s rejected a valid detail aggregate", test.name)
		}
		alerts, err := NewTTLLeaksSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatal(err)
		}
		redisKeyspaceAuthorityPrivate(t, alerts)
		breach := requireAlertClass(t, alerts, "ttl-leaks")
		if len(alerts) != 1 || strings.Contains(breach.Observed, "sample_status=unavailable") ||
			!strings.Contains(breach.Observed, fmt.Sprintf("sample_max_pttl_ms=%d", attribution.maxPTTLMillis)) ||
			strings.Contains(breach.Action, "expire-leaked-ttls") != test.stream || strings.Contains(breach.Verify, "d8e34003") != test.stream {
			t.Fatalf("TTL attribution healthy control assertion: %s lost its independent breach or selected a wrong cleanup", test.name)
		}
	}
}

// An older valid stream sample is not attribution authority for a current
// failed sample, and a later complete other-family sample replaces it.
func TestRedisTtlAttributionAuthorityCurrentDetailReplacesPrior(t *testing.T) {
	reply := "5000 10 10 0 0 0 0 2000 300 28799985312000000 legacy-contracts"
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		if strings.Contains(command, "EVAL_RO") {
			return reply, nil
		}
		return "6380 100 90 908000000000000\n6381 0 0 0\n6382 0 0 0", nil
	}}
	settings := syntheticSettings(source)
	settings.Hosts[1].Name = "redis-keyspace.example"
	signal := NewTTLLeaksSignal()
	alerts, err := signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || !strings.Contains(alerts[0].Action, "expire-leaked-ttls") {
		t.Fatal("detail freshness control did not begin with valid stream attribution")
	}
	reply = "5000 10 10 0 0 0 synthetic-private-sentinel 2000 300 28799985312000000 legacy-contracts"
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || !strings.Contains(alerts[0].Observed, "sample_status=unavailable") || strings.Contains(alerts[0].Action, "expire-leaked-ttls") {
		t.Fatal("TTL detail freshness assertion: a prior stream sample supplied current attribution")
	}
	redisKeyspaceAuthorityPrivate(t, alerts)
	reply = "5000 10 0 0 0 0 10 2000 300 28799985312000000 other"
	alerts, err = signal.Run(context.Background(), settings)
	if err != nil || len(alerts) != 1 || strings.Contains(alerts[0].Observed, "sample_status=unavailable") ||
		!strings.Contains(alerts[0].Observed, "sample_other=10") || strings.Contains(alerts[0].Action, "expire-leaked-ttls") {
		t.Fatal("corrected current other-family attribution did not replace the prior stream sample")
	}
}
