package monitor

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeStream builds the tailer's injected stream around an arbitrary shell
// script. Tests that need runner.warpctlStream's stdout/stderr boundary use the
// real runner below.
func fakeStream(script string) func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
	return func(ctx context.Context) (*exec.Cmd, io.ReadCloser, error) {
		cmd := exec.CommandContext(ctx, "sh", "-c", script)
		pr, pw, err := os.Pipe()
		if err != nil {
			return nil, nil, err
		}
		cmd.Stdout = pw
		cmd.Stderr = pw
		if err := cmd.Start(); err != nil {
			pr.Close()
			pw.Close()
			return nil, nil, err
		}
		pw.Close()
		return cmd, pr, nil
	}
}

// A Loki failure belongs to the local observation transport, not to the
// remote service whose logs were requested. The production failure was an
// exhausted 502 retry whose stderr included `panic:`; when warpctlStream
// merged stderr with stdout, every standing service tailer classified that as
// a page-tier service panic.
func TestWarpctlStreamDoesNotClassifyTransportStderr(t *testing.T) {
	binDir := t.TempDir()
	warpctlPath := filepath.Join(binDir, "warpctl")
	script := `#!/bin/sh
printf '%s\n' '[edge-0][taskworker][g1][cid:abc] ordinary remote log line'
printf '%s\n' 'panic: Loki query error (502): Bad Gateway' >&2
`
	if err := os.WriteFile(warpctlPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	tailer := newLogTailer("taskworker", &probeEnv{
		cfg:    &monitorConfig{env: "main"},
		runner: newRunner(&monitorConfig{env: "main"}),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tailer.tailOnce(ctx); err != nil {
		t.Fatalf("tailOnce: %v", err)
	}

	if finding := findingByClass(t, tailer.drainWindow(), "panic"); !finding.healthy {
		t.Fatalf("local warpctl stderr became a remote panic finding: %+v", finding)
	}
}

// Grafana's Loki/Mimir query engine logs the complete query at info level.
// Searching for an error signature therefore echoes that signature into the
// grafana service log; the standing grafana tailer must not feed the monitor's
// own observation back into the error classifier.
func TestGrafanaQueryEchoCannotCreateLogAlerts(t *testing.T) {
	tailer := newLogTailer("grafana", nil)
	redisEcho := `[by-us-fmt-5-edge-4][grafana][g1][cid:test][2026-08-31T15:16:29Z]level=info ts=2026-08-31T15:16:29Z caller=roundtrip.go:412 org_id=fake msg=\"executing query\" type=range query=\"{env=\\\"main\\\"} |= \\\"[redis][ttl]\\\"\" query_hash=1`
	panicEcho := `[fireside][grafana][g1][cid:test][2026-08-31T15:16:30Z]level=info ts=2026-08-31T15:16:30Z caller=engine.go:274 component=querier org_id=fake msg=\"executing query\" query=\"{env=\\\"main\\\"} |= \\\"panic:\\\"\" query_hash=2`
	metricsEcho := `[by-us-fmt-5-edge-4][grafana][g1][cid:test][2026-08-31T15:23:53Z]level=info ts=2026-08-31T15:23:53Z caller=metrics.go:285 component=querier org_id=fake latency=fast query=\"{env=\\\"main\\\", service=\\\"api\\\"} |= \\\"[redis][ttl]\\\"\" query_hash=3 query_type=filter range_type=range duration=2ms status=200 returned_lines=0`
	tailer.classify(redisEcho)
	tailer.classify(metricsEcho)
	for range 5 {
		tailer.classify(panicEcho)
	}

	findings := tailer.drainWindow()
	for _, class := range []string{"redis-ttl-suspect", "panic", "novel"} {
		if finding := findingByClass(t, findings, class); !finding.healthy {
			t.Fatalf("grafana query echo became a %s finding: %+v", class, finding)
		}
	}

	// The exclusion is deliberately narrow: an actual Grafana warning and the
	// same text from a non-Grafana service must remain visible.
	grafanaWarning := newLogTailer("grafana", nil)
	grafanaWarning.classify(`[fireside][grafana][g1][cid:test]level=warn caller=redis.go:89 [redis][ttl] suspicious ttl`)
	if finding := findingByClass(t, grafanaWarning.drainWindow(), "redis-ttl-suspect"); finding.healthy {
		t.Fatal("real Grafana TTL warning was hidden with query metadata")
	}
	apiEcho := newLogTailer("api", nil)
	apiEcho.classify(redisEcho)
	if finding := findingByClass(t, apiEcho.drainWindow(), "redis-ttl-suspect"); finding.healthy {
		t.Fatal("query-echo exclusion leaked outside the grafana service")
	}
}

func TestNetEscrowAlertRetainsSiteAndRedactsEntityIDs(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	tailer.classify("[netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-21434368")
	finding := findingByClass(t, tailer.drainWindow(), "netescrow-negative")
	if finding.healthy {
		t.Fatal("one negative mirror must alert")
	}
	if !strings.Contains(finding.evidence, "after settle") || !strings.Contains(finding.evidence, "balance=<id> contract=<id>") {
		t.Fatalf("redacted evidence lost its useful site: %q", finding.evidence)
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("net-escrow alert leaked an entity id: %q", finding.evidence)
	}
	if !strings.Contains(finding.evidence, "clamp_marker=absent") {
		t.Fatalf("net-escrow alert did not distinguish an absent clamp marker from truncation: %q", finding.evidence)
	}
	if finding.frame != "site=settle" || !strings.Contains(finding.observed, "frame=site=settle") {
		t.Fatalf("net-escrow alert lost its structured mutation site: frame=%q observed=%q", finding.frame, finding.observed)
	}
}

// The production line's clamp marker follows enough metadata and identifiers
// to fall outside the generic sample limit. Preserve it because it separates
// an atomically contained current-binary aftermath from legacy behavior.
func TestNetEscrowAlertPreservesClampMarkerBeyondSampleLimit(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	line := "[by-us-fmt-5-edge-4][taskworker][g1][cid:16a73fdaca8f][E][2026-08-31T11:30:52.458075-05:00][subscription_model.go:748][netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-10066 clamped_to=0"
	if strings.Index(line, "clamped_to=0") <= 200 {
		t.Fatal("fixture no longer places the clamp marker beyond the generic sample limit")
	}
	tailer.classify(line)

	finding := findingByClass(t, tailer.drainWindow(), "netescrow-negative")
	if !strings.Contains(finding.evidence, "clamped_to=0") {
		t.Fatalf("net-escrow evidence lost the clamp marker: %q", finding.evidence)
	}
	if strings.Contains(finding.evidence, "clamp_marker=absent") {
		t.Fatalf("present clamp marker was classified absent: %q", finding.evidence)
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("net-escrow clamp evidence leaked an entity id: %q", finding.evidence)
	}
}

func TestNetEscrowAlertSeparatesMutationSites(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.classify("[netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-1")
	tailer.classify("[netescrow]negative counter after quarantine release: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-2")

	findings := tailer.drainWindow()
	frames := map[string]bool{}
	for _, finding := range findings {
		if finding.class == "netescrow-negative" && !finding.healthy {
			frames[finding.frame] = true
			if logIDRe.MatchString(finding.evidence) {
				t.Fatalf("net-escrow alert leaked an entity id: %q", finding.evidence)
			}
		}
	}
	for _, want := range []string{"site=settle", "site=quarantine release"} {
		if !frames[want] {
			t.Fatalf("net-escrow findings frames = %v, missing %q", frames, want)
		}
	}
}

func TestNetEscrowNegativeStormPages(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for range netEscrowNegativePageRate {
		tailer.classify("[netescrow]negative counter after settle: balance=01a04ff7-83b0-1970-2353-4b9ccf6e461d contract=01a05086-db24-dde0-dd4b-cbd20ace42ca result=-10760936105")
	}

	finding := findingByClass(t, tailer.drainWindow(), "netescrow-negative")
	if finding.tier != tierPage {
		t.Fatalf("storm tier = %q, want page: %+v", finding.tier, finding)
	}
	for _, want := range []string{
		"rate=100/min",
		"page_threshold=100/min",
		"frame=site=settle",
	} {
		if !strings.Contains(finding.observed, want) {
			t.Fatalf("storm observation missing %q: %s", want, finding.observed)
		}
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("storm evidence leaked an entity id: %q", finding.evidence)
	}
}

func TestRedisNetEscrowTTLAlertRedactsEntityIDs(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.classify(`[redis][ttl]"expireat" key="{escrow_019c640e-f467-4fa7-177f-d7ca43c33b6f}net" ttl 3139421360s-from-now exceeds 9600h0m0s`)
	finding := findingByClass(t, tailer.drainWindow(), "redis-netescrow-ttl")
	if finding.healthy {
		t.Fatal("one suspect Redis TTL must alert")
	}
	if !strings.Contains(finding.evidence, `"expireat"`) ||
		!strings.Contains(finding.evidence, `{escrow_<id>}net`) {
		t.Fatalf("redacted evidence lost command or key family: %q", finding.evidence)
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("Redis TTL alert leaked an entity id: %q", finding.evidence)
	}
}

func payoutAttemptLogLines(second string, attempt int) (string, string) {
	id := fmt.Sprintf("019f77ae-de17-db98-b22d-%012x", attempt)
	processorLine := fmt.Sprintf(
		`[edge-3][taskworker][g2][cid:test][I][%s.100000Z][circle_client_controller.go:142][circlec]error sending payment: wallet %s: asset amount owned by the wallet is insufficient`,
		second,
		id,
	)
	evaluatorLine := fmt.Sprintf(
		`[edge-3][taskworker][g2][cid:test][I][%s.200000Z][task.go:1930][%s]eval error = asset amount owned by the wallet is insufficient`,
		second,
		id,
	)
	return processorLine, evaluatorLine
}

// Four canonical task attempts in one source second are the exact live shape
// that immediately preceded a Circle 429. The Circle-client copy of each error
// contributes to diagnostic volume but not attempt concurrency, and an exact
// tail replay must not manufacture a fifth attempt.
func TestPayoutRetryMicroburstCountsDistinctTaskAttemptsPerSecond(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	var replay string
	for attempt := 0; attempt < 4; attempt++ {
		processorLine, evaluatorLine := payoutAttemptLogLines("2026-08-31T15:46:23", attempt)
		tailer.classify(processorLine)
		tailer.classify(evaluatorLine)
		if attempt == 0 {
			replay = evaluatorLine
		}
	}
	tailer.classify(replay)

	finding := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst")
	if finding.healthy {
		t.Fatal("four same-second payout attempts did not create a microburst finding")
	}
	for _, want := range []string{
		"peak_task_attempts_per_second=4",
		"threshold=4/s",
		"task_attempts=4",
		"diagnostic_lines=9",
		"exact-replay-deduplicated task evaluator lines",
		"separate from the operational liquidity alert",
		"commit 70b0d269 or later only to older blocks",
		"peak_task_attempts_per_second below 4",
		"[<id>]eval error",
	} {
		if combined := finding.observed + "\n" + finding.evidence + "\n" + finding.context + "\n" + finding.action + "\n" + finding.verify; !strings.Contains(combined, want) {
			t.Fatalf("microburst finding missing %q: %+v", want, finding)
		}
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("microburst evidence leaked a payment id: %q", finding.evidence)
	}
}

// A minute can begin with a sparse attempt and peak later. The alert must
// retain a representative line from the actual peak second, not the first
// canonical line seen in the window (the live 16:31 window peaked at five at
// 16:31:47 but previously rendered a 16:31:33 sample).
func TestPayoutRetryMicroburstSampleComesFromPeakSecond(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	_, first := payoutAttemptLogLines("2026-08-31T16:31:33", 1)
	tailer.classify(first)
	for attempt := 10; attempt < 15; attempt++ {
		_, peak := payoutAttemptLogLines("2026-08-31T16:31:47", attempt)
		tailer.classify(peak)
	}

	finding := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst")
	for _, want := range []string{
		"peak_task_attempts_per_second=5",
		"peak_source_second=2026-08-31T16:31:47",
		"peak source second: 2026-08-31T16:31:47",
		"sample from peak second: [edge-3][taskworker][g2][cid:test][I][2026-08-31T16:31:47",
	} {
		if combined := finding.observed + "\n" + finding.evidence; !strings.Contains(combined, want) {
			t.Fatalf("peak finding missing %q: %+v", want, finding)
		}
	}
	if strings.Contains(finding.evidence, "2026-08-31T16:31:33") {
		t.Fatalf("peak evidence retained the first sparse second: %q", finding.evidence)
	}
}

func TestPaymentProcessorRateLimitCountsOneLogicalEventPerDiagnosticPair(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	id := "019f77ae-de17-db98-b22d-2642f6f67594"
	providerLine := "[edge-1][taskworker][g2][cid:test][I][2026-08-31T16:31:47.578203Z][circle_client_controller.go:142][circlec]error sending payment: Bad status: 429 Too Many Requests {\"code\":5,\"message\":\"API rate limit error\",\"payment_id\":\"" + id + "\"}"
	evaluatorLine := "[edge-1][taskworker][g2][cid:test][I][2026-08-31T16:31:47.578638Z][task.go:1930][" + id + "]eval error = Bad status: 429 Too Many Requests {\"code\":5,\"message\":\"API rate limit error\"}"
	tailer.classify(providerLine)
	tailer.classify(evaluatorLine)
	tailer.classify(evaluatorLine)

	finding := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit")
	for _, want := range []string{
		"rate=3/min",
		"processor_rate_limit_events=1",
		"diagnostic_lines=3",
		"canonical_source=exact-replay-deduplicated-task-evaluator",
		"logical event count: 1 exact-replay-deduplicated task evaluator line(s) from 3 diagnostic line(s)",
	} {
		if combined := finding.observed + "\n" + finding.evidence; !strings.Contains(combined, want) {
			t.Fatalf("processor rate-limit finding missing %q: %+v", want, finding)
		}
	}
	if logIDRe.MatchString(finding.evidence) {
		t.Fatalf("processor rate-limit evidence leaked an entity id: %q", finding.evidence)
	}

	// A reconnect can replay the final evaluator line after the cadence drain.
	// Preserve its diagnostic visibility but do not manufacture another logical
	// provider event in the next window.
	tailer.classify(evaluatorLine)
	replay := findingByClass(t, tailer.drainWindow(), "payment-processor-rate-limit")
	if !strings.Contains(replay.observed, "processor_rate_limit_events=0") ||
		!strings.Contains(replay.observed, "diagnostic_lines=1") {
		t.Fatalf("cross-window replay manufactured a logical event: %+v", replay)
	}
}

// Minute volume is the liquidity/retry-amplification signal, but it is not a
// synchronized microburst when canonical attempts occupy different seconds.
// The subsequent empty window must also resolve a prior burst identity.
func TestPayoutRetryMicroburstRejectsSpreadMinuteAndResets(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	for attempt := 0; attempt < 8; attempt++ {
		second := fmt.Sprintf("2026-08-31T15:47:%02d", attempt)
		processorLine, evaluatorLine := payoutAttemptLogLines(second, attempt)
		tailer.classify(processorLine)
		tailer.classify(evaluatorLine)
	}
	findings := tailer.drainWindow()
	if payout := findingByClass(t, findings, "payout-wallet-insufficient"); payout.healthy {
		t.Fatal("spread minute lost the parent liquidity finding")
	}
	if burst := findingByClass(t, findings, "payout-retry-microburst"); !burst.healthy {
		t.Fatalf("spread minute became a synchronized burst: %+v", burst)
	}
	if burst := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst"); !burst.healthy {
		t.Fatalf("empty window did not resolve burst identity: %+v", burst)
	}
}

// A tail reconnect uses --since=1s and can replay the final source second of
// the prior drain window. Preserve exactly one prior fingerprint window so a
// cadence-boundary reconnect cannot open the same burst twice.
func TestPayoutRetryMicroburstDeduplicatesReplayAcrossDrainBoundary(t *testing.T) {
	tailer := newLogTailer("taskworker", nil)
	lines := make([]string, 0, 4)
	for attempt := 0; attempt < 4; attempt++ {
		_, evaluatorLine := payoutAttemptLogLines("2026-08-31T15:46:33", attempt)
		lines = append(lines, evaluatorLine)
		tailer.classify(evaluatorLine)
	}
	if first := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst"); first.healthy {
		t.Fatal("initial same-second burst was not detected")
	}

	for _, line := range lines {
		tailer.classify(line)
	}
	if replay := findingByClass(t, tailer.drainWindow(), "payout-retry-microburst"); !replay.healthy {
		t.Fatalf("cross-window exact replay manufactured another burst: %+v", replay)
	}
}

// a > 1MB line must cost one counted stream restart, not a dead tailer:
// before the fix the scan loop exited on bufio.ErrTooLong but cmd.Wait()
// blocked forever on the still-writing child and the full pipe.
func TestTailerOversizedLineDoesNotWedge(t *testing.T) {
	tailer := newLogTailer("api", nil)
	// one classifiable line, then a ~2MB single line (overflowing the 1MB
	// scanner buffer), then the child keeps the pipe open forever — the wedge
	// shape.
	tailer.stream = fakeStream(
		`echo "short line"; head -c 2097152 /dev/zero | tr '\0' 'a'; echo; sleep 3600`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct{ err error }
	done := make(chan result, 1)
	go func() {
		done <- result{err: tailer.tailOnce(ctx)}
	}()

	select {
	case r := <-done:
		if r.err != bufio.ErrTooLong {
			t.Fatalf("tailOnce err = %v; want bufio.ErrTooLong", r.err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tailOnce wedged on an oversized line (child not killed before Wait)")
	}

	_, _, scanErrors := tailer.healthSnapshot()
	if scanErrors != 1 {
		t.Fatalf("scanErrorCount = %d; want 1", scanErrors)
	}
}

// a clean stream end (child exits, pipe closes) returns nil so run() resets
// its backoff, and the lines were classified.
func TestTailerCleanStreamEnd(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.stream = fakeStream(`echo "a error b"; echo "plain line"`)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- tailer.tailOnce(ctx)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("tailOnce err = %v; want nil", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("tailOnce did not return after a clean stream end")
	}

	lastLine, _, scanErrors := tailer.healthSnapshot()
	if scanErrors != 0 {
		t.Fatalf("scanErrorCount = %d; want 0", scanErrors)
	}
	if time.Since(lastLine) > time.Minute {
		t.Fatalf("lastLineTime not updated by classify: %s", lastLine)
	}
}

// An exhausted observation-transport request makes warpctl exit nonzero.
// tailOnce must preserve that exit status so run() uses its escalating
// failure backoff; discarding cmd.Wait's error caused every service tailer to
// retry once a second through a Grafana startup outage.
func TestTailerFailedStreamReturnsChildError(t *testing.T) {
	tailer := newLogTailer("api", nil)
	tailer.stream = fakeStream(`exit 2`)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tailer.tailOnce(ctx); err == nil {
		t.Fatal("tailOnce returned nil for a nonzero child exit")
	}
}

func findingByClass(t *testing.T, findings []finding, class string) finding {
	t.Helper()
	for _, f := range findings {
		if f.class == class {
			return f
		}
	}
	t.Fatalf("no finding with class %q in %d findings", class, len(findings))
	return finding{}
}

// the §3.7 tailer self-health thresholds: silent-too-long and restarting-hot
// raise monitor/visibility findings; a live, stable tailer reports healthy.
func TestTailerHealthFindings(t *testing.T) {
	now := time.Now()

	healthy := tailerHealthFindings("api", now, now.Add(-time.Minute), 0, 0)
	if f := findingByClass(t, healthy, "tailer-silent"); !f.healthy {
		t.Fatalf("recent line reported silent: %+v", f)
	}
	if f := findingByClass(t, healthy, "tailer-restarting"); !f.healthy {
		t.Fatalf("no restarts reported hot: %+v", f)
	}

	silent := tailerHealthFindings("api", now, now.Add(-11*time.Minute), 0, 0)
	f := findingByClass(t, silent, "tailer-silent")
	if f.healthy {
		t.Fatal("11 minutes silent must raise a tailer-silent finding")
	}
	if f.probeId != "monitor/visibility" || f.target != "logs/api" {
		t.Fatalf("wrong identity: probeId=%s target=%s", f.probeId, f.target)
	}
	if f := findingByClass(t, silent, "tailer-restarting"); !f.healthy {
		t.Fatalf("silent-only case reported restarting: %+v", f)
	}

	hot := tailerHealthFindings("api", now, now.Add(-time.Minute), tailerHotRestartThreshold, 2)
	if f := findingByClass(t, hot, "tailer-restarting"); f.healthy {
		t.Fatal("restart delta at threshold must raise a tailer-restarting finding")
	}
	calm := tailerHealthFindings("api", now, now.Add(-time.Minute), tailerHotRestartThreshold-1, 0)
	if f := findingByClass(t, calm, "tailer-restarting"); !f.healthy {
		t.Fatalf("restart delta below threshold reported hot: %+v", f)
	}
}

type recordingEmitter struct {
	events []ticketEvent
}

func (self *recordingEmitter) emit(ctx context.Context, ev ticketEvent) error {
	self.events = append(self.events, ev)
	return nil
}

// the novel class carries a varying top shape; if that shape were the ticket
// frame (as it once was), two minutes with different shapes would never
// accumulate the sustain-2 streak and the ticket could never open.
func TestNovelTicketOpensAcrossVaryingShapes(t *testing.T) {
	ctx := context.Background()
	emitter := &recordingEmitter{}
	manager := newTicketManager("test", emitter)
	tailer := newLogTailer("api", nil)

	// minute 1: one novel shape at rate
	for i := 0; i < novelRateThreshold+5; i += 1 {
		tailer.classify(fmt.Sprintf("widget error: alpha failure %d", i))
	}
	findings := tailer.drainWindow()
	novel := findingByClass(t, findings, "novel")
	if novel.healthy {
		t.Fatal("novel lines at rate must produce a broken finding")
	}
	if novel.frame != "" {
		t.Fatalf("novel finding frame = %q; the varying shape must not be identity", novel.frame)
	}
	manager.ingest(ctx, findings)
	for _, ev := range emitter.events {
		if ev.kind == ticketOpen {
			t.Fatalf("ticket opened after one tick despite sustain 2: %+v", ev.t.ticketIdentity)
		}
	}

	// minute 2: a different top shape — the streak must still accumulate
	for i := 0; i < novelRateThreshold+5; i += 1 {
		tailer.classify(fmt.Sprintf("gadget error: beta mode %d", i))
	}
	manager.ingest(ctx, tailer.drainWindow())

	opened := false
	for _, ev := range emitter.events {
		if ev.kind == ticketOpen && ev.t.probeId == "logs/novel" {
			opened = true
		}
	}
	if !opened {
		t.Fatal("two consecutive novel minutes with different top shapes did not open a ticket")
	}
}

// Public endpoints receive bursts of unrelated vulnerability probes. Nginx
// logs every nonexistent path as an error, but many one-off paths are not one
// novel server failure recurring at rate. The novelty threshold is per
// normalized shape, not the sum of unrelated shapes in the minute.
func TestNovelDiverseOneOffShapesDoNotAlert(t *testing.T) {
	tailer := newLogTailer("web", nil)
	for i := 0; i < novelRateThreshold*3; i += 1 {
		path := fmt.Sprintf("%c%c", 'a'+rune(i/26), 'a'+rune(i%26))
		tailer.classify(fmt.Sprintf(
			`2026/08/30 04:08:45 [error] 16#16: *4607 open() "/etc/nginx/html/probe-%s.php" failed (2: No such file or directory)`,
			path,
		))
	}

	novel := findingByClass(t, tailer.drainWindow(), "novel")
	if !novel.healthy {
		t.Fatalf("unrelated one-off web probes produced a novel alert: %+v", novel)
	}
}
