// Independent admission processes keep reports separate from private diagnostics.
package work

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
	"testing"
	"time"
)

// Reports use an anonymous inherited pipe. The Go test runner owns stdout, so
// a child assertion must never be mistaken for a malformed report. Raw
// diagnostics can include fixture context and remain in owner-only files.
type privateLoadPeerProcess struct {
	command     *exec.Cmd
	stdin       io.WriteCloser
	reports     *os.File
	stdout      *os.File
	stderr      *os.File
	encoder     *json.Encoder
	decoder     *json.Decoder
	diagnostics string
	waited      bool
	waitError   error
}

// Start the selected child with anonymous configuration and report channels.
func newPrivateLoadPeerProcess(ctx context.Context, testName string, timeout time.Duration) (_ *privateLoadPeerProcess, err error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	process := &privateLoadPeerProcess{}
	process.diagnostics, err = os.MkdirTemp("", "urnetwork-private-load-child-")
	if err != nil {
		return nil, err
	}
	started := false
	defer func() {
		if !started {
			process.closeFiles()
			_ = process.removeDiagnostics()
		}
	}()
	process.stdout, err = os.OpenFile(filepath.Join(process.diagnostics, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	process.stderr, err = os.OpenFile(filepath.Join(process.diagnostics, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	var reportWriter *os.File
	process.reports, reportWriter, err = os.Pipe()
	if err != nil {
		return nil, err
	}
	defer reportWriter.Close()
	process.command = exec.CommandContext(ctx, executable, "-test.run=^"+testName+"$", "-test.count=1", "-test.timeout="+timeout.String())
	process.command.Env = append(os.Environ(), "URNETWORK_PRIVATE_LOAD_CHILD=1", "URNETWORK_PRIVATE_LOAD_REPORT_FD=3")
	process.command.ExtraFiles = []*os.File{reportWriter} // fd 3 in the child
	process.command.Stdout, process.command.Stderr = process.stdout, process.stderr
	process.stdin, err = process.command.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err = process.command.Start(); err != nil {
		return nil, err
	}
	process.encoder, process.decoder = json.NewEncoder(process.stdin), json.NewDecoder(process.reports)
	started = true
	return process, nil
}

// The actual workload uses the same process boundary as the protocol controls.
func privateLoadStartPeerProcess(t testing.TB, ctx context.Context, timeout time.Duration) *privateLoadPeerProcess {
	t.Helper()
	process, err := newPrivateLoadPeerProcess(ctx, "TestPrivateProviderLoadedPeerProcess", timeout)
	if err != nil {
		t.Fatal("start independent process", err)
	}
	return process
}

// Qualify readiness and join a rejected child before reporting its safe failure.
func (self *privateLoadPeerProcess) requireReady(t testing.TB) {
	t.Helper()
	var ready privateLoadProcessReport
	err := self.decoder.Decode(&ready)
	if err != nil || !ready.Ready {
		if ready.Failure != "" {
			// A rejected child is already exiting. Join it so the retained Go
			// assertion is complete, rather than killing it after its report.
			_ = self.wait()
		}
		t.Fatalf("independent process not ready: decode=%v qualification=%s private_diagnostics=%s", err, ready.Failure, self.diagnostics)
	}
}

// Join at most once, with the command's existing finite context and timeout.
func (self *privateLoadPeerProcess) wait() error {
	if !self.waited {
		_ = self.stdin.Close()
		self.waitError = self.command.Wait()
		self.waited = true
	}
	return self.waitError
}

// Defer this in the fixture callback, so the process joins before TestEnv drops
// its database or releases its Redis lease. testing.T.Cleanup runs too late.
func (self *privateLoadPeerProcess) close(t testing.TB) {
	t.Helper()
	completed := self.waited && self.waitError == nil
	if !self.waited {
		_ = self.command.Process.Kill()
		_ = self.wait()
	}
	self.closeFiles()
	// TestEnv records a recovered parent panic after callback defers finish.
	// Only the artifact decision waits for final cleanup; the child is joined.
	t.Cleanup(func() {
		if !completed || t.Failed() {
			t.Logf("independent process private diagnostics retained at %s", self.diagnostics)
			return
		}
		if err := self.removeDiagnostics(); err != nil {
			t.Errorf("remove successful process diagnostics: %v", err)
		}
	})
}

// Release only files constructed and owned by this process wrapper.
func (self *privateLoadPeerProcess) closeFiles() {
	if self.stdin != nil {
		_ = self.stdin.Close()
	}
	for _, f := range []*os.File{self.reports, self.stdout, self.stderr} {
		if f != nil {
			_ = f.Close()
		}
	}
}

// Remove exact files after success; failed workloads retain their evidence.
func (self *privateLoadPeerProcess) removeDiagnostics() error {
	var result error
	for _, name := range []string{"stdout.log", "stderr.log", ""} {
		if err := os.Remove(filepath.Join(self.diagnostics, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}

// Reject unqualified invocations before accessing an inherited descriptor.
func privateLoadChildReportPipe() (*os.File, error) {
	if os.Getenv("URNETWORK_PRIVATE_LOAD_REPORT_FD") != "3" {
		return nil, errors.New("child report pipe ownership is absent")
	}
	return privateLoadInheritedWritePipe(3)
}

// This child supplies deterministic success, rejection and malformed controls.
func TestPrivateLoadProcessProtocolChild(t *testing.T) {
	if os.Getenv("URNETWORK_PRIVATE_LOAD_CHILD") != "1" {
		t.Skip("owned subprocess only")
	}
	pipe, err := privateLoadChildReportPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	fmt.Fprintln(os.Stdout, "synthetic private stdout diagnostic, not JSON")
	fmt.Fprintln(os.Stderr, "synthetic private stderr diagnostic")
	switch os.Getenv("URNETWORK_PRIVATE_LOAD_PROTOCOL_ARM") {
	case "success":
		err = json.NewEncoder(pipe).Encode(privateLoadProcessReport{Ready: true})
	case "reject":
		_ = json.NewEncoder(pipe).Encode(privateLoadProcessReport{Failure: "synthetic fixture rejection"})
		t.Fatal("synthetic child assertion")
	case "malformed":
		_, err = io.WriteString(pipe, "not a report\n")
	default:
		t.Fatal("unknown protocol control")
	}
	if err != nil {
		t.Fatal(err)
	}
}

// Exercise one report control within its own top-level test lifetime.
func checkPrivateLoadProcessReportPipe(t *testing.T, arm string) {
	t.Helper()
	t.Setenv("URNETWORK_PRIVATE_LOAD_PROTOCOL_ARM", arm)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	process, err := newPrivateLoadPeerProcess(ctx, "TestPrivateLoadProcessProtocolChild", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer process.close(t)
	var report privateLoadProcessReport
	decodeError := process.decoder.Decode(&report)
	waitError := process.wait()
	if arm == "malformed" {
		if decodeError == nil {
			t.Fatal("malformed report was accepted")
		}
	} else if decodeError != nil {
		t.Fatal("stdout diagnostic corrupted the report", decodeError)
	}
	if arm == "success" && (!report.Ready || waitError != nil) {
		t.Fatal("successful independent process did not qualify and join")
	}
	if arm == "reject" && (report.Ready || report.Failure != "synthetic fixture rejection" || waitError == nil) {
		t.Fatal("rejected process lost its qualification failure")
	}
	info, err := os.Stat(process.diagnostics)
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("process diagnostics directory is not private")
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		path := filepath.Join(process.diagnostics, name)
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("process diagnostics file is not private")
		}
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), "synthetic private") {
			t.Fatal("private child diagnostics were not retained")
		}
		if arm == "reject" && name == "stdout.log" && !strings.Contains(string(data), "synthetic child assertion") {
			t.Fatal("failed process was killed before its assertion was retained")
		}
	}
	// These files contain only this control's synthetic messages. Real
	// failure diagnostics are retained by close for private inspection.
	t.Cleanup(func() { _ = process.removeDiagnostics() })
}

// A clean report must remain readable despite arbitrary stdout diagnostics.
func TestPrivateLoadProcessReportPipeSuccess(t *testing.T) {
	checkPrivateLoadProcessReportPipe(t, "success")
}

// A rejected child must retain its joined assertion outside the report channel.
func TestPrivateLoadProcessReportPipeRejection(t *testing.T) {
	checkPrivateLoadProcessReportPipe(t, "reject")
}

// Invalid report bytes must still fail the report decoder.
func TestPrivateLoadProcessReportPipeMalformed(t *testing.T) {
	checkPrivateLoadProcessReportPipe(t, "malformed")
}

// Invalid descriptors must fail safely without closing a runtime-owned handle.
func TestPrivateLoadProcessRequiresReportPipe(t *testing.T) {
	for _, arm := range []string{"absent marker", "forged marker", "regular file", "read-only pipe"} {
		func() {
			t.Logf("report descriptor case: %s", arm)
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			command := exec.CommandContext(ctx, executable, "-test.run=^TestPrivateLoadProcessProtocolChild$", "-test.count=1", "-test.timeout=10s")
			marker, want := "3", "child report descriptor is not an owned write pipe"
			switch arm {
			case "absent marker":
				marker, want = "", "child report pipe ownership is absent"
			case "regular file":
				file, err := os.OpenFile(filepath.Join(t.TempDir(), "not-a-pipe"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				defer file.Close()
				command.ExtraFiles = []*os.File{file}
			case "read-only pipe":
				reader, writer, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				defer reader.Close()
				defer writer.Close()
				command.ExtraFiles = []*os.File{reader}
			}
			command.Env = append(os.Environ(), "URNETWORK_PRIVATE_LOAD_CHILD=1", "URNETWORK_PRIVATE_LOAD_PROTOCOL_ARM=success", "URNETWORK_PRIVATE_LOAD_REPORT_FD="+marker)
			diagnostics, err := command.CombinedOutput()
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 1 || !strings.Contains(string(diagnostics), want) ||
				strings.Contains(string(diagnostics), "fatal error:") || strings.Contains(string(diagnostics), `"Ready":true`) {
				directory, createErr := os.MkdirTemp("", "urnetwork-private-load-pipe-rejection-")
				if createErr == nil {
					_ = os.WriteFile(filepath.Join(directory, "combined.log"), diagnostics, 0600)
				}
				t.Fatalf("child did not safely reject its unowned report descriptor: exit=%v private_diagnostics=%s", err, directory)
			}
		}()
	}
}

// A config parse failure remains a joined child assertion, not report bytes.
func TestPrivateLoadProcessMalformedConfigJoinsAndRetainsAssertion(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	process := privateLoadStartPeerProcess(t, ctx, 10*time.Second)
	defer process.close(t)
	if _, err := io.WriteString(process.stdin, "not JSON\n"); err != nil {
		t.Fatal("write malformed synthetic configuration")
	}
	var report privateLoadProcessReport
	if err := process.decoder.Decode(&report); !errors.Is(err, io.EOF) {
		t.Fatal("child assertion was mixed into the report pipe")
	}
	if err := process.wait(); err == nil {
		t.Fatal("child accepted malformed configuration")
	}
	diagnostics, err := os.ReadFile(filepath.Join(process.diagnostics, "stdout.log"))
	if err != nil || !strings.Contains(string(diagnostics), "--- FAIL: TestPrivateProviderLoadedPeerProcess") {
		t.Fatal("malformed configuration lost the joined child assertion")
	}
	t.Cleanup(func() { _ = process.removeDiagnostics() })
}

// A rejected fixture emits a safe report before the child assertion is joined.
func TestPrivateLoadProcessQualificationFailureIsStructured(t *testing.T) {
	t.Setenv("WARP_ENV", "main")
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	process := privateLoadStartPeerProcess(t, ctx, 10*time.Second)
	defer process.close(t)
	if err := process.encoder.Encode(privateLoadProcessConfig{PoolSize: 16}); err != nil {
		t.Fatal("send rejected synthetic configuration")
	}
	var report privateLoadProcessReport
	if err := process.decoder.Decode(&report); err != nil || report.Ready || report.Failure != "child local fixture configuration is invalid" {
		t.Fatal("child qualification failure was not reported independently of its assertion")
	}
	if err := process.wait(); err == nil {
		t.Fatal("child qualification failure did not fail the independent process")
	}
	diagnostics, err := os.ReadFile(filepath.Join(process.diagnostics, "stdout.log"))
	if err != nil || !strings.Contains(string(diagnostics), "--- FAIL: TestPrivateProviderLoadedPeerProcess") {
		t.Fatal("qualification failure lost the joined child assertion")
	}
	t.Cleanup(func() { _ = process.removeDiagnostics() })
}

// Model TestEnv marking a recovered parent panic after callback defers finish.
type privateLoadDiagnosticCleanupTb struct {
	testing.TB
	failed       bool
	cleanupFuncs []func()
}

// Hold final cleanup until the synthetic parent outcome is known.
func (self *privateLoadDiagnosticCleanupTb) Cleanup(cleanup func()) {
	self.cleanupFuncs = append(self.cleanupFuncs, cleanup)
}

// Callback defers and final test cleanup observe different failure states.
func (self *privateLoadDiagnosticCleanupTb) Failed() bool {
	return self.failed
}

// Return a joined successful child whose diagnostics still belong to its owner.
func privateLoadCompletedProtocolProcess(t *testing.T) *privateLoadPeerProcess {
	t.Helper()
	t.Setenv("URNETWORK_PRIVATE_LOAD_PROTOCOL_ARM", "success")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	process, err := newPrivateLoadPeerProcess(ctx, "TestPrivateLoadProcessProtocolChild", 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !process.waited {
			_ = process.command.Process.Kill()
			_ = process.wait()
		}
		process.closeFiles()
		_ = process.removeDiagnostics()
	})
	process.requireReady(t)
	if err := process.wait(); err != nil {
		t.Fatal("synthetic successful child did not join")
	}
	return process
}

// A parent panic after child join must retain logs when the attempt marks failure.
func TestPrivateLoadProcessLateParentFailureRetainsDiagnostics(t *testing.T) {
	process := privateLoadCompletedProtocolProcess(t)
	parent := &privateLoadDiagnosticCleanupTb{TB: t}
	process.close(parent)
	if _, err := process.stdout.Write([]byte("must not be written")); !errors.Is(err, os.ErrClosed) {
		t.Fatal("callback cleanup did not synchronously close the joined child's files")
	}
	parent.failed = true
	for index := len(parent.cleanupFuncs) - 1; index >= 0; index-- {
		parent.cleanupFuncs[index]()
	}
	for _, name := range []string{"stdout.log", "stderr.log"} {
		info, err := os.Stat(filepath.Join(process.diagnostics, name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("joined child diagnostics were removed before late parent failure")
		}
	}
}

// Final success removes only the process's exact diagnostic files and directory.
func TestPrivateLoadProcessSuccessfulCleanupRemovesDiagnostics(t *testing.T) {
	process := privateLoadCompletedProtocolProcess(t)
	parent := &privateLoadDiagnosticCleanupTb{TB: t}
	process.close(parent)
	if _, err := os.Stat(process.diagnostics); err != nil {
		t.Fatal("diagnostic removal ran before the final parent outcome")
	}
	for index := len(parent.cleanupFuncs) - 1; index >= 0; index-- {
		parent.cleanupFuncs[index]()
	}
	for _, name := range []string{"stdout.log", "stderr.log", ""} {
		if _, err := os.Stat(filepath.Join(process.diagnostics, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("successful process diagnostics were not removed")
		}
	}
}
