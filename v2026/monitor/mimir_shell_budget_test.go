// Fake local SSH/curl executables exercise the actual native receipt path,
// with no network operation and no response-body or credential logging.
package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Only the shim executes the final SSH command locally; curl itself is fake.
func mimirShellBudgetFixture(t *testing.T, curlBody string) (string, *runner) {
	t.Helper()
	directory := t.TempDir()
	countPath := filepath.Join(directory, "ssh-count")
	ssh := "#!/bin/sh\nprintf 'run\\n' >> " + shellSingleQuote(countPath) + "\nlast=''\nfor arg do last=$arg; done\nexec /bin/sh -c \"$last\"\n"
	for name, body := range map[string]string{"ssh": ssh, "curl": "#!/bin/sh\n" + curlBody + "\n"} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(body), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", directory)
	return countPath, newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
}

// Size refusal can happen before one stdout byte, or after a valid prefix.
// The private channel must survive every existing script capture shape.
func TestMimirShellRemoteCurlBudgetStopsGatewayReplay(t *testing.T) {
	commands := []string{
		"curl --max-filesize 65536 synthetic",
		"value=$(curl --max-filesize 65536 synthetic 2>/dev/null); printf '%s' \"$value\"; exit 0",
		"curl --max-filesize 65536 synthetic 2>/dev/null | while IFS= read -r line || [ -n \"$line\" ]; do printf '%s' \"$line\"; done; exit 0",
		"(exec 3>&1; curl --max-filesize 65536 synthetic 2>/dev/null; printf 'apparently-valid' >&3; exit 0)",
		"curl --max-filesize 65536 synthetic 2>/dev/null; printf 'apparently-valid'; exit 0",
	}
	for index, command := range commands {
		for _, prefix := range []bool{false, true} {
			t.Run(strings.Repeat("x", index+1)+map[bool]string{false: "early", true: "prefix"}[prefix], func(t *testing.T) {
				body := "exit 63"
				if prefix {
					body = "printf '{\"status\":\"success\"}'; exit 63"
				}
				countPath, runner := mimirShellBudgetFixture(t, body)
				hosts := []*host{{name: "synthetic-a", overlayIp: "192.0.2.1"}, {name: "synthetic-b", overlayIp: "192.0.2.2"}}
				output, selected, err := mimirShellFirstServiceGateway(context.Background(), runner, hosts, nil, command, 65536)
				var overflow *mimirShellOutputError
				if output != "" || selected != nil || !errors.As(err, &overflow) || overflow.stream != "remote-body" {
					t.Fatal("remote size refusal lost its typed cause or exposed a prefix")
				}
				var boundary *observationBoundaryError
				if !errors.As(err, &boundary) || boundary.boundary != observationBoundaryNativeWait {
					t.Fatal("remote budget refusal lost joined native authority")
				}
				counts, readErr := os.ReadFile(countPath)
				if readErr != nil || string(counts) != "run\n" {
					t.Fatal("remote size refusal replayed another gateway")
				}
				findings, ok := mimirShellOverflowFindings(err, "synthetic-query")
				if !ok || len(findings) != 1 || findings[0].healthy || findings[0].class != "cannot-observe" || !strings.Contains(findings[0].observed, "stream=remote-body") {
					t.Fatal("remote budget refusal became health")
				}
				alert := alertFromFinding(syntheticSettings(nil), "1.7", "synthetic-query", "Synthetic query", findings[0])
				requireAlertOmits(t, alert, mimirShellBudgetPrefix, "apparently-valid")
			})
		}
	}
}

