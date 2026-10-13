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

func pgSampleTestFrames(now time.Time) []map[string]any {
	at := float64(now.Unix())
	frames := []map[string]any{{"kind": "identity", "sample": 0, "at": at, "primary": true, "read_only": true, "database_ok": true, "track_activity_query_size": 1024, "pgss_version": "1.10"}}
	history := func(i int, t float64, calls, ms float64) map[string]any {
		return map[string]any{"kind": "history", "sample": i, "at": t, "reset": at - 1000, "total": 1, "rows": [][]any{{"714727414314", calls, ms, 100., "audit_daily_delete"}}}
	}
	frames = append(frames, history(0, at+.01, 10, 1000))
	for i := 0; i < 12; i++ {
		frames = append(frames, map[string]any{"kind": "activity", "sample": i, "at": at + .02 + float64(2*i), "total": 9, "groups": 2, "query_text_truncated": 0, "rows": [][]any{{"714727414314", "active", "Lock:transactionid", "loopback", "unset", "client backend", "audit_daily_delete", "current", 5, 200., 250., 2.}, {"none", "idle in transaction", "Client:ClientRead", "loopback", "other", "client backend", "other", "current", 4, 20., 25., 20.}}})
	}
	frames = append(frames, map[string]any{"kind": "blockers", "sample": 0, "at": at + 22.03, "total": 5, "rows": [][]any{{"5432167", "7865423", "714727414314", "9087614561", "audit_daily_delete", "commit", "active", "IO:WALSync", "loopback", "unset", 260., true, 1}}}, history(1, at+22.04, 14, 1200))
	return frames
}
func pgSampleTestEncode(t *testing.T, frames []map[string]any) string {
	t.Helper()
	var b strings.Builder
	for _, f := range frames {
		raw, err := json.Marshal(f)
		if err != nil {
			t.Fatal("fixture marshal")
		}
		b.Write(raw)
		b.WriteByte('\n')
	}
	return b.String()
}
func TestPgQuerySampleCompleteAndPrivacy(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	r, err := parsePgQuerySample(pgSampleTestEncode(t, pgSampleTestFrames(now)), now)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Complete || r.Samples != 12 || len(r.Load) != 2 || r.Load[0].BackendSamples != 60 || r.Load[0].Peak != 5 || r.Load[0].SeenSamples != 12 || r.Load[0].CompletedMeanMS == nil || *r.Load[0].CompletedMeanMS != 1200./14 || r.HistoryDeltaQualified || len(r.Completed) != 1 || r.Completed[0].Calls != 14 || r.Completed[0].ExecMS != 1200 {
		t.Fatal("wrong finite aggregation")
	}
	if len(r.Blockers) != 1 || r.Blockers[0].BlockerFamily != "commit" || r.Blockers[0].Wait != "IO:WALSync" {
		t.Fatal("blocker attribution missing")
	}
	raw, _ := json.Marshal(r)
	for _, private := range []string{"714727414314", "5432167", "7865423", "9087614561", "DELETE FROM"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private value escaped reducer")
		}
	}
}
func TestPgQuerySampleFailClosed(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	for _, name := range []string{"partial", "clock", "duplicate", "unreadable", "secondary", "rowcap", "rawlabel", "negative", "null", "fraction", "mixedfield"} {
		t.Run(name, func(t *testing.T) {
			f := pgSampleTestFrames(now)
			switch name {
			case "partial":
				f = f[:len(f)-1]
			case "clock":
				f[3]["at"] = f[2]["at"]
			case "duplicate":
				f[3]["sample"] = 0
			case "unreadable":
				f[0]["read_only"] = false
			case "secondary":
				f[0]["primary"] = false
			case "rowcap":
				f[2]["rows"] = make([][]any, 129)
			case "rawlabel":
				f[2]["rows"].([][]any)[0][4] = "private-customer-value"
			case "negative":
				f[2]["rows"].([][]any)[0][8] = -1
			case "null":
				f[2]["rows"].([][]any)[0][8] = nil
			case "fraction":
				f[2]["rows"].([][]any)[0][8] = 1.5
			case "mixedfield":
				f[2]["rows"].([][]any)[0][1] = "loopback"
			}
			_, err := parsePgQuerySample(pgSampleTestEncode(t, f), now)
			if err == nil {
				t.Fatal("invalid source admitted")
			}
			if strings.Contains(err.Error(), "private-customer") {
				t.Fatal("source leaked in error")
			}
		})
	}
	raw := pgSampleTestEncode(t, pgSampleTestFrames(now))
	if _, err := parsePgQuerySample(strings.Replace(raw, `"kind":"identity"`, `"kind":"identity","kind":"identity"`, 1), now); err == nil {
		t.Fatal("duplicate JSON admitted")
	}
}
func TestPgQuerySampleHistoryResetUnknown(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	f := pgSampleTestFrames(now)
	f[len(f)-1]["reset"] = float64(now.Unix())
	r, err := parsePgQuerySample(pgSampleTestEncode(t, f), now)
	if err != nil || !r.Complete || r.HistoryDeltaQualified || len(r.Completed) != 1 {
		t.Fatal("reset became completed interval")
	}
}
func pgSampleTestEnv(t *testing.T, response func([]string, string) (string, string, error)) *probeEnv {
	t.Helper()
	s := syntheticSettings(nil)
	now := time.Now().UTC().Truncate(time.Second)
	s.Now = func() time.Time { return now }
	s.StateDir = t.TempDir()
	s.SSHUser = "fixture"
	s.Hosts[0].OverlayAddress = "192.0.2.1"
	s.PostgreSQL.Password = "secret-test-value"
	s.PGQuerySampleUntil = s.Now().Add(time.Hour)
	s = s.withDefaults().withRuntime()
	env, err := newProbeEnv(s)
	if err != nil {
		t.Fatal(err)
	}
	r := env.runner.(*runner)
	r.runSSH = func(_ context.Context, args []string, stdin string) (string, string, error) {
		return response(args, stdin)
	}
	return env
}
func TestPgQuerySampleDefaultOffExpiryAndOneAttempt(t *testing.T) {
	calls := 0
	now := time.Now().UTC().Truncate(time.Second)
	env := pgSampleTestEnv(t, func(args []string, stdin string) (string, string, error) {
		calls++
		command := strings.Join(args, " ")
		if !strings.Contains(command, "hostname -s") || !strings.Contains(command, "timeout -k 2s 35s") || strings.Contains(command, "secret-test-value") {
			t.Fatal("unsafe transport contract")
		}
		var payload map[string]any
		if json.Unmarshal([]byte(stdin), &payload) != nil || payload["password"] != "secret-test-value" || !strings.Contains(payload["sql"].(string), "pg_sleep(2)") {
			t.Fatal("stdin binding failed")
		}
		return pgSampleTestEncode(t, pgSampleTestFrames(now)), "", nil
	})
	probe := pgQuerySampleProbe{}
	until := env.cfg.pgQuerySampleUntil
	env.cfg.pgQuerySampleUntil = time.Time{}
	if _, err := probe.check(context.Background(), env); err != nil || calls != 0 {
		t.Fatal("default off contacted")
	}
	env.cfg.pgQuerySampleUntil = now.Add(-time.Second)
	if _, err := probe.check(context.Background(), env); err != nil || calls != 0 {
		t.Fatal("expired contacted")
	}
	env.cfg.pgQuerySampleUntil = until
	for i := 0; i < 2; i++ {
		if _, err := probe.check(context.Background(), env); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("consumed attempt retried")
	}
	entries, _ := os.ReadDir(filepath.Join(env.cfg.stateDir, "pg-query-sample"))
	if len(entries) != 2 {
		t.Fatal("missing marker or receipt")
	}
}
func TestPgQuerySampleFailurePrivacyAndNoRetry(t *testing.T) {
	calls := 0
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		calls++
		return "private-source-data", "private-error-password", errors.New("native transport failed")
	})
	probe := pgQuerySampleProbe{}
	findings, err := probe.check(context.Background(), env)
	if err != nil || len(findings) != 1 {
		t.Fatal("failure not observable")
	}
	if strings.Contains(findings[0].observed, "private-") {
		t.Fatal("source failure escaped")
	}
	probe.check(context.Background(), env)
	if calls != 1 {
		t.Fatal("failure retried")
	}
}
func TestPgQuerySampleInventoryChangeNoContact(t *testing.T) {
	s := syntheticSettings(&syntheticSource{hostTimeoutFn: func(HostSettings, string, time.Duration) (string, error) {
		t.Fatal("changed inventory contacted")
		return "", nil
	}})
	s.StateDir = t.TempDir()
	s.PGQuerySampleUntil = s.Now().Add(time.Hour)
	s.SettingsGenerationCheck = func(context.Context, SignalSettings) (bool, error) { return false, nil }
	if _, err := NewPgQuerySampleSignal().Run(context.Background(), s); err == nil {
		t.Fatal("changed inventory admitted")
	}
}
func TestPgQuerySampleSourceBounds(t *testing.T) {
	sql := pgQuerySampleSQL("fixture")
	if strings.Count(sql, "SELECT pg_sleep(2)") != 11 || strings.Count(sql, "'kind','activity'") != 12 || strings.Count(sql, "'kind','history'") != 2 || strings.Contains(sql, "SELECT query,") || !strings.Contains(sql, "load_rank<=64 OR age_rank<=64") || !strings.Contains(sql, "LIMIT 5000") {
		t.Fatal("sampling query scope changed")
	}
	s := NewPgQuerySampleSignal()
	if s.Cadence() != 15*time.Minute || s.(*signalAdapter).runBudget() != 40*time.Second {
		t.Fatal("cadence or queue budget changed")
	}
}

