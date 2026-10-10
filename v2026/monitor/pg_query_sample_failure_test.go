package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runPgSampleProgramFixture(t *testing.T, program, stub, input string) (string, error) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	// LookPath may name a pyenv shell shim. Resolve the actual interpreter
	// before restricting PATH to the fake psql directory; otherwise the test
	// fails in the shim's env/bash bootstrap instead of our Python program.
	resolveCtx, cancelResolve := context.WithTimeout(t.Context(), 5*time.Second)
	resolved, resolveErr := exec.CommandContext(resolveCtx, python, "-c", "import sys;print(sys.executable)").Output()
	cancelResolve()
	if resolveErr != nil || !filepath.IsAbs(strings.TrimSpace(string(resolved))) {
		t.Fatal("Python interpreter resolution failed")
	}
	python = strings.TrimSpace(string(resolved))
	dir := t.TempDir()
	if stub != "" {
		if err := os.WriteFile(filepath.Join(dir, "psql"), []byte("#!"+python+"\nimport sys\n"+stub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-c", program)
	cmd.Env = append(os.Environ(), "PATH="+dir)
	cmd.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if stderr.Len() != 0 {
		t.Fatal("remote adapter leaked a traceback or child stderr")
	}
	return string(out), err
}

func pgSampleProgramInput(t *testing.T) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"password": "private-credential-canary", "user": "fixture", "database": "fixture", "port": 5432, "sql": pgQuerySampleSQL("fixture")})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestPgQuerySampleRemoteFailurePhasesAndPrivacy(t *testing.T) {
	for _, item := range []struct{ name, stderr, phase, cause string }{
		{"connect", "connection refused to private-address-canary", "connection", "connection_refused"},
		{"auth", "password authentication failed for private-credential-canary", "connection", "authentication"},
		{"hba", "no pg_hba.conf entry for private-address-canary", "connection", "pg_hba"},
		{"connect-timeout", "timeout expired", "connection", "connection_timeout"},
		{"tls", "SSL error private-credential-canary", "connection", "tls"},
		{"missing-database", "database private-database-canary does not exist", "connection", "database_missing"},
		{"unknown", "private-query-canary", "child_process", "child_exit_unknown"},
	} {
		t.Run(item.name, func(t *testing.T) {
			message, _ := json.Marshal(item.stderr)
			stub := "sys.stdin.read()\nsys.stderr.write(" + string(message) + ")\nsys.exit(2)\n"
			out, err := runPgSampleProgramFixture(t, pgQuerySampleProgram, stub, pgSampleProgramInput(t))
			f, valid := parsePgSampleSourceFailure(out)
			if err == nil || !valid || f.Phase != item.phase || f.Cause != item.cause || strings.Contains(out, "private-") {
				t.Fatal("source failure did not retain only its finite owning class")
			}
		})
	}
	for _, phase := range []string{"identity", "authority", "history_start", "sample_wait", "activity", "blockers", "history_end"} {
		t.Run(phase, func(t *testing.T) {
			stub := "s=sys.stdin.read().splitlines()\nn=next(i+2 for i,v in enumerate(s) if v=='-- monitor_query_sample_phase:" + phase + "')\nsys.stderr.write('psql:<stdin>:%s: ERROR: canceling statement due to statement timeout private-query-canary' % n)\nsys.exit(3)\n"
			out, err := runPgSampleProgramFixture(t, pgQuerySampleProgram, stub, pgSampleProgramInput(t))
			f, valid := parsePgSampleSourceFailure(out)
			if err == nil || !valid || f.Phase != phase || f.Cause != "statement_timeout" || strings.Contains(out, "private-") {
				t.Fatal("psql source location did not resolve to its exact fixed SQL phase")
			}
		})
	}
}

func TestPgQuerySampleRemoteBootstrapBoundsAndCompleteControl(t *testing.T) {
	t.Run("input", func(t *testing.T) {
		out, err := runPgSampleProgramFixture(t, pgQuerySampleProgram, "", "private-credential-canary")
		f, valid := parsePgSampleSourceFailure(out)
		if err == nil || !valid || f.Phase != "bootstrap" || f.Cause != "invalid_input" {
			t.Fatal("invalid bootstrap input lost its phase")
		}
	})
	t.Run("executable", func(t *testing.T) {
		out, err := runPgSampleProgramFixture(t, pgQuerySampleProgram, "", pgSampleProgramInput(t))
		f, valid := parsePgSampleSourceFailure(out)
		if err == nil || !valid || f.Phase != "child_start" || f.Cause != "missing_executable" {
			t.Fatal("missing psql lost its launch phase")
		}
	})
	t.Run("stderr-cap", func(t *testing.T) {
		stub := "sys.stdin.read()\nsys.stderr.write('x'*100000+'private-credential-canary')\nsys.exit(2)\n"
		out, err := runPgSampleProgramFixture(t, pgQuerySampleProgram, stub, pgSampleProgramInput(t))
		f, valid := parsePgSampleSourceFailure(out)
		if err == nil || !valid || !f.StderrTruncated || f.Cause != "child_exit_unknown" || strings.Contains(out, "private-") {
			t.Fatal("stderr cap failed to drain or invented a cause beyond its prefix")
		}
	})
	t.Run("owner", func(t *testing.T) {
		// Execute the same subprocess/select/kill path, scaling only its remote
		// deadline for a local stalled-child test. Production remains32 seconds.
		program := strings.Replace(pgQuerySampleProgram, "deadline=time.monotonic()+32", "deadline=time.monotonic()+0.05", 1)
		stub := "import time\nsys.stdin.read()\ntime.sleep(2)\n"
		out, err := runPgSampleProgramFixture(t, program, stub, pgSampleProgramInput(t))
		f, valid := parsePgSampleSourceFailure(out)
		if err == nil || !valid || f.Cause != "owner_deadline" || f.Phase != "child_process" {
			t.Fatal("stalled child lost its bounded owner terminal")
		}
	})
	t.Run("complete", func(t *testing.T) {
		now := time.Now().UTC().Truncate(time.Second)
		frames := pgSampleTestEncode(t, pgSampleTestFrames(now))
		quoted, _ := json.Marshal(frames)
		stub := "sys.stdin.read()\nsys.stdout.write(" + string(quoted) + ")\n"
		out, err := runPgSampleProgramFixture(t, pgQuerySampleProgram, stub, pgSampleProgramInput(t))
		r, parseErr := parsePgQuerySample(out, now)
		if err != nil || parseErr != nil || !r.Complete || r.Samples != 12 {
			t.Fatal("finite failure projection broke the healthy12-snapshot control")
		}
	})
}

