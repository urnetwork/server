package quotahost

import (
	"context"
	"errors"
	"maps"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The one sidecar.Quota (§20.7) every engine host gets as ServiceHost.Quota, handed to
// sidecarhost.NewServiceHost as its Quota option by each block's main (QUOTA-M2-11). Two constructors
// differ only in where Postgres-held state comes from, a QuotaSource:
//
//	NewLocalQuota   api, connect and taskworker, over model directly
//	NewApiQuota     hosts with no Postgres (§19.4: evs, gkm, sockets, tlsedge, mxedge), over api's internal
//	                surface (QD-055); with no transport the host is never ready, failing closed
//
// and both run CountRate and the lease reservations straight on the Redis they are given, a host's
// ServiceHost.Redis or this process's own, so neither reaches Postgres or api on those paths.
//
//	Limits       cached LimitsTtl (§20.7's 5 s) a network, served from the cache however old while a
//	             refresh runs in the background, so a §21 tightening reaches a host within LimitsTtl and
//	             one load. A network with nothing cached waits for its first load, which runs off the
//	             caller's goroutine, only for as long as the caller's ctx allows: an inline caller passes a
//	             short one and takes the error (I-12; TLS reading 11's Community caps meanwhile)
//	OverLimit    a fresh read each call; when it fails the last list answers, marked stale, and a Quota
//	             that never read one answers the error, so an engine's readiness decides (Ready)
//	CountRate    model.QuotaRateCounter under the network's cached limits, on the caller's clock
//	Lease        lease.go's set; ReportUsage its lease half (lease_batch.go), posting through the source
//
// Every call takes a ctx and holds no lock across I/O. Safe for concurrent use.

type QuotaSettings struct {
	// How long a network's limits serve before a refresh: §20.7's 5 s.
	LimitsTtl time.Duration
	// Bounds one load of a network's limits, which runs on the Quota's own goroutine.
	LimitsLoadTimeout time.Duration
	// A network whose limits nobody asked for this long is forgotten: 10 min.
	LimitsForgetAge time.Duration
	// Bounds one over-limit read: 2 s, as 13.2.3's poll bounds it.
	OverLimitTimeout time.Duration
	// Bounds one batch's post to the source.
	ReportUsageTimeout time.Duration
	// nil takes each one's defaults.
	Lease *LeaseSettings
	Rate  *model.QuotaRateSettings
}

func DefaultQuotaSettings() *QuotaSettings {
	return &QuotaSettings{
		LimitsTtl:          5 * time.Second,
		LimitsLoadTimeout:  5 * time.Second,
		LimitsForgetAge:    10 * time.Minute,
		OverLimitTimeout:   2 * time.Second,
		ReportUsageTimeout: 10 * time.Second,
	}
}

// Where a Quota reads what Postgres holds: model (NewLocalQuota) or api's internal surface (NewApiQuota).
// Each call is one read or write, never cached by the source; an error leaves the Quota with what it holds.
type QuotaSource interface {
	BalanceSource
	// The network's effective limits (model.NetworkQuotaLimits).
	LoadLimits(ctx context.Context, networkId server.Id) (*sidecar.QuotaLimits, error)
	// Networks over their storage limit or spent (model.GetQuotaOverLimit).
	LoadOverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error)
	// Applies a batch with its lease carves once by its id (model.ApplyUsageBatch).
	ApplyUsageBatch(ctx context.Context, batch *sidecar.UsageBatch, carves []*model.UsageCarve) error
}

// Not ready: no over-limit list was ever read.
var ErrQuotaNotReady = errors.New("quotahost: no over-limit list has been read yet")

var quotaOverLimitStaleCounter = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_quota_over_limit_stale_total",
	Help: "Over-limit reads that failed and answered the last list read, marked stale.",
})

func init() {
	prometheus.MustRegister(quotaOverLimitStaleCounter)
}

// One network's limits in the cache.
type quotaLimitsEntry struct {
	// nil until the first load answers
	limits   *sidecar.QuotaLimits
	loadTime time.Time
	askTime  time.Time
	// closed when the load in flight ends; nil when none is
	loading chan struct{}
	// the last load's error and when it failed: no load is tried again for LimitsTtl after a failure,
	// so a source that is down sees one load a network a period, not one a call
	err      error
	failTime time.Time
}

// Whether a load may start now: none in flight, and none failed within LimitsTtl.
func (self *quotaLimitsEntry) mayLoad(now time.Time, ttl time.Duration) bool {
	return self.loading == nil && (self.failTime.IsZero() || ttl <= now.Sub(self.failTime))
}