func TestPgQuerySampleRemoteProgramStdinAndOutputCap(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 unavailable")
	}
	for _, name := range []string{"large-stdin", "output-cap", "stderr-private"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stub := "#!/usr/bin/env python3\nimport sys\ns=sys.stdin.read()\n"
			switch name {
			case "large-stdin":
				stub += "assert len(s)>32768\nsys.stdout.write('finite-output\\n')\n"
			case "output-cap":
				stub += "sys.stdout.write('x'*4194305)\n"
			case "stderr-private":
				stub += "sys.stderr.write('private-password-value')\nsys.exit(2)\n"
			}
			if os.WriteFile(filepath.Join(dir, "psql"), []byte(stub), 0700) != nil {
				t.Fatal("stub write")
			}
			payload, _ := json.Marshal(map[string]any{"password": "private-password-value", "user": "fixture", "database": "fixture", "port": 5432, "sql": strings.Repeat("SELECT 1;\n", 6000)})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, python, "-c", pgQuerySampleProgram)
			cmd.Env = append(os.Environ(), "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd.Stdin = bytes.NewReader(payload)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			out, runErr := cmd.Output()
			if name == "large-stdin" {
				if runErr != nil || string(out) != "finite-output\n" {
					t.Fatal("actual program failed bounded stdin delivery")
				}
			} else {
				failure, valid := parsePgSampleSourceFailure(string(out))
				if runErr == nil || !valid || failure == nil || (name == "output-cap" && failure.Cause != "output_cap") || (name == "stderr-private" && failure.Cause != "child_exit_unknown") {
					t.Fatal("invalid child output lost its finite failure")
				}
			}
			if strings.Contains(stderr.String()+string(out), "private-password") {
				t.Fatal("child stderr escaped")
			}
		})
	}
}
func TestPgQuerySampleTransportQueueDeadline(t *testing.T) {
	env := pgSampleTestEnv(t, func([]string, string) (string, string, error) {
		t.Fatal("queued sampler escaped host cap")
		return "", "", nil
	})
	r := env.runner.(*runner)
	hold1, e := r.remoteCommands.acquire(context.Background(), "192.0.2.1")
	if e != nil {
		t.Fatal(e)
	}
	defer hold1()
	hold2, e := r.remoteCommands.acquire(context.Background(), "192.0.2.1")
	if e != nil {
		t.Fatal(e)
	}
	defer hold2()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	pgQuerySampleProbe{}.check(ctx, env)
	if time.Since(start) > time.Second {
		t.Fatal("host queue exceeded parent deadline")
	}
}
func TestPgQuerySampleHistoryCounterRegression(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	f := pgSampleTestFrames(now)
	f[len(f)-1]["rows"].([][]any)[0][1] = 1.
	r, err := parsePgQuerySample(pgSampleTestEncode(t, f), now)
	if err != nil || r.HistoryDeltaQualified || len(r.Completed) != 1 || r.HistoryNonmonotonic != 1 {
		t.Fatal("nonmonotonic series became delta")
	}
}

