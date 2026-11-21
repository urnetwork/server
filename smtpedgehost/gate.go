package smtpedgehost

// smtpedge's production send gate (SMTP-M9-20; §14.5, §14.7, plan-SMTP-small-readings#20, plan-SOCK-gate-data,
// I-12): smtpedgeservice.SmtpGate answered from memory, filled off every preamble's path. A miss asks for the
// network's load and answers not loaded, so the preamble answers TRY_LATER and the mail waits at its sender, never
// at the edge.
//
//	facts       model.GetSmtpGateFacts on the block's Postgres pool with Quota.Limits beside it, loaded on a miss
//	            and refreshed each FactsRefreshInterval while a preamble asked within FactsKeepInterval; past
//	            FactsMaxCount networks a new one is not loaded
//	community   Community's tier limits from pro.yml, refreshed with the facts
//	mail_send   the subject's grant and Team scope labels from the same watched projection
//	breaker     §21.5's level and listed sending addresses from model.GetMailBreakerState (ABUSE-M9-02), read
//	            with the facts each FactsRefreshInterval. A read that fails, or rows that do not read whole, leave
//	            none loaded, TRY_LATER: a state last read a refresh ago is not kept, since the level it lacks could
//	            be a pause
//
// Safe for concurrent use. A preamble reads only in-memory decisions.

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The gate's tunables.
type HostGateSettings struct {
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

// The defaults.
func DefaultHostGateSettings() *HostGateSettings {
	return &HostGateSettings{
		FactsRefreshInterval: 60 * time.Second,
		FactsKeepInterval:    time.Hour,
		FactsMaxCount:        100000,
		LoadTimeout:          10 * time.Second,
	}
}

// What the gate loads from, a test's in place of Postgres and api.
type HostGateSource interface {
	// The network's facts, its limits among them.
	Facts(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpGateFacts, error)
	// Community's tier limits.
	CommunityLimits(ctx context.Context) (*sidecar.QuotaLimits, error)
	// §21.5's breaker state; an error is a state that could not be read whole.
	Breaker(ctx context.Context) (*smtpedgeservice.SmtpBreakerState, error)
}

// The block's source: the model on its Postgres pool, and Quota.Limits.
type modelGateSource struct {
	root  string
	quota sidecar.Quota
}

// The block's gate source: the model on the process's Postgres pool for the networks under root, and quota's
// limits, nil answering every network's facts not loaded.
func NewModelGateSource(root string, quota sidecar.Quota) HostGateSource {
	return &modelGateSource{root: root, quota: quota}
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

// §21.5's breaker state as the standing abuse_action rows hold it: the level, value for value, and the listed
// sending addresses.
func (self *modelGateSource) Breaker(ctx context.Context) (state *smtpedgeservice.SmtpBreakerState, returnErr error) {
	if r := server.HandleError(func() {
		breaker, ok := model.GetMailBreakerState(ctx)
		if !ok {
			returnErr = errBreakerLoad
			return
		}
		state = &smtpedgeservice.SmtpBreakerState{
			Level:           smtpedgeservice.SmtpBreakerLevel(breaker.Level),
			ListedAddresses: slices.Clone(breaker.ListedAddresses),
		}
	}); r != nil {
		return nil, errBreakerLoad
	}
	return state, returnErr
}

var (
	// The breaker rows did not read whole, or this binary links no MG5.
	errBreakerLoad = errors.New("smtpedge gate: the breaker state did not load")
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
	mail     *sidecar.AclGateView
	ctx      context.Context
	source   HostGateSource
	clock    sidecar.Clock
	settings *HostGateSettings
	group    sync.WaitGroup

	stateLock      sync.Mutex
	networkIdFacts map[sidecar.Id]*hostGateEntry
	loading        map[sidecar.Id]bool
	community      *sidecar.QuotaLimits
	// nil until a refresh read it, and again after one that could not
	breaker *smtpedgeservice.SmtpBreakerState
}

var _ smtpedgeservice.SmtpGate = (*hostGate)(nil)

func newHostGate(ctx context.Context, source HostGateSource, clock sidecar.Clock, settings *HostGateSettings) *hostGate {
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

// The network's database facts. A miss starts their background load.
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
	// A copy for the projection's scope labels.
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
		delete(self.networkIdFacts, networkId)
		return
	}
	self.networkIdFacts[networkId] = &hostGateEntry{facts: facts, askTime: askTime}
}

// Each preamble takes its subject's grant and scopes from one watched projection. Database facts
// refresh separately. Reading both starts both loads even when neither has arrived.
func (self *hostGate) SendFacts(networkId sidecar.Id, sidecarId sidecar.Id, now time.Time) (*smtpedgeservice.SmtpGateFacts, sidecar.AclDecision) {
	facts, _ := self.Facts(networkId)
	grant, labels := self.mail.MaySendMailWithScopes(networkId, sidecar.AclSidecarSubject(sidecarId), now)
	if facts != nil {
		facts.ScopeLabels = labels
	}
	return facts, grant
}

// See smtpedgeservice.SmtpGate: §21.5's state as the last refresh read it, a copy, or not loaded.
func (self *hostGate) Breaker() (*smtpedgeservice.SmtpBreakerState, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.breaker == nil {
		return nil, false
	}
	return &smtpedgeservice.SmtpBreakerState{
		Level:           self.breaker.Level,
		ListedAddresses: slices.Clone(self.breaker.ListedAddresses),
	}, true
}

// Reads §21.5's breaker state once, bounded by LoadTimeout, keeping it or, when it did not read, nothing.
func (self *hostGate) loadBreaker() {
	ctx, cancel := context.WithCancel(self.ctx)
	stop := self.clock.AfterFunc(self.settings.LoadTimeout, cancel)
	defer func() {
		stop()
		cancel()
	}()
	breaker, err := self.source.Breaker(ctx)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err != nil || breaker == nil {
		self.breaker = nil
		return
	}
	self.breaker = breaker
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

// Refreshes §21.5's breaker state, Community's limits and every network asked within FactsKeepInterval, forgetting
// the rest, each FactsRefreshInterval until ctx ends.
func (self *hostGate) run() {
	defer self.group.Done()
	for {
		self.loadBreaker()
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
