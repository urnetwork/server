package monitor

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

// These controls inspect the actual private receipt without borrowing the
// implementation's observation type or accepting raw transport diagnostics.
type pgTransportControlReceipt struct {
	Complete             bool                   `json:"complete"`
	Samples              int                    `json:"samples"`
	SourceFailure        *pgSampleSourceFailure `json:"source_failure"`
	TransportObservation *struct {
		pgTransportReceiptProof
		NativeExitCode *int   `json:"native_exit_code"`
		ProgressStatus string `json:"progress_status"`
	} `json:"transport_observation"`
}

// Every check below owns a new state directory and exactly one source attempt.
func pgTransportReadControlReceipt(t *testing.T, env *probeEnv) (pgTransportControlReceipt, []byte) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(env.cfg.stateDir, "pg-query-sample", "receipt-*.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal("fixture lacks exactly one immutable PG receipt")
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var receipt pgTransportControlReceipt
	if json.Unmarshal(raw, &receipt) != nil || receipt.TransportObservation == nil {
		t.Fatal("receipt lost its finite transport observation")
	}
	for _, private := range []string{"secret-test-value", "private-pg-transport-stderr-canary", "192.0.2.1"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private transport bytes entered the receipt")
		}
	}
	return receipt, raw
}

// A successful injected callback does not prove an exec.Cmd lifecycle, even
// when it returns apparently valid progress records for the current nonce.
func TestPgTransportAuthorityInjectedSuccessKeepsNativeUnknown(t *testing.T) {
	var nonce string
	var env *probeEnv
	env = pgSampleTestEnv(t, func(args []string, stdin string) (string, string, error) {
		var input struct {
			Nonce string `json:"transport_nonce"`
		}
		if json.Unmarshal([]byte(stdin), &input) != nil {
			t.Fatal("fixture stdin is not JSON")
		}
		nonce = input.Nonce
		if decoded, err := hex.DecodeString(nonce); err != nil || len(decoded) != 16 {
			t.Fatal("PG producer omitted its attempt nonce")
		}
		if strings.Contains(strings.Join(args, " "), nonce) {
			t.Fatal("private attempt nonce entered argv")
		}
		stderr := pgCompletedProgressStream(nonce, 12, true) + "private-pg-transport-stderr-canary\n"
		return pgSampleTestEncode(t, pgSampleTestFrames(env.now())), stderr, nil
	})
	if _, err := (pgQuerySampleProbe{}).check(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	receipt, raw := pgTransportReadControlReceipt(t, env)
	proof := receipt.TransportObservation
	if !receipt.Complete || receipt.Samples != 12 || receipt.SourceFailure != nil || proof.Phase != "ssh_start" || proof.Spawn != "unknown" || proof.RemotePhase != "unknown" || proof.Joined || proof.NativeExitCode != nil || proof.SpawnRemainingBudgetMs != nil || proof.ProgressStatus != "unobserved" || proof.OwnerDeadlineExceeded {
		t.Fatal("injected success invented native execution or lost the complete sample")
	}
	if proof.RemainingBudgetMs <= 0 || proof.RemainingBudgetMs > 40000 || strings.Contains(string(raw), nonce) {
		t.Fatal("receipt reset its owner or retained its nonce")
	}
	if pgCompletedProgressReceipt(t, raw).Frames != nil {
		t.Fatal("injected callback invented observed remote sample frames")
	}
}

// Already-running work may legitimately consume this queue-inclusive owner.
// The observer records that loss without releasing another owner's slots or
// advancing the durable next-attempt floor to obtain an immediate retry.
func TestPgTransportAuthorityHostAdmissionKeepsParentBudgetAndCadence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { calls.Add(1); return "", "", nil })
		env.cfg.pgQuerySampleUntil = time.Time{}
		env.cfg.pgQuerySampleContinuous = true
		env.now = time.Now
		runner := env.runner.(*runner)
		host := env.cfg.hostByRole("pg-primary")
		address := host.addr(env.cfg.addressMode)
		for range maxConcurrentRemoteCommandsPerHost {
			release, err := runner.remoteCommands.acquire(t.Context(), address)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
		}
		ctx, cancel := context.WithTimeout(t.Context(), pgQuerySampleBudget)
		defer cancel()
		time.Sleep(9 * time.Second) // virtual time spent before this probe is admitted
		if _, err := (pgQuerySampleProbe{}).check(ctx, env); err != nil {
			t.Fatal(err)
		}
		receipt, _ := pgTransportReadControlReceipt(t, env)
		proof := receipt.TransportObservation
		if receipt.Complete || calls.Load() != 0 || proof.Phase != "host_admission" || proof.Spawn != "not_attempted" || proof.RemotePhase != "unknown" || proof.Joined || !proof.OwnerDeadlineExceeded || proof.RemainingBudgetMs != 31000 || proof.SpawnRemainingBudgetMs != nil {
			t.Fatal("host queue expiration invented a child or renewed the inherited owner")
		}
		if len(runner.remoteCommands.hostSlots(address)) != maxConcurrentRemoteCommandsPerHost {
			t.Fatal("ungranted PG request released another owner's host slot")
		}
		state, err := pgSampleReadCadence(filepath.Join(env.cfg.stateDir, "pg-query-sample"), time.Now())
		if err != nil || !state.NextEligibleAt.Equal(time.Now().Add(pgQuerySampleCadence)) {
			t.Fatal("queue failure changed the terminal cadence floor")
		}
		findings, err := (pgQuerySampleProbe{}).check(t.Context(), env)
		if err != nil || calls.Load() != 0 || len(findings) != 1 || !strings.Contains(findings[0].observed, "cadence-not-due") {
			t.Fatal("queue failure caused an immediate retry")
		}
		_, _ = pgTransportReadControlReceipt(t, env)
	})
}

