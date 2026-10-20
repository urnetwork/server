// Exercise the real reliability shell, parser and public signal with synthetic
// local commands. No test contacts Redis, SSH, PostgreSQL or production hosts.
package monitor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Build complete native-format observations for reducer and multi-host tests.
func redisReliabilityTestFrame(ports []int, blocks string, latency func(int) string) string {
	var output strings.Builder
	fmt.Fprintf(&output, "blocks\t%s\t0\t0\n", base64.StdEncoding.EncodeToString([]byte(blocks)))
	for _, port := range ports {
		output.WriteString(redisReliabilityTestLatencyRow(port, latency(port)))
	}
	fmt.Fprintf(&output, "complete\t%d\n", len(ports))
	return output.String()
}

// A successful latency source supplies its explicit exit trailer.
func redisReliabilityTestLatencyRow(port int, latency string) string {
	return fmt.Sprintf("latency\t%d\t%s\t0\t0\n", port, base64.StdEncoding.EncodeToString([]byte(latency+"\nreliability_exit=0\n")))
}

// Require exact incident targets and the stable host-scoped visibility alert.
// Assert both JSON and Markdown privacy, including identities and observations.
func redisReliabilityRequireAlerts(t *testing.T, alerts Alerts, unknown bool, targets ...string) {
	t.Helper()
	incidentTargets := []string{}
	unknownCount := 0
	for _, alert := range alerts {
		if alert.SignalKey != "reliability-pipeline" || alert.SignalNumber != "3.7" || alert.Severity != SeverityWarn || alert.Sustain != 2 {
			t.Fatal("reliability observation changed its public warning identity")
		}
		switch alert.Class {
		case "cannot-observe":
			unknownCount++
			if alert.Target != "redis-reliability.example/reliability-load" || !alert.unobservable {
				t.Fatal("reliability observation gap lost its stable host identity")
			}
		case "reliability-pipeline-degraded":
			incidentTargets = append(incidentTargets, alert.Target)
		default:
			t.Fatal("reliability observation produced an unrelated class")
		}
		encoded, err := json.Marshal(alert)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "synthetic-private-sentinel") || strings.Contains(alert.Markdown(), "synthetic-private-sentinel") {
			t.Fatal("native reliability output escaped into the public alert")
		}
	}
	wantUnknown := 0
	if unknown {
		wantUnknown = 1
	}
	if unknownCount != wantUnknown {
		t.Fatalf("reliability authority assertion: observation gaps=%d, want %d", unknownCount, wantUnknown)
	}
	slices.Sort(incidentTargets)
	slices.Sort(targets)
	if !slices.Equal(incidentTargets, targets) {
		t.Fatalf("reliability authority assertion: incident targets=%v, want %v", incidentTargets, targets)
	}
}

// Parameters select native output and status independently. The cutoff flag
// belongs to the timeout fixture, distinct from a Redis child returning 124.
type redisReliabilityNativeCase struct {
	blocks         string
	blockStatus    int
	latency        string
	latencyStatus  int
	cutoff         bool
	encodingFailed bool
	blocksNul      bool
	latencyNul     bool
}

