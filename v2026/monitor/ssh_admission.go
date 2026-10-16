// SSH admission is shared by the watcher and one bounded local diagnostic.
// A hop reserves both destinations together. Only joined work can release an
// active reservation; expiry removes queued work, never running work.
package monitor

import (
	"context"
	"errors"
	"slices"
	"strings"
)

const (
	sshAdmissionGlobalLimit = 4
	sshAdmissionHostLimit   = 2
	sshAdmissionQueueLimit  = 256
	sshAdmissionStateLimit  = 1024 * 1024
	sshAdmissionWatch       = "watcher"
	sshAdmissionDiagnostic  = "diagnostic"
)

// Identity is additionally checked against the process-owned systemd cgroup.
// The caller still verifies its existing full binary/inventory authority.
type SshAdmissionWatcher struct {
	Pid        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
	BootId     string `json:"boot_id"`
}

// A private request selects existing watcher host names, never shell commands.
// The CLI helper writes one grant then waits for "release\n" after the caller
// has joined all transport children. EOF, cancellation and timeout retain it.
type SshAdmissionLeaseRequest struct {
	Directory   string              `json:"directory"`
	Watcher     SshAdmissionWatcher `json:"watcher"`
	Hosts       []string            `json:"hosts"`
	WaitSeconds int                 `json:"wait_seconds"`
}

type sshAdmissionBackend interface {
	acquire(context.Context, string) (func() error, error)
}

type sshAdmissionUnavailableError struct{ err error }

func (self *sshAdmissionUnavailableError) Error() string {
	return "local shared SSH admission unavailable"
}
func (self *sshAdmissionUnavailableError) Unwrap() error { return self.err }

// Explicit Main profile opt-in; other library embedders retain their existing
// transport. Registration refuses a second live watcher and malformed state.
func (self SignalSettings) WithSharedSshAdmission(ctx context.Context, directory string) (SignalSettings, error) {
	hosts := make([]string, 0, len(self.Hosts)+len(self.Routers))
	for _, host := range self.Hosts {
		hosts = append(hosts, host.Name)
	}
	for _, host := range self.Routers {
		if !host.Disabled {
			hosts = append(hosts, host.Name)
		}
	}
	slices.Sort(hosts)
	hosts = slices.Compact(hosts)
	admission, err := newSharedSshAdmission(ctx, directory, hosts)
	if err != nil {
		return SignalSettings{}, err
	}
	self.sharedSshAdmission = admission
	return self, nil
}

type sshAdmissionOwner struct {
	Identity     SshAdmissionWatcher `json:"identity"`
	Unit         string              `json:"unit"`
	InvocationId string              `json:"invocation_id"`
	Cgroup       string              `json:"cgroup"`
	Device       uint64              `json:"device"`
	Inode        uint64              `json:"inode"`
}

type sshAdmissionRequest struct {
	Token        string            `json:"token"`
	Ticket       uint64            `json:"ticket"`
	Class        string            `json:"class"`
	Owner        sshAdmissionOwner `json:"owner"`
	Hosts        []string          `json:"hosts"`
	ExpiresNanos int64             `json:"expires_nanos"`
	Active       bool              `json:"active"`
}

type sshAdmissionState struct {
	Version    int                   `json:"version"`
	BootId     string                `json:"boot_id"`
	Watcher    sshAdmissionOwner     `json:"watcher"`
	Hosts      []string              `json:"hosts"`
	NextTicket uint64                `json:"next_ticket"`
	Turn       string                `json:"turn"`
	Requests   []sshAdmissionRequest `json:"requests"`
}

type sshAdmissionOwnerStatus int

const (
	sshAdmissionOwnerUnknown sshAdmissionOwnerStatus = iota
	sshAdmissionOwnerLive
	sshAdmissionOwnerDead
	sshAdmissionOwnerGone
)

// Safe process liveness is enough to discard queued work. Active work also
// needs proof that the exact original cgroup has no possible live children.
func (self *sshAdmissionState) prune(now int64, statuses map[sshAdmissionOwner]sshAdmissionOwnerStatus) {
	kept := self.Requests[:0]
	for _, request := range self.Requests {
		status := statuses[request.Owner]
		if request.Active {
			if status == sshAdmissionOwnerGone {
				continue
			}
		} else if request.ExpiresNanos <= now || status == sshAdmissionOwnerDead || status == sshAdmissionOwnerGone {
			continue
		}
		kept = append(kept, request)
	}
	self.Requests = kept
}