// ServiceHost.Quota's one implementation.
var _ sidecar.Quota = (*HostQuota)(nil)

// sidecar.Quota over a QuotaSource and a Redis. See the file header.
type HostQuota struct {
	ctx      context.Context
	cancel   context.CancelFunc
	source   QuotaSource
	rates    *model.QuotaRateCounter
	leases   *quotaLeaseSet
	clock    sidecar.Clock
	settings *QuotaSettings

	stateLock       sync.Mutex
	networkLimits   map[server.Id]*quotaLimitsEntry
	lastForgetTime  time.Time
	overLimit       *sidecar.QuotaOverLimit
	overLimitTime   time.Time
	overLimitStale  bool
	limitsLoadGroup sync.WaitGroup
}

func newHostQuota(ctx context.Context, source QuotaSource, redis model.QuotaRateRedis, clock sidecar.Clock, settings *QuotaSettings) (*HostQuota, error) {
	if settings == nil {
		settings = DefaultQuotaSettings()
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	if redis == nil {
		return nil, errors.New("quotahost: a Quota needs a Redis")
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	quota := &HostQuota{
		ctx:           cancelCtx,
		cancel:        cancel,
		source:        source,
		rates:         model.NewQuotaRateCounter(redis, settings.Rate),
		clock:         clock,
		settings:      settings,
		networkLimits: map[server.Id]*quotaLimitsEntry{},
	}
	leases, err := newQuotaLeaseSet(cancelCtx, redis, source, func(ctx context.Context, networkId server.Id) (string, error) {
		limits, err := quota.Limits(ctx, connect.Id(networkId))
		if err != nil {
			return "", err
		}
		return limits.Tier, nil
	}, clock, settings.Lease)
	if err != nil {
		cancel()
		return nil, err
	}
	quota.leases = leases
	return quota, nil
}

func cloneQuotaLimits(limits *sidecar.QuotaLimits) *sidecar.QuotaLimits {
	return &sidecar.QuotaLimits{Tier: limits.Tier, Limits: maps.Clone(limits.Limits)}
}

func (self *HostQuota) Limits(ctx context.Context, networkId connect.Id) (*sidecar.QuotaLimits, error) {
	if networkId == (connect.Id{}) {
		return nil, errors.New("quotahost: a zero network id")
	}
	id := server.Id(networkId)
	var cached *sidecar.QuotaLimits
	var loading chan struct{}
	var loadErr error
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		now := self.clock.Now()
		self.forgetOldLimitsWithLock(now)
		entry, ok := self.networkLimits[id]
		if !ok {
			entry = &quotaLimitsEntry{}
			self.networkLimits[id] = entry
		}
		entry.askTime = now
		if entry.limits != nil {
			cached = cloneQuotaLimits(entry.limits)
			if self.settings.LimitsTtl <= now.Sub(entry.loadTime) && entry.mayLoad(now, self.settings.LimitsTtl) {
				self.startLimitsLoadWithLock(id, entry)
			}
			return
		}
		if entry.loading == nil && !entry.mayLoad(now, self.settings.LimitsTtl) {
			// the last load failed a moment ago: its error answers until the next try is due
			loadErr = entry.err
			return
		}
		loading = self.startLimitsLoadWithLock(id, entry)
	}()
	if cached != nil {
		return cached, nil
	}
	if loadErr != nil {
		return nil, loadErr
	}
	select {
	case <-loading:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if entry, ok := self.networkLimits[id]; ok && entry.limits != nil {
		return cloneQuotaLimits(entry.limits), nil
	} else if ok && entry.err != nil {
		return nil, entry.err
	}
	return nil, errors.New("quotahost: the network's limits did not load")
}

// Starts one load of the network's limits unless one is in flight, answering its channel. Under the
// Quota's stateLock.
func (self *HostQuota) startLimitsLoadWithLock(networkId server.Id, entry *quotaLimitsEntry) chan struct{} {
	if entry.loading != nil {
		return entry.loading
	}
	loading := make(chan struct{})
	entry.loading = loading
	self.limitsLoadGroup.Add(1)
	go func() {
		defer self.limitsLoadGroup.Done()
		defer close(loading)
		loadCtx, cancel := context.WithTimeout(self.ctx, self.settings.LimitsLoadTimeout)
		limits, err := self.source.LoadLimits(loadCtx, networkId)
		cancel()
		if err == nil && limits == nil {
			err = errors.New("quotahost: the source answered no limits")
		}
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		entry.loading = nil
		if err != nil {
			entry.err = err
			entry.failTime = self.clock.Now()
			if glog.V(1) {
				glog.Infof("[quota]a limits load failed: %s\n", err)
			}
			return
		}
		entry.limits = cloneQuotaLimits(limits)
		entry.loadTime = self.clock.Now()
		entry.err = nil
		entry.failTime = time.Time{}
	}()
	return loading
}

// Forgets networks nobody asked about for LimitsForgetAge, at most once each half of it. Under the
// Quota's stateLock.
func (self *HostQuota) forgetOldLimitsWithLock(now time.Time) {
	if now.Sub(self.lastForgetTime) < self.settings.LimitsForgetAge/2 {
		return
	}
	self.lastForgetTime = now
	for networkId, entry := range self.networkLimits {
		if entry.loading == nil && self.settings.LimitsForgetAge <= now.Sub(entry.askTime) {
			delete(self.networkLimits, networkId)
		}
	}
}

func (self *HostQuota) CountRate(ctx context.Context, networkId connect.Id, keyAmounts map[string]int64, debitId connect.Id) (*sidecar.QuotaRefusal, error) {
	if networkId == (connect.Id{}) || debitId == (connect.Id{}) {
		return nil, errors.New("quotahost: a zero network or debit id")
	}
	limits, err := self.Limits(ctx, networkId)
	if err != nil {
		return nil, err
	}
	return self.rates.CountRate(ctx, server.Id(networkId), limits, keyAmounts, server.Id(debitId), self.clock.Now())
}

func (self *HostQuota) Lease(ctx context.Context, networkId connect.Id, meter string) (sidecar.QuotaLease, *sidecar.QuotaRefusal, error) {
	return self.leases.Lease(ctx, networkId, meter)
}

func (self *HostQuota) ReportUsage(ctx context.Context, batch *sidecar.UsageBatch) error {
	return self.leases.reportUsage(ctx, batch, func(ctx context.Context, sent *sidecar.UsageBatch, carves []*model.UsageCarve) error {
		postCtx, cancel := context.WithTimeout(ctx, self.settings.ReportUsageTimeout)
		defer cancel()
		return self.source.ApplyUsageBatch(postCtx, sent, carves)
	})
}

func cloneQuotaOverLimit(overLimit *sidecar.QuotaOverLimit) *sidecar.QuotaOverLimit {
	return &sidecar.QuotaOverLimit{
		StorageNetworkIdRefusals: maps.Clone(overLimit.StorageNetworkIdRefusals),
		SpentNetworkIdRefusals:   maps.Clone(overLimit.SpentNetworkIdRefusals),
	}
}

func (self *HostQuota) OverLimit(ctx context.Context) (*sidecar.QuotaOverLimit, error) {
	callCtx, cancel := context.WithTimeout(ctx, self.settings.OverLimitTimeout)
	overLimit, err := self.source.LoadOverLimit(callCtx)
	cancel()
	if err == nil && overLimit == nil {
		err = errors.New("quotahost: the source answered no over-limit list")
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if err == nil {
		self.overLimit = cloneQuotaOverLimit(overLimit)
		self.overLimitTime = self.clock.Now()
		self.overLimitStale = false
		return cloneQuotaOverLimit(overLimit), nil
	}
	if self.overLimit == nil {
		return nil, err
	}
	self.overLimitStale = true
	quotaOverLimitStaleCounter.Inc()
	return cloneQuotaOverLimit(self.overLimit), nil
}

// Whether the last over-limit read failed, and the age of the list the Quota holds: an engine's readiness
// reads it. A Quota that never read one is not ready.
func (self *HostQuota) OverLimitStale() (stale bool, age time.Duration) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.overLimit == nil {
		return true, 0
	}
	return self.overLimitStale, self.clock.Now().Sub(self.overLimitTime)
}

// nil once an over-limit list has been read: before then no engine on this Quota may serve.
func (self *HostQuota) Ready() error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.overLimit == nil {
		return ErrQuotaNotReady
	}
	return nil
}

// Closes every lease, settling it; the engine's batcher drains first, so its batches carve what was spent.
// Then stops the Quota's goroutines.
func (self *HostQuota) Drain(ctx context.Context) {
	self.leases.Drain(ctx)
	self.cancel()
	self.limitsLoadGroup.Wait()
}

// Forgets what the cache holds of a network, so its next Limits loads afresh: a test's lever for limits
// it changed by hand.
func (self *HostQuota) forgetLimits(networkId server.Id) {
	self.limitsLoadGroup.Wait()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.networkLimits, networkId)
}
