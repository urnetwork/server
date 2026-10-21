// Synthetic tail boundaries exercise the actual reader and native lifecycle;
// no retained production output, identity or SQL is a fixture.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Independently encode the public compact protocol, not its production parser.
func pgTailFixtureLine(nonce string, sequence int) string {
	code := "wWcCaAwWcCaAcCbBhH"[sequence]
	index := 10
	if sequence >= 6 {
		index = 11
	}
	if sequence >= 12 {
		index = 12
	}
	return fmt.Sprintf("mpg3 %s %02d %c %02d\n", nonce, sequence, code, index)
}

// Interleave synthetic complete JSON responses with actual psql marker lines.
func pgTailFixtureOutput(t *testing.T, count int) (string, string) {
	t.Helper()
	frames := pgSampleTestFrames(time.Now().UTC().Truncate(time.Second))
	prefix := pgSampleTestEncode(t, frames[:12])
	original := pgSampleTestEncode(t, frames)
	for sequence := 0; sequence < count; sequence++ {
		line := strings.Replace(pgTailFixtureLine(pgTransportFixtureNonce, sequence), "mpg3 ", "monitor-pg-tail ", 1)
		prefix += line
		if sequence == 4 {
			prefix += pgSampleTestEncode(t, frames[12:13])
		}
		if sequence == 10 {
			prefix += pgSampleTestEncode(t, frames[13:14])
		}
		if sequence == 14 {
			prefix += pgSampleTestEncode(t, frames[14:15])
		}
		if sequence == 16 {
			prefix += pgSampleTestEncode(t, frames[15:16])
		}
	}
	return prefix, original
}

// Force the existing reader failure boundary after an exact consumed marker;
// the child cannot pass this barrier by scheduler timing or a short timeout.
func pgTailReaderDeadline(t *testing.T, count int, phase string, index, frames int) {
	t.Helper()
	prefix, _ := pgTailFixtureOutput(t, count)
	quoted, _ := json.Marshal(prefix)
	const boundary = "    tail_sequence+=1\n"
	if strings.Count(pgQuerySampleProgram, boundary) != 1 {
		t.Fatal("reader owner boundary changed")
	}
	program := strings.Replace(pgQuerySampleProgram, boundary, boundary+fmt.Sprintf("    if tail_sequence==%d:raise Failure('child_process','owner_deadline')\n", count), 1)
	stub := "import signal\nsys.stdin.read()\nsys.stdout.write(" + string(quoted) + ");sys.stdout.flush()\nsignal.pause()\n"
	out, stderr, err := runPgSampleProgramOutputFixture(t, program, stub, pgSampleProgramInput(t))
	failure, valid := parsePgSampleSourceFailure(out)
	tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
	old := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
	if err == nil || !valid || failure.Cause != "owner_deadline" || tail.Status != "valid" || tail.Phase != phase || tail.Edge != "entered" || tail.Index == nil || *tail.Index != index || old.Frames == nil || *old.Frames != frames {
		t.Fatal("partial deadline lost independent tail and completed-frame authority")
	}
	if strings.Contains(out, `"kind":"activity"`) || len(stderr) > 4096 {
		t.Fatal("partial source was published or diagnostic cap changed")
	}
}

func TestPgTailBoundaryPartialTenWaitDeadline(t *testing.T) {
	pgTailReaderDeadline(t, 1, "sample_wait", 10, 10)
}

func TestPgTailBoundaryPartialElevenActivityDeadline(t *testing.T) {
	pgTailReaderDeadline(t, 11, "activity", 11, 11)
}

// A completed source must preserve every byte after removing only its fixed
// diagnostic markers, with both legacy progress and all twelve samples intact.
func TestPgTailBoundaryCompleteTwelvePreservesSource(t *testing.T) {
	input, original := pgTailFixtureOutput(t, 18)
	quoted, _ := json.Marshal(input)
	out, stderr, err := runPgSampleProgramOutputFixture(t, pgQuerySampleProgram, "sys.stdin.read()\nsys.stdout.write("+string(quoted)+")\n", pgSampleProgramInput(t))
	tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
	old := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
	receipt, parseErr := parsePgQuerySample(out, time.Now())
	if err != nil || out != original || parseErr != nil || !receipt.Complete || receipt.Samples != 12 || tail.Status != "valid" || tail.Phase != "history_end" || tail.Edge != "returned" || tail.Records != 18 || old.Frames == nil || *old.Frames != 12 || len(stderr) != 2624 {
		t.Fatal("tail diagnostics changed successful sampling or legacy/cap authority")
	}
}

