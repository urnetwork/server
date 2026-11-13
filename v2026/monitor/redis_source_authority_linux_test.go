//go:build linux

// The native family-scan qualification runs the generated collector under the
// local /bin/bash and text tools as a stand-in for the Linux Redis host it
// targets. Its fixed-chunk validator needs Bash 4.1 or later for read -N.
// macOS ships /bin/bash 3.2, where that read fails and the scan is unknown by
// design; TestRedisFamilyAuthorityNativeUnsupportedChunkReadIsUnknown covers
// that rejection on every platform.
package monitor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Each fake producer's actual exit status crosses timeout, head and reduction.
// A million rows forces the real head cap; upstream SIGPIPE is expected only
// when all 300,000 requested lines were successfully selected.
func TestRedisFamilyAuthorityNativeBoundedScan(t *testing.T) {
	cases := []struct {
		name            string
		rows            string
		status          int
		cap             bool
		exactCapFailure bool
		nulMode         string
		wantUnknown     bool
	}{
		{name: "complete empty"},
		{name: "unterminated nul-only record", nulMode: "unterminated", wantUnknown: true},
		{name: "complete nul-only record", nulMode: "complete"},
		{name: "nul within known namespace", nulMode: "namespace"},
		{name: "unterminated single record", rows: "synthetic-key", wantUnknown: true},
		{name: "unterminated twenty-first record", rows: strings.TrimSuffix(redisAuthorityDistinctScanRows(), "\n"), wantUnknown: true},
		{name: "complete twenty-one records", rows: redisAuthorityDistinctScanRows()},
		{name: "complete chunk boundary", rows: strings.Repeat("a", 16383) + "\n"},
		{name: "unterminated full chunk", rows: strings.Repeat("a", 16384), wantUnknown: true},
		{name: "complete two chunks", rows: strings.Repeat("a", 16383) + "\n" + strings.Repeat("b", 16383) + "\n"},
		{name: "complete positive", rows: "ckey_00000000-0000-0000-0000-000000000001\n"},
		{name: "bounded completion", cap: true},
		{name: "failure at exact cap", exactCapFailure: true, wantUnknown: true},
		{name: "failed empty", status: 1, wantUnknown: true},
		{name: "failed partial", rows: "ckey_00000000-0000-0000-0000-000000000001\n", status: 1, wantUnknown: true},
		{name: "timeout partial", rows: "ckey_00000000-0000-0000-0000-000000000001\n", status: 124, wantUnknown: true},
		{name: "unavailable", status: 127, wantUnknown: true},
		{name: "premature sigpipe", rows: "ckey_00000000-0000-0000-0000-000000000001\n", status: 141, wantUnknown: true},
	}
	for _, test := range cases {
		directory := t.TempDir()
		for name, script := range map[string]string{
			"timeout":   "#!/bin/sh\nshift\nexec \"$@\"\n",
			"redis-cli": "#!/bin/sh\nprintf '%s\\n' synthetic-private-diagnostic >&2\ncase \"$AUTHORITY_SCAN_NUL_MODE\" in unterminated) printf '\\000'; exit 0;; complete) printf '\\000\\n'; exit 0;; namespace) printf 'ckey_00000000-0000-0000-0000-000000000001\\000\\n'; exit 0;; esac\nif [ \"$AUTHORITY_SCAN_EXACT_CAP_FAILURE\" = true ]; then awk 'BEGIN { for (i=0; i<300000; i++) print \"ckey_00000000-0000-0000-0000-000000000001\" }'; exit 1; fi\nif [ \"$AUTHORITY_SCAN_CAP\" = true ]; then exec awk 'BEGIN { for (i=0; i<1000000; i++) print \"ckey_00000000-0000-0000-0000-000000000001\" }'; fi\nprintf '%s' \"$AUTHORITY_SCAN_ROWS\"\nexit \"$AUTHORITY_SCAN_STATUS\"\n",
		} {
			if err := os.WriteFile(filepath.Join(directory, name), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
		}
		source := &syntheticSource{
			hostFn: func(_ HostSettings, command string) (string, error) {
				if !strings.Contains(command, "INFO memory") {
					t.Fatal("unexpected source invocation")
				}
				return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
			},
			hostTimeoutFn: func(_ HostSettings, command string, _ time.Duration) (string, error) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				cmd := redisAuthorityNativeCommand(ctx, command)
				cmd.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C", "TMPDIR=" + directory, "AUTHORITY_SCAN_ROWS=" + test.rows, fmt.Sprintf("AUTHORITY_SCAN_STATUS=%d", test.status), fmt.Sprintf("AUTHORITY_SCAN_CAP=%t", test.cap), fmt.Sprintf("AUTHORITY_SCAN_EXACT_CAP_FAILURE=%t", test.exactCapFailure), "AUTHORITY_SCAN_NUL_MODE=" + test.nulMode}
				output, err := cmd.Output()
				redisAuthorityKillNativeGroup(cmd)
				return string(output), err
			},
		}
		env, err := newProbeEnv(syntheticSettings(source).withDefaults())
		if err != nil {
			t.Fatal(err)
		}
		env.baseline, err = newBaselineStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		findings, err := (redisFamilyProbe{}).check(context.Background(), env)
		if err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		findings = keyFamilyHistoryTargetFindings(t, findings, "redis-1:6380")
		if len(findings) != 1 {
			t.Fatalf("%s: expected exactly one observation", test.name)
		}
		if test.wantUnknown {
			if findings[0].class != "cannot-observe" || findings[0].healthy || len(env.baseline.metricSamples) != 0 {
				t.Errorf("%s: failed bounded scan acquired health or baseline authority", test.name)
			}
		} else {
			if test.cap {
				if !findings[0].clearHold || findings[0].healthy {
					t.Error("material complete sample without a learned band certified recovery")
				}
			} else if !findings[0].healthy {
				t.Errorf("%s: complete subthreshold sample lost its static health authority", test.name)
			}
			if test.nulMode == "complete" || test.nulMode == "namespace" {
				samples := env.baseline.metricSamples["redis/family/redacted-binary-family"]
				if len(samples) != 1 || samples[0].v != 1 || len(env.baseline.metricSamples) != 1 {
					t.Errorf("complete NUL-bearing line escaped the binary aggregate")
				}
			}
			if test.name == "complete twenty-one records" {
				samples := env.baseline.metricSamples["redis/family/redacted-unnormalized-family"]
				if len(samples) != 1 || samples[0].v != 20 {
					t.Errorf("complete top-20 sample did not retain twenty counted rows")
				}
			}
			if test.cap {
				samples := env.baseline.metricSamples["redis/family/ckey_<id>"]
				if len(samples) != 1 || samples[0].v != familyScanKeyLimit {
					t.Errorf("%s: full requested cap not recorded", test.name)
				}
			}
		}
		alerts, err := NewKeyFamiliesSignal().Run(context.Background(), syntheticSettings(source))
		if err != nil {
			t.Fatal(err)
		}
		if test.wantUnknown {
			alert := requireAlertClass(t, alerts, "cannot-observe")
			if alert.Target != "redis-1:6380/key-families" || alert.SignalNumber != "3.3" || alert.SignalKey != "key-families" || alert.Sustain != 2 || !alert.unobservable {
				t.Errorf("%s: scan visibility identity changed", test.name)
			}
			requireAlertOmits(t, alert, "synthetic-private-diagnostic")
		} else if len(alerts) != 0 {
			t.Errorf("%s: complete sample unexpectedly alerted", test.name)
		}
	}
}

