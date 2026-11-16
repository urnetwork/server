// Byte attribution and optional rollout context require complete native replies.
// All sources, identities, diagnostics, and executables here are synthetic.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const redisByteAuthorityPrivate = "synthetic-private-redis-byte-diagnostic"

// Mutate one token while preserving the independent header/family evidence.
func redisByteAuthorityField(index int, value string) string {
	fields := strings.Fields(syntheticRedisByteSample)
	fields[index] = value
	return strings.Join(fields, " ")
}

// Numeric conversion failures cannot become a zero score family or healthy.
func TestRedisByteAuthorityRejectsMalformedNumbers(t *testing.T) {
	for _, index := range []int{0, 1, 2, 4, 5, 7, 8, 10, 11, 13, 14, 16, 17, 19, 20} {
		for _, value := range []string{redisByteAuthorityPrivate, "-1", "+1", "01", "1.5", "NaN", "9223372036854775808"} {
			output := redisByteAuthorityField(index, value)
			source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return output, nil }}
			env := &probeEnv{runner: &sourceRunner{source: source}}
			if _, err := sampleRedisByteFamilies(context.Background(), env, &host{}, 6380); err == nil {
				t.Errorf("numeric field %d accepted an invalid sample integer", index)
			} else if strings.Contains(err.Error(), redisByteAuthorityPrivate) {
				t.Fatal("sample parse error disclosed a private token")
			}
		}
	}
}

// Exact family cardinality, sample bounds and conserved totals precede ratios.
func TestRedisByteAuthorityRejectsContradictorySamples(t *testing.T) {
	for _, output := range []string{
		"",
		"NOAUTH " + redisByteAuthorityPrivate,
		strings.TrimSuffix(syntheticRedisByteSample, " other 2 324"),
		syntheticRedisByteSample + " score 0 0",
		syntheticRedisByteSample + " " + redisByteAuthorityPrivate + " 0 0",
		redisByteAuthorityField(18, "score"),
		redisByteAuthorityField(18, redisByteAuthorityPrivate),
		redisByteAuthorityField(0, "1001"),
		redisByteAuthorityField(0, "999"),
		redisByteAuthorityField(1, "1001"),
		redisByteAuthorityField(1, "999"),
		redisByteAuthorityField(2, "1409965"),
		redisByteAuthorityField(2, "1409967"),
		redisByteAuthorityField(4, "1001"),
		redisByteAuthorityField(5, "1409967"),
		redisByteAuthorityField(5, "0"),
		redisByteAuthorityField(4, "0"),
		"1000 1000 10 score 1000 10 provide 0 0 client-key 0 0 connect 0 0 stream 0 0 other 0 0",
		"1000 1000 9223372036854775807 score 500 9223372036854775807 provide 500 9223372036854775807 client-key 0 0 connect 0 0 stream 0 0 other 0 0",
	} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return output, nil }}
		env := &probeEnv{runner: &sourceRunner{source: source}}
		if _, err := sampleRedisByteFamilies(context.Background(), env, &host{}, 6380); err == nil {
			t.Error("incomplete or contradictory sample was accepted")
		} else if strings.Contains(err.Error(), redisByteAuthorityPrivate) {
			t.Fatal("sample shape error disclosed a private token")
		}
	}
}

// Zero families and empty native samples are valid structures, not parse errors.
func TestRedisByteAuthorityAcceptsConservedSamples(t *testing.T) {
	for _, output := range []string{
		syntheticRedisByteSample,
		strings.ReplaceAll(syntheticRedisByteSample, " ", "\n") + "\n",
		"1000 1000 500000 score 0 0 provide 1000 500000 client-key 0 0 connect 0 0 stream 0 0 other 0 0",
		"1000 500 500000 score 500 500000 provide 0 0 client-key 0 0 connect 0 0 stream 0 0 other 0 0",
		"0 0 0 score 0 0 provide 0 0 client-key 0 0 connect 0 0 stream 0 0 other 0 0",
		"10 0 0 score 0 0 provide 0 0 client-key 0 0 connect 0 0 stream 0 0 other 0 0",
	} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return output, nil }}
		env := &probeEnv{runner: &sourceRunner{source: source}}
		if _, err := sampleRedisByteFamilies(context.Background(), env, &host{}, 6380); err != nil {
			t.Error("valid conserved sample was rejected")
		}
	}
}

