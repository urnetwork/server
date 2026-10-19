package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func releaseTestNow() time.Time { return time.Date(2026, 10, 1, 14, 0, 0, 0, time.UTC) }

func releaseTestPolicy() *APIReleaseProofSettings {
	return &APIReleaseProofSettings{Environment: "synthetic", Revision: strings.Repeat("a", 40),
		Version: "2026.10.1-planetoid+123", ImageDigests: []string{"sha256:" + strings.Repeat("b", 64)},
		SelectionFloors: map[string]time.Time{"beta": releaseTestNow().Add(-time.Hour), "g1": releaseTestNow().Add(-10 * time.Minute)},
		ExpiresAt:       releaseTestNow().Add(time.Hour)}
}

func releaseTestScope() providerPickerScope {
	s := providerPickerScope{slots: map[string]bool{}, hosts: []string{"private-host-a", "private-host-b", "private-host-c", "private-host-d"},
		blocks: []string{"beta", "g1", "g2", "g3", "g4"}, excluded: map[string]bool{}}
	for _, h := range s.hosts {
		for _, b := range s.blocks {
			s.slots[h+"\x00"+b] = true
		}
	}
	return s
}

// Real Mimir value rows retain __name__; timestamp() rows do not. Query
// timestamps differ from the paired source-scrape clock.
func releaseTestRows(scope providerPickerScope) []map[string]any {
	p, now := releaseTestPolicy(), releaseTestNow()
	var rows []map[string]any
	for slot := range scope.slots {
		h, b, _ := strings.Cut(slot, "\x00")
		for _, family := range []string{"start", "source", "build", "ready"} {
			for _, timed := range []bool{false, true} {
				m := map[string]string{"env": p.Environment, "job": "api", "host": h, "block": b, "instance": "private-process-" + h + b, "monitor_release": family}
				value := float64(1)
				if family == "source" {
					m["revision"], m["modified"], m["image_digest"] = p.Revision, "false", p.ImageDigests[0]
				}
				if family == "build" {
					m["version"] = p.Version
				}
				if family == "start" {
					value = float64(now.Add(-5 * time.Minute).Unix())
				}
				if timed {
					m["monitor_release"] += "/time"
					value = float64(now.Add(-5*time.Second).Unix()) + 0.125
				} else {
					m["__name__"] = apiReleaseProofFamilies[family]
				}
				rows = append(rows, map[string]any{"metric": m, "value": []any{now.Unix(), fmt.Sprint(value)}})
			}
		}
	}
	return rows
}

func releaseChange(rows []map[string]any, field, value string) {
	for _, row := range rows {
		m := row["metric"].(map[string]string)
		if m["host"] == "private-host-a" && m["block"] == "g1" && m["monitor_release"] == field {
			row["value"].([]any)[1] = value
		}
	}
}

func releaseChangeLabel(rows []map[string]any, family, label, value string) {
	for _, row := range rows {
		m := row["metric"].(map[string]string)
		if m["host"] == "private-host-a" && m["block"] == "g1" && strings.TrimSuffix(m["monitor_release"], "/time") == family {
			m[label] = value
		}
	}
}

func TestAPIReleaseProofCompleteFiniteFleet(t *testing.T) {
	scope := releaseTestScope()
	r := parseAPIReleaseProof(pickerTestPayload(t, releaseTestRows(scope)), "synthetic", releaseTestNow(), scope, releaseTestPolicy())
	if !r.SourceAvailable || !r.Qualified || len(r.Blocks) != 5 || len(r.ExpectationHash) != 64 {
		t.Fatal("complete fresh fleet did not qualify")
	}
	for b, out := range r.Blocks {
		want := 0
		if b == "beta" || b == "g1" {
			want = 4
		}
		if out.Expected != 4 || out.CurrentReady != 4 || out.Qualified != want || out.Targeted != (want > 0) || len(out.Reasons) != 0 {
			t.Fatalf("wrong finite block denominator: %s %+v", b, out)
		}
	}
	raw, _ := json.Marshal(r)
	for _, private := range []string{"private-host", "private-process", releaseTestPolicy().Revision, releaseTestPolicy().ImageDigests[0], "monitor_release"} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("raw runtime identity escaped the finite receipt")
		}
	}
}