// Twenty-one distinct sorted families force an actual omitted histogram row;
// the unterminated version differs only in its final newline.
func redisAuthorityDistinctScanRows() string {
	var rows strings.Builder
	for index := 0; index < 21; index++ {
		fmt.Fprintf(&rows, "synthetic-family-%02d\n", index)
	}
	return rows.String()
}

// Bound interpreter work rather than elapsed time: a synthetic read wrapper
// refuses the 8193rd invocation. The unchanged full sample requires 300001
// line reads in the former validator, versus fewer than 1024 fixed-chunk reads.
func TestRedisFamilyAuthorityNativeBufferedReadBudget(t *testing.T) {
	directory := t.TempDir()
	countPath := filepath.Join(directory, "read-count")
	budgetPath := filepath.Join(directory, "read-budget")
	for name, script := range map[string]string{
		"timeout":   "#!/bin/sh\nshift\nexec \"$@\"\n",
		"redis-cli": "#!/bin/sh\nexec awk 'BEGIN { for (i=0; i<1000000; i++) print \"ckey_00000000-0000-0000-0000-000000000001\" }'\n",
		"read-budget": `umask 077
read_calls=0
read() {
 read_calls=$((read_calls + 1))
 if (( read_calls > 8192 )); then
  printf '%s\n' "$read_calls" > "$AUTHORITY_READ_COUNT_PATH"
  exit 75
 fi
 builtin read "$@"
 read_status=$?
 if (( read_status != 0 )); then
  printf '%s\n' "$read_calls" > "$AUTHORITY_READ_COUNT_PATH"
 fi
 return "$read_status"
}
`,
	} {
		mode := os.FileMode(0700)
		if name == "read-budget" {
			mode = 0600
		}
		if err := os.WriteFile(filepath.Join(directory, name), []byte(script), mode); err != nil {
			t.Fatal(err)
		}
	}
	source := &syntheticSource{
		hostFn: func(HostSettings, string) (string, error) {
			return "6380 9000000 10000000\n6381 4000000 10000000\n6382 3000000 10000000", nil
		},
		hostTimeoutFn: func(_ HostSettings, command string, _ time.Duration) (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := redisAuthorityNativeCommand(ctx, command)
			cmd.Env = []string{"PATH=" + directory + ":/usr/bin:/bin", "LC_ALL=C", "TMPDIR=" + directory, "BASH_ENV=" + budgetPath, "AUTHORITY_READ_COUNT_PATH=" + countPath}
			output, err := cmd.Output()
			redisAuthorityKillNativeGroup(cmd)
			return string(output), err
		},
	}
	env, err := newProbeEnv(syntheticSettings(source).withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	env.baseline, err = newBaselineStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := (redisFamilyProbe{}).check(context.Background(), env)
	if err != nil {
		t.Fatalf("bounded read-budget observation: %v", err)
	}
	countBytes, err := os.ReadFile(countPath)
	if err != nil {
		t.Fatal("native read-budget receipt unavailable")
	}
	calls, err := strconv.Atoi(strings.TrimSpace(string(countBytes)))
	if err != nil || calls <= 0 {
		t.Fatal("native read-budget receipt invalid")
	}
	if calls > 8192 {
		t.Fatal("native collector exceeded the deterministic 8192-read budget")
	}
	findings = keyFamilyHistoryTargetFindings(t, findings, "redis-1:6380")
	if len(findings) != 1 || !findings[0].clearHold || findings[0].healthy {
		t.Fatal("complete material read-budget sample without history certified recovery")
	}
	samples := env.baseline.metricSamples["redis/family/ckey_<id>"]
	if len(samples) != 1 || samples[0].v != familyScanKeyLimit {
		t.Fatal("read budget changed the requested sample scope")
	}
	if calls >= 1024 {
		t.Fatal("native collector did not use bounded chunk reads")
	}
	t.Logf("native_read_invocations=%d selected_lines=%d", calls, familyScanKeyLimit)
}