// Execute the exact generated command with fake binaries that accept both the
// old and corrected argv. Baseline failures must reach the authority assertion.
func redisReliabilityNativeAlerts(t *testing.T, test redisReliabilityNativeCase) Alerts {
	t.Helper()
	directory := t.TempDir()
	callPath := filepath.Join(directory, "calls")
	redisScript := `#!/bin/sh
port=
mode=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -c|--raw) shift ;;
    -p) [ "$#" -ge 2 ] || exit 126; port=$2; shift 2 ;;
    SCARD) [ "$#" -eq 2 ] && [ "$2" = client_reliability_stats_blocks ] || exit 126; mode=blocks; shift 2 ;;
    --latency) mode=latency; shift ;;
    *) exit 126 ;;
  esac
done
case "$port" in 6380|6382) ;; *) exit 126 ;; esac
printf '%s %s\n' "$mode" "$port" >> "$RELIABILITY_TEST_CALLS"
printf '%s\n' synthetic-private-sentinel >&2
if [ "$mode" = blocks ]; then
  if [ "$RELIABILITY_TEST_BLOCKS_NUL" = true ]; then printf '\000'; fi
  printf '%s' "$RELIABILITY_TEST_BLOCKS"
  exit "$RELIABILITY_TEST_BLOCK_STATUS"
fi
[ "$mode" = latency ] || exit 126
if [ "$port" = 6382 ]; then
  if [ "$RELIABILITY_TEST_LATENCY_NUL" = true ]; then printf '\000'; fi
  printf '%s' "$RELIABILITY_TEST_LATENCY"
  exit "$RELIABILITY_TEST_LATENCY_STATUS"
fi
printf '0.1 1.0 0.3 98\n'
`
	timeoutScript := `#!/bin/sh
[ "$1" = 2 ] || exit 126
shift
if [ "$RELIABILITY_TEST_CUTOFF" = true ]; then
  cutoff=false
  if [ "$1" = sh ] && [ "$#" -eq 5 ] && [ "$5" = 6382 ]; then cutoff=true; fi
  if [ "$1" = redis-cli ] && [ "$#" -eq 5 ] && [ "$4" = 6382 ] && [ "$5" = --latency ]; then cutoff=true; fi
  if [ "$cutoff" = true ]; then
    printf 'latency 6382\n' >> "$RELIABILITY_TEST_CALLS"
    printf '%s' "$RELIABILITY_TEST_LATENCY"
    exit 124
  fi
fi
exec "$@"
`
	scripts := map[string]string{"redis-cli": redisScript, "timeout": timeoutScript}
	if test.encodingFailed {
		scripts["base64"] = "#!/bin/sh\nprintf OQ==\nexit 1\n"
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := &syntheticSource{hostFn: func(_ HostSettings, command string) (string, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
		cmd.Env = []string{
			"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C",
			"RELIABILITY_TEST_CALLS=" + callPath,
			"RELIABILITY_TEST_BLOCKS=" + test.blocks,
			fmt.Sprintf("RELIABILITY_TEST_BLOCK_STATUS=%d", test.blockStatus),
			"RELIABILITY_TEST_LATENCY=" + test.latency,
			fmt.Sprintf("RELIABILITY_TEST_LATENCY_STATUS=%d", test.latencyStatus),
			fmt.Sprintf("RELIABILITY_TEST_CUTOFF=%t", test.cutoff),
			fmt.Sprintf("RELIABILITY_TEST_BLOCKS_NUL=%t", test.blocksNul),
			fmt.Sprintf("RELIABILITY_TEST_LATENCY_NUL=%t", test.latencyNul),
		}
		output, err := cmd.Output()
		return string(output), err
	}}
	settings := syntheticSettings(source)
	settings.Hosts[1].Name = "redis-reliability.example"
	settings.Hosts[1].RedisNodePorts = []int{6380, 6382}
	alerts, err := NewReliabilityPipelineSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatalf("native reliability fixture failed before its authority assertion: %v", err)
	}
	calls, err := os.ReadFile(callPath)
	if err != nil {
		t.Fatal("native reliability fixture did not reach its fake producer")
	}
	if string(calls) != "blocks 6380\nlatency 6380\nlatency 6382\n" {
		t.Fatal("native reliability collection did not read one cluster set and exactly the sparse configured ports")
	}
	return alerts
}

// A successful but empty transport response supplies no measurement authority.
func TestReliabilityPipelineAuthorityEmptySuccessIsUnknown(t *testing.T) {
	source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return "", nil }}
	settings := syntheticSettings(source)
	settings.Hosts[1].Name = "redis-reliability.example"
	alerts, err := NewReliabilityPipelineSignal().Run(context.Background(), settings)
	if err != nil {
		t.Fatal(err)
	}
	redisReliabilityRequireAlerts(t, alerts, true)
}

// Numeric partial output from a failed SCARD is unknown; independently valid
// slow-node evidence survives and does not inherit an invented block count.
func TestReliabilityPipelineAuthorityNativeScardFailuresPreserveLatency(t *testing.T) {
	for _, status := range []int{1, 124, 126, 127, 137} {
		alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{
			blocks: "9", blockStatus: status, latency: "0.1 20.0 14.2 98",
		})
		redisReliabilityRequireAlerts(t, alerts, true, "redis-reliability.example:6382")
		for _, alert := range alerts {
			if alert.Class == "reliability-pipeline-degraded" && strings.Contains(alert.Observed, "blocks=") {
				t.Fatal("failed SCARD contributed a block count to a valid latency finding")
			}
		}
	}
}

// A failed latency child cannot hide behind the normalization pipeline, even
// when its own exit is 124. The independent cluster backlog remains reportable.
func TestReliabilityPipelineAuthorityNativeLatencyFailuresPreserveBacklog(t *testing.T) {
	for _, status := range []int{1, 124, 126, 127, 137} {
		alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{
			blocks: "9", latency: "0.1 20.0 14.2 98", latencyStatus: status,
		})
		redisReliabilityRequireAlerts(t, alerts, true, "redis-reliability.example")
		for _, alert := range alerts {
			if alert.Class == "reliability-pipeline-degraded" && !strings.Contains(alert.Observed, "nodes_sampled=1 nodes_configured=2") {
				t.Fatal("backlog evidence overstated latency coverage")
			}
		}
	}
}

