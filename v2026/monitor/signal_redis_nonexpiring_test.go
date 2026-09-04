package monitor

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func redisPersistentSampleFixture(families map[string]redisPersistentFamily) string {
	totals := [5]int64{}
	parts := []string{"0", "0", "0", "0", "0"}
	for _, name := range redisPersistentFamilyOrder {
		family := families[name]
		parts = append(parts, name,
			fmt.Sprint(family.sampled),
			fmt.Sprint(family.persistent),
			fmt.Sprint(family.expiring),
			fmt.Sprint(family.missing),
			fmt.Sprint(family.invalid),
		)
		totals[0] += family.sampled
		totals[1] += family.persistent
		totals[2] += family.expiring
		totals[3] += family.missing
		totals[4] += family.invalid
	}
	for index := range totals {
		parts[index] = fmt.Sprint(totals[index])
	}
	return strings.Join(parts, " ")
}

func redisPersistentProvideFixtures() (string, string) {
	candidate := map[string]redisPersistentFamily{
		"provide-pms": {sampled: 300, persistent: 75, expiring: 225},
		"provide-rp":  {sampled: 1600, persistent: 420, expiring: 1180},
		"provide-sk":  {sampled: 500, persistent: 79, expiring: 421},
		"score":       {sampled: 500, expiring: 500},
		"client-key":  {sampled: 500, persistent: 400, expiring: 100},
		"other":       {sampled: 1600, persistent: 1600},
	}
	control := map[string]redisPersistentFamily{
		"provide-pms": {sampled: 300, expiring: 300},
		"provide-rp":  {sampled: 1600, expiring: 1600},
		"provide-sk":  {sampled: 500, expiring: 500},
		"score":       {sampled: 500, expiring: 500},
		"client-key":  {sampled: 500, persistent: 400, expiring: 100},
		"other":       {sampled: 1600, persistent: 1600},
	}
	return redisPersistentSampleFixture(candidate), redisPersistentSampleFixture(control)
}

func TestRedisNonexpiringSignalAttributesProvideMirrorResidue(t *testing.T) {
	candidateSample, controlSample := redisPersistentProvideFixtures()
	samplePorts := []int{}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, "INFO keyspace"):
			if !strings.Contains(command, "CLUSTER NODES") {
				t.Fatal("keyspace census omitted owned-slot measurement")
			}
			return "6380 4810000 2555000 512\n6381 4811000 2555000 512\n6382 5399000 2554000 512", nil
		case strings.Contains(command, "EVAL_RO"):
			if !strings.Contains(command, "timeout 45") || !strings.Contains(command, "base64 -d") ||
				!strings.Contains(command, " 0 5000 25 250") || strings.Contains(command, "--scan") {
				t.Fatalf("PTTL attribution was not bounded and in-Redis: %s", command)
			}
			if strings.Contains(command, "-p 6382 ") {
				samplePorts = append(samplePorts, 6382)
				return candidateSample, nil
			}
			if strings.Contains(command, "-p 6381 ") {
				samplePorts = append(samplePorts, 6381)
				return controlSample, nil
			}
			t.Fatalf("unexpected Redis sample port: %s", command)
			return "", nil
		default:
			t.Fatalf("unexpected Redis non-expiring command: %s", command)
			return "", nil
		}
	}}

	alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "nonexpiring-key-skew")
	if alert.Target != "redis-1:6382" {
		t.Fatalf("skew target = %q, want exact Redis host:port", alert.Target)
	}
	markdown := alert.Markdown()
	for _, want := range []string{
		"worst_port=6382",
		"non_expiring_keys=2845000",
		"owned_slots=512",
		"estimated_excess_keys=589000",
		"control_port=6381",
		"candidate_provide_pms_persistent=75",
		"candidate_provide_rp_persistent=420",
		"candidate_provide_sk_persistent=79",
		"control_provide_pms_persistent=0",
		"detail_status=provide",
		"legacy residue",
		"missed/interrupted cleanup from restoration of an older RDB",
		"separate maintenance authorization",
		"EXPIRE-NX",
		"one full 72-hour window",
		"no key, value, or identifier crossed",
	} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("provide-residue alert missing %q:\n%s", want, markdown)
		}
	}
	if fmt.Sprint(samplePorts) != "[6382 6381]" {
		t.Fatalf("candidate/control sample ports = %v, want [6382 6381]", samplePorts)
	}
}

func TestRedisNonexpiringSignalNormalizesByOwnedSlotsAndKeepsHealthyBoundaries(t *testing.T) {
	tests := []struct {
		name string
		rows string
	}{
		{
			name: "different raw totals but equal per-slot density",
			rows: "6380 1100000 100000 256\n6381 2100000 100000 512\n6382 3100000 100000 768",
		},
		{
			name: "ratio just below threshold",
			rows: "6380 1100000 100000 512\n6381 1100000 100000 512\n6382 1299999 100000 512",
		},
		{
			name: "material excess just below floor",
			rows: "6380 200000 100000 512\n6381 200000 100000 512\n6382 299999 100000 512",
		},
	}
	for _, testCase := range tests {
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "EVAL_RO") {
				t.Fatalf("%s: healthy fleet unexpectedly ran detailed key sampling", testCase.name)
			}
			return testCase.rows, nil
		}}
		alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		if len(alerts) != 0 {
			t.Fatalf("%s: healthy slot-normalized fleet alerted: %+v", testCase.name, alerts)
		}
	}
}