// An executable format error comes from the actual native Start operation;
// entering the runner is not enough to claim a started or joined child.
func TestPgTransportAuthorityNativeStartFailureHasNoChild(t *testing.T) {
	fixture := t.TempDir()
	if err := os.WriteFile(filepath.Join(fixture, "ssh"), []byte("synthetic invalid executable\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fixture)
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { t.Fatal("injected runner used"); return "", "", nil })
	env.runner.(*runner).runSSH = runSSHCommand
	if _, err := (pgQuerySampleProbe{}).check(t.Context(), env); err != nil {
		t.Fatal(err)
	}
	receipt, _ := pgTransportReadControlReceipt(t, env)
	proof := receipt.TransportObservation
	if receipt.Complete || receipt.SourceFailure == nil || proof.Phase != "ssh_start" || proof.Spawn != "start_failed" || proof.RemotePhase != "unknown" || proof.Joined || proof.NativeExitCode != nil || proof.SpawnRemainingBudgetMs != nil || proof.ProgressStatus != "unobserved" || proof.OwnerDeadlineExceeded {
		t.Fatal("failed native Start became a joined child or remote observation")
	}
}

// The parent exclusively substitutes this local process for SSH. It consumes
// the existing stdin frame, emits finite synthetic output, and exits directly
// so the test harness cannot append a PASS line to the source protocol.
func TestPgTransportAuthorityCompletedSshHelper(t *testing.T) {
	if os.Getenv("MONITOR_PG_TRANSPORT_COMPLETE_CHILD") != "1" {
		return
	}
	var input struct {
		Nonce string `json:"transport_nonce"`
	}
	if json.NewDecoder(io.LimitReader(os.Stdin, 262145)).Decode(&input) != nil || len(input.Nonce) != 32 {
		os.Exit(91)
	}
	mode := os.Getenv("MONITOR_PG_TRANSPORT_COMPLETE_MODE")
	if mode == "complete" {
		for _, phase := range []string{"remote_started", "database_identity"} {
			raw, _ := json.Marshal(map[string]any{"schema": 1, "nonce": input.Nonce, "phase": phase})
			fmt.Fprintln(os.Stderr, "monitor-pg-phase "+string(raw))
		}
		io.WriteString(os.Stdout, pgSampleTestEncode(t, pgSampleTestFrames(time.Now().UTC().Truncate(time.Second))))
		os.Exit(0)
	}
	fmt.Fprintln(os.Stderr, "private-pg-transport-stderr-canary")
	if mode == "remote-owner" {
		os.Exit(124)
	}
	if mode == "ssh-exit" {
		os.Exit(255)
	}
	os.Exit(92)
}

// Native exit is retained separately from source authority; a zero exit plus
// all twelve validated frames remains complete, while owner/255 exits do not
// become database progress or host-health evidence.
func TestPgTransportAuthorityNativeTerminalControls(t *testing.T) {
	for _, testCase := range []struct {
		mode     string
		exit     int
		complete bool
	}{
		{mode: "complete", exit: 0, complete: true},
		{mode: "remote-owner", exit: 124},
		{mode: "ssh-exit", exit: 255},
	} {
		fixture := t.TempDir()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		launcher := "#!/bin/sh\nexec " + shellSingleQuote(executable) + " -test.run '^TestPgTransportAuthorityCompletedSshHelper$' -test.count=1\n"
		if err := os.WriteFile(filepath.Join(fixture, "ssh"), []byte(launcher), 0700); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", fixture+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("WARP_HOME", t.TempDir())
		t.Setenv("MONITOR_PG_TRANSPORT_COMPLETE_CHILD", "1")
		t.Setenv("MONITOR_PG_TRANSPORT_COMPLETE_MODE", testCase.mode)
		if resolved, err := exec.LookPath("ssh"); err != nil || resolved != filepath.Join(fixture, "ssh") {
			t.Fatal("fixture did not select the local SSH child")
		}
		env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { t.Fatal("injected runner used"); return "", "", nil })
		env.runner.(*runner).runSSH = runSSHCommand
		findings, err := (pgQuerySampleProbe{}).check(t.Context(), env)
		if err != nil {
			t.Fatal(err)
		}
		receipt, raw := pgTransportReadControlReceipt(t, env)
		proof := receipt.TransportObservation
		if receipt.Complete != testCase.complete || proof.Phase != "ssh_running" || proof.Spawn != "started" || !proof.Joined || proof.NativeExitCode == nil || *proof.NativeExitCode != testCase.exit || proof.OwnerDeadlineExceeded || proof.SpawnRemainingBudgetMs == nil || *proof.SpawnRemainingBudgetMs > proof.RemainingBudgetMs {
			t.Fatalf("mode %s lost native terminal authority or renewed its owner", testCase.mode)
		}
		if testCase.complete {
			if receipt.Samples != 12 || receipt.SourceFailure != nil || proof.RemotePhase != "database_identity" || proof.ProgressStatus != "valid" {
				t.Fatal("native healthy sample lost exact source/progress coverage")
			}
		} else if receipt.SourceFailure == nil || proof.RemotePhase != "unknown" || proof.ProgressStatus != "absent" {
			t.Fatal("native failure invented remote progress")
		}
		if strings.Contains(string(raw), "collection_budget=") {
			t.Fatal("completed or non-owner failure invented an owner-window shortfall")
		}
		for _, finding := range findings {
			if strings.Contains(finding.observed, "collection_budget=") {
				t.Fatal("completed or non-owner finding invented an owner-window shortfall")
			}
		}
	}
}