// Legacy, unsupported, truncated, corrupt and foreign lanes cannot invent a
// current client boundary. No malformed string or nonce enters the projection.
func TestPgTailBoundaryUnknownAndPrivacy(t *testing.T) {
	nonce := pgTransportFixtureNonce
	base := pgTailFixtureLine(nonce, 0)
	for _, item := range []struct{ raw, status string }{
		{raw: "", status: "absent"},
		{raw: pgCompletedProgressStream(nonce, 12, true), status: "absent"},
		{raw: pgTailFixtureLine(strings.Repeat("a", 32), 0), status: "absent"},
		{raw: strings.TrimSuffix(base, "\n"), status: "invalid"},
		{raw: base + base, status: "invalid"},
		{raw: pgTailFixtureLine(nonce, 1), status: "invalid"},
		{raw: strings.Replace(base, " w ", " ! ", 1), status: "invalid"},
		{raw: base + strings.Repeat("x", 4096), status: "capped"},
	} {
		value := parsePgSampleTailBoundary(item.raw, nonce)
		raw, err := json.Marshal(value)
		if err != nil || value.Status != item.status || value.Index != nil || strings.Contains(string(raw), nonce) {
			t.Fatal("unsupported tail acquired false or private authority")
		}
	}
}

// Generate the source once and remove only its eighteen metacommand lines;
// old SQL bytes and the existing waits/query counts must remain exact.
func TestPgTailBoundarySqlWorkloadConservation(t *testing.T) {
	sql := pgQuerySampleSQL("fixture")
	var retained []string
	markers := 0
	for _, line := range strings.SplitAfter(sql, "\n") {
		if strings.HasPrefix(line, "\\echo monitor-pg-tail ") {
			markers++
			continue
		}
		retained = append(retained, line)
	}
	plain := strings.Join(retained, "")
	if markers != 18 || strings.Count(plain, "SELECT pg_sleep(2);") != 11 || strings.Count(plain, "SELECT pg_stat_clear_snapshot();") != 13 || strings.Count(plain, "'kind','activity'") != 12 || pgQuerySampleCount != 12 || pgQuerySampleBudget != 40*time.Second || pgSampleMinimumWaitBudget != 22*time.Second {
		t.Fatal("diagnostics changed the bounded SQL workload")
	}
}

// Actual EOF, copy error and unavailable copy paths are separate authorities.
func TestPgTailBoundaryNativeEofAndUnknown(t *testing.T) {
	var out pgSampleNativeBuffer
	n, err := io.Copy(&out, strings.NewReader("synthetic-source"))
	// strings.Reader.WriteTo may use Write directly: that must remain unknown.
	if err != nil || n != 16 || out.String() != "synthetic-source" || !out.eofAt.IsZero() {
		t.Fatal("Write inferred EOF")
	}
	var observed pgSampleNativeBuffer
	_, err = observed.ReadFrom(strings.NewReader("synthetic-source"))
	if err != nil || observed.eofAt.IsZero() {
		t.Fatal("owned EOF was not measured")
	}
	observation := &pgSampleTransportObservation{localOrigin: time.Now().Add(-time.Second)}
	now := time.Now()
	observation.recordNativeLifecycle(now, now, now, now, &out, &out)
	wire := observation.receipt.NativeLifecycle
	if wire.ClockStatus != "ok" || wire.StdoutEofNs != nil || wire.StderrEofNs != nil {
		t.Fatal("unknown copy path invented EOF")
	}
	observation.recordNativeLifecycle(now, now.Add(-time.Second), now, now, &out, &out)
	if observation.receipt.NativeLifecycle.ClockStatus != "invalid" || observation.receipt.NativeLifecycle.StartEnteredNs != nil {
		t.Fatal("invalid clocks supplied causal timing")
	}
}

