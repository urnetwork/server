package main

// smtpedge's production send gate (SMTP-M9-20; §14.5, §14.7, plan-SMTP-small-readings#20, plan-SOCK-gate-data,
// I-12): smtpedgeservice.SmtpGate answered from memory, filled off every preamble's path. A miss asks for the
// network's load and answers not loaded, so the preamble answers TRY_LATER and the mail waits at its sender, never
// at the edge.
//
//	facts       model.GetSmtpGateFacts on the block's Postgres pool with Quota.Limits beside it, loaded on a miss
//	            and refreshed each FactsRefreshInterval while a preamble asked within FactsKeepInterval; past
//	            FactsMaxCount networks a new one is not loaded
//	community   Community's tier limits from pro.yml, refreshed with the facts
//	mail_send   AclGateView over the network's projection: no projection reader is wired into this block yet
//	            (plan-SOCK-gate-data's route for smtpedge), so every network answers no state, TRY_LATER
//	breaker     §21.5's level and listed addresses: ABUSE-M9-02's source is not here yet, so none is loaded,
//	            TRY_LATER
//
// So this gate admits nothing until both closed rows are wired, the direction R-SMTP-13 wants while D-SMTP-16 has
// not passed. Safe for concurrent use; no method that SmtpGate names does I/O.

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The gate's tunables.
type hostGateSettings struct {
	// How often loaded facts and Community's limits are read again: 60 s, the gate data's refresh
	// (plan-SOCK-gate-data).
	FactsRefreshInterval time.Duration
	// How long a network's facts stay loaded with no preamble asking: 1 h.
	FactsKeepInterval time.Duration
	// The most networks held: 100,000.
	FactsMaxCount int
	// How long one network's load may take: 10 s.
	LoadTimeout time.Duration
}

func defaultHostGateSettings() *hostGateSettings {
	return &hostGateSettings{
		FactsRefreshInterval: 60 * time.Second,
		FactsKeepInterval:    time.Hour,
		FactsMaxCount:        100000,
		LoadTimeout:          10 * time.Second,
	}
}

// What the gate loads from, a test's in place of Postgres and api.
type hostGateSource interface {
	// The network's facts, its limits among them.
	Facts(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpGateFacts, error)
	// Community's tier limits.
	CommunityLimits(ctx context.Context) (*sidecar.QuotaLimits, error)
}

// The block's source: the model on its Postgres pool, and Quota.Limits.
type modelGateSource struct {
	root  string
	quota sidecar.Quota
}

func (self *modelGateSource) Facts(ctx context.Context, networkId sidecar.Id) (facts *smtpedgeservice.SmtpGateFacts, returnErr error) {
	if self.quota == nil {
		return nil, errNoQuota
	}
	if r := server.HandleError(func() {
		facts, returnErr = model.GetSmtpGateFacts(ctx, server.Id(networkId), self.root)
	}); r != nil {
		return nil, errGateLoad
	}
	if returnErr != nil {
		return nil, returnErr
	}
	limits, err := self.quota.Limits(ctx, connect.Id(networkId))
	if err != nil {
		return nil, err
	}
	facts.Limits = limits
	return facts, nil
}

// Community's plan, with enforce_sidecar_limits's state as NetworkQuotaLimits sets it.
func (self *modelGateSource) CommunityLimits(ctx context.Context) (*sidecar.QuotaLimits, error) {
	pro := model.Pro()
	limits := pro.TierFor(model.NetworkTierFree).Sidecar.Limits()
	return &sidecar.QuotaLimits{Tier: string(model.NetworkTierFree), Limits: limits, Enforced: pro.EnforceSidecarLimits}, nil
}

var (
	// The block has no Quota, so no network's limits can be read.
	errNoQuota = errors.New("smtpedge gate: no quota")
	// The model raised on a read.
	errGateLoad = errors.New("smtpedge gate: a facts read failed")
)

// One network's loaded facts.
type hostGateEntry struct {
	facts *smtpedgeservice.SmtpGateFacts
	// when a preamble last asked
	askTime time.Time
}

