// Independent wire controls compile against the old reader and receipt too.
// Progress is a completed response header, never a retained sample or verdict.
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

// Decode the public projection without depending on the new production field.
type pgCompletedProgressWire struct {
	Phase  string `json:"remote_phase"`
	Status string `json:"progress_status"`
	Frames *int   `json:"remote_sample_frames_observed"`
}

// Exercise the owning parser through its existing native-join handoff. This
// parser-only helper makes no claim that a native process actually ran.
func pgCompletedProgressRead(t *testing.T, stderr, nonce string) pgCompletedProgressWire {
	t.Helper()
	observation := &pgSampleTransportObservation{nonce: nonce}
	observation.joined(nil, stderr)
	raw, err := json.Marshal(observation.receipt)
	var wire pgCompletedProgressWire
	if err != nil || json.Unmarshal(raw, &wire) != nil {
		t.Fatal("completed-frame projection did not encode")
	}
	return wire
}

// The fixture owns only these fixed phase/count records and its private nonce.
func pgCompletedProgressLine(nonce, phase string, count int) string {
	raw, _ := json.Marshal(map[string]any{"schema": 2, "nonce": nonce, "phase": phase, "samples": count})
	return "monitor-pg-phase " + string(raw) + "\n"
}

// Build an independent prefix of the exact seventeen-record source protocol.
func pgCompletedProgressStream(nonce string, count int, complete bool) string {
	result := pgCompletedProgressLine(nonce, "remote_started", 0) + pgCompletedProgressLine(nonce, "database_identity", 0) + pgCompletedProgressLine(nonce, "history_start", 0)
	for sample := 1; sample <= count; sample++ {
		result += pgCompletedProgressLine(nonce, "activity", sample)
	}
	if complete {
		result += pgCompletedProgressLine(nonce, "blockers", 12) + pgCompletedProgressLine(nonce, "history_end", 12)
	}
	return result
}

// Trigger the real reader's existing watchdog path only after it has consumed
// the exact six-frame prefix. The child is killed and reaped by that path; no
// short timeout, scheduler luck or new production test hook proves the loss.
func TestPgCompletedFrameProgressSurvivesReaderOwnerDeadline(t *testing.T) {
	frames := pgSampleTestFrames(time.Now().UTC().Truncate(time.Second))
	prefix := pgSampleTestEncode(t, frames[:8])
	quoted, _ := json.Marshal(prefix)
	const boundary = "  left=deadline-time.monotonic()"
	if strings.Count(pgQuerySampleProgram, boundary) != 1 {
		t.Fatal("reader fixture lost the single existing owner-check boundary")
	}
	program := strings.Replace(pgQuerySampleProgram, boundary, "  if bytes(output)=="+string(quoted)+".encode():raise Failure('child_process','owner_deadline')\n"+boundary, 1)
	stub := "import signal\nsys.stdin.read()\nsys.stderr.write('private-pg-transport-stderr-canary')\nsys.stdout.write(" + string(quoted) + ");sys.stdout.flush()\nsignal.pause()\n"
	out, stderr, err := runPgSampleProgramOutputFixture(t, program, stub, pgSampleProgramInput(t))
	failure, valid := parsePgSampleSourceFailure(out)
	if err == nil || !valid || failure.Phase != "child_process" || failure.Cause != "owner_deadline" || strings.Contains(out+stderr, "private-") {
		t.Fatal("reader did not reach its owned partial-output kill/join failure")
	}
	wire := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
	if wire.Phase != "activity" || wire.Status != "valid" || wire.Frames == nil || *wire.Frames != 6 {
		t.Fatal("reader discarded completed activity-frame progress before its owner deadline")
	}
	if len(stderr) > 4096 || strings.Count(stderr, "monitor-pg-phase ") != 9 || strings.Contains(out, `"kind":"activity"`) {
		t.Fatal("partial progress exceeded its bound or published buffered samples")
	}
}

