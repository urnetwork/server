// Package acceptance tests the shell boundary that owns the deployed proxy
// runner as well as the in-process protocol campaigns.
package acceptance

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	proxyWrapperSignalChild = "URNETWORK_PROXY_WRAPPER_SIGNAL_CHILD"
	proxyWrapperSignalName  = "URNETWORK_PROXY_WRAPPER_SIGNAL_NAME"
)

// A terminal interrupt must remain a request for graceful cancellation until
// the timed runner and log consumer finish, not tear down their output pipe.
func TestProxyAcceptanceWrapperWaitsForCleanupAfterInterrupt(t *testing.T) {
	t.Setenv(proxyWrapperSignalName, "INT")
	testProxyAcceptanceWrapperWaitsForCleanupAfterSignal(t)
}

// An outer termination uses the same joined cleanup path as an interactive
// interrupt and retains the runner's canceled status.
func TestProxyAcceptanceWrapperWaitsForCleanupAfterTerm(t *testing.T) {
	t.Setenv(proxyWrapperSignalName, "TERM")
	testProxyAcceptanceWrapperWaitsForCleanupAfterSignal(t)
}

// Runs the real wrapper around a controlled child with descriptor-backed
// lifecycle barriers, keeping timing out of the signal-ordering proof.
func testProxyAcceptanceWrapperWaitsForCleanupAfterSignal(t *testing.T) {
	if os.Getenv(proxyWrapperSignalChild) == "1" {
		startedWriter := os.NewFile(3, "started-writer")
		canceledWriter := os.NewFile(4, "canceled-writer")
		releaseReader := os.NewFile(5, "release-reader")
		markerDirectory := os.Getenv("URNETWORK_PROXY_WRAPPER_MARKER_DIRECTORY")
		credentialPath := os.Getenv("URNETWORK_PROXY_WRAPPER_CREDENTIAL_PATH")
		resultPath := os.Getenv("URNETWORK_PROXY_WRAPPER_RESULT_PATH")
		if startedWriter == nil || canceledWriter == nil || releaseReader == nil || markerDirectory == "" || credentialPath == "" || resultPath == "" {
			os.Exit(70)
		}
		if err := os.WriteFile(filepath.Join(markerDirectory, "runner-pid"), []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			os.Exit(70)
		}
		if err := os.WriteFile(filepath.Join(markerDirectory, "credential-path"), []byte(credentialPath), 0o600); err != nil {
			os.Exit(70)
		}
		signalChannel := make(chan os.Signal, 1)
		signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
		if _, err := startedWriter.Write([]byte{1}); err != nil {
			os.Exit(70)
		}
		_ = startedWriter.Close()
		<-signalChannel
		if _, err := canceledWriter.Write([]byte{1}); err != nil {
			os.Exit(70)
		}
		_ = canceledWriter.Close()
		var releaseByte [1]byte
		if _, err := releaseReader.Read(releaseByte[:]); err != nil {
			os.Exit(70)
		}
		_ = releaseReader.Close()
		if _, err := os.Stat(credentialPath); err != nil {
			_ = os.WriteFile(filepath.Join(markerDirectory, "credential-removed-before-cleanup"), []byte(err.Error()), 0o600)
			os.Exit(70)
		}
		if _, err := fmt.Fprintln(os.Stderr, "[proxy acceptance] temporary client cleanup completed"); err != nil {
			os.Exit(141)
		}
		if err := os.WriteFile(filepath.Join(markerDirectory, "cleanup-completed"), []byte("complete\n"), 0o600); err != nil {
			os.Exit(70)
		}
		if err := os.MkdirAll(filepath.Dir(resultPath), 0o700); err != nil {
			os.Exit(70)
		}
		if err := os.WriteFile(resultPath, []byte("server/proxy\thttp\tFAIL\tcanceled after cleanup\n"), 0o600); err != nil {
			os.Exit(70)
		}
		os.Exit(130)
	}
	if runtime.GOOS == "windows" {
		t.Skip("the proxy acceptance wrapper requires Unix process groups")
	}
	for _, commandName := range []string{"bash", "mkfifo", "tee", "timeout"} {
		if _, err := exec.LookPath(commandName); err != nil {
			t.Skipf("%s is unavailable: %v", commandName, err)
		}
	}

	temporaryRoot := t.TempDir()
	workspaceRoot := filepath.Join(temporaryRoot, "workspace")
	serverRoot := filepath.Join(workspaceRoot, "server")
	proxyDirectory := filepath.Join(serverRoot, "proxy")
	testsDirectory := filepath.Join(workspaceRoot, "tests")
	markerDirectory := filepath.Join(temporaryRoot, "markers")
	temporaryDirectory := filepath.Join(temporaryRoot, "tmp")
	for _, directory := range []string{proxyDirectory, testsDirectory, markerDirectory, temporaryDirectory} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	wrapperBytes, err := os.ReadFile(filepath.Join("..", "test-main.sh"))
	if err != nil {
		t.Fatal(err)
	}
	wrapperPath := filepath.Join(proxyDirectory, "test-main.sh")
	if err := os.WriteFile(wrapperPath, wrapperBytes, 0o700); err != nil {
		t.Fatal(err)
	}
	gatePath := filepath.Join(testsDirectory, "network-intensive-suite-lock.sh")
	if err := os.WriteFile(gatePath, []byte(`#!/usr/bin/env bash
[[ "${1:-}" == --verify-held && "${2:-}" == main-acceptance ]]
`), 0o700); err != nil {
		t.Fatal(err)
	}
	configReaderPath := filepath.Join(testsDirectory, "read-tests-config.sh")
	if err := os.WriteFile(configReaderPath, []byte(`#!/usr/bin/env bash
case "${1:-}" in
  --ready) exit 0 ;;
  get)
    case "${2:-}" in
      data_plane_account.email) printf 'proxy-wrapper@example.invalid\n' ;;
      data_plane_account.password) printf 'synthetic-password\n' ;;
      *) exit 64 ;;
    esac
    ;;
  *) exit 64 ;;
esac
`), 0o700); err != nil {
		t.Fatal(err)
	}
	vaultPath := filepath.Join(temporaryRoot, "synthetic-tests.yml")
	if err := os.WriteFile(vaultPath, []byte("synthetic: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testBinaryPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	fakeRunnerPath := filepath.Join(temporaryRoot, "proxy-acceptance-main")
	if err := os.WriteFile(fakeRunnerPath, []byte(`#!/usr/bin/env bash
credential_path=""
result_path=""
for argument in "$@"; do
  case "$argument" in
    --credentials=*) credential_path="${argument#*=}" ;;
    --result-file=*) result_path="${argument#*=}" ;;
  esac
done
export URNETWORK_PROXY_WRAPPER_CREDENTIAL_PATH="$credential_path"
export URNETWORK_PROXY_WRAPPER_RESULT_PATH="$result_path"
exec "$URNETWORK_PROXY_WRAPPER_TEST_BINARY" -test.run='^TestProxyAcceptanceWrapperWaitsForCleanupAfterInterrupt$' -test.count=1
`), 0o700); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(temporaryRoot, "output", "results.tsv")
	outputPath := filepath.Join(temporaryRoot, "wrapper-output")
	outputFile, err := os.OpenFile(outputPath, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer outputFile.Close()
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

	overrides := map[string]string{
		"TMPDIR":                                   temporaryDirectory,
		"URNETWORK_NETWORK_TEST_LOCK_HELD":         "1",
		"URNETWORK_ROOT":                           workspaceRoot,
		"UR_ACCEPT_PROXY_BIN":                      fakeRunnerPath,
		"UR_ACCEPT_PROXY_TIMEOUT":                  "20s",
		"UR_ACCEPT_RESULT_FILE":                    resultPath,
		"UR_ACCEPT_VAULT":                          vaultPath,
		proxyWrapperSignalChild:                    "1",
		"URNETWORK_PROXY_WRAPPER_MARKER_DIRECTORY": markerDirectory,
		"URNETWORK_PROXY_WRAPPER_TEST_BINARY":      testBinaryPath,
	}
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, variable := range os.Environ() {
		name, _, _ := strings.Cut(variable, "=")
		if _, overridden := overrides[name]; !overridden {
			environment = append(environment, variable)
		}
	}
	for name, value := range overrides {
		environment = append(environment, name+"="+value)
	}

	command := exec.Command("bash", wrapperPath, "--skip-build")
	command.Env = environment
	command.ExtraFiles = []*os.File{startedWriter, canceledWriter, releaseReader}
	command.Stdout = outputFile
	command.Stderr = outputFile
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = startedWriter.Close()
	_ = canceledWriter.Close()
	_ = releaseReader.Close()
	waitErrChannel := make(chan error, 1)
	go func() {
		waitErrChannel <- command.Wait()
	}()
	wrapperWaited := false
	defer func() {
		if !wrapperWaited {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-waitErrChannel
		}
	}()
	readBarrier := func(name string, reader *os.File) {
		resultChannel := make(chan error, 1)
		go func() {
			var markerByte [1]byte
			_, readErr := reader.Read(markerByte[:])
			resultChannel <- readErr
		}()
		select {
		case readErr := <-resultChannel:
			if readErr != nil {
				t.Fatalf("read %s barrier: %v", name, readErr)
			}
		case wrapperErr := <-waitErrChannel:
			wrapperWaited = true
			t.Fatalf("wrapper exited before %s barrier: %v", name, wrapperErr)
		case <-time.After(10 * time.Second):
			t.Fatalf("wrapper did not reach %s barrier", name)
		}
	}
	readBarrier("runner started", startedReader)
	wrapperSignal := syscall.SIGINT
	if os.Getenv(proxyWrapperSignalName) == "TERM" {
		wrapperSignal = syscall.SIGTERM
	}
	if err := syscall.Kill(-command.Process.Pid, wrapperSignal); err != nil {
		t.Fatal(err)
	}
	readBarrier("runner canceled", canceledReader)
	if _, err := releaseWriter.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	_ = releaseWriter.Close()

	var wrapperErr error
	select {
	case wrapperErr = <-waitErrChannel:
		wrapperWaited = true
	case <-time.After(10 * time.Second):
		t.Fatal("wrapper did not join the canceled runner")
	}
	exitErr, ok := wrapperErr.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 130 {
		t.Fatalf("wrapper exit = %v, want status 130", wrapperErr)
	}
	if err := outputFile.Sync(); err != nil {
		t.Fatal(err)
	}
	outputBytes, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(outputBytes), "temporary client cleanup completed") {
		t.Fatalf("wrapper output omitted cleanup completion: %q", outputBytes)
	}
	if _, err := os.Stat(filepath.Join(markerDirectory, "cleanup-completed")); err != nil {
		t.Fatalf("runner cleanup marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(markerDirectory, "credential-removed-before-cleanup")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrapper removed credentials before runner cleanup: %v", err)
	}
	resultBytes, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("runner result marker: %v", err)
	}
	if !strings.Contains(string(resultBytes), "canceled after cleanup") {
		t.Fatalf("runner result = %q", resultBytes)
	}
	credentialPathBytes, err := os.ReadFile(filepath.Join(markerDirectory, "credential-path"))
	if err != nil {
		t.Fatal(err)
	}
	credentialPath := string(credentialPathBytes)
	if _, err := os.Stat(credentialPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential temporary file remained: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(credentialPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential temporary directory remained: %v", err)
	}
	runnerPidBytes, err := os.ReadFile(filepath.Join(markerDirectory, "runner-pid"))
	if err != nil {
		t.Fatal(err)
	}
	runnerPid, err := strconv.Atoi(string(runnerPidBytes))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(runnerPid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("runner process %d was not reaped: %v", runnerPid, err)
	}
	if err := syscall.Kill(-command.Process.Pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("wrapper process group remained after exit: %v", err)
	}
}
