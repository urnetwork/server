// SIGNALS.md §1.7 retains local admission phase independently of lifecycle
// classification. These markers describe returned errors, never native spawns.
package monitor

import "strings"

// Only the acquisition and release call sites assign a known phase. Legacy or
// malformed values remain unknown rather than inheriting a sibling's authority.
type sshAdmissionFailurePhase uint8

const (
	sshAdmissionFailureUnknown sshAdmissionFailurePhase = iota
	sshAdmissionFailureAcquire
	sshAdmissionFailureRelease
)

// Traverse every joined branch: gateways can fail in different phases. The
// fixed set carries neither attempt counts/order nor raw underlying text.
func sshAdmissionFailurePhases(err error) string {
	acquire, release, unknown := false, false, false
	pending := []error{err}
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if current == nil {
			continue
		}
		if failure, ok := current.(*sshAdmissionUnavailableError); ok {
			switch failure.phase {
			case sshAdmissionFailureAcquire:
				acquire = true
			case sshAdmissionFailureRelease:
				release = true
			default:
				unknown = true
			}
		}
		switch wrapped := current.(type) {
		case interface{ Unwrap() []error }:
			pending = append(pending, wrapped.Unwrap()...)
		case interface{ Unwrap() error }:
			pending = append(pending, wrapped.Unwrap())
		}
	}
	phases := make([]string, 0, 3)
	if acquire {
		phases = append(phases, "acquire")
	}
	if release {
		phases = append(phases, "release")
	}
	if unknown {
		phases = append(phases, "unknown")
	}
	return strings.Join(phases, ",")
}

// The fixed lifecycle class remains first and unchanged. An admission marker
// adds a proved local boundary even when deadline/cancellation has precedence.
func observationFailureDetails(err error, errorClass string) string {
	details := "error_class=" + errorClass
	if phases := sshAdmissionFailurePhases(err); phases != "" {
		details += " ssh_admission_failures=" + phases
	}
	return details
}
