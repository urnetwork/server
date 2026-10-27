package monitor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// A source-owned receipt must distinguish waiting for shared admission from
// an owned SSH process that reached the remote reader before its deadline.
type pgTransportReceiptProof struct {
	Phase                  string `json:"phase"`
	Spawn                  string `json:"spawn"`
	RemotePhase            string `json:"remote_phase"`
	RemainingBudgetMs      int64  `json:"remaining_budget_ms"`
	SpawnRemainingBudgetMs *int64 `json:"spawn_remaining_budget_ms"`
	OwnerDeadlineExceeded  bool   `json:"owner_deadline_exceeded"`
	Joined                 bool   `json:"joined"`
}

// The barrier proves admission was entered before the real parent deadline.
// No native command is allowed to escape this ungranted request.
type pgTransportBlockedAdmission struct {
	entered chan struct{}
}

func (self *pgTransportBlockedAdmission) acquire(ctx context.Context, _ string) (func() error, error) {
	close(self.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}

// Read the real immutable receipt, including any additive private projection.
func pgTransportReadReceipt(t *testing.T, env *probeEnv) (*pgTransportReceiptProof, []byte) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(env.cfg.stateDir, "pg-query-sample", "receipt-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal("fixture did not produce exactly one immutable PG receipt")
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt struct {
		Complete             bool                     `json:"complete"`
		Reason               string                   `json:"reason"`
		Samples              int                      `json:"samples"`
		SourceFailure        *pgSampleSourceFailure   `json:"source_failure"`
		TransportObservation *pgTransportReceiptProof `json:"transport_observation"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Complete || receipt.Samples != 0 || receipt.Reason != "deadline" || receipt.SourceFailure == nil || receipt.SourceFailure.Phase != "host_transport" || receipt.SourceFailure.Cause != observationErrorClassTimeout {
		t.Fatal("fixture did not reach the actual incomplete PG deadline receipt")
	}
	for _, private := range []string{"secret-test-value", "private-pg-transport-stderr-canary", "192.0.2.1"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private source bytes escaped into the receipt")
		}
	}
	return receipt.TransportObservation, raw
}

// The missing transport proof must fail only after the actual source receipt
// and unstarted-child controls have established the precontact failure.
func TestPgTransportAuthorityPreservesUnspawnedAdmissionDeadline(t *testing.T) {
	var nativeCalls atomic.Int32
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		nativeCalls.Add(1)
		return "", "", errors.New("fixture escaped admission")
	})
	admission := &pgTransportBlockedAdmission{entered: make(chan struct{})}
	env.cfg.sharedSshAdmission = admission
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var findings []finding
	done := make(chan error, 1)
	go func() {
		var err error
		findings, err = (pgQuerySampleProbe{}).check(ctx, env)
		done <- err
	}()
	select {
	case <-admission.entered:
	case <-ctx.Done():
		t.Fatal("fixture never entered shared admission before its deadline")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("fixture failed before immutable receipt storage")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("fixture did not join the canceled admission")
	}
	if nativeCalls.Load() != 0 {
		t.Fatal("ungranted PG request launched an SSH command")
	}
	proof, _ := pgTransportReadReceipt(t, env)
	if proof == nil {
		t.Fatal("PG deadline discarded transport phase/spawn/budget authority for an unstarted admission")
	}
	if proof.Phase != "shared_admission" || proof.Spawn != "not_attempted" || proof.RemotePhase != "unknown" || proof.SpawnRemainingBudgetMs != nil || proof.Joined || !proof.OwnerDeadlineExceeded || proof.RemainingBudgetMs <= 0 || proof.RemainingBudgetMs > 5000 {
		t.Fatal("unstarted shared admission receipt invented execution or reset its parent budget")
	}
	if len(findings) != 1 || strings.Contains(findings[0].observed, "collection_budget=") {
		t.Fatal("unstarted admission invented a native-spawn sampling-window shortfall")
	}
}

// Run only as the local fake-SSH child. A FIFO is an explicit readiness
// barrier; it carries a synthetic child pid and the private nonce solely for
// a receipt non-disclosure assertion. It never carries credentials.
func TestPgTransportAuthoritySshHelper(t *testing.T) {
	if os.Getenv("MONITOR_PG_TRANSPORT_FIXTURE_CHILD") != "1" {
		return
	}
	var input map[string]json.RawMessage
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 262145)).Decode(&input); err != nil {
		os.Exit(91)
	}
	// The fallback keeps the unchanged producer on the same causal RED path.
	nonce := "000102030405060708090a0b0c0d0e0f"
	if raw, ok := input["transport_nonce"]; ok {
		if json.Unmarshal(raw, &nonce) != nil {
			os.Exit(95)
		}
	}
	if decoded, err := hex.DecodeString(nonce); err != nil || len(decoded) != 16 {
		os.Exit(96)
	}
	for _, phase := range []string{"remote_started", "database_identity"} {
		raw, _ := json.Marshal(map[string]any{"schema": 1, "nonce": nonce, "phase": phase})
		_, _ = fmt.Fprintln(os.Stderr, "monitor-pg-phase "+string(raw))
	}
	_, _ = io.WriteString(os.Stderr, "private-pg-transport-stderr-canary\n")
	ready, err := os.OpenFile(os.Getenv("MONITOR_PG_TRANSPORT_FIXTURE_READY"), os.O_WRONLY, 0)
	if err != nil {
		os.Exit(92)
	}
	_, err = fmt.Fprintln(ready, os.Getpid(), nonce)
	_ = ready.Close()
	if err != nil {
		os.Exit(93)
	}
	// The parent kills and waits for this exact process after the barrier.
	time.Sleep(time.Hour)
	os.Exit(94)
}

// Force a passed boundary check followed by a shorter actual Start window,
// modeling descheduling after the check. The native local child still proves
// Start/Wait and remote progress; passing the guard never promises completion.
func TestPgTransportAuthorityPreservesRemoteProgressAfterStartCheckRace(t *testing.T) {
	fixture := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	launcher := "#!/bin/sh\nexec " + shellSingleQuote(executable) + " -test.run '^TestPgTransportAuthoritySshHelper$' -test.count=1\n"
	if err := os.WriteFile(filepath.Join(fixture, "ssh"), []byte(launcher), 0700); err != nil {
		t.Fatal(err)
	}
	readyPath := filepath.Join(fixture, "ready")
	if err := syscall.Mkfifo(readyPath, 0600); err != nil {
		t.Fatal(err)
	}
	ready, err := os.OpenFile(readyPath, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer ready.Close()
	t.Setenv("PATH", fixture+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("WARP_HOME", t.TempDir())
	t.Setenv("MONITOR_PG_TRANSPORT_FIXTURE_CHILD", "1")
	t.Setenv("MONITOR_PG_TRANSPORT_FIXTURE_READY", readyPath)
	if resolved, err := exec.LookPath("ssh"); err != nil || resolved != filepath.Join(fixture, "ssh") {
		t.Fatal("fixture did not exclusively select the local fake SSH executable")
	}
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("fixture used the injected transport instead of the owned SSH process")
		return "", "", nil
	})
	env.runner.(*runner).runSSH = func(ctx context.Context, args []string, stdin string) (string, string, error) {
		deadline, known := ctx.Deadline()
		if !known {
			t.Fatal("fixture lost its original owner deadline")
		}
		observation := pgSampleTransportFromContext(ctx)
		observation.startClock = func() time.Time { return deadline.Add(-pgSampleMinimumWaitBudget) }
		return runSSHCommand(ctx, args, stdin)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var findings []finding
	done := make(chan error, 1)
	go func() {
		var err error
		findings, err = (pgQuerySampleProbe{}).check(ctx, env)
		done <- err
	}()
	type readyProof struct {
		pid   int
		nonce string
	}
	childReady := make(chan readyProof, 1)
	go func() {
		var proof readyProof
		if _, err := fmt.Fscan(ready, &proof.pid, &proof.nonce); err == nil {
			childReady <- proof
		}
	}()
	var childProof readyProof
	select {
	case childProof = <-childReady:
		if childProof.pid <= 0 || len(childProof.nonce) != 32 {
			t.Fatal("fixture child readiness lacked a process identity")
		}
	case <-ctx.Done():
		<-done
		t.Fatal("fixture child never reached its remote-progress barrier")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal("fixture failed before immutable receipt storage")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("fixture did not join the owned SSH child")
	}
	if err := syscall.Kill(childProof.pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatal("fixture child still exists after the transport returned")
	}
	proof, raw := pgTransportReadReceipt(t, env)
	if strings.Contains(string(raw), childProof.nonce) {
		t.Fatal("private attempt nonce escaped into the receipt")
	}
	if proof == nil {
		t.Fatal("PG deadline discarded transport phase/spawn/budget authority after proved remote progress")
	}
	if proof.Phase != "ssh_running" || proof.Spawn != "started" || proof.RemotePhase != "database_identity" || !proof.Joined || !proof.OwnerDeadlineExceeded || proof.RemainingBudgetMs <= 0 || proof.RemainingBudgetMs > 5000 || proof.SpawnRemainingBudgetMs == nil || *proof.SpawnRemainingBudgetMs <= 0 || *proof.SpawnRemainingBudgetMs > proof.RemainingBudgetMs {
		t.Fatal("owned SSH deadline lost its remote phase or renewed its parent budget")
	}
	if len(findings) != 1 || findings[0].class != "pg-query-sample-unavailable" || findings[0].tier != tierWarn || findings[0].sustain != 1 || !strings.Contains(findings[0].observed, "reason=deadline phase=host_transport cause=observation-timeout") {
		t.Fatal("sampling-window qualification changed the original failed observation")
	}
	want := fmt.Sprintf("collection_budget=below-mandatory-waits minimum_wait_ms=22000 spawn_remaining_budget_ms=%d", *proof.SpawnRemainingBudgetMs)
	if !strings.Contains(findings[0].observed, want) || !strings.Contains(string(raw), want) {
		t.Fatal("actual started/joined deadline hid the proved insufficient mandatory-wait budget")
	}
}
