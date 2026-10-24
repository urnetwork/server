// The settlement recovery chains, flush_transfer_debits_* and
// flush_legacy_settlements_*, re-arm themselves from their Posts. An empty
// partition used to re-arm every two seconds like a busy one: the 32 chains
// made at least 16 completions a second, each a pending_task insert, claim and
// delete plus a finished_task row, about 1.4M rows a day with both queues
// empty. A chain now backs off to the idle delay once consecutive passes have
// found its partition empty.
//
// The back-off goes straight to the idle delay rather than growing toward it.
// A debit journaled while its chain is backed off kicks the chain
// (model.WakeTransferDebitFlushes), so the back-off does not set a debit's
// latency, and each step of a growing curve would only add an idle run to every
// quiet spell. Without a kick (a lost Redis mark, a crash before the mark, a
// producer from an older build) a debit waits at most the idle delay plus one
// run, inside the 60-second warning of SIGNALS.md §2.5a. Legacy intents get
// no kick: current contracts no longer queue them, and the other producers
// wake their partition through the RunOnce merge. Their warning allows for the
// idle delay (§2.5b).
//
// The chain backs off only after a second consecutive empty pass. Debits that
// arrive every few seconds leave single empty passes between them; backing off
// at the first would trade the following two-second run for a kick, a queue
// write, without saving any run. Waiting for four quiet seconds costs a quiet
// spell one extra run.
package work

import (
	"time"

	"github.com/urnetwork/server/task"
)

// Tunables of both settlement chain families.
type SettlementChainSettings struct {
	// Delay before the next run after a run that found work and has no
	// immediate continuation, or found the partition empty fewer than
	// EmptyPassesBeforeBackOff times in a row.
	ActiveDelay time.Duration
	// Delay before the next run once the chain has backed off. It must stay
	// below Wake.MarkTtl less one run, so that a backed-off debit chain renews
	// its mark before the mark lapses.
	IdleDelay time.Duration
	// Consecutive passes that must find the partition empty before the chain
	// backs off. Values below one back off at the first empty pass.
	EmptyPassesBeforeBackOff int
	// Bound of one run.
	MaxTime time.Duration
	// The debit chains' idle marks.
	Wake *task.IdleWakeSettings
}

// The two-second active cadence is the former cadence of every run. Thirty
// seconds idle plus one run's 15-second page budget keeps an un-kicked debit
// under its 60-second warning.
func DefaultSettlementChainSettings() *SettlementChainSettings {
	return &SettlementChainSettings{
		ActiveDelay:              2 * time.Second,
		IdleDelay:                30 * time.Second,
		EmptyPassesBeforeBackOff: 2,
		MaxTime:                  30 * time.Second,
		Wake:                     task.DefaultIdleWakeSettings(),
	}
}

// The consecutive empty passes a run hands its successor: one more after an
// empty pass, capped where the chain backs off so that a backed-off chain's
// arguments stay the same, and none after a pass that found work.
func (self *SettlementChainSettings) nextEmptyPasses(emptyPasses int, empty bool) int {
	if !empty {
		return 0
	}
	return min(emptyPasses+1, max(self.EmptyPassesBeforeBackOff, 1))
}

// Whether a chain whose last passes found its partition empty this many times
// in a row has backed off.
func (self *SettlementChainSettings) backedOff(emptyPasses int) bool {
	return max(self.EmptyPassesBeforeBackOff, 1) <= emptyPasses
}

// When the successor of a run is due: at once while a page reports more work,
// after the idle delay once the chain has backed off, and after the active
// delay otherwise.
func (self *SettlementChainSettings) nextRunAt(now time.Time, more bool, backedOff bool) time.Time {
	switch {
	case more:
		return now
	case backedOff:
		return now.Add(self.IdleDelay)
	default:
		return now.Add(self.ActiveDelay)
	}
}