// New progress leaves the complete successful source bytes and all twelve
// validated samples intact. The maximum sequence remains below the old 4KiB cap.
func TestPgCompletedFrameProgressHealthyStreamPreservesSource(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	frames := pgSampleTestEncode(t, pgSampleTestFrames(now))
	// Psql also prints empty results for snapshot clears and sample waits.
	frames = strings.ReplaceAll(frames, "\n", "\n\n")
	quoted, _ := json.Marshal(frames)
	// Force every JSON line across actual pipe reads without timed writes.
	const read = "chunk=os.read(key.fileobj.fileno(),65536)"
	if strings.Count(pgQuerySampleProgram, read) != 1 {
		t.Fatal("reader fixture lost its single bounded pipe-read site")
	}
	program := strings.Replace(pgQuerySampleProgram, read, "chunk=os.read(key.fileobj.fileno(),17)", 1)
	out, stderr, err := runPgSampleProgramOutputFixture(t, program, "sys.stdin.read()\nsys.stdout.write("+string(quoted)+")\n", pgSampleProgramInput(t))
	receipt, parseErr := parsePgQuerySample(out, now)
	wire := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
	if err != nil || out != frames || parseErr != nil || !receipt.Complete || receipt.Samples != 12 {
		t.Fatal("completed-frame progress changed the successful full observation")
	}
	if wire.Phase != "history_end" || wire.Status != "valid" || wire.Frames == nil || *wire.Frames != 12 || len(stderr) > 4096 || strings.Count(stderr, "monitor-pg-phase ") != 17 {
		t.Fatal("healthy reader lost its finite complete progress sequence")
	}
}

// An incomplete, malformed or unordered source header stops later progress.
// A valid later row cannot resynchronize a stream past an unknown response.
func TestPgCompletedFrameProgressReaderRejectsBrokenHeaders(t *testing.T) {
	frames := pgSampleTestFrames(time.Now().UTC().Truncate(time.Second))
	before := pgSampleTestEncode(t, frames[:2])
	activity := pgSampleTestEncode(t, frames[2:3])
	after := pgSampleTestEncode(t, frames[3:4])
	for _, item := range []struct{ name, broken string }{
		{"partial", strings.TrimSuffix(activity, "\n")},
		{"duplicate-field", strings.Replace(activity, `"sample":0`, `"sample":0,"sample":0`, 1) + after},
		{"out-of-order", after + activity},
		{"wrong-kind", strings.Replace(activity, `"kind":"activity"`, `"kind":"history"`, 1) + after},
		{"null-index", strings.Replace(activity, `"sample":0`, `"sample":null`, 1) + after},
		{"boolean-index", strings.Replace(activity, `"sample":0`, `"sample":false`, 1) + after},
		{"fraction-index", strings.Replace(activity, `"sample":0`, `"sample":0.0`, 1) + after},
		{"invalid-json", "private-source-canary\n" + activity},
		{"missing-time", "{\"kind\":\"activity\",\"sample\":0}\n" + after},
		{"nonfinite-time", "{\"kind\":\"activity\",\"sample\":0,\"at\":NaN}\n" + after},
		{"overflow-time", "{\"kind\":\"activity\",\"sample\":0,\"at\":" + strings.Repeat("9", 500) + "}\n" + after},
	} {
		input := before + item.broken
		quoted, _ := json.Marshal(input)
		out, stderr, err := runPgSampleProgramOutputFixture(t, pgQuerySampleProgram, "sys.stdin.read()\nsys.stdout.write("+string(quoted)+")\n", pgSampleProgramInput(t))
		wire := pgCompletedProgressRead(t, stderr, pgTransportFixtureNonce)
		if err != nil || out != input || wire.Phase != "history_start" || wire.Status != "valid" || wire.Frames == nil || *wire.Frames != 0 || strings.Contains(stderr, "private-") {
			t.Fatalf("%s: broken response invented a completed activity frame or changed source bytes", item.name)
		}
	}
}