func TestAPIReleaseProofBoundariesAndArtifactFailures(t *testing.T) {
	for _, tc := range []struct {
		name      string
		change    func([]map[string]any)
		qualifies bool
		reason    string
	}{
		{"start-equal-floor", func(r []map[string]any) {
			releaseChange(r, "start", fmt.Sprint(releaseTestPolicy().SelectionFloors["g1"].Unix()))
		}, false, "start-not-after-selection"},
		{"start-after-floor", func(r []map[string]any) {
			releaseChange(r, "start", fmt.Sprint(float64(releaseTestPolicy().SelectionFloors["g1"].Unix())+.001))
		}, true, ""},
		{"not-ready", func(r []map[string]any) { releaseChange(r, "ready", "0") }, false, "missing-stale-or-not-ready"},
		{"mixed-scrape", func(r []map[string]any) {
			releaseChange(r, "ready/time", fmt.Sprint(releaseTestNow().Add(-6*time.Second).Unix()))
		}, false, "missing-stale-or-not-ready"},
		{"wrong-revision", func(r []map[string]any) { releaseChangeLabel(r, "source", "revision", strings.Repeat("c", 40)) }, false, "source-mismatch"},
		{"dirty-source", func(r []map[string]any) { releaseChangeLabel(r, "source", "modified", "true") }, false, "source-mismatch"},
		{"bad-modified", func(r []map[string]any) { releaseChangeLabel(r, "source", "modified", "1") }, false, "invalid-artifact-witness"},
		{"other-image", func(r []map[string]any) {
			releaseChangeLabel(r, "source", "image_digest", "sha256:"+strings.Repeat("c", 64))
		}, false, "image-mismatch"},
		{"other-version", func(r []map[string]any) { releaseChangeLabel(r, "build", "version", "2026.9.29+old") }, false, "version-mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scope := releaseTestScope()
			rows := releaseTestRows(scope)
			tc.change(rows)
			r := parseAPIReleaseProof(pickerTestPayload(t, rows), "synthetic", releaseTestNow(), scope, releaseTestPolicy())
			if r.Qualified != tc.qualifies || !r.SourceAvailable || (tc.reason != "" && r.Blocks["g1"].Reasons[tc.reason] != 1) {
				t.Fatalf("wrong artifact qualification: %+v block=%+v", r, r.Blocks["g1"])
			}
		})
	}
	for _, age := range []int64{-31, -30, 90, 91} {
		t.Run(fmt.Sprint("source-age-", age), func(t *testing.T) {
			scope := releaseTestScope()
			rows := releaseTestRows(scope)
			for _, family := range []string{"start", "source", "build", "ready"} {
				releaseChange(rows, family+"/time", fmt.Sprint(releaseTestNow().Unix()-age))
			}
			r := parseAPIReleaseProof(pickerTestPayload(t, rows), "synthetic", releaseTestNow(), scope, releaseTestPolicy())
			if r.Qualified != (age >= -30 && age <= 90) {
				t.Fatal("source-age bound changed")
			}
		})
	}
}

func TestAPIReleaseProofNoOlderGenerationRescue(t *testing.T) {
	for _, kind := range []string{"missing-start", "newer-not-ready", "equal-start"} {
		t.Run(kind, func(t *testing.T) {
			scope := releaseTestScope()
			rows := releaseTestRows(scope)
			for _, row := range releaseTestRows(scope) {
				m := row["metric"].(map[string]string)
				if m["host"] != "private-host-a" || m["block"] != "g1" {
					continue
				}
				m["instance"] = "private-overlap-generation"
				field := m["monitor_release"]
				if kind == "missing-start" && strings.HasPrefix(field, "start") {
					continue
				}
				if kind == "newer-not-ready" {
					if field == "start" {
						row["value"].([]any)[1] = fmt.Sprint(releaseTestNow().Add(-time.Minute).Unix())
					}
					if field == "ready" {
						row["value"].([]any)[1] = "0"
					}
				}
				rows = append(rows, row)
			}
			r := parseAPIReleaseProof(pickerTestPayload(t, rows), "synthetic", releaseTestNow(), scope, releaseTestPolicy())
			if r.Qualified || !r.SourceAvailable || r.Blocks["g1"].Qualified != 3 {
				t.Fatal("old process rescued an unknown or unready current generation")
			}
		})
	}
}