// The sampling owner may stop a still-running latency stream at its bounded
// deadline; retained complete samples remain authoritative at the threshold.
func TestReliabilityPipelineAuthorityNativeTimeoutCutoffRetainsSamples(t *testing.T) {
	alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{
		blocks: "0", latency: "0.1 10.0 10.0 98\r", cutoff: true,
	})
	redisReliabilityRequireAlerts(t, alerts, false)
}

// Timeout without a complete positive sample count is absence, not zero latency.
func TestReliabilityPipelineAuthorityNativeTimeoutCutoffNeedsSamples(t *testing.T) {
	for _, latency := range []string{"", "0 0 0 0", "synthetic-private-sentinel\n0 1 0.3 98"} {
		alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{blocks: "0", latency: latency, cutoff: true})
		redisReliabilityRequireAlerts(t, alerts, true)
	}
}

// Explicit zero is a healthy empty discovery set; threshold edges retain the
// existing five-block and ten-millisecond warning policy.
func TestReliabilityPipelineAuthorityNativeHealthyAndThresholds(t *testing.T) {
	for _, test := range []struct {
		blocks  string
		latency string
		targets []string
	}{
		{blocks: "0", latency: "0 0 0 1"},
		{blocks: "0\n", latency: "0 0 0 1\n"},
		{blocks: "5", latency: "0 10 10 98"},
		{blocks: "0", latency: "min: 0, max: 2, avg: 0.31 (98 samples)"},
		{blocks: "0", latency: "0 2 0.5 20\r0 2 0.3 98\r"},
		{blocks: "6", latency: "0 10 10 98", targets: []string{"redis-reliability.example"}},
		{blocks: "0", latency: "0 20 10.1 98", targets: []string{"redis-reliability.example:6382"}},
		{blocks: "6", latency: "0 20 10.1 98", targets: []string{"redis-reliability.example", "redis-reliability.example:6382"}},
	} {
		alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{blocks: test.blocks, latency: test.latency})
		redisReliabilityRequireAlerts(t, alerts, false, test.targets...)
	}
}

// Errors, partial records and arbitrary text stay inside their encoded source
// body. Neither an injected frame nor a numeric last line creates authority.
func TestReliabilityPipelineAuthorityNativeMalformedBodiesArePrivate(t *testing.T) {
	for _, blocks := range []string{"", "NOAUTH synthetic-private-sentinel", "9\nsynthetic-private-sentinel", "0\n\n", "-1", "+9", "9.0", "9e0", "NaN", strings.Repeat("9", 100), "0\ncomplete\t2"} {
		alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{blocks: blocks, latency: "0 1 0.3 98"})
		redisReliabilityRequireAlerts(t, alerts, true)
	}
	for _, latency := range []string{"", "synthetic-private-sentinel\n0 1 0.3 98", "0 1 0.3 98\nsynthetic-private-sentinel", "0 1 NaN 98", "0 1 Inf 98", "0 1 -1 98", "bad bad 0.3 98", "0 1 0.3 0", "0 1 0.3 -1", "0 1 0.3 1.5", "0 1 0.3 9e1", "0 1 0.3 " + strings.Repeat("9", 100), "0 1 0.3 98 extra", "0 1 2 98", "2 1 0.3 98", "0 1 0.3 98\nreliability_exit=0"} {
		alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{blocks: "0", latency: latency})
		redisReliabilityRequireAlerts(t, alerts, true)
	}
}

// Even a normalizer emitting plausible partial output must complete before its
// result is admitted; its failure cannot manufacture either independent metric.
func TestReliabilityPipelineAuthorityNativeEncodingFailureIsUnknown(t *testing.T) {
	alerts := redisReliabilityNativeAlerts(t, redisReliabilityNativeCase{
		blocks: "9", latency: "0.1 20.0 14.2 98", encodingFailed: true,
	})
	redisReliabilityRequireAlerts(t, alerts, true)
}

// Shell substitution must not delete invalid source bytes before numeric
// validation. Streaming encoding preserves each NUL for the actual parser.
func TestReliabilityPipelineAuthorityNativeNulCannotBecomeNumeric(t *testing.T) {
	for _, test := range []redisReliabilityNativeCase{
		{blocks: "9", latency: "0 1 0.3 98", blocksNul: true},
		{blocks: "0", latency: "0 1 0.3 98", latencyNul: true},
	} {
		alerts := redisReliabilityNativeAlerts(t, test)
		redisReliabilityRequireAlerts(t, alerts, true)
	}
}