// The real public signal must fail observation before a malformed ratio is used.
func TestRedisByteAuthorityPublicUnknownAndHealthyControls(t *testing.T) {
	for _, testCase := range []struct {
		sample      string
		wantUnknown bool
	}{
		{sample: redisByteAuthorityField(5, redisByteAuthorityPrivate), wantUnknown: true},
		{sample: syntheticRedisByteSample + " score 0 0", wantUnknown: true},
		{sample: redisByteAuthorityField(5, "1409967"), wantUnknown: true},
		{sample: "0 0 0 score 0 0 provide 0 0 client-key 0 0 connect 0 0 stream 0 0 other 0 0", wantUnknown: true},
		{sample: "1000 1000 500000 score 0 0 provide 1000 500000 client-key 0 0 connect 0 0 stream 0 0 other 0 0"},
	} {
		source := &syntheticSource{
			hostFn: func(_ HostSettings, command string) (string, error) {
				if strings.Contains(command, "INFO memory") {
					return "6380 9000000 10000000\n6381 8000000 10000000\n6382 7000000 10000000", nil
				}
				return testCase.sample, nil
			},
			redisFn: func(HostSettings, int, ...string) (string, error) {
				t.Error("invalid or zero-score sample attempted marker attribution")
				return "null", nil
			},
		}
		settings := syntheticSettings(source)
		signal := NewRedisBytesSignal()
		alerts, err := signal.Run(context.Background(), settings)
		if (err != nil) != testCase.wantUnknown || len(alerts) != 0 {
			t.Fatal("sample authority changed an unknown/healthy verdict")
		}
		if err != nil {
			alert := visibilityAlert(settings, signal, err)
			if alert.Class != "cannot-observe" || !alert.unobservable {
				t.Fatal("invalid byte sample did not retain observation failure")
			}
			requireRedisByteAuthorityPublicPrivacy(t, alert)
		}
	}
}

// A failed optional marker cannot suppress independently measured byte pressure.
func TestRedisByteAuthorityMarkerUnknownPreservesPrivatePositive(t *testing.T) {
	for _, testCase := range []struct {
		output string
		err    error
	}{
		{output: "NOAUTH " + redisByteAuthorityPrivate},
		{output: "NOPERM " + redisByteAuthorityPrivate},
		{output: `"` + redisByteAuthorityPrivate + `"`},
		{output: `""`},
		{output: `" 1 "`},
		{output: "1"},
		{output: "null\nnull"},
		{output: ""},
		{output: `"1"`, err: errors.New(redisByteAuthorityPrivate)},
	} {
		source := &syntheticSource{
			hostFn: func(_ HostSettings, command string) (string, error) {
				if strings.Contains(command, "INFO memory") {
					return "6380 9000000 10000000\n6381 8000000 10000000\n6382 7000000 10000000", nil
				}
				return syntheticRedisByteSample, nil
			},
			redisFn: func(HostSettings, int, ...string) (string, error) { return testCase.output, testCase.err },
		}
		alerts, err := NewRedisBytesSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil || len(alerts) != 1 {
			t.Fatal("unknown optional marker suppressed byte dominance")
		}
		alert := requireAlertClass(t, alerts, "score-byte-dominance")
		if !strings.Contains(alert.Observed, "alias_schema_ready=unknown") ||
			!strings.Contains(alert.Observed, "score_bytes=1318490") || alert.Sustain != 1 || alert.Severity != SeverityWarn {
			t.Fatal("marker unknown changed independently valid pressure or its identity")
		}
		requireRedisByteAuthorityPublicPrivacy(t, alert)
	}
}

// Exercise the optional-error leak independently of malformed-reply authority.
func TestRedisByteAuthorityMarkerTransportErrorPrivacy(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "INFO memory") {
				return "6380 9000000 10000000\n6381 8000000 10000000\n6382 7000000 10000000", nil
			}
			return syntheticRedisByteSample, nil
		},
		redisFn: func(HostSettings, int, ...string) (string, error) {
			return "", errors.New(redisByteAuthorityPrivate)
		},
	}
	alerts, err := NewRedisBytesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 {
		t.Fatal("optional marker failure suppressed byte dominance")
	}
	alert := requireAlertClass(t, alerts, "score-byte-dominance")
	if !strings.Contains(alert.Observed, "alias_schema_ready=unknown") {
		t.Fatal("failed marker was not unknown")
	}
	requireRedisByteAuthorityPublicPrivacy(t, alert)
}