func TestAPIReleaseProofMalformedAndMissingStayUnknown(t *testing.T) {
	for _, kind := range []string{"empty", "duplicate", "wrong-clock", "unexpected-label", "wrong-metric-name", "source-pair-mismatch", "nan", "rows-over-cap", "bytes-over-cap", "warnings"} {
		t.Run(kind, func(t *testing.T) {
			scope, rows := releaseTestScope(), releaseTestRows(releaseTestScope())
			switch kind {
			case "empty":
				rows = nil
			case "duplicate":
				rows = append(rows, rows[0])
			case "wrong-clock":
				rows[0]["value"].([]any)[0] = releaseTestNow().Unix() - 1
			case "unexpected-label":
				rows[0]["metric"].(map[string]string)["private-token"] = "do-not-retain"
			case "wrong-metric-name":
				rows[0]["metric"].(map[string]string)["__name__"] = "not-allowed"
			case "source-pair-mismatch":
				for _, row := range rows {
					if row["metric"].(map[string]string)["monitor_release"] == "source/time" {
						row["metric"].(map[string]string)["modified"] = "true"
						break
					}
				}
			case "nan":
				rows[0]["value"].([]any)[1] = "NaN"
			case "rows-over-cap":
				for len(rows) <= apiReleaseProofMaxRows {
					rows = append(rows, rows[0])
				}
			}
			raw := pickerTestPayload(t, rows)
			if kind == "bytes-over-cap" {
				raw = strings.Repeat(" ", apiReleaseProofMaxBytes+1)
			}
			if kind == "warnings" {
				raw = strings.Replace(raw, `"status":"success"`, `"warnings":["private-source-warning"],"status":"success"`, 1)
			}
			r := parseAPIReleaseProof(raw, "synthetic", releaseTestNow(), scope, releaseTestPolicy())
			if r.Qualified || r.SourceAvailable != (kind == "empty") {
				t.Fatal("missing or invalid source manufactured qualification")
			}
		})
	}
}

func TestAPIReleaseProofExpectationFile(t *testing.T) {
	raw, _ := json.Marshal(releaseTestPolicy())
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"valid", string(raw), true},
		{"duplicate-root", strings.Replace(string(raw), `"environment":`, `"environment":"other","environment":`, 1), false},
		{"duplicate-nested", strings.Replace(string(raw), `"beta":`, `"beta":"2026-10-01T01:00:00Z","beta":`, 1), false},
		{"case-alias", strings.Replace(string(raw), `"environment":`, `"Environment":"other","environment":`, 1), false},
		{"unknown", strings.Replace(string(raw), `"environment":`, `"credential":"private-secret","environment":`, 1), false},
		{"trailing", string(raw) + `{}`, false},
		{"oversize", strings.Repeat(" ", 16385), false},
		{"infinite-policy", strings.Replace(string(raw), `2026-10-01T15:00:00Z`, `2027-10-01T15:00:00Z`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "expectation.json")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := LoadAPIReleaseProofSettings(path)
			if (err == nil) != tc.valid || (p != nil) != tc.valid {
				t.Fatal("wrong expectation admission")
			}
			if err != nil && strings.Contains(err.Error(), "private-secret") {
				t.Fatal("expectation content entered error")
			}
		})
	}
	path := filepath.Join(t.TempDir(), "fifo")
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := LoadAPIReleaseProofSettings(path); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("expectation FIFO blocked before admission")
	}
	copy := cloneAPIReleaseProofSettings(releaseTestPolicy())
	settings := SignalSettings{APIReleaseProof: copy}.withDefaults()
	copy.ImageDigests[0] = "changed"
	copy.SelectionFloors["g1"] = time.Time{}
	if settings.APIReleaseProof.ImageDigests[0] == "changed" || settings.APIReleaseProof.SelectionFloors["g1"].IsZero() {
		t.Fatal("expectation retained mutable caller state")
	}
}

func releaseTestEnv(t *testing.T, run sshCommandRunner) *probeEnv {
	t.Helper()
	s := syntheticSettings(nil)
	s.Environment = "synthetic"
	s.Now = releaseTestNow
	s.StateDir = t.TempDir()
	s.APIReleaseProof = releaseTestPolicy()
	s.Hosts = []HostSettings{{Name: "private-host-a", Roles: []string{"services"}, OverlayAddress: "192.0.2.1"}, {Name: "private-host-b", Roles: []string{"services"}, OverlayAddress: "192.0.2.2"}}
	s.LogServices = []string{"api"}
	s.LogServiceHosts = map[string][]string{"api": {"private-host-a", "private-host-b"}}
	s.LogServiceBlocks = map[string][]string{"api": {"beta", "g1"}}
	s = s.withDefaults().withRuntime()
	env, err := newProbeEnv(s)
	if err != nil {
		t.Fatal(err)
	}
	env.runner.(*runner).runSSH = run
	return env
}

