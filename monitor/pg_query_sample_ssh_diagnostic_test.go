// Synthetic native SSH and fixed typed controls exercise receipt loss before
// normalization. No test selects a production endpoint or changes probe policy.
package monitor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The public immutable receipt, rather than the private observer object, is the
// regression boundary. The selected local shell child owns Start/Wait and EOF.
func TestPgSshConnectDiagnosticNativeReceipt(t *testing.T) {
	for _, test := range []struct {
		name     string
		stderr   func(string) string
		exit     int
		complete bool
		matched  bool
	}{
		{"reported-timeout", func(addr string) string { return "ssh: connect to host " + addr + " port 22: Connection timed out\n" }, 255, true, true},
		{"wrong-target", func(string) string {
			return "ssh: connect to host private-wrong-target port 22: Connection timed out\n"
		}, 255, true, false},
		{"partial-line", func(addr string) string { return "ssh: connect to host " + addr + " port 22: Connection timed out" }, 255, false, false},
		{"over-cap", func(string) string { return strings.Repeat("x", 4096) + "\n" }, 255, false, false},
		{"healthy-local-exit", func(string) string { return "" }, 0, true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := pgSampleTestEnv(t, func([]string, string) (string, string, error) { t.Fatal("callback used"); return "", "", nil })
			runner := env.runner.(*runner)
			runner.runSSH = runSSHCommand
			addr := env.cfg.hostByRole("pg-primary").addr(env.cfg.addressMode)
			stderr := test.stderr(addr)
			fixture := t.TempDir()
			// A quoted heredoc prevents the synthetic stderr from becoming code.
			script := "#!/bin/sh\ncat >/dev/null\ncat >&2 <<'MONITOR_SYNTHETIC_STDERR'\n" + stderr
			if !strings.HasSuffix(script, "\n") {
				script += "\n"
			}
			if test.name == "partial-line" {
				script = "#!/bin/sh\ncat >/dev/null\nprintf '%s' '" + stderr + "' >&2\n"
			} else {
				script += "MONITOR_SYNTHETIC_STDERR\n"
			}
			script += fmt.Sprintf("exit %d\n", test.exit)
			if err := os.WriteFile(filepath.Join(fixture, "ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", fixture+":"+os.Getenv("PATH"))
			if selected, err := exec.LookPath("ssh"); err != nil || selected != filepath.Join(fixture, "ssh") {
				t.Fatal("local fixture not selected")
			}
			if _, err := (pgQuerySampleProbe{}).check(t.Context(), env); err != nil {
				t.Fatal(err)
			}
			receipt, raw := pgTransportReadControlReceipt(t, env)
			var wire struct {
				Transport struct {
					Diagnostic *struct {
						StderrComplete          bool  `json:"stderr_complete"`
						ReportedConnectTimeout  *bool `json:"reported_connect_timeout"`
						CommandDeadlineExceeded bool  `json:"command_deadline_exceeded"`
						ParentDeadlineExceeded  bool  `json:"parent_deadline_exceeded"`
					} `json:"ssh_connect_diagnostic"`
				} `json:"transport_observation"`
			}
			if json.Unmarshal(raw, &wire) != nil || wire.Transport.Diagnostic == nil {
				t.Fatal("native receipt dropped own connect diagnostic")
			}
			diagnostic := wire.Transport.Diagnostic
			if diagnostic.StderrComplete != test.complete || diagnostic.CommandDeadlineExceeded || diagnostic.ParentDeadlineExceeded {
				t.Fatal("diagnostic completeness or deadline changed")
			}
			if test.complete {
				if diagnostic.ReportedConnectTimeout == nil || *diagnostic.ReportedConnectTimeout != test.matched {
					t.Fatal("target-matched diagnostic lost or invented")
				}
			} else if diagnostic.ReportedConnectTimeout != nil {
				t.Fatal("incomplete stderr became known false or true")
			}
			if !receipt.TransportObservation.Joined || receipt.TransportObservation.NativeExitCode == nil || *receipt.TransportObservation.NativeExitCode != test.exit {
				t.Fatal("native terminal evidence changed")
			}
			if strings.Contains(string(raw), "private-wrong-target") || strings.Contains(string(raw), "Connection timed out") || strings.Contains(string(raw), addr) {
				t.Fatal("raw diagnostic or selected address leaked")
			}
		})
	}
}