// Redaction must not erase typed deadline/admission authority from the chain.
func TestRedisByteAuthoritySourceErrorPreservesCause(t *testing.T) {
	cause := fmt.Errorf("%s: %w", redisByteAuthorityPrivate, context.DeadlineExceeded)
	source := &syntheticSource{
		hostFn:  func(HostSettings, string) (string, error) { return "", cause },
		redisFn: func(HostSettings, int, ...string) (string, error) { return "", cause },
	}
	env := &probeEnv{runner: &sourceRunner{source: source}}
	_, sampleErr := sampleRedisByteFamilies(context.Background(), env, &host{}, 6380)
	_, markerErr := redisScoreAliasesReady(context.Background(), env, &host{})
	for _, err := range []error{sampleErr, markerErr} {
		if !errors.Is(err, cause) || !errors.Is(err, context.DeadlineExceeded) ||
			classifyObservationError(err) != observationErrorClassTimeout {
			t.Fatal("fixed diagnostic erased the source deadline cause")
		}
	}
	if strings.Contains(markerErr.Error(), redisByteAuthorityPrivate) {
		t.Fatal("marker wrapper exposed private error text")
	}
	alert := visibilityAlert(syntheticSettings(source), NewRedisBytesSignal(), sampleErr)
	requireRedisByteAuthorityPublicPrivacy(t, alert)
}

// The old raw and new typed protocols describe the same retained global marker.
func TestRedisByteAuthorityHistoricalMarkerDoesNotAttestWriterOrWindow(t *testing.T) {
	source := &syntheticSource{
		hostFn: func(_ HostSettings, command string) (string, error) {
			if strings.Contains(command, "INFO memory") {
				return "6380 9000000 10000000\n6381 8000000 10000000\n6382 7000000 10000000", nil
			}
			return syntheticRedisByteSample, nil
		},
		redisFn: func(_ HostSettings, _ int, args ...string) (string, error) {
			if strings.Contains(strings.Join(args, " "), "--json") {
				return `"1"`, nil
			}
			return "1", nil
		},
	}
	alerts, err := NewRedisBytesSignal().Run(context.Background(), syntheticSettings(source))
	if err != nil || len(alerts) != 1 {
		t.Fatal("retained marker changed the independently valid byte finding")
	}
	alert := requireAlertClass(t, alerts, "score-byte-dominance")
	for _, want := range []string{"alias_schema_ready=true", "alias_publication_time=unknown", "score_exporter_capability=unverified"} {
		if !strings.Contains(alert.Observed, want) {
			t.Error("global marker invented current-writer or publication-time authority")
		}
	}
	requireAlertOmits(t, alert, "software fix is already active", "continuing dominance now measures legacy duplicate payloads inside their normal five-hour TTL")
}

// Native fixtures execute the generated command through a local fake redis-cli.
// Both raw and JSON encodings are supplied so the old producer remains causal.
func newRedisByteAuthorityNativeRunner(t *testing.T, raw, typed, sample string, status int) (*runner, *host) {
	t.Helper()
	directory := t.TempDir()
	for name, content := range map[string]string{
		"raw": raw, "json": typed, "sample": sample,
		"status": fmt.Sprint(status), "byte-script": redisByteFamiliesLua,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(content), 0600); err != nil {
			t.Fatal("write private native reply")
		}
	}
	for name, script := range map[string]string{
		"timeout": "#!/bin/sh\n[ \"$1\" = 45 ] || exit 126\nshift\nexec \"$@\"\n",
		"redis-cli": `#!/bin/sh
[ "$1" = -p ] || exit 126
if [ "$#" -eq 6 ]; then
  [ "$2" = 6379 ] && [ "$3" = -c ] && [ "$5" = GET ] && [ "$6" = client_score_alias_v1_ready ] || exit 126
  case "$4" in --raw) reply=raw;; --json) reply=json;; *) exit 126;; esac
elif [ "$#" -eq 9 ]; then
  [ "$2" = 6380 ] && [ "$3" = --raw ] && [ "$4" = EVAL_RO ] && [ "$6" = 0 ] && [ "$7" = 1000 ] && [ "$8" = 20 ] && [ "$9" = 250 ] || exit 126
  [ "$5" = "$(cat "$REDIS_BYTE_AUTHORITY_DIR/byte-script")" ] || exit 126
  reply=sample
else
  exit 126
fi
cat "$REDIS_BYTE_AUTHORITY_DIR/$reply" || exit 126
printf '%s\n' synthetic-private-redis-byte-diagnostic >&2
exit "$(cat "$REDIS_BYTE_AUTHORITY_DIR/status")"
`,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal("write private native fixture")
		}
	}
	r := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: 5 * time.Second})
	r.runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
		if stdin != "" || len(args) == 0 {
			t.Fatal("unexpected native fixture transport")
		}
		command := redisAuthorityNativeCommand(ctx, args[len(args)-1])
		command.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C", "REDIS_BYTE_AUTHORITY_DIR=" + directory}
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		redisAuthorityKillNativeGroup(command)
		return stdout.String(), stderr.String(), err
	}
	return r, &host{name: "redis-byte.example", overlayIp: "192.0.2.90", redisEntryPort: 6379}
}