func TestAPIReleaseProofSingleGuardedReadAndPrivateReceipt(t *testing.T) {
	var env *probeEnv
	calls := 0
	broken := false
	env = releaseTestEnv(t, func(ctx context.Context, args []string, stdin string) (string, string, error) {
		calls++
		command := args[len(args)-1]
		if command != `[ "$(hostname -s)" = 'private-host-a' ] || exit 74; exec timeout 20s python3 -` || !strings.Contains(stdin, "urnetwork_api_ready") || !strings.Contains(stdin, "process_start_time_seconds") {
			t.Fatal("guarded finite query changed")
		}
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 20*time.Second {
			t.Fatal("transport lacks child deadline")
		}
		if broken {
			return "private-output", "private-stderr", errors.New("private-error")
		}
		return pickerTestPayload(t, releaseTestRows(pickerScope(env))), "", nil
	})
	findings, err := (apiReleaseProofProbe{}).check(context.Background(), env)
	if err != nil || len(findings) != 0 || calls != 1 {
		t.Fatalf("healthy read failed: calls=%d findings=%d err=%v", calls, len(findings), err)
	}
	path := filepath.Join(env.cfg.stateDir, "api-release-proof", "latest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r apiReleaseProofReceipt
	if json.Unmarshal(raw, &r) != nil || !r.Qualified || r.CompletedAt.IsZero() {
		t.Fatal("positive proof was lost with healthy alert suppression")
	}
	info, _ := os.Stat(path)
	dirInfo, _ := os.Stat(filepath.Dir(path))
	if info.Mode().Perm() != 0600 || dirInfo.Mode().Perm() != 0700 {
		t.Fatal("private receipt permissions changed")
	}
	broken = true
	findings, err = (apiReleaseProofProbe{}).check(context.Background(), env)
	if err != nil || len(findings) != 1 || calls != 2 {
		t.Fatal("failed read retried or disappeared")
	}
	raw, _ = os.ReadFile(path)
	json.Unmarshal(raw, &r)
	if r.Qualified || r.SourceAvailable || r.Reason != "bounded-source-unavailable" {
		t.Fatal("failed read retained old positive receipt")
	}
	if bytes.Contains(raw, []byte("private-")) || strings.Contains(findings[0].observed, "private-") {
		t.Fatal("transport diagnostic escaped reduction")
	}
	files, _ := os.ReadDir(filepath.Dir(path))
	if len(files) != 1 {
		t.Fatal("atomic receipt left a partial file")
	}
}

func TestAPIReleaseProofDisabledExpiredAndCanceledDoNotContact(t *testing.T) {
	for _, kind := range []string{"disabled", "expired", "future", "wrong-env", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			env := releaseTestEnv(t, func(context.Context, []string, string) (string, string, error) {
				t.Fatal("inadmissible release signal contacted a host")
				return "", "", nil
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch kind {
			case "disabled":
				env.cfg.apiReleaseProof = nil
			case "expired":
				env.cfg.apiReleaseProof.ExpiresAt = releaseTestNow()
			case "future":
				env.cfg.apiReleaseProof.SelectionFloors["g1"] = releaseTestNow().Add(time.Second)
			case "wrong-env":
				env.cfg.apiReleaseProof.Environment = "other"
			case "canceled":
				cancel()
			}
			findings, err := (apiReleaseProofProbe{}).check(ctx, env)
			if kind == "canceled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal("cancellation lost")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if (kind == "disabled" || kind == "canceled") && len(findings) != 0 {
				t.Fatal("disabled/canceled signal manufactured an alert")
			}
			if kind == "disabled" || kind == "canceled" {
				if _, err := os.Stat(filepath.Join(env.cfg.stateDir, "api-release-proof", "latest.json")); !os.IsNotExist(err) {
					t.Fatal("disabled/canceled signal published a receipt")
				}
			}
		})
	}
}