// Alternate classes when both wait. A diagnostic turn reserves its entire
// demand while existing commands drain, preventing fanout from refilling it.
// A busy watcher host does not prevent another watcher host from progressing.
func (self *sshAdmissionState) next() string {
	used := 0
	counts := map[string]int{}
	for _, request := range self.Requests {
		if request.Active {
			used += len(request.Hosts)
			for _, host := range request.Hosts {
				counts[host]++
			}
		}
	}
	fits := func(request sshAdmissionRequest) bool {
		if used+len(request.Hosts) > sshAdmissionGlobalLimit {
			return false
		}
		for _, host := range request.Hosts {
			if counts[host] >= sshAdmissionHostLimit {
				return false
			}
		}
		return true
	}
	var watcher, diagnostic string
	diagnosticWaiting := false
	for _, request := range self.Requests {
		if !request.Active {
			if request.Class == sshAdmissionDiagnostic {
				diagnosticWaiting = true
				if fits(request) {
					diagnostic = request.Token
				}
			} else if watcher == "" && fits(request) {
				watcher = request.Token
			}
		}
	}
	if diagnosticWaiting && self.Turn == sshAdmissionDiagnostic {
		return diagnostic
	}
	if watcher != "" {
		return watcher
	}
	return diagnostic
}

// Only the selected caller marks its own claim active. There is no grant to an
// absent waiter, so a crashed queued caller can expire without leaking a slot.
func (self *sshAdmissionState) grant(token string) bool {
	if self.next() != token {
		return false
	}
	for i := range self.Requests {
		if self.Requests[i].Token == token {
			self.Requests[i].Active = true
			if self.Requests[i].Class == sshAdmissionDiagnostic {
				self.Turn = sshAdmissionWatch
			} else {
				self.Turn = sshAdmissionDiagnostic
			}
			return true
		}
	}
	return false
}

// Validate the complete stored envelope before any mutation or capacity use.
func (self *sshAdmissionState) validate() error {
	if self.Version != 1 || self.BootId == "" || self.Watcher.Identity.BootId != self.BootId || !self.Watcher.valid() ||
		(self.Turn != sshAdmissionWatch && self.Turn != sshAdmissionDiagnostic) || self.NextTicket == 0 ||
		len(self.Hosts) == 0 || len(self.Hosts) > 512 || len(self.Requests) > sshAdmissionQueueLimit || !slices.IsSorted(self.Hosts) {
		return errors.New("SSH admission state invalid")
	}
	hosts := map[string]bool{}
	for _, host := range self.Hosts {
		if !sshAdmissionHostValid(host) || hosts[host] {
			return errors.New("SSH admission host set invalid")
		}
		hosts[host] = true
	}
	tokens := map[string]bool{}
	var last uint64
	diagnostics, total := 0, 0
	counts := map[string]int{}
	for _, request := range self.Requests {
		if !sshAdmissionHex(request.Token, 32) || tokens[request.Token] || request.Ticket <= last || request.Ticket >= self.NextTicket ||
			!request.Owner.valid() || request.Owner.Identity.BootId != self.BootId || request.ExpiresNanos <= 0 ||
			(request.Class != sshAdmissionWatch && request.Class != sshAdmissionDiagnostic) || len(request.Hosts) < 1 || len(request.Hosts) > 2 ||
			!slices.IsSorted(request.Hosts) || (len(request.Hosts) == 2 && request.Hosts[0] == request.Hosts[1]) {
			return errors.New("SSH admission request invalid")
		}
		if request.Class == sshAdmissionWatch && (request.Owner != self.Watcher || len(request.Hosts) != 1) {
			return errors.New("SSH admission watcher changed")
		}
		if request.Class == sshAdmissionDiagnostic {
			diagnostics++
		}
		for _, host := range request.Hosts {
			if !hosts[host] {
				return errors.New("SSH admission target unknown")
			}
			if request.Active {
				counts[host]++
				total++
			}
		}
		last = request.Ticket
		tokens[request.Token] = true
	}
	if diagnostics > 1 || total > sshAdmissionGlobalLimit {
		return errors.New("SSH admission global limit invalid")
	}
	for _, count := range counts {
		if count > sshAdmissionHostLimit {
			return errors.New("SSH admission host limit invalid")
		}
	}
	return nil
}

func (self sshAdmissionOwner) valid() bool {
	return self.Identity.Pid > 1 && self.Identity.StartTicks > 0 && sshAdmissionBootIdValid(self.Identity.BootId) &&
		sshAdmissionHex(self.InvocationId, 32) && self.Device != 0 && self.Inode != 0 &&
		strings.HasSuffix(self.Unit, ".service") && !strings.ContainsAny(self.Unit, "/\n\r\x00") &&
		strings.HasPrefix(self.Cgroup, "/") && self.Cgroup != "/" && !strings.Contains(self.Cgroup, "..") &&
		!strings.ContainsAny(self.Cgroup, "\n\r\x00")
}

func sshAdmissionBootIdValid(value string) bool {
	return len(value) == 36 && value[8] == '-' && value[13] == '-' && value[18] == '-' && value[23] == '-' && sshAdmissionHex(strings.ReplaceAll(value, "-", ""), 32)
}

func sshAdmissionHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func sshAdmissionHostValid(value string) bool {
	if len(value) == 0 || len(value) > 253 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