// Count and sequence checks reject fabricated progress; legacy records retain
// their old phase with an unknown count. Valid foreign records consume no lane.
func TestPgCompletedFrameProgressProtocolBoundsAndCompatibility(t *testing.T) {
	nonce := pgTransportFixtureNonce
	start := pgCompletedProgressLine(nonce, "remote_started", 0)
	identity := pgCompletedProgressLine(nonce, "database_identity", 0)
	history := pgCompletedProgressLine(nonce, "history_start", 0)
	base := start + identity + history
	one := pgCompletedProgressLine(nonce, "activity", 1)
	foreign := pgCompletedProgressLine("101112131415161718191a1b1c1d1e1f", "activity", 7)
	legacy := "monitor-pg-phase {\"schema\":1,\"nonce\":\"" + nonce + "\",\"phase\":\"remote_started\"}\n"
	for _, item := range []struct {
		name, raw, phase, status string
		count                    int
		known                    bool
	}{
		{"absent", "", "unknown", "absent", 0, false},
		{"legacy", legacy, "remote_started", "valid", 0, false},
		{"known-zero", start, "remote_started", "valid", 0, true},
		{"partial-progress", pgCompletedProgressStream(nonce, 6, false), "activity", "valid", 6, true},
		{"complete", pgCompletedProgressStream(nonce, 12, true), "history_end", "valid", 12, true},
		{"foreign-only", foreign, "unknown", "absent", 0, false},
		{"foreign-interleaved", foreign + start + foreign + identity + history + one + foreign, "activity", "valid", 1, true},
		{"duplicate", base + one + one, "unknown", "invalid", 0, false},
		{"skip-count", base + pgCompletedProgressLine(nonce, "activity", 2), "unknown", "invalid", 0, false},
		{"early-blockers", base + pgCompletedProgressLine(nonce, "blockers", 12), "unknown", "invalid", 0, false},
		{"out-of-order", identity + start, "unknown", "invalid", 0, false},
		{"null-count", strings.Replace(start, `"samples":0`, `"samples":null`, 1), "unknown", "invalid", 0, false},
		{"fraction-count", strings.Replace(start, `"samples":0`, `"samples":0.0`, 1), "unknown", "invalid", 0, false},
		{"boolean-count", strings.Replace(start, `"samples":0`, `"samples":false`, 1), "unknown", "invalid", 0, false},
		{"negative-count", pgCompletedProgressLine(nonce, "remote_started", -1), "unknown", "invalid", 0, false},
		{"excess-count", base + pgCompletedProgressLine(nonce, "activity", 13), "unknown", "invalid", 0, false},
		{"unknown-field", strings.Replace(start, "{", "{\"private-source-canary\":0,", 1), "unknown", "invalid", 0, false},
		{"unknown-phase", strings.Replace(start, "remote_started", "private-source-canary", 1), "unknown", "invalid", 0, false},
		{"duplicate-field", strings.Replace(start, `"schema":2`, `"schema":2,"schema":2`, 1), "unknown", "invalid", 0, false},
		{"malformed", base + "monitor-pg-phase {\n", "unknown", "invalid", 0, false},
		{"partial-line", base + strings.TrimSuffix(one, "\n"), "unknown", "invalid", 0, false},
		{"mixed-schema", legacy + identity, "unknown", "invalid", 0, false},
		{"extra-record", pgCompletedProgressStream(nonce, 12, true) + one, "unknown", "invalid", 0, false},
		{"capped", base + strings.Repeat("x", 4096), "unknown", "capped", 0, false},
	} {
		wire := pgCompletedProgressRead(t, item.raw, nonce)
		if wire.Phase != item.phase || wire.Status != item.status || (wire.Frames != nil) != item.known || (wire.Frames != nil && *wire.Frames != item.count) {
			t.Fatalf("%s: invalid or legacy progress acquired false frame authority", item.name)
		}
	}
}