func TestAPIReleaseProofProgramLocalHTTPBounds(t *testing.T) {
	// Only the test subprocess substitutes an httptest loopback port. The
	// production endpoint remains the fixed gateway loopback Mimir address.
	for _, kind := range []string{"success", "redirect", "oversize", "failure"} {
		t.Run(kind, func(t *testing.T) {
			var redirects atomic.Int32
			redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirects.Add(1)
				w.Write([]byte("private-redirect-body"))
			}))
			defer redirect.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/prometheus/api/v1/query" {
					t.Error("unexpected query target")
					w.WriteHeader(400)
					return
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("query") != "fixed synthetic query" || r.Form.Get("time") != fmt.Sprint(releaseTestNow().Unix()) {
					t.Error("query or evaluation clock changed")
				}
				switch kind {
				case "redirect":
					http.Redirect(w, r, redirect.URL, 302)
				case "oversize":
					w.Write(bytes.Repeat([]byte("x"), apiReleaseProofMaxBytes+1))
				case "failure":
					w.WriteHeader(500)
					w.Write([]byte("private-error-body"))
				default:
					w.Write([]byte("synthetic bounded metrics"))
				}
			}))
			defer server.Close()
			program := strings.Replace(apiReleaseProofProgram("fixed synthetic query", releaseTestNow()), "http://127.0.0.1:3100", server.URL, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/usr/bin/python3", "-Es", "-")
			cmd.Env = append(os.Environ(), "HTTP_PROXY="+redirect.URL, "http_proxy="+redirect.URL, "NO_PROXY=", "no_proxy=")
			cmd.Stdin = strings.NewReader(program)
			out, err := cmd.CombinedOutput()
			if kind == "success" {
				if err != nil || string(out) != "synthetic bounded metrics" {
					t.Fatal("bounded query rejected legitimate response")
				}
			} else if err == nil || len(out) != 0 {
				t.Fatal("failed query leaked output or was accepted")
			}
			if redirects.Load() != 0 {
				t.Fatal("loopback query followed redirect or environment proxy")
			}
		})
	}
}

type releaseBudgetSignal struct {
	steppedAlertSignal
	budget time.Duration
	calls  atomic.Int32
}

func (s *releaseBudgetSignal) runBudget() time.Duration { return s.budget }
func (s *releaseBudgetSignal) Run(ctx context.Context, _ SignalSettings) (Alerts, error) {
	s.calls.Add(1)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAPIReleaseProofQueueBudgetIncludesAdmission(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(fmt.Sprint("occupied-", occupied), func(t *testing.T) {
			slots := newRunSlotPool(1)
			if occupied {
				release, err := slots.acquire(context.Background(), false)
				if err != nil {
					t.Fatal(err)
				}
				defer release()
			}
			signal := &releaseBudgetSignal{budget: 10 * time.Millisecond}
			_, err := runSignalInSlot(context.Background(), slots, signal, SignalSettings{})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("queue/execution escaped owner deadline")
			}
			want := int32(1)
			size := 0
			if occupied {
				want = 0
				size = 1
			}
			if signal.calls.Load() != want || slots.active != size {
				t.Fatal("queue expiry started a child or released an unowned slot")
			}
		})
	}
	if NewAPIReleaseProofSignal().Cadence() != 15*time.Minute || NewAPIReleaseProofSignal().(*signalAdapter).runBudget() != 40*time.Second {
		t.Fatal("release observation cadence/budget changed")
	}
}

func TestAPIReleaseProofSharesRuntimeHostBudget(t *testing.T) {
	env := releaseTestEnv(t, func(context.Context, []string, string) (string, string, error) {
		t.Fatal("saturated destination started an SSH child")
		return "", "", nil
	})
	runner := env.runner.(*runner)
	other := newRunner(env.cfg)
	if runner.remoteCommands != other.remoteCommands || runner.remoteCommands.limit != 2 {
		t.Fatal("release signal created a separate host limiter")
	}
	var releases []func()
	for i := 0; i < 2; i++ {
		release, err := other.remoteCommands.acquire(context.Background(), "192.0.2.1")
		if err != nil {
			t.Fatal(err)
		}
		releases = append(releases, release)
	}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	_, err := (apiReleaseProofProbe{}).check(ctx, env)
	if !errors.Is(err, context.DeadlineExceeded) || len(runner.remoteCommands.hostSlots("192.0.2.1")) != 2 {
		t.Fatal("host queue escaped owner deadline or released other work")
	}
	if _, err := os.Stat(filepath.Join(env.cfg.stateDir, "api-release-proof", "latest.json")); !os.IsNotExist(err) {
		t.Fatal("canceled queued read published a completed observation")
	}
	other.runSSH = func(context.Context, []string, string) (string, string, error) { return "other-host-ok", "", nil }
	if out, err := other.shell(context.Background(), env.cfg.hosts[1], "true"); err != nil || out != "other-host-ok" {
		t.Fatal("saturated host blocked an unrelated destination")
	}
}