func TestRedisNonexpiringSignalTreatsIncompleteMasterCensusAsVisibility(t *testing.T) {
	tests := []struct {
		name string
		rows string
	}{
		{name: "omitted master", rows: "6380 4810000 2555000 512\n6381 4811000 2555000 512"},
		{name: "unreachable master", rows: "6380 4810000 2555000 512\n6381 4811000 2555000 512\n6382 unreachable"},
		{name: "zero owned slots", rows: "6380 4810000 2555000 512\n6381 4811000 2555000 512\n6382 5399000 2554000 0"},
		{name: "expires exceed keys", rows: "6380 4810000 2555000 512\n6381 4811000 2555000 512\n6382 2553999 2554000 512"},
	}
	for _, testCase := range tests {
		sampled := false
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "EVAL_RO") {
				sampled = true
				t.Fatalf("%s: incomplete census ran detailed sampling", testCase.name)
			}
			return testCase.rows, nil
		}}
		monitor := NewWithSignals(syntheticSettings(source), NewRedisNonexpiringSignal())
		alerts, err := monitor.Run(context.Background())
		if err == nil {
			t.Fatalf("%s: incomplete census returned no monitor error", testCase.name)
		}
		visibility := requireAlertClass(t, alerts, "cannot-observe")
		if visibility.SignalKey != "redis-nonexpiring" || sampled {
			t.Fatalf("%s: visibility framing or sampling wrong: %+v sampled=%t", testCase.name, visibility, sampled)
		}
		for _, alert := range alerts {
			if alert.Class == "nonexpiring-key-skew" {
				t.Fatalf("%s: incomplete census emitted data-state alert: %+v", testCase.name, alert)
			}
		}
	}
}

func TestRedisNonexpiringSignalKeepsMixedFamilyAttributionAmbiguous(t *testing.T) {
	candidateSample, controlSample := redisPersistentProvideFixtures()
	candidate, err := parseRedisPersistentSample(candidateSample)
	if err != nil {
		t.Fatal(err)
	}
	// A second nearly equal fixed-family delta prevents a cleanup-specific
	// attribution even though both aggregates are individually material.
	candidate.families["score"] = redisPersistentFamily{sampled: 500, persistent: 500}
	candidateSample = redisPersistentSampleFixture(candidate.families)

	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		switch {
		case strings.Contains(command, "INFO keyspace"):
			return "6380 4810000 2555000 512\n6381 4811000 2555000 512\n6382 5399000 2554000 512", nil
		case strings.Contains(command, "-p 6382 "):
			return candidateSample, nil
		case strings.Contains(command, "-p 6381 "):
			return controlSample, nil
		default:
			t.Fatalf("unexpected command: %s", command)
			return "", nil
		}
	}}
	alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil {
		t.Fatal(err)
	}
	alert := requireAlertClass(t, alerts, "nonexpiring-key-skew")
	if !strings.Contains(alert.Observed, "detail_status=ambiguous") ||
		!strings.Contains(alert.Mechanism, "mixed fixed-family deltas") ||
		strings.Contains(alert.Action, "EXPIRE-NX") {
		t.Fatalf("mixed attribution selected unsafe family cleanup: %s", alert.Markdown())
	}
}

func TestRedisNonexpiringSignalTreatsMalformedOrEmptyDetailAsUnavailableAndRedactsIt(t *testing.T) {
	candidateSample, _ := redisPersistentProvideFixtures()
	rawFamily := "customer@example.invalid"
	malformedSample := strings.Replace(candidateSample, "provide-pms", rawFamily, 1)
	for _, testCase := range []struct {
		name   string
		sample string
	}{
		{name: "unexpected label", sample: malformedSample},
		{name: "empty response", sample: ""},
	} {
		source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
			switch {
			case strings.Contains(command, "INFO keyspace"):
				return "6380 4810000 2555000 512\n6381 4811000 2555000 512\n6382 5399000 2554000 512", nil
			case strings.Contains(command, "EVAL_RO"):
				return testCase.sample, nil
			default:
				t.Fatalf("%s: unexpected command: %s", testCase.name, command)
				return "", nil
			}
		}}
		alerts, err := NewRedisNonexpiringSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatalf("%s: %v", testCase.name, err)
		}
		skew := requireAlertClass(t, alerts, "nonexpiring-key-skew")
		visibility := requireAlertClass(t, alerts, "cannot-observe")
		if !strings.Contains(skew.Observed, "detail_status=unavailable") ||
			strings.Contains(skew.Observed, "candidate_provide_pms_persistent=0") {
			t.Fatalf("%s: incomplete detail was rendered as a zero cohort: %s", testCase.name, skew.Markdown())
		}
		for _, alert := range []Alert{skew, visibility} {
			requireAlertOmits(t, alert, rawFamily, "customer@example")
		}
	}
}

func TestParseRedisPersistentSampleRejectsMalformedAggregatesWithoutEchoingInput(t *testing.T) {
	candidateSample, _ := redisPersistentProvideFixtures()
	for _, testCase := range []struct {
		name  string
		value string
	}{
		{name: "duplicate or reordered label", value: strings.Replace(candidateSample, "provide-rp", "provide-pms", 1)},
		{name: "negative aggregate", value: strings.Replace(candidateSample, "300 75", "300 -75", 1)},
		{name: "inconsistent total", value: strings.Replace(candidateSample, "300 75 225", "300 75 224", 1)},
	} {
		_, err := parseRedisPersistentSample(testCase.value)
		if err == nil {
			t.Fatalf("%s: malformed aggregate parsed successfully", testCase.name)
		}
		if strings.Contains(err.Error(), "provide-pms provide-pms") || strings.Contains(err.Error(), "-75") {
			t.Fatalf("%s: parser error echoed untrusted aggregate: %q", testCase.name, err)
		}
	}
}