func TestPgQuerySampleHistoryRecreatedEntryDoesNotBecomeInterval(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	f := pgSampleTestFrames(now)
	// An evicted/recreated entry can exceed its old counters without changing
	// pg_stat_statements_info.stats_reset. Identical samples cannot prove lifetime.
	f[len(f)-1]["rows"].([][]any)[0][1] = 100.
	f[len(f)-1]["rows"].([][]any)[0][2] = 9000.
	r, err := parsePgQuerySample(pgSampleTestEncode(t, f), now)
	if err != nil || r.HistoryDeltaQualified || len(r.Completed) != 1 || r.Completed[0].Calls != 100 || r.Completed[0].ExecMS != 9000 {
		t.Fatal("unproved lifetime became interval")
	}
	raw, _ := json.Marshal(r)
	if strings.Contains(string(raw), "calls_delta") || strings.Contains(string(raw), "exec_ms_delta") {
		t.Fatal("interval fields retained")
	}
}
func TestPgQuerySampleDurabilityFailureNeverContacts(t *testing.T) {
	for _, failAt := range []int{1, 2, 3} {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			calls, syncs := 0, 0
			env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { calls++; return "", "", nil })
			p := pgQuerySampleProbe{syncAttemptFile: func(f *os.File) error {
				syncs++
				if syncs == failAt {
					return errors.New("private fsync error")
				}
				return f.Sync()
			}}
			_, err := p.check(context.Background(), env)
			if err == nil || calls != 0 || strings.Contains(err.Error(), "private") {
				t.Fatal("unsynced marker contacted or leaked")
			}
			if _, err = p.check(context.Background(), env); err != nil || calls != 0 {
				t.Fatal("failed durable attempt retried")
			}
		})
	}
}
