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
	if finding.frame != "site=settle" || !strings.Contains(finding.observed, "frame=site=settle") {
		t.Fatalf("net-escrow alert lost its structured mutation site: frame=%q observed=%q", finding.frame, finding.observed)
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