// Healthy responses still select one gateway; ordinary curl failures and a
// forged stale receipt cannot become budget proof; marker-free failures keep fallback.
func TestMimirShellRemoteCurlOrdinaryFallbackAndHealthy(t *testing.T) {
	for _, mode := range []string{"healthy", "network", "stale-receipt"} {
		t.Run(mode, func(t *testing.T) {
			body := "printf 'healthy'; exit 0"
			if mode == "network" {
				body = "exit 7"
			}
			if mode == "stale-receipt" {
				body = "printf 'MIMIR_OBSERVATION_CURL_BUDGET_V1 stale-token 63\\n' >&2; exit 7"
			}
			countPath, runner := mimirShellBudgetFixture(t, body)
			hosts := []*host{{name: "synthetic-a", overlayIp: "192.0.2.1"}, {name: "synthetic-b", overlayIp: "192.0.2.2"}}
			output, selected, err := mimirShellFirstServiceGateway(context.Background(), runner, hosts, nil, "curl --max-filesize 65536 synthetic", 65536)
			counts, readErr := os.ReadFile(countPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if mode == "healthy" {
				if err != nil || output != "healthy" || selected != hosts[0] || string(counts) != "run\n" {
					t.Fatal("healthy query contract changed")
				}
			} else {
				var overflow *mimirShellOutputError
				if mode == "stale-receipt" {
					if output != "" || selected != nil || !errors.As(err, &overflow) || overflow.stream != "receipt" || string(counts) != "run\n" {
						t.Fatal("foreign receipt became budget proof, health or replay")
					}
				} else if err == nil || errors.As(err, &overflow) || output != "" || selected != nil || string(counts) != "run\nrun\n" {
					t.Fatal("ordinary failure lost fallback or acquired invented budget proof")
				}
				if strings.Contains(err.Error(), "stale-token") {
					t.Fatal("private diagnostic escaped")
				}
			}
		})
	}
}

// Only exact complete current-token framing qualifies; malformed and
// duplicate receipts refuse observation without inventing a budget cause.
func TestMimirShellBudgetReceiptFramingAndCancellation(t *testing.T) {
	marker := mimirShellBudgetPrefix + "0123456789abcdef0123456789abcdef 63"
	for _, testCase := range []struct {
		value           string
		seen, qualified bool
	}{
		{"private diagnostic\n" + marker + "\n", true, true},
		{marker, true, false},
		{"prefix " + marker + "\n", true, false},
		{marker + " extra\n", true, false},
		{marker + "\n" + marker + "\n", true, false},
		{strings.TrimSuffix(marker, "63") + "7\n", true, false},
		{mimirShellBudgetPrefix + "stale-token 63\n", true, false},
	} {
		seen, qualified := mimirShellBudgetReceipt(testCase.value, marker)
		if seen != testCase.seen || qualified != testCase.qualified {
			t.Fatal("receipt framing supplied false authority")
		}
	}
	canceled := 0
	writer := &mimirShellWriter{limit: 4096, cancel: func() { canceled++ }, receiptMarker: marker}
	// Fragmentation and a redirected-stream prefix cannot hide the receipt.
	for _, part := range []string{"private diagnostic\n", marker[:20], marker[20:], "\n"} {
		if _, err := writer.Write([]byte(part)); err != nil {
			t.Fatal(err)
		}
	}
	if canceled != 1 {
		t.Fatal("a complete receipt did not promptly stop the native child")
	}
}

// The callback binds fresh receipt framing while preserving caller argv and
// the original command. No token is inferred from an exit63-shaped error.
func TestMimirShellBudgetNativeArgumentOwnership(t *testing.T) {
	original := []string{"-o", "BatchMode=yes", "synthetic-target", "curl --max-filesize 65536 synthetic"}
	bounded, marker, err := mimirShellBudgetArgs(original, 65536)
	if err != nil || len(bounded) != len(original)+1 || marker == "" || original[3] != "curl --max-filesize 65536 synthetic" || !strings.HasSuffix(bounded[4], original[3]) || !strings.Contains(bounded[4], marker) {
		t.Fatal("receipt did not bind owned command without mutating caller")
	}
	if bounded[0] != "-T" {
		t.Fatal("native receipt allowed PTY stream merging")
	}
	for index := 0; index < 3; index++ {
		if bounded[index+1] != original[index] {
			t.Fatal("receipt changed SSH identity/admission arguments")
		}
	}
	if args, receipt, err := mimirShellBudgetArgs(nil, 65536); err == nil || args != nil || receipt != "" {
		t.Fatal("missing native command acquired a receipt")
	}
}

// Corrupt current-token framing is not remote budget proof, but it still
// refuses the observation and cannot trigger another gateway request.
func TestMimirShellUnqualifiedReceiptStopsReplay(t *testing.T) {
	marker := mimirShellBudgetPrefix + "0123456789abcdef0123456789abcdef 63"
	for _, diagnostics := range []string{marker, marker + " extra\n", marker + "\n" + marker + "\n"} {
		runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
		calls := 0
		runner.runSSH = func(ctx context.Context, _ []string, _ string) (string, string, error) {
			calls++
			script := "printf 'apparently-valid'; printf '%s' " + shellSingleQuote(diagnostics) + " >&2"
			return runMimirShellCommand(ctx, "/bin/sh", []string{"-c", script}, "", 65536, marker)
		}
		hosts := []*host{{name: "synthetic-a", overlayIp: "192.0.2.1"}, {name: "synthetic-b", overlayIp: "192.0.2.2"}}
		output, selected, err := mimirShellFirstServiceGateway(context.Background(), runner, hosts, nil, "synthetic-command", 65536)
		var overflow *mimirShellOutputError
		if output != "" || selected != nil || !errors.As(err, &overflow) || overflow.stream != "receipt" || calls != 1 {
			t.Fatal("unqualified receipt became proof, health or replay")
		}
	}
}

// An arbitrary command's63-shaped native status cannot identify curl or its
// reason. Without an owned receipt it retains ordinary failure fallback.
func TestMimirShellUnmarkedExit63IsNotBudgetAuthority(t *testing.T) {
	runner := newRunner(&monitorConfig{addressMode: addressModeOverlay, commandTimeout: time.Minute})
	calls := 0
	runner.runSSH = func(ctx context.Context, _ []string, _ string) (string, string, error) {
		calls++
		return runMimirShellCommand(ctx, "/bin/sh", []string{"-c", "exit 63"}, "", 65536, mimirShellBudgetPrefix+"0123456789abcdef0123456789abcdef 63")
	}
	hosts := []*host{{name: "synthetic-a", overlayIp: "192.0.2.1"}, {name: "synthetic-b", overlayIp: "192.0.2.2"}}
	output, selected, err := mimirShellFirstServiceGateway(context.Background(), runner, hosts, nil, "synthetic-command", 65536)
	var overflow *mimirShellOutputError
	if err == nil || errors.As(err, &overflow) || output != "" || selected != nil || calls != 2 {
		t.Fatal("native status63 acquired invented budget authority")
	}
}

// The legacy injected command stays unchanged, while actual curl receives
// its missing cap and refuses before native stdout can become a sample.
func TestMimirShellNativeBudgetPreservesHookAndCallerPolicy(t *testing.T) {
	for _, mode := range []string{"missing-overflow", "missing-healthy", "explicit-lower", "discard-writeout"} {
		t.Run(mode, func(t *testing.T) {
			size := "20"
			if mode == "missing-overflow" || mode == "discard-writeout" {
				size = "2048"
			}
			body := "payload_size=" + size + "\n" + `cap=1000000
cap_next=0
output_next=0
discard=0
for arg do
 if [ "$cap_next" -eq 1 ]; then cap=$arg; cap_next=0; continue; fi
 if [ "$output_next" -eq 1 ]; then [ "$arg" != /dev/null ] || discard=1; output_next=0; continue; fi
 case "$arg" in --max-filesize) cap_next=1;; --max-filesize=*) cap=${arg#--max-filesize=};; -o|--output) output_next=1;; --output=/dev/null) discard=1;; esac
done
if [ "$payload_size" -gt "$cap" ]; then exit 63; fi
if [ "$discard" -eq 1 ]; then printf '200 0.1'; else printf '{"status":"success"}'; fi
exit 0`
			countPath, runner := mimirShellBudgetFixture(t, body)
			command := "curl --max-time 15 synthetic"
			limit := 1024
			if mode == "explicit-lower" {
				command = "curl --max-time 15 --max-filesize 16 synthetic"
			}
			if mode == "discard-writeout" {
				command = "curl --max-time 4 -o /dev/null -w '%{http_code} %{time_total}' synthetic"
				limit = 64
			}
			hosts := []*host{{name: "synthetic-a", overlayIp: "192.0.2.1"}, {name: "synthetic-b", overlayIp: "192.0.2.2"}}
			output, selected, err := mimirShellFirstServiceGateway(context.Background(), runner, hosts, nil, command, limit)
			counts, readErr := os.ReadFile(countPath)
			if readErr != nil || string(counts) != "run\n" {
				t.Fatal("native policy changed one-attempt ownership")
			}
			if mode == "missing-healthy" || mode == "discard-writeout" {
				expected := `{"status":"success"}`
				if mode == "discard-writeout" {
					expected = "200 0.1"
				}
				if err != nil || output != expected || selected != hosts[0] {
					t.Fatal("within-policy native response or discard-only writeout changed")
				}
			} else {
				var overflow *mimirShellOutputError
				if output != "" || selected != nil || !errors.As(err, &overflow) || overflow.stream != "remote-body" {
					t.Fatal("missing or caller-specific native HTTP cap was not effective")
				}
			}
		})
	}
}

// A caller-owned parser cannot bypass actual native stdout intake. Only a
// preallocated successful fixture body reaches that parser's own byte guard.
func TestMimirShellOwnerParserCannotBypassNativeIntake(t *testing.T) {
	for _, direct := range []bool{false, true} {
		t.Run(map[bool]string{false: "gateway", true: "direct"}[direct], func(t *testing.T) {
			countPath, runner := mimirShellBudgetFixture(t, "printf '%65s' x; exit 0")
			hosts := []*host{{name: "synthetic-a", overlayIp: "192.0.2.1"}, {name: "synthetic-b", overlayIp: "192.0.2.2"}}
			var output string
			var err error
			if direct {
				output, err = mimirShellParsed(context.Background(), runner, hosts[0], "curl synthetic", 64)
			} else {
				output, _, err = mimirShellFirstServiceGatewayParsed(context.Background(), runner, hosts, nil, "curl synthetic", 64)
			}
			var overflow *mimirShellOutputError
			counts, readErr := os.ReadFile(countPath)
			if output != "" || !errors.As(err, &overflow) || overflow.stream != "stdout" || readErr != nil || string(counts) != "run\n" {
				t.Fatal("owner parser accepted native overflow or replayed it")
			}
		})
	}
}