// The gate. newHostGate makes one, running until ctx ends.
type hostGate struct {
	ctx      context.Context
	source   hostGateSource
	clock    sidecar.Clock
	settings *hostGateSettings
	group    sync.WaitGroup

	stateLock      sync.Mutex
	networkIdFacts map[sidecar.Id]*hostGateEntry
	loading        map[sidecar.Id]bool
	community      *sidecar.QuotaLimits
}

var _ smtpedgeservice.SmtpGate = (*hostGate)(nil)

func newHostGate(ctx context.Context, source hostGateSource, clock sidecar.Clock, settings *hostGateSettings) *hostGate {
	self := &hostGate{
		ctx:            ctx,
		source:         source,
		clock:          clock,
		settings:       settings,
		networkIdFacts: map[sidecar.Id]*hostGateEntry{},
		loading:        map[sidecar.Id]bool{},
	}
	self.group.Add(1)
	go self.run()
	return self
}

// Waits for the gate's goroutines once ctx ended.
func (self *hostGate) wait() {
	self.group.Wait()
}

// See smtpedgeservice.SmtpGate.
func (self *hostGate) Facts(networkId sidecar.Id) (*smtpedgeservice.SmtpGateFacts, bool) {
	now := self.clock.Now()
	load := false
	facts, ok := func() (*smtpedgeservice.SmtpGateFacts, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if entry, ok := self.networkIdFacts[networkId]; ok {
			entry.askTime = now
			return entry.facts, true
		}
		if !self.loading[networkId] && len(self.networkIdFacts)+len(self.loading) < self.settings.FactsMaxCount {
			self.loading[networkId] = true
			load = true
		}
		return nil, false
	}()
	if load {
		self.group.Add(1)
		go func() {
			defer self.group.Done()
			self.load(networkId, now)
		}()
	}
	if !ok {
		return nil, false
	}
	// a copy, so a refresh never changes one handed out
	kept := *facts
	return &kept, true
}

// Loads one network's facts, keeping them on success.
func (self *hostGate) load(networkId sidecar.Id, askTime time.Time) {
	ctx, cancel := context.WithCancel(self.ctx)
	stop := self.clock.AfterFunc(self.settings.LoadTimeout, cancel)
	defer func() {
		stop()
		cancel()
	}()
	facts, err := self.source.Facts(ctx, networkId)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.loading, networkId)
	if err != nil || facts == nil {
		return
	}
	self.networkIdFacts[networkId] = &hostGateEntry{facts: facts, askTime: askTime}
}

// See smtpedgeservice.SmtpGate: no projection is read into this block yet, so no network's state is loaded.
func (self *hostGate) MaySendMail(networkId sidecar.Id, sidecarId sidecar.Id, now time.Time) sidecar.AclDecision {
	return sidecar.AclDecision{Reason: sidecar.AclReasonNoState}
}

// See smtpedgeservice.SmtpGate: §21.5's source is not here yet, so nothing is loaded.
func (self *hostGate) Breaker() (*smtpedgeservice.SmtpBreakerState, bool) {
	return nil, false
}

// See smtpedgeservice.SmtpGate.
func (self *hostGate) CommunityLimits() (*sidecar.QuotaLimits, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.community == nil {
		return nil, false
	}
	return &sidecar.QuotaLimits{Tier: self.community.Tier, Limits: maps.Clone(self.community.Limits), Enforced: self.community.Enforced}, true
}

// Refreshes Community's limits and every network asked within FactsKeepInterval, forgetting the rest, each
// FactsRefreshInterval until ctx ends.
func (self *hostGate) run() {
	defer self.group.Done()
	for {
		if community, err := self.source.CommunityLimits(self.ctx); err == nil && community != nil {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.community = community
			}()
		}
		now := self.clock.Now()
		refresh := func() map[sidecar.Id]time.Time {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			refresh := map[sidecar.Id]time.Time{}
			for networkId, entry := range self.networkIdFacts {
				if self.settings.FactsKeepInterval < now.Sub(entry.askTime) {
					delete(self.networkIdFacts, networkId)
					continue
				}
				refresh[networkId] = entry.askTime
			}
			return refresh
		}()
		for networkId, askTime := range refresh {
			if self.ctx.Err() != nil {
				return
			}
			self.load(networkId, askTime)
		}
		select {
		case <-self.ctx.Done():
			return
		case <-self.clock.After(self.settings.FactsRefreshInterval):
		}
	}
}