// Callback results alone never enter the actual native/EOF authority lane.
func TestPgTailBoundaryCallbackKeepsNativeUnknown(t *testing.T) {
	ctx, observer := observePgSampleTransport(context.Background())
	_ = ctx
	observer.stage("ssh_start", "unknown")
	if observer.receipt.NativeLifecycle != nil || observer.receipt.TailBoundary != nil || observer.receipt.Joined {
		t.Fatal("callback-only path invented native or remote markers")
	}
}

// The private local helper owns no descendants and reports its barrier only
// after fixed partial/tail records have been written. The parent owns cancel.
func TestPgTailBoundarySshHelper(t *testing.T) {
	if os.Getenv("MONITOR_PG_TAIL_CHILD") != "1" {
		return
	}
	var input struct {
		Nonce string `json:"transport_nonce"`
	}
	if json.NewDecoder(io.LimitReader(os.Stdin, 262145)).Decode(&input) != nil || len(input.Nonce) != 32 {
		os.Exit(91)
	}
	fmt.Fprint(os.Stderr, pgCompletedProgressStream(input.Nonce, 10, false)+pgTailFixtureLine(input.Nonce, 0))
	ready, err := os.OpenFile(os.Getenv("MONITOR_PG_COMPLETED_PROGRESS_READY"), os.O_WRONLY, 0)
	if err != nil {
		os.Exit(92)
	}
	_, err = fmt.Fprintln(ready, os.Getpid(), input.Nonce)
	ready.Close()
	if err != nil {
		os.Exit(93)
	}
	time.Sleep(time.Hour)
	os.Exit(94)
}

// Reuse the existing native fixture's source/env setup, replacing only its
// exclusively local launcher with this test's helper before its first call.
func pgTailNativeFixture(t *testing.T) *probeEnv {
	t.Helper()
	env := pgCompletedNativeFixture(t, "cancel")
	t.Setenv("MONITOR_PG_TAIL_CHILD", "1")
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal("fixture lacks local launcher")
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal("fixture lacks own executable")
	}
	launcher := "#!/bin/sh\nexec " + shellSingleQuote(binary) + " -test.run '^TestPgTailBoundarySshHelper$' -test.count=1\n"
	if os.WriteFile(ssh, []byte(launcher), 0700) != nil {
		t.Fatal("fixture launcher was not replaced before execution")
	}
	return env
}
func TestPgTailBoundaryNativeCanceledJoinAndEof(t *testing.T) {
	env := pgTailNativeFixture(t)
	readyPath := filepath.Join(t.TempDir(), "ready")
	if syscall.Mkfifo(readyPath, 0600) != nil {
		t.Fatal("native progress barrier was not created")
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	t.Setenv("MONITOR_PG_COMPLETED_PROGRESS_READY", readyPath)
	ctx, cancel := context.WithTimeout(t.Context(), pgQuerySampleBudget)
	defer cancel()
	done := make(chan error, 1)
	var findings []finding
	go func() {
		var err error
		findings, err = (pgQuerySampleProbe{}).check(ctx, env)
		done <- err
	}()
	type readyProof struct {
		pid   int
		nonce string
	}
	barrier := make(chan readyProof, 1)
	go func() {
		var proof readyProof
		if _, err := fmt.Fscan(ready, &proof.pid, &proof.nonce); err == nil {
			barrier <- proof
		}
	}()
	var child readyProof
	select {
	case child = <-barrier:
		if child.pid <= 0 || len(child.nonce) != 32 {
			t.Fatal("native progress barrier lacked process ownership")
		}
	case <-ctx.Done():
		<-done
		t.Fatal("native child never reached its partial-progress barrier")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("canceled source did not retain its normal failure receipt")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled native child did not join")
	}
	if err := syscall.Kill(child.pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("native progress child survived its owned cancellation")
	}
	receipt, raw := pgTransportReadControlReceipt(t, env)
	proof := receipt.TransportObservation
	if receipt.Complete || receipt.Samples != 0 || receipt.SourceFailure == nil || receipt.SourceFailure.Phase != "host_transport" || receipt.SourceFailure.Cause != observationErrorClassCanceled || proof.Spawn != "started" || !proof.Joined || proof.NativeExitCode != nil || proof.OwnerDeadlineExceeded || strings.Contains(string(raw), child.nonce) {
		t.Fatal("partial progress changed canceled native/source authority or privacy")
	}
	wire := pgCompletedProgressReceipt(t, raw)
	if wire.Phase != "activity" || wire.Status != "valid" || wire.Frames == nil || *wire.Frames != 10 {
		t.Fatal("native cancellation discarded completed-frame progress from its receipt")
	}
	if len(findings) != 1 || findings[0].class != "pg-query-sample-unavailable" || findings[0].tier != tierWarn || findings[0].sustain != 1 {
		t.Fatal("partial progress weakened the first-miss cannot-observe finding")
	}
	var extended struct {
		Transport struct {
			Tail   *pgSampleTailBoundary    `json:"tail_boundary"`
			Native *pgSampleNativeLifecycle `json:"native_lifecycle"`
		} `json:"transport_observation"`
	}
	if json.Unmarshal(raw, &extended) != nil || extended.Transport.Tail == nil || extended.Transport.Tail.Phase != "sample_wait" || extended.Transport.Tail.Edge != "entered" || extended.Transport.Tail.Index == nil || *extended.Transport.Tail.Index != 10 || extended.Transport.Native == nil || extended.Transport.Native.ClockStatus != "ok" || extended.Transport.Native.StartReturnedNs == nil || extended.Transport.Native.WaitReturnedNs == nil || extended.Transport.Native.StdoutEofNs == nil || extended.Transport.Native.StderrEofNs == nil {
		t.Fatal("canceled native join lost tail or actual pipe EOF authority")
	}
	state, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), time.Now())
	if err != nil || state.Outcome == "complete" || state.NextEligibleAt.Sub(state.TerminalAt) != pgQuerySampleCadence {
		t.Fatal("partial progress weakened the spent-attempt cadence")
	}
}

