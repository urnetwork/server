package evshost

// The versionless purge in the block (EVSB-M10-01; §11.13's purge, who, holds and budget rows, R-EVS-20, D-EVS-37,
// I-EVS-9): the process that commits a push replacing a versionless version deletes what that version leaves, once
// the commit is certain and the readers' 60 s have passed, rather than leaving it to evacuation's sweep for ~26 h.
// The engine's step 7 hands each certain commit's candidates to its PurgeSink (EvsEngineDeps.Purge); this host's is
// the block's evsservice.Purger, built over what the engine itself reads:
//
//	format2  the block's format2 gate (Host.Format2Gate): the purge deletes only while sidecar.yml's format2.purge
//	         reads exactly on; observe, or any other value, logs each candidate and counts its ids in
//	         urnetwork_evs_versionless_observed_total, deleting nothing
//	newest   the engine's reader of a path's newest version, over its head store and record cache
//	holds    the hold cache the engine's slot takeovers read, so one hold/ read at most 5 s old serves both
//	usage    the usage producer the engine counts stored bytes with, which takes each freed segment's negative delta
//
// The Purger needs the engine's reader, which exists only once the engine does, so the engine starts with a sink
// that the host fills before NewHost returns and so before the block serves anything. A commit handed to the sink
// unfilled queues nowhere, as with a nil sink, and evacuation's sweep takes its list. A host with no usage producer,
// since it was given no Quota, builds no Purger and lists it missing, so the block is not ready rather than running
// with every purge left to the sweep.
//
// The Purger runs on the host's context (RunHost's ends only at Close). Host.Drain drains it after the engine and
// before the usage producer, so the producer's last window carries what it freed, and Host.Close ends it before the
// producer. What it leaves queued is the sweep's.

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
)

// The engine's PurgeSink: the block's Purger once startPurger has built it, and until then nothing, as a nil sink.
// Safe for concurrent use: step 8 calls it from goroutines of its own.
type hostPurgeSink struct {
	purger atomic.Pointer[evsservice.Purger]
}

var _ evsservice.PurgeSink = (*hostPurgeSink)(nil)

// Queues commit's candidates on the block's Purger, or, before it is built, nowhere: the sweep's.
func (self *hostPurgeSink) Certain(commit *evsservice.PurgeCommit) {
	if purger := self.purger.Load(); purger != nil {
		purger.Certain(commit)
	}
}

// Builds the block's Purger on the engine's parts and the usage producer and fills purgeSink with it, or says in
// the missing list why it could not. settings have passed Validate.
func (self *Host) startPurger(ctx context.Context, host *sidecar.ServiceHost, settings *evsservice.PurgerSettings, holds *evsservice.HoldCache, purgeSink *hostPurgeSink) {
	if self.usage == nil {
		// the usage producer needs ServiceHost.Quota, whose absence the list names already
		self.missing = append(self.missing, "the versionless purge (EVSB-M10-01), which reports what it frees to the usage producer over ServiceHost.Quota")
		return
	}
	purger, err := evsservice.NewPurger(ctx, host, &evsservice.PurgerDeps{
		Format2:  self.Format2Gate(),
		Newest:   self.engine.PurgeNewest(),
		Holds:    holds,
		Usage:    self.usage,
		Settings: settings,
		Log:      hostLogger{},
	})
	if err != nil {
		self.missing = append(self.missing, fmt.Sprintf("the versionless purge (EVSB-M10-01): %v", err))
		return
	}
	self.purger = purger
	purgeSink.purger.Store(purger)
}

// The versionless purge, for a test that reads what it did: nil when the host could not build it.
func (self *Host) Purger() *evsservice.Purger {
	return self.purger
}