// Local fake SSH owns no descendants. Its barrier is written only after the
// complete partial-progress prefix; the parent then cancels and joins it.
func TestPgCompletedFrameProgressSshHelper(t *testing.T) {
	if os.Getenv("MONITOR_PG_COMPLETED_PROGRESS_CHILD") != "1" {
		return
	}
	var input struct {
		Nonce string `json:"transport_nonce"`
	}
	if json.NewDecoder(io.LimitReader(os.Stdin, 262145)).Decode(&input) != nil || len(input.Nonce) != 32 {
		os.Exit(91)
	}
	mode := os.Getenv("MONITOR_PG_COMPLETED_PROGRESS_MODE")
	if mode == "cancel" {
		fmt.Fprint(os.Stderr, pgCompletedProgressStream(input.Nonce, 6, false))
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
	fmt.Fprint(os.Stderr, pgCompletedProgressStream(input.Nonce, 12, true))
	frames := pgSampleTestFrames(time.Now().UTC().Truncate(time.Second))
	if mode == "invalid-source" {
		frames[0]["primary"] = false
	} else if mode != "complete" {
		os.Exit(95)
	}
	fmt.Fprint(os.Stdout, pgSampleTestEncode(t, frames))
	os.Exit(0)
}

// Use the existing native process runner with an exclusively local executable.
func pgCompletedNativeFixture(t *testing.T, mode string) *probeEnv {
	t.Helper()
	dir := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!/bin/sh\nexec " + shellSingleQuote(executable) + " -test.run '^TestPgCompletedFrameProgressSshHelper$' -test.count=1\n"
	if os.WriteFile(filepath.Join(dir, "ssh"), []byte(launcher), 0700) != nil {
		t.Fatal("local progress executable was not written")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("MONITOR_PG_COMPLETED_PROGRESS_CHILD", "1")
	t.Setenv("MONITOR_PG_COMPLETED_PROGRESS_MODE", mode)
	if resolved, err := exec.LookPath("ssh"); err != nil || resolved != filepath.Join(dir, "ssh") {
		t.Fatal("fixture did not select only its local progress child")
	}
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("fixture bypassed native ownership")
		return "", "", nil
	})
	env.cfg.pgQuerySampleUntil = time.Time{}
	env.cfg.pgQuerySampleContinuous = true
	env.now = time.Now
	env.runner.(*runner).runSSH = runSSHCommand
	return env
}

// Read the new transport field independently from the retained receipt schema.
func pgCompletedProgressReceipt(t *testing.T, raw []byte) pgCompletedProgressWire {
	t.Helper()
	var receipt struct {
		Transport pgCompletedProgressWire `json:"transport_observation"`
	}
	if json.Unmarshal(raw, &receipt) != nil {
		t.Fatal("progress receipt was not valid JSON")
	}
	return receipt.Transport
}

// Native cancellation after an explicit six-frame barrier must retain the
// progress while keeping the original cannot-observe verdict and cadence.
func TestPgCompletedFrameProgressNativeCancellationRemainsIncomplete(t *testing.T) {
	env := pgCompletedNativeFixture(t, "cancel")
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
	if wire.Phase != "activity" || wire.Status != "valid" || wire.Frames == nil || *wire.Frames != 6 {
		t.Fatal("native cancellation discarded completed-frame progress from its receipt")
	}
	if len(findings) != 1 || findings[0].class != "pg-query-sample-unavailable" || findings[0].tier != tierWarn || findings[0].sustain != 1 {
		t.Fatal("partial progress weakened the first-miss cannot-observe finding")
	}
	state, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), time.Now())
	if err != nil || state.Outcome == "complete" || state.NextEligibleAt.Sub(state.TerminalAt) != pgQuerySampleCadence {
		t.Fatal("partial progress weakened the spent-attempt cadence")
	}
}

// A complete progress stream cannot override the source's existing authority
// checks. Only the unchanged successful parser may set Complete and Samples.
func TestPgCompletedFrameProgressNativeTerminalPreservesSourceAuthority(t *testing.T) {
	for _, mode := range []string{"complete", "invalid-source"} {
		env := pgCompletedNativeFixture(t, mode)
		if _, err := (pgQuerySampleProbe{}).check(t.Context(), env); err != nil {
			t.Fatal(err)
		}
		receipt, raw := pgTransportReadControlReceipt(t, env)
		wire := pgCompletedProgressReceipt(t, raw)
		if wire.Phase != "history_end" || wire.Status != "valid" || wire.Frames == nil || *wire.Frames != 12 || !receipt.TransportObservation.Joined || receipt.TransportObservation.NativeExitCode == nil || *receipt.TransportObservation.NativeExitCode != 0 {
			t.Fatal("native terminal lost its complete progress or process authority")
		}
		if mode == "complete" {
			if !receipt.Complete || receipt.Samples != 12 || receipt.SourceFailure != nil {
				t.Fatal("progress changed the healthy full observation")
			}
		} else if receipt.Complete || receipt.Samples != 0 || receipt.SourceFailure == nil || receipt.SourceFailure.Phase != "projection" {
			t.Fatal("completed-frame progress certified invalid database authority")
		}
	}
}