// Distinguish a missing key from empty/padded strings and zero-exit server errors.
func TestRedisByteAuthorityNativeMarkerStates(t *testing.T) {
	for _, testCase := range []struct {
		raw, typed  string
		status      int
		wantReady   bool
		wantUnknown bool
	}{
		{raw: "\n", typed: "null\n"},
		{raw: "1\n", typed: "\"1\"\n", wantReady: true},
		{raw: "\n", typed: "\"\"\n", wantUnknown: true},
		{raw: " \t\n", typed: `" \t"`, wantUnknown: true},
		{raw: " 1 \n", typed: `" 1 "`, wantUnknown: true},
		{raw: "0\n", typed: `"0"`, wantUnknown: true},
		{raw: "NOAUTH " + redisByteAuthorityPrivate, typed: "NOAUTH " + redisByteAuthorityPrivate, wantUnknown: true},
		{raw: "NOPERM " + redisByteAuthorityPrivate, typed: "NOPERM " + redisByteAuthorityPrivate, wantUnknown: true},
		{raw: redisByteAuthorityPrivate, typed: `"` + redisByteAuthorityPrivate + `"`, wantUnknown: true},
		{wantUnknown: true},
		{typed: `"1`, wantUnknown: true},
		{raw: "1\n", typed: "\"1\"\n", status: 1, wantUnknown: true},
		{status: 127, wantUnknown: true},
	} {
		r, h := newRedisByteAuthorityNativeRunner(t, testCase.raw, testCase.typed, "", testCase.status)
		ready, err := redisScoreAliasesReady(context.Background(), &probeEnv{runner: r}, h)
		if ready != testCase.wantReady || (err != nil) != testCase.wantUnknown {
			t.Error("native marker reply lost its typed authority or exit status")
		}
		if err != nil && strings.Contains(err.Error(), redisByteAuthorityPrivate) {
			t.Fatal("native marker error disclosed stderr")
		}
	}
}

// Actual shell arguments and native status are checked before accepting a sample.
func TestRedisByteAuthorityNativeSampleContract(t *testing.T) {
	for _, testCase := range []struct {
		output      string
		status      int
		wantUnknown bool
	}{
		{output: strings.ReplaceAll(syntheticRedisByteSample, " ", "\n") + "\n"},
		{output: syntheticRedisByteSample, status: 1, wantUnknown: true},
		{output: "NOAUTH " + redisByteAuthorityPrivate, wantUnknown: true},
		{output: redisByteAuthorityField(5, redisByteAuthorityPrivate), wantUnknown: true},
		{output: syntheticRedisByteSample + " score 0 0", wantUnknown: true},
		{output: redisByteAuthorityField(5, "1409967"), wantUnknown: true},
	} {
		r, h := newRedisByteAuthorityNativeRunner(t, "", "", testCase.output, testCase.status)
		_, err := sampleRedisByteFamilies(context.Background(), &probeEnv{runner: r}, h, 6380)
		if (err != nil) != testCase.wantUnknown {
			t.Error("native sample violated complete-reply or status authority")
		}
		if err != nil {
			alert := visibilityAlert(syntheticSettings(nil), NewRedisBytesSignal(), err)
			requireRedisByteAuthorityPublicPrivacy(t, alert)
		}
	}
}

