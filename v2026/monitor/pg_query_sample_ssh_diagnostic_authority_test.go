// Fixed diagnostic authority/deadline controls do not run a native child.
package monitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Owned and parent deadline facts survive independently of normalization.
// These contexts are already expired deterministically, without timing sleeps.
func TestPgSshConnectDiagnosticDeadlinesAndAuthority(t *testing.T) {
	live := context.Background()
	expired, cancel := context.WithDeadline(live, time.Unix(1, 0))
	defer cancel()
	for _, test := range []struct {
		name                          string
		parent, command               context.Context
		parentExpired, commandExpired bool
	}{
		{"command-owner", live, expired, false, true},
		{"parent", expired, expired, true, true},
		{"neither", live, live, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			observation := &pgSampleTransportObservation{nativeDiagnosticComplete: true, receipt: pgSampleTransportReceipt{Joined: true}}
			observation.recordSshConnectDiagnostic(test.parent, test.command, "synthetic-target", "", "ssh: connect to host synthetic-target port 22: Connection timed out\n", errors.New("synthetic native failure"))
			proof := observation.receipt.SshConnectDiagnostic
			if proof == nil || proof.ReportedConnectTimeout == nil || !*proof.ReportedConnectTimeout || proof.ParentDeadlineExceeded != test.parentExpired || proof.CommandDeadlineExceeded != test.commandExpired {
				t.Fatal("normalization erased own diagnostic or merged deadlines")
			}
		})
	}
	for _, joined := range []bool{false, true} {
		observation := &pgSampleTransportObservation{receipt: pgSampleTransportReceipt{Joined: joined}}
		observation.recordSshConnectDiagnostic(live, live, "synthetic-target", "", "ssh: connect to host synthetic-target port 22: Connection timed out\n", errors.New("synthetic"))
		if !joined && observation.receipt.SshConnectDiagnostic != nil {
			t.Fatal("preflight/callback/refusal invented a native diagnostic")
		}
		if joined && (observation.receipt.SshConnectDiagnostic == nil || observation.receipt.SshConnectDiagnostic.ReportedConnectTimeout != nil) {
			t.Fatal("missing owned EOF became complete")
		}
	}
}

// Target spelling alone cannot select remote completion or a diagnostic from
// nonempty stdout, duplicate lines or a noncanonical/out-of-range port.
func TestPgSshConnectDiagnosticExactTargetRefusals(t *testing.T) {
	for _, test := range []struct{ out, stderr string }{
		{"remote response", "ssh: connect to host selected port 22: Connection timed out"},
		{"", "ssh: connect to host other port 22: Connection timed out"},
		{"", "ssh: connect to host selected port 022: Connection timed out"},
		{"", "ssh: connect to host selected port 65536: Connection timed out"},
		{"", "ssh: connect to host selected port 22: Connection timed out\nssh: connect to host selected port 22: Connection timed out"},
	} {
		if sshConnectTimeoutDiagnostic("selected", test.out, test.stderr) {
			t.Fatal("non-owned complete diagnostic accepted")
		}
	}
}
