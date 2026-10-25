//go:build unix

// Re-enter only the synthetic child branch, never the signal-driving wrapper.
package acceptance

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Correct inherited endpoints retain the existing completion protocol.
func TestProxyWrapperChildAcceptsOwnedPipes(t *testing.T) {
	checkProxyWrapperPipeChild(t, "owned")
}

// Missing inheritance must be refused before numeric runtime handles are owned.
func TestProxyWrapperChildRejectsMissingPipes(t *testing.T) {
	checkProxyWrapperPipeChild(t, "missing")
}

// A writable regular file cannot impersonate the started barrier.
func TestProxyWrapperChildRejectsRegularStartedFile(t *testing.T) {
	checkProxyWrapperPipeChild(t, "regular-started")
}

// Validate the second endpoint even when this child will not wait for a signal.
func TestProxyWrapperChildRejectsRegularCanceledFile(t *testing.T) {
	checkProxyWrapperPipeChild(t, "regular-canceled")
}

// A readable regular file cannot impersonate the cleanup-release barrier.
func TestProxyWrapperChildRejectsRegularReleaseFile(t *testing.T) {
	checkProxyWrapperPipeChild(t, "regular-release")
}

// A read endpoint must not acquire a writer owner.
func TestProxyWrapperChildRejectsReadOnlyStartedPipe(t *testing.T) {
	checkProxyWrapperPipeChild(t, "read-started")
}

// Refusing the second endpoint must precede the first barrier's write.
func TestProxyWrapperChildRejectsReadOnlyCanceledPipe(t *testing.T) {
	checkProxyWrapperPipeChild(t, "read-canceled")
}

// Refusing the last endpoint must precede every marker and barrier effect.
func TestProxyWrapperChildRejectsWriteOnlyReleasePipe(t *testing.T) {
	checkProxyWrapperPipeChild(t, "write-release")
}