// Reconcile exact configured identities independently of transport success.
// Valid backlog and sibling latency positives survive incomplete framing.
func TestReliabilityPipelineAuthorityConfiguredPortCensus(t *testing.T) {
	block := "blocks\tOQ==\t0\t0\n"
	zeroBlock := "blocks\tMA==\t0\t0\n"
	slow := redisReliabilityTestLatencyRow(6380, "0 20 14.2 98")
	healthy := redisReliabilityTestLatencyRow(6382, "0 1 0.3 98")
	complete := "complete\t2\n"
	for _, test := range []struct {
		name    string
		output  string
		targets []string
	}{
		{name: "missing port", output: block + slow + complete, targets: []string{"redis-reliability.example", "redis-reliability.example:6380"}},
		{name: "duplicate substitutes for missing", output: block + slow + slow + complete, targets: []string{"redis-reliability.example"}},
		{name: "duplicate invalidates complete port", output: zeroBlock + slow + healthy + slow + complete},
		{name: "malformed duplicate invalidates port", output: zeroBlock + slow + healthy + "latency\t6380\n" + complete},
		{name: "duplicate after terminal invalidates port", output: zeroBlock + slow + healthy + complete + slow},
		{name: "foreign substitutes for missing", output: zeroBlock + healthy + redisReliabilityTestLatencyRow(6381, "0 20 14.2 98") + complete},
		{name: "foreign alongside complete", output: zeroBlock + slow + healthy + redisReliabilityTestLatencyRow(6381, "0 20 14.2 98") + complete, targets: []string{"redis-reliability.example:6380"}},
		{name: "raw identity", output: zeroBlock + healthy + "latency\tsynthetic-private-sentinel\tMA==\t0\t0\n" + complete},
		{name: "missing terminal", output: block + slow + healthy, targets: []string{"redis-reliability.example", "redis-reliability.example:6380"}},
		{name: "wrong terminal count", output: block + slow + healthy + "complete\t1\n", targets: []string{"redis-reliability.example", "redis-reliability.example:6380"}},
		{name: "duplicate terminal", output: block + slow + healthy + complete + complete, targets: []string{"redis-reliability.example", "redis-reliability.example:6380"}},
		{name: "missing block source", output: slow + healthy + complete, targets: []string{"redis-reliability.example:6380"}},
		{name: "duplicate block source", output: block + block + slow + healthy + complete, targets: []string{"redis-reliability.example:6380"}},
		{name: "block survives missing all latencies", output: block + complete, targets: []string{"redis-reliability.example"}},
		{name: "invalid encoded body", output: zeroBlock + "latency\t6380\tsynthetic-private-sentinel\t0\t0\n" + healthy + complete},
		{name: "oversized encoded body", output: zeroBlock + "latency\t6380\t" + strings.Repeat("A", 64*1024+4) + "\t0\t0\n" + healthy + complete},
		{name: "missing native exit", output: zeroBlock + "latency\t6380\t" + base64.StdEncoding.EncodeToString([]byte("0 20 14.2 98")) + "\t0\t0\n" + healthy + complete},
	} {
		source := &syntheticSource{hostFn: func(HostSettings, string) (string, error) { return test.output, nil }}
		settings := syntheticSettings(source)
		settings.Hosts[1].Name = "redis-reliability.example"
		settings.Hosts[1].RedisNodePorts = []int{6380, 6382}
		alerts, err := NewReliabilityPipelineSignal().Run(context.Background(), settings)
		if err != nil {
			t.Fatalf("%s: framed observation failed: %v", test.name, err)
		}
		redisReliabilityRequireAlerts(t, alerts, true, test.targets...)
		for _, alert := range alerts {
			if test.name == "block survives missing all latencies" && alert.Class == "reliability-pipeline-degraded" &&
				(!strings.Contains(alert.Observed, "latency_observed=false") || strings.Contains(alert.Observed, "max_latency_ms=")) {
				t.Fatal("missing latency was reported as a measured zero maximum")
			}
		}
	}
}

// Parser errors are fixed classifications, with no raw source diagnostics.
func TestReliabilityPipelineAuthorityLatencyParserRejectsUnprovenNumbers(t *testing.T) {
	for _, output := range []string{"synthetic-private-sentinel 1 0.3 98", "0 1 0.3 0", "0 1 0.3 +98", "0 1 NaN 98", "0 1 2 98", "0 1 0.3 98\nsynthetic-private-sentinel", "synthetic-private-sentinel\n0 1 0.3 98", "0 1 0.3 98 extra"} {
		_, err := parseRedisLatency(output)
		if err == nil {
			t.Fatal("latency parser accepted an unproven numeric sample")
		}
		if strings.Contains(err.Error(), "synthetic-private-sentinel") {
			t.Fatal("latency parser leaked a raw source diagnostic")
		}
	}
}