// A markerless legacy producer remains usable without inventing tail entry.
func TestPgTailBoundaryLegacyHealthyKeepsTailUnknown(t *testing.T) {
	frames := pgSampleTestEncode(t, pgSampleTestFrames(time.Now().UTC().Truncate(time.Second)))
	quoted, _ := json.Marshal(frames)
	out, stderr, err := runPgSampleProgramOutputFixture(t, pgQuerySampleProgram, "sys.stdin.read()\nsys.stdout.write("+string(quoted)+")\n", pgSampleProgramInput(t))
	old := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
	tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
	if err != nil || out != frames || old.Frames == nil || *old.Frames != 12 || tail.Status != "absent" || tail.Index != nil {
		t.Fatal("legacy source lost compatibility or acquired false tail authority")
	}
}

// Invalid diagnostics cannot be stripped into an otherwise healthy source.
func TestPgTailBoundaryBrokenMarkerCannotCertifySource(t *testing.T) {
	input, _ := pgTailFixtureOutput(t, 18)
	input = strings.Replace(input, " 00 w 10\n", " 00 a 10\n", 1)
	quoted, _ := json.Marshal(input)
	out, stderr, _ := runPgSampleProgramOutputFixture(t, pgQuerySampleProgram, "sys.stdin.read()\nsys.stdout.write("+string(quoted)+")\n", pgSampleProgramInput(t))
	receipt, err := parsePgQuerySample(out, time.Now())
	tail := parsePgSampleTailBoundary(stderr, pgTransportFixtureNonce)
	if err == nil || receipt.Complete || tail.Status != "invalid" || tail.Index != nil {
		t.Fatal("corrupt diagnostic was stripped into healthy source authority")
	}
	raw, err := json.Marshal(tail)
	if err != nil || strings.Contains(string(raw), pgTransportFixtureNonce) {
		t.Fatal("corrupt marker leaked private correlation")
	}
}

// A failed native Start returns measured Start points, not Wait or EOF facts.
func TestPgTailBoundaryUnavailableNativeHasNoWaitOrEof(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), pgQuerySampleBudget)
	defer cancel()
	ctx, observation := observePgSampleTransport(ctx)
	_, _, err := runSSHCommand(ctx, nil, "")
	wire := observation.receipt.NativeLifecycle
	if err == nil || observation.receipt.Spawn != "start_failed" || observation.receipt.Joined || wire == nil || wire.ClockStatus != "ok" || wire.StartEnteredNs == nil || wire.StartReturnedNs == nil || wire.WaitEnteredNs != nil || wire.WaitReturnedNs != nil || wire.StdoutEofNs != nil || wire.StderrEofNs != nil {
		t.Fatal("native start failure invented downstream authority")
	}
}
