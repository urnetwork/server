package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	servermonitor "github.com/urnetwork/server/monitor"
)

const jsonlQualificationCommandBoundary = "synthetic-jsonl-capture: before-runMonitorProcess\n"

type jsonlQualificationSplitWriter struct {
	writer     io.Writer
	chunkBytes int
}

func (self jsonlQualificationSplitWriter) Write(raw []byte) (int, error) {
	written := 0
	for written < len(raw) {
		end := min(written+self.chunkBytes, len(raw))
		n, err := self.writer.Write(raw[written:end])
		written += n
		if err != nil {
			return written, err
		}
		if n != end-(written-n) {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}

func jsonlQualificationAlerts() servermonitor.Alerts {
	unknown := servermonitor.Alert{
		SignalNumber: "UNASSIGNED", SignalKey: "synthetic-owner", SignalID: "monitor/visibility-unmapped",
		SignalName: "synthetic unknown", Severity: servermonitor.SeverityWarn,
		Class: "synthetic-unobservable", Target: "synthetic-target", Frame: "synthetic-frame",
		Environment: "synthetic", ObservedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		Sustain: 2, PageSustain: 3, Symptom: "synthetic peer 192.0.2.23",
		Mechanism: "synthetic mechanism", Baseline: "synthetic baseline", Observed: "source_complete=false reason=unknown",
		Evidence: strings.Repeat("x", 180*1024), Context: "synthetic peer 2001:db8::7",
		Action: "synthetic action", Verify: "synthetic verify", Playbook: "synthetic playbook",
	}
	page := unknown
	page.SignalID = "synthetic/page"
	page.Severity = servermonitor.SeverityPage
	page.Class = "synthetic-failure"
	page.Evidence = "synthetic evidence"
	return servermonitor.Alerts{unknown, page}
}

// Only this dedicated child replaces process descriptors; it never loads settings or probes.
func TestJsonlCoupledCaptureFixture(t *testing.T) {
	mode := os.Getenv("URNETWORK_JSONL_CAPTURE_FIXTURE")
	if mode == "" {
		return
	}
	switch mode {
	case "full", "split", "partial", "cancelled", "failure", "empty":
	default:
		t.Fatal("unknown synthetic fixture mode")
	}
	// Package initialization precedes this boundary and the real scrubber.
	// Keep its bytes separate and validate them rather than assuming silence.
	if _, err := io.WriteString(os.Stderr, jsonlQualificationCommandBoundary); err != nil {
		os.Exit(97)
	}
	code := runMonitorProcess(func() error {
		opts, err := parseMonitorOptions([]string{"-format=jsonl"})
		if err != nil {
			return err
		}
		var stdout io.Writer = os.Stdout
		if mode == "split" {
			stdout = jsonlQualificationSplitWriter{writer: os.Stdout, chunkBytes: 31}
		}
		handle := continuousAlertHandler(stdout, opts.format, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if mode == "cancelled" {
			cancel()
		}
		execution := servermonitor.SignalExecution{}
		if mode != "empty" {
			execution.Alerts = jsonlQualificationAlerts()
		}
		if err := handle(ctx, nil, execution); err != nil {
			return err
		}
		if mode == "partial" {
			if _, err := io.WriteString(stdout, `{"signal_id":"synthetic/partial`); err != nil {
				return err
			}
		}
		if mode != "empty" {
			if _, err := io.WriteString(os.Stderr, "synthetic diagnostic peer 192.0.2.23"); err != nil {
				return err
			}
		}
		if mode == "failure" {
			return errors.New(" synthetic failure peer 2001:db8::7")
		}
		return nil
	}, os.Stderr, server.ScrubProcessLogs)
	os.Exit(code)
}

type jsonlQualificationCapture struct {
	stdout        []byte
	stderr        []byte
	startupStderr []byte
	commandStderr []byte
	exitCode      int
}

func runJsonlQualificationCapture(t *testing.T, mode string) jsonlQualificationCapture {
	t.Helper()
	directory := t.TempDir()
	openCapture := func(name string) *os.File {
		file, err := os.OpenFile(filepath.Join(directory, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { file.Close() })
		return file
	}
	stdout, stderr := openCapture("stdout.private.jsonl"), openCapture("stderr.private.log")
	stdoutBefore, err := stdout.Stat()
	if err != nil {
		t.Fatal(err)
	}
	stderrBefore, err := stderr.Stat()
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestJsonlCoupledCaptureFixture$", "-test.count=1")
	childHome := t.TempDir()
	command.Env = []string{"WARP_HOME=" + childHome, "WARP_ENV=local", "WARP_TEST_ENV_FAIL_FAST=1", "URNETWORK_JSONL_CAPTURE_FIXTURE=" + mode}
	command.Stdout, command.Stderr = stdout, stderr
	err = command.Run()
	if ctx.Err() != nil {
		t.Fatal("synthetic coupled child exceeded ownership deadline")
	}
	result := jsonlQualificationCapture{}
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			t.Fatal("synthetic coupled child failed to start")
		}
		result.exitCode = exitError.ExitCode()
	}
	readCapture := func(file *os.File, before os.FileInfo) []byte {
		after, err := file.Stat()
		if err != nil || !after.Mode().IsRegular() || after.Mode().Perm() != 0600 || !os.SameFile(before, after) {
			t.Fatal("held private capture identity or mode changed")
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(io.LimitReader(file, 1024*1024+1))
		if err != nil || len(raw) > 1024*1024 || int64(len(raw)) != after.Size() {
			t.Fatal("held capture exceeds synthetic bound or lacks exact EOF")
		}
		t.Logf("synthetic retained capture bytes=%d sha256=%x", len(raw), sha256.Sum256(raw))
		return raw
	}
	result.stdout = readCapture(stdout, stdoutBefore)
	result.stderr = readCapture(stderr, stderrBefore)
	boundary := []byte(jsonlQualificationCommandBoundary)
	if bytes.Count(result.stderr, boundary) != 1 {
		t.Fatal("missing or ambiguous synthetic command phase boundary")
	}
	result.startupStderr, result.commandStderr, _ = bytes.Cut(result.stderr, boundary)
	// Match every initializer byte to its bound source site and child profile.
	// Timestamp metadata varies; message bodies, source sites, PID and paths do not.
	header := `I[0-9]{4} [0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6} +` + strconv.Itoa(command.Process.Pid) + ` env\.go:`
	startupPattern := `\A` + regexp.QuoteMeta("[env]glog not configured from command line. Using default configuration.\n") +
		header + `1202\] ` + regexp.QuoteMeta("[env]try "+filepath.Join(childHome, "config")+", [settings.yml]\n") +
		header + `1202\] ` + regexp.QuoteMeta("[env]try "+filepath.Join(childHome, "site")+", [settings.yml]\n") +
		header + `81\] ` + regexp.QuoteMeta("[env]settings loaded\n") + `\z`
	if !regexp.MustCompile(startupPattern).Match(result.startupStderr) {
		t.Fatalf("initializer source grammar or private child profile mismatch: bytes=%d", len(result.startupStderr))
	}
	if !bytes.Equal(result.stderr, bytes.Join([][]byte{result.startupStderr, boundary, result.commandStderr}, nil)) {
		t.Fatal("stderr phase views failed exact raw-byte partition")
	}
	for _, address := range []string{"192.0.2.23", "2001:db8::7"} {
		if bytes.Contains(result.stderr, []byte(address)) {
			t.Fatal("documentation address survived complete captured stderr")
		}
	}
	return result
}

func assertJsonlQualificationCapture(t *testing.T, mode string, result jsonlQualificationCapture) {
	t.Helper()
	var expected bytes.Buffer
	if err := writeAlerts(&expected, alertFormatJSONL, jsonlQualificationAlerts()); err != nil {
		t.Fatal(err)
	}
	expectedBytes := []byte(server.ScrubAddrsText(expected.String()))
	if mode == "partial" {
		expectedBytes = append(expectedBytes, []byte(`{"signal_id":"synthetic/partial`)...)
	}
	expectedExit := 0
	expectedDiagnostic := "synthetic diagnostic peer [scrubbed]"
	if mode == "failure" {
		expectedExit = 1
		expectedDiagnostic += " synthetic failure peer [scrubbed]\n"
	}
	if result.exitCode != expectedExit || !bytes.Equal(result.stdout, expectedBytes) || string(result.commandStderr) != expectedDiagnostic {
		t.Fatalf("coupled privacy/capture mismatch mode=%s exit=%d expected_exit=%d stdout_bytes=%d expected_bytes=%d stderr_matches=%t", mode, result.exitCode, expectedExit, len(result.stdout), len(expectedBytes), string(result.commandStderr) == expectedDiagnostic)
	}
	for _, address := range []string{"192.0.2.23", "2001:db8::7"} {
		if bytes.Contains(result.stdout, []byte(address)) || bytes.Contains(result.stderr, []byte(address)) {
			t.Fatal("documentation address survived ordinary coupled privacy path")
		}
	}
	complete := bytes.Split(result.stdout, []byte{'\n'})
	if len(complete) != 3 {
		t.Fatalf("expected exactly two complete Alert records and a retained tail, got_parts=%d", len(complete))
	}
	var unknown map[string]any
	for i := 0; i < 2; i++ {
		var value map[string]any
		if err := json.Unmarshal(complete[i], &value); err != nil || len(value) != 21 {
			t.Fatalf("complete public Alert schema missing after privacy/capture: record=%d fields=%d", i, len(value))
		}
		if _, exists := value["unobservable"]; exists {
			t.Fatal("private notifier field invented in public JSON")
		}
		if value["signal_id"] == "monitor/visibility-unmapped" {
			unknown = value
		}
	}
	if unknown == nil || unknown["class"] != "synthetic-unobservable" || unknown["observed"] != "source_complete=false reason=unknown" || unknown["signal_number"] != "UNASSIGNED" {
		t.Fatal("unknown finding or nonnumeric identity lost during composed output")
	}
	if len(unknown["evidence"].(string)) != 180*1024 {
		t.Fatal("long Alert field truncated")
	}
	if mode == "partial" && json.Valid(complete[2]) {
		t.Fatal("unterminated synthetic suffix unexpectedly became a complete Alert")
	}
}

func TestJsonlCoupledFullAlertPrivacyHeldCapture(t *testing.T) {
	assertJsonlQualificationCapture(t, "full", runJsonlQualificationCapture(t, "full"))
}

func TestJsonlCoupledSplitWritesPreserveLongRecord(t *testing.T) {
	assertJsonlQualificationCapture(t, "split", runJsonlQualificationCapture(t, "split"))
}

func TestJsonlCoupledPartialTailRemainsRetainedUnknown(t *testing.T) {
	assertJsonlQualificationCapture(t, "partial", runJsonlQualificationCapture(t, "partial"))
}

// This checks late output/drain for already-gated Alerts; it makes no cancellation health claim.
func TestJsonlCoupledCancelledContextStillDrainsSuppliedOutput(t *testing.T) {
	assertJsonlQualificationCapture(t, "cancelled", runJsonlQualificationCapture(t, "cancelled"))
}

func TestJsonlCoupledFailedCommandDrainsBothDescriptors(t *testing.T) {
	assertJsonlQualificationCapture(t, "failure", runJsonlQualificationCapture(t, "failure"))
}

func TestJsonlCoupledEmptyStreamCannotSupplyHealthEvidence(t *testing.T) {
	result := runJsonlQualificationCapture(t, "empty")
	if result.exitCode != 0 || len(result.stdout) != 0 || len(result.commandStderr) != 0 {
		t.Fatal("empty synthetic execution unexpectedly emitted source bytes")
	}
}

type jsonlQualificationShortWriter struct{}

func (self jsonlQualificationShortWriter) Write(raw []byte) (int, error) {
	return max(0, len(raw)-1), nil
}

func TestJsonlCoupledHandlerRefusesShortWrite(t *testing.T) {
	handle := continuousAlertHandler(jsonlQualificationShortWriter{}, alertFormatJSONL, nil)
	err := handle(context.Background(), nil, servermonitor.SignalExecution{Alerts: jsonlQualificationAlerts()})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short native Alert write was accepted as complete")
	}
}