// Every case owns synthetic files, anonymous pipes and one synchronously joined child.
func checkProxyWrapperPipeChild(t *testing.T, arm string) {
	t.Helper()
	directory, err := os.MkdirTemp("", "urnetwork-wrapper-pipe-child-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("private wrapper descriptor diagnostics retained at %s", directory)
			return
		}
		if err := os.RemoveAll(directory); err != nil {
			t.Errorf("remove successful private descriptor control: %v", err)
		}
	})
	for _, name := range []string{"markers", "empty-config", "empty-vault", "empty-site", "tmp"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	credentialPath := filepath.Join(directory, "synthetic-credential")
	credentialBytes := []byte("synthetic wrapper descriptor control\n")
	if err := os.WriteFile(credentialPath, credentialBytes, 0600); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(directory, "result.tsv")
	startedReader, startedWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer startedReader.Close()
	defer startedWriter.Close()
	canceledReader, canceledWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer canceledReader.Close()
	defer canceledWriter.Close()
	releaseReader, releaseWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseReader.Close()
	defer releaseWriter.Close()
	if _, err := releaseWriter.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	extraFiles := []*os.File{startedWriter, canceledWriter, releaseReader}
	var regularFile *os.File
	var regularPath string
	var regularBytes []byte
	if strings.HasPrefix(arm, "regular-") {
		index, flags := 0, os.O_WRONLY
		switch arm {
		case "regular-canceled":
			index = 1
		case "regular-release":
			index, flags = 2, os.O_RDONLY
			regularBytes = []byte{1}
		}
		regularPath = filepath.Join(directory, "synthetic-descriptor")
		if err := os.WriteFile(regularPath, regularBytes, 0600); err != nil {
			t.Fatal(err)
		}
		regularFile, err = os.OpenFile(regularPath, flags, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer regularFile.Close()
		extraFiles[index] = regularFile
	} else {
		switch arm {
		case "owned":
		case "missing":
			extraFiles = nil
		case "read-started":
			extraFiles[0] = startedReader
		case "read-canceled":
			extraFiles[1] = canceledReader
		case "write-release":
			extraFiles[2] = releaseWriter
		default:
			t.Fatal("unknown descriptor control")
		}
	}
	stdout, err := os.OpenFile(filepath.Join(directory, "stdout.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.OpenFile(filepath.Join(directory, "stderr.log"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, executable,
		"-test.run=^TestProxyAcceptanceWrapperWaitsForCleanupAfterInterrupt$",
		"-test.count=1", "-test.timeout=8s", "-logtostderr")
	command.Dir = directory
	command.Env = []string{
		"PATH=/usr/bin:/bin", "GOMAXPROCS=2", "TMPDIR=" + filepath.Join(directory, "tmp"),
		"WARP_ENV=local", "WARP_HOME=" + directory,
		"WARP_CONFIG_HOME=" + filepath.Join(directory, "empty-config"),
		"WARP_VAULT_HOME=" + filepath.Join(directory, "empty-vault"),
		"WARP_SITE_HOME=" + filepath.Join(directory, "empty-site"),
		proxyWrapperSignalChild + "=1", proxyWrapperRunnerExit + "=0",
		"URNETWORK_PROXY_WRAPPER_MARKER_DIRECTORY=" + filepath.Join(directory, "markers"),
		"URNETWORK_PROXY_WRAPPER_CREDENTIAL_PATH=" + credentialPath,
		"URNETWORK_PROXY_WRAPPER_RESULT_PATH=" + resultPath,
	}
	command.ExtraFiles, command.Stdout, command.Stderr = extraFiles, stdout, stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	// Drop every parent writer before readback; only the joined child can emit.
	_ = startedWriter.Close()
	_ = canceledWriter.Close()
	_ = releaseReader.Close()
	_ = releaseWriter.Close()
	if regularFile != nil {
		_ = regularFile.Close()
	}
	waitErr := command.Wait()
	_ = stdout.Close()
	_ = stderr.Close()
	if ctx.Err() != nil {
		t.Fatalf("descriptor child exceeded its emergency bound: %v", ctx.Err())
	}
	if waitErr != nil {
		var exitError *exec.ExitError
		if !errors.As(waitErr, &exitError) {
			t.Fatal(waitErr)
		}
	}
	diagnostics, err := os.ReadFile(filepath.Join(directory, "stderr.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(diagnostics), "fatal error:") || strings.Contains(string(diagnostics), "panic:") {
		t.Fatalf("descriptor child crashed; private stderr bytes=%d", len(diagnostics))
	}
	startedBytes, err := io.ReadAll(startedReader)
	if err != nil {
		t.Fatal(err)
	}
	canceledBytes, err := io.ReadAll(canceledReader)
	if err != nil {
		t.Fatal(err)
	}
	actualCredentials, err := os.ReadFile(credentialPath)
	if err != nil || !bytes.Equal(actualCredentials, credentialBytes) {
		t.Fatalf("synthetic credential changed: bytes=%d err=%v", len(actualCredentials), err)
	}
	if arm == "owned" {
		if waitErr != nil || !bytes.Equal(startedBytes, []byte{1}) || len(canceledBytes) != 0 {
			t.Fatalf("owned pipes did not preserve completion: exit=%d started=%d canceled=%d",
				command.ProcessState.ExitCode(), len(startedBytes), len(canceledBytes))
		}
		resultBytes, err := os.ReadFile(resultPath)
		if err != nil || !bytes.Contains(resultBytes, []byte("\tPASS\t")) {
			t.Fatalf("owned pipes did not report success: bytes=%d err=%v", len(resultBytes), err)
		}
		if _, err := os.Stat(filepath.Join(directory, "markers", "cleanup-completed")); err != nil {
			t.Fatalf("owned pipes omitted cleanup marker: %v", err)
		}
		return
	}
	if command.ProcessState.ExitCode() != 70 {
		t.Errorf("unqualified descriptors exit=%d, want 70", command.ProcessState.ExitCode())
	}
	if len(startedBytes) != 0 || len(canceledBytes) != 0 {
		t.Errorf("unqualified descriptors emitted barriers: started=%d canceled=%d", len(startedBytes), len(canceledBytes))
	}
	for _, relative := range []string{"markers/runner-pid", "markers/credential-path", "markers/cleanup-completed", "result.tsv"} {
		if _, err := os.Stat(filepath.Join(directory, relative)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("unqualified descriptors produced %s: %v", relative, err)
		}
	}
	if regularFile != nil {
		actual, err := os.ReadFile(regularPath)
		if err != nil || !bytes.Equal(actual, regularBytes) {
			t.Errorf("unqualified descriptors mutated regular file: bytes=%d err=%v", len(actual), err)
		}
	}
}
