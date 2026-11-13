package work

// The uptime probe's way back (connect/EXTENDER.md C3,
// model/network_extender_recovery_model.go).
//
// An address the probes deactivated is probed again, in the same passes and
// with the same dial, for ExtenderProbeRecoveryWindow after it last worked,
// and is restored the first time it answers: an active address again, and a
// revoked extender active again with a record queued that supersedes its
// revocation. Released activators re-activate on their own only on their daily
// tick, a restart or a network change, or when gossip brings them their own
// revocation, so without this an extender revoked by a passing fault stayed
// out of every directory for up to a day.
//
// The probe for the return is cheap and backs off: every pass while the
// address last worked within ExtenderProbeRecoveryEveryPassWindow, which
// covers the faults that pass within minutes, then at most once every
// ExtenderProbeRecoveryInterval. Its failures change nothing but that
// schedule, and they never count toward the pass's verdict, which is about
// the addresses that are active. Probes for a return run after the active
// addresses, so a pass that runs short spends its time on those.
//
// The removal is untouched. An extender that is dead or unreachable fails its
// six probes and is revoked half an hour after it stopped answering, as
// before, and an extender that never answers again is never restored.

import (
	"time"

	"github.com/urnetwork/server/v2026/model"
)

const (
	// How long after an address last worked -- an uptime probe or its
	// activation passed -- it is probed for its return. A released activator
	// posts at least once a day, so a live extender that saw its revocation is
	// back through its own activation within a day; the window outlasts that
	// three times over for one that never saw it, or whose activation is
	// refused.
	ExtenderProbeRecoveryWindow = 72 * time.Hour
	// For this long after an address last worked, every pass probes it for its
	// return. Six failed probes take half an hour, so this is the first hour
	// and a half after the deactivation.
	ExtenderProbeRecoveryEveryPassWindow = 2 * time.Hour
	// After that, a probe for its return at most this often.
	ExtenderProbeRecoveryInterval = 30 * time.Minute
)

// Whether a pass at now probes the address for its return.
func extenderProbeRecoveryDue(target *model.NetworkExtenderRecoveryProbeTarget, now time.Time) bool {
	sinceWork := now.Sub(target.LastWorkTime)
	if ExtenderProbeRecoveryWindow <= sinceWork {
		return false
	}
	if sinceWork < ExtenderProbeRecoveryEveryPassWindow {
		return true
	}
	return target.LastProbeTime == nil || ExtenderProbeRecoveryInterval <= now.Sub(*target.LastProbeTime)
}

// The addresses a pass at now probes for their return, in the order given.
func extenderProbeDueRecoveryTargets(
	targets []*model.NetworkExtenderRecoveryProbeTarget,
	now time.Time,
) []*model.NetworkExtenderRecoveryProbeTarget {
	due := []*model.NetworkExtenderRecoveryProbeTarget{}
	for _, target := range targets {
		if extenderProbeRecoveryDue(target, now) {
			due = append(due, target)
		}
	}
	return due
}
