// SIGNALS.md §22.10 counterexample retention for the §22.1--§22.5 readers on
// the shared subnetReplayLatch, with its keys. Every proved page of these
// families is a custody, authorization, finalized-proof or accounting
// counterexample, so each stays open for the watcher's lifetime. A healthy
// read is never a reproduction, and a run's counterexamples are recorded only
// when the run publishes, so a withheld run cannot open or refresh one.
package monitor

import (
	"fmt"
	"sort"
	"time"
)

// A proved §22.1--§22.5 page; warnings and unknown states are not retained.
func subnetCorrectnessRetained(value finding) bool {
	return !value.healthy && !value.isUnobservable() && value.tier == tierPage
}

// Returns the run's findings plus every retained counterexample the run did
// not reproduce, and the commit that records the run's own counterexamples.
// A reproduced counterexample keeps its first proof time.
func (self *subnetReplayLatch) stage(now time.Time, current []finding) ([]finding, func()) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	present := map[string]bool{}
	proved := []finding{}
	output := make([]finding, 0, len(current)+len(self.retained))
	for _, observed := range current {
		identity := subnetReplayIdentity(observed)
		if !observed.healthy {
			present[identity] = true
		}
		if subnetCorrectnessRetained(observed) {
			proved = append(proved, observed)
			if retained, ok := self.retained[identity]; ok {
				observed.context += fmt.Sprintf(" first_proved=%s", retained.firstSeen.UTC().Format(time.RFC3339))
			}
		}
		output = append(output, observed)
	}
	identities := make([]string, 0, len(self.retained))
	for identity := range self.retained {
		if !present[identity] {
			identities = append(identities, identity)
		}
	}
	sort.Strings(identities)
	for _, identity := range identities {
		retained := self.retained[identity]
		replay := retained.finding
		replay.observed += fmt.Sprintf(" retained=true first_proved=%s last_proved=%s current_read=absent", retained.firstSeen.UTC().Format(time.RFC3339), retained.lastSeen.UTC().Format(time.RFC3339))
		replay.verify = "The counterexample closes only after independent replay at its original finalized checkpoint, attribution and a recorded disposition in the run ledger. A later healthy read, an unknown source or a restart of the reader's inputs cannot erase it while this watcher runs."
		output = append(output, replay)
	}
	return output, func() {
		self.record(now, proved)
	}
}

// Records proved counterexamples at one published run.
func (self *subnetReplayLatch) record(now time.Time, proved []finding) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, observed := range proved {
		identity := subnetReplayIdentity(observed)
		if retained, ok := self.retained[identity]; ok {
			retained.finding = observed
			retained.lastSeen = now
			continue
		}
		self.retained[identity] = &subnetReplayRetained{finding: observed, firstSeen: now, lastSeen: now}
	}
}