// Both public encodings must omit raw source tokens and optional errors.
func requireRedisByteAuthorityPublicPrivacy(t *testing.T, alert Alert) {
	t.Helper()
	requireAlertOmits(t, alert, redisByteAuthorityPrivate)
	encoded, err := json.Marshal(alert)
	if err != nil || strings.Contains(string(encoded), redisByteAuthorityPrivate) {
		t.Fatal("public JSON disclosed a private source token")
	}
}

// The shared marker parser must preserve the heap finding and phase enrichment.
func TestRedisByteAuthorityWorkerMemoryMarkerUnknown(t *testing.T) {
	testRedisByteAuthorityWorkerMarker(t, NewWorkerMemorySignal, "worker-memory-skew")
}

// The same marker boundary applies to the independent CPU/allocation finding.
func TestRedisByteAuthorityWorkerChurnMarkerUnknown(t *testing.T) {
	testRedisByteAuthorityWorkerMarker(t, NewWorkerChurnSignal, "worker-cpu-allocation-churn")
}

// Fixed replies cover both worker consumers without changing their metrics.
func testRedisByteAuthorityWorkerMarker(t *testing.T, newSignal func() Signal, class string) {
	t.Helper()
	now := time.Date(2026, 9, 16, 11, 58, 30, 0, time.UTC)
	workers := []workerMetricFixture{
		{host: "worker-a", block: "g1", instance: "runtime-a", heap: 256 * 1024 * 1024, cpuRate: 0.02, allocRate: 1024 * 1024},
		{host: "worker-b", block: "g1", instance: "runtime-b", heap: 256 * 1024 * 1024, cpuRate: 0.04, allocRate: 2 * 1024 * 1024},
		{host: "worker-c", block: "g1", instance: "runtime-c", heap: 256 * 1024 * 1024, cpuRate: 0.06, allocRate: 4 * 1024 * 1024},
		{host: "worker-hot", block: "g2", instance: "runtime-hot", heap: 9 * 1024 * 1024 * 1024, cpuRate: 4.001, allocRate: 320 * 1024 * 1024},
	}
	for _, marker := range []string{"NOAUTH " + redisByteAuthorityPrivate, "NOPERM " + redisByteAuthorityPrivate, `""`, `" 1 "`, `"` + redisByteAuthorityPrivate + `"`} {
		markerReads := 0
		source := &syntheticSource{
			hostFn: func(_ HostSettings, command string) (string, error) {
				if strings.Contains(command, "monitor_score_phase_metric") {
					return workerScorePhaseFixtureJSON(t, now, "worker-hot", "g2", "runtime-hot", true), nil
				}
				if strings.Contains(command, "monitor_rate") {
					if class == "worker-cpu-allocation-churn" {
						return workerChurnRatesFixtureJSON(t, now, workers...), nil
					}
					return workerRatesFixtureJSON(t, now, workers...), nil
				}
				return workerMetricsFixtureJSON(t, now, workers...), nil
			},
			localFn: func(string, ...string) (string, error) {
				return "[worker-hot][taskworker][g2][cid:runtime-hot][I][2026-09-16T11:58:25Z][task.go:1938][00000000-0000-4000-8000-000000000091]eval active(101s) synthetic/taskworker/work.UpdateClientScores({})", nil
			},
			redisFn: func(HostSettings, int, ...string) (string, error) {
				markerReads++
				return marker, nil
			},
		}
		alerts, err := newSignal().Run(context.Background(), workerMemorySyntheticSettings(source, now))
		if err != nil || len(alerts) != 1 || markerReads != 1 {
			t.Fatal("marker authority changed the independent worker finding")
		}
		alert := requireAlertClass(t, alerts, class)
		if !strings.Contains(alert.Observed, "score_alias_schema_ready=unknown") ||
			!strings.Contains(alert.Observed, "score_phase_observability=ready") ||
			alert.Target != "worker-hot/g2" || alert.Frame != "runtime-hot" || alert.Sustain != 2 || alert.Severity != SeverityWarn {
			t.Fatal("worker marker unknown lost identity, severity, or valid phase evidence")
		}
		requireRedisByteAuthorityPublicPrivacy(t, alert)
	}
}