func TestPgQuerySampleRemoteSQLGuardAndPGSSFailure(t *testing.T) {
	for _, item := range []struct{ phase, message, cause string }{
		{"authority", "sample authority mismatch", "authority_mismatch"},
		{"history_start", "relation pg_stat_statements does not exist", "pgss_unavailable"},
		{"history_start", "pg_stat_statements must be loaded via shared_preload_libraries", "pgss_unavailable"},
		{"history_end", "relation pg_stat_statements_info does not exist", "pgss_unavailable"},
		{"activity", "column private-column-canary does not exist", "schema_mismatch"},
		{"history_start", "permission denied for view pg_stat_statements", "permission_denied"},
		{"activity", "canceling statement due to lock timeout", "lock_timeout"},
	} {
		t.Run(item.phase+"/"+item.cause, func(t *testing.T) {
			message, _ := json.Marshal(item.message)
			stub := "s=sys.stdin.read().splitlines()\nn=next(i+2 for i,v in enumerate(s) if v=='-- monitor_query_sample_phase:" + item.phase + "')\nsys.stderr.write('psql:<stdin>:%s: ERROR: ' % n + " + string(message) + ")\nsys.exit(3)\n"
			out, err := runPgSampleProgramFixture(t, pgQuerySampleProgram, stub, pgSampleProgramInput(t))
			f, valid := parsePgSampleSourceFailure(out)
			if err == nil || !valid || f.Phase != item.phase || f.Cause != item.cause || strings.Contains(out, "private-") {
				t.Fatal("SQL/PGSS authority boundary lost its fixed cause")
			}
		})
	}
}

func TestPgQuerySampleFailureEnvelopeRejectsUnknownOrRawFields(t *testing.T) {
	good := `{"kind":"source_failure","schema":1,"phase":"history_start","cause":"statement_timeout","stderr_truncated":false}`
	for _, raw := range []string{
		strings.Replace(good, `"history_start"`, `"private-query-canary"`, 1),
		strings.Replace(good, `"statement_timeout"`, `"private-credential-canary"`, 1),
		strings.Replace(good, `false`, `null`, 1),
		strings.Replace(good, `"schema":1`, `"schema":2`, 1),
		strings.Replace(good, `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(good, `"schema":1`, `"schema":1,"stderr":"private-query-canary"`, 1),
		good + good,
		good + strings.Repeat(" ", 1024),
	} {
		if _, valid := parsePgSampleSourceFailure(raw); valid {
			t.Fatal("unbounded, ambiguous or private remote projection admitted")
		}
	}
}

func TestPgQuerySampleFailureReceiptRetainsPhaseWithoutPrivateData(t *testing.T) {
	raw := `{"kind":"source_failure","schema":1,"phase":"history_start","cause":"statement_timeout","stderr_truncated":false}`
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		return raw, "private-credential-canary", errors.New("private-query-canary")
	})
	findings, err := (pgQuerySampleProbe{}).check(t.Context(), env)
	if err != nil || len(findings) != 1 || !strings.Contains(findings[0].observed, "phase=history_start cause=statement_timeout") {
		t.Fatal("owning failure did not reach the first-cadence finding", err)
	}
	paths, _ := filepath.Glob(filepath.Join(env.cfg.stateDir, "pg-query-sample", "receipt-*.json"))
	if len(paths) != 1 {
		t.Fatal("missing immutable failure receipt")
	}
	data, err := os.ReadFile(paths[0])
	var receipt pgQuerySampleReceipt
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.Complete || receipt.SourceFailure == nil || receipt.SourceFailure.Phase != "history_start" || strings.Contains(string(data), "private-") {
		t.Fatal("private failure data escaped or missing sample became complete")
	}
}

func TestPgQuerySampleTransportFailureCannotInventSQLPhase(t *testing.T) {
	for _, item := range []struct {
		exit         int
		phase, cause string
	}{
		{74, "host_identity", "authority_mismatch"},
		{124, "remote_owner", "owner_deadline"},
		{127, "remote_bootstrap", "missing_executable"},
		{255, "host_transport", observationErrorClassSSHExit255},
	} {
		err := exec.Command("sh", "-c", fmt.Sprintf("exit %d", item.exit)).Run()
		f := pgSampleTransportFailure(&sshCommandError{err: fmt.Errorf("private-credential-canary: %w", err)})
		if f.Phase != item.phase || f.Cause != item.cause {
			t.Fatalf("transport exit lost finite phase: %v", item.exit)
		}
	}
}
