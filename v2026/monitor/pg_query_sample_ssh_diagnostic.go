// Own-attempt SSH diagnostic evidence is recorded before error normalization.
// It describes complete local stderr, never remote execution or a network cause.
package monitor

import (
	"context"
	"strconv"
	"strings"
)

// Optional and PG-only. Absent or incomplete diagnostics remain unknown; no
// selected address, raw stderr, command, credential or private nonce persists.
type pgSampleSshConnectDiagnostic struct {
	StderrComplete          bool  `json:"stderr_complete"`
	ReportedConnectTimeout  *bool `json:"reported_connect_timeout"`
	CommandDeadlineExceeded bool  `json:"command_deadline_exceeded"`
	ParentDeadlineExceeded  bool  `json:"parent_deadline_exceeded"`
}

// The native runner alone establishes joined copies. A callback or preflight
// cannot populate this block, even if its returned text looks like OpenSSH.
func (self *pgSampleTransportObservation) recordSshConnectDiagnostic(parent, command context.Context, address, out, stderr string, err error) {
	if self == nil || !self.receipt.Joined {
		return
	}
	complete := self.nativeDiagnosticComplete && len(stderr) <= 4*1024 &&
		(stderr == "" || strings.HasSuffix(stderr, "\n"))
	diagnostic := &pgSampleSshConnectDiagnostic{
		StderrComplete:          complete,
		CommandDeadlineExceeded: command.Err() == context.DeadlineExceeded,
		ParentDeadlineExceeded:  parent.Err() == context.DeadlineExceeded,
	}
	if complete {
		matched := err != nil && sshConnectTimeoutDiagnostic(address, out, strings.TrimSpace(stderr))
		diagnostic.ReportedConnectTimeout = &matched
	}
	self.receipt.SshConnectDiagnostic = diagnostic
}

// Retain the existing exact selected-address spelling and canonical port test.
// A matching diagnostic reports what SSH printed, not TCP or remote authority.
func sshConnectTimeoutDiagnostic(address, out, stderr string) bool {
	if out != "" {
		return false
	}
	portAndReason, targetMatches := strings.CutPrefix(stderr, "ssh: connect to host "+address+" port ")
	port, reasonMatches := strings.CutSuffix(portAndReason, ": Connection timed out")
	if !targetMatches || !reasonMatches {
		return false
	}
	portNumber, portErr := strconv.Atoi(port)
	return portErr == nil && 0 < portNumber && portNumber <= 65535 && strconv.Itoa(portNumber) == port
}
