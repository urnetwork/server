package model

// §21.7's standing and network_verified in memory, for the gates that read them on an inline path (ABUSE-M5-07;
// D-ABUSE-4, I-12): ServiceHost.Abuse's real AbuseGate in a process that holds Postgres, and the set of networks
// CreateContract's hard deny refuses. Neither reads Postgres when asked; both load off the path, and every answer
// they cannot give is a refusal, never a pass.
//
// AbuseHardDenySnapshot is the small set of networks §8.6's hard deny holds: those network_abuse_state holds
// suspended, or in a state this build does not know. It reloads every Interval, 5 s, as §8.10's heads snapshot does,
// on its own goroutine, and a failed load keeps the last set. A set never loaded, or older than MaxAge, answers not
// loaded, so a process cut off from Postgres stops passing what it can no longer check. In a binary that does not
// link MG5 there is no table and no network to hold, so every load is the empty set and reads nothing.
//
// NewAbuseGate's NetworkAbuseGate answers sidecar.AbuseGate from that set and a bounded per-network cache:
//
//	Standing  suspended (or the unknown state) from the set; else the cached network's probation, restricted or
//	          standard, derived at the moment asked from its creation and its row, so probation's end and a
//	          restriction's end take effect on time without a load
//	Verified  the cached network_verified
//
// A network not cached, cached past MaxAge, or cached as no network answers ok false, and asks the loader for it; an
// entry past RefreshAfter still answers and asks for a refresh. The loader is one goroutine that reads the asked
// networks in batches of LoadBatchSize, each batch one statement (loadNetworkAbuseFactsWithConn), and backs off after
// a failure. A cached entry that saw the network suspended while a newer set does not hold it cannot tell what
// followed the lift, so it answers ok false until it reloads. The cache holds at most MaxNetworkCount networks: past
// it, entries nobody has read within MaxAge go first, then any.
//
// Inline packet and frame paths read only this memory (I-12). A paid balance's window ends with no writer, so
// network_verified's staleness is bounded by RefreshAfter and MaxAge, never by a writer's invalidation alone.
//
// Both are safe for concurrent use. Neither creates, derives or unwraps a key.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The network's standing could not be read: no such network, or not loaded where only memory may be read.
var ErrAbuseStandingUnknown = errors.New("abuse standing unknown")

// Where a hard deny snapshot reads the held networks, each with its stored state. Must return its failures, never
// panic.
type AbuseHardDenySource func(ctx context.Context) (map[server.Id]AbuseStanding, error)

// Tunables of the hard deny snapshot.
type AbuseHardDenySettings struct {
	// How often the set reloads: 5 s, §8.10's heads snapshot's interval, so a suspension and its lift reach
	// CreateContract and the gates within about this long. Non-positive loads once, at construction.
	Interval time.Duration
	// How long one load may run before it is given up and the last set kept: 4 s, inside one interval. Non-positive
	// is no deadline but the snapshot's own.
	Timeout time.Duration
	// How old the last clean load may be before the set answers not loaded: a minute, twelve intervals, so a process
	// that cannot reach Postgres stops passing what it cannot check. Non-positive keeps a set however old.
	MaxAge time.Duration
	// The most held networks one load reads: a load finding more fails, keeping the last set, since a cut set would
	// pass a suspended network. 100,000, far past any count of suspensions.
	MaxCount int
	// The least time between two default-level reports of a failed load: a minute.
	ReportInterval time.Duration
	// Called after each load on the snapshot's goroutine, which it must not hold: the set's size and why the load
	// failed, nil when it read cleanly. What a test waits on. Nil calls nothing.
	OnLoad func(count int, err error)
}

// §21.7's hard deny's numbers.
func DefaultAbuseHardDenySettings() *AbuseHardDenySettings {
	return &AbuseHardDenySettings{
		Interval:       5 * time.Second,
		Timeout:        4 * time.Second,
		MaxAge:         time.Minute,
		MaxCount:       100000,
		ReportInterval: time.Minute,
	}
}

// One clean load, never changed once stored.
type abuseHardDenySet struct {
	networkIdStandings map[server.Id]AbuseStanding
	loadTime           time.Time
}

// The held networks. See the file header.
type AbuseHardDenySnapshot struct {
	ctx      context.Context
	cancel   context.CancelFunc
	source   AbuseHardDenySource
	now      func() time.Time
	settings *AbuseHardDenySettings
	failures *abuseGateReport

	set atomic.Pointer[abuseHardDenySet]
}

// The snapshot on Postgres, loading at once and then every Interval until ctx ends or Close. settings may be nil for
// the defaults.
func NewAbuseHardDenySnapshot(ctx context.Context, settings *AbuseHardDenySettings) *AbuseHardDenySnapshot {
	if settings == nil {
		settings = DefaultAbuseHardDenySettings()
	}
	return newAbuseHardDenySnapshot(ctx, func(ctx context.Context) (map[server.Id]AbuseStanding, error) {
		return loadAbuseHardDeny(ctx, settings.MaxCount)
	}, nil, settings)
}

// A snapshot on a test's source and clock, nil now being server.NowUtc and nil settings the defaults.
func Testing_NewAbuseHardDenySnapshot(
	ctx context.Context,
	source AbuseHardDenySource,
	now func() time.Time,
	settings *AbuseHardDenySettings,
) *AbuseHardDenySnapshot {
	return newAbuseHardDenySnapshot(ctx, source, now, settings)
}

func newAbuseHardDenySnapshot(
	ctx context.Context,
	source AbuseHardDenySource,
	now func() time.Time,
	settings *AbuseHardDenySettings,
) *AbuseHardDenySnapshot {
	if settings == nil {
		settings = DefaultAbuseHardDenySettings()
	}
	if now == nil {
		now = server.NowUtc
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	self := &AbuseHardDenySnapshot{
		ctx:      cancelCtx,
		cancel:   cancel,
		source:   source,
		now:      now,
		settings: settings,
		failures: newAbuseGateReport(settings.ReportInterval),
	}
	go self.run()
	return self
}

// Stops the loads. The last set keeps answering until it passes MaxAge.
func (self *AbuseHardDenySnapshot) Close() {
	self.cancel()
}

// Whether a set has loaded and is younger than MaxAge.
func (self *AbuseHardDenySnapshot) Ready() bool {
	return self.current(self.now()) != nil
}

// The set in force at now, nil when none has loaded or the last is past MaxAge.
func (self *AbuseHardDenySnapshot) current(now time.Time) *abuseHardDenySet {
	set := self.set.Load()
	if set == nil {
		return nil
	}
	if 0 < self.settings.MaxAge && !now.Before(set.loadTime.Add(self.settings.MaxAge)) {
		return nil
	}
	return set
}

// Whether the hard deny holds the network, with the state it is held in, suspended or one this build does not know.
// ok false while no set is in force, which every caller refuses on. One map read, no I/O and no allocation.
func (self *AbuseHardDenySnapshot) Denied(networkId server.Id) (standing AbuseStanding, denied bool, ok bool) {
	set := self.current(self.now())
	if set == nil {
		return "", false, false
	}
	standing, denied = set.networkIdStandings[networkId]
	return standing, denied, true
}

// Loads now and then every Interval, until the snapshot closes.
func (self *AbuseHardDenySnapshot) run() {
	for {
		self.load()
		if self.settings.Interval <= 0 {
			return
		}
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.Interval):
		}
	}
}

// One load: a clean one installs its set, a failed one keeps the last.
func (self *AbuseHardDenySnapshot) load() {
	if self.ctx.Err() != nil {
		return
	}
	ctx := self.ctx
	if 0 < self.settings.Timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(self.ctx, self.settings.Timeout)
		defer cancel()
	}
	loadTime := self.now()
	networkIdStandings, err := self.source(ctx)
	count := 0
	if err == nil {
		if networkIdStandings == nil {
			networkIdStandings = map[server.Id]AbuseStanding{}
		}
		self.set.Store(&abuseHardDenySet{
			networkIdStandings: networkIdStandings,
			loadTime:           loadTime,
		})
		count = len(networkIdStandings)
	} else if self.ctx.Err() == nil {
		if suppressedCount, report := self.failures.Allow(time.Now()); report {
			glog.Infof("[abuse]the hard deny's load failed and its last set keeps serving: %v; %d more since the last report\n", err, suppressedCount)
		}
	}
	if onLoad := self.settings.OnLoad; onLoad != nil {
		onLoad(count, err)
	}
}

// Every network network_abuse_state holds in a state other than restricted, each with that state: suspended, or one
// this build does not know, which the hard deny holds too, the closed direction. More than maxCount fails, since a
// cut set would pass a held network. The empty set, reading nothing, in a binary that does not link MG5.
func loadAbuseHardDeny(ctx context.Context, maxCount int) (networkIdStandings map[server.Id]AbuseStanding, returnErr error) {
	networkIdStandings = map[server.Id]AbuseStanding{}
	if !server.SidecarMg5Linked() {
		return networkIdStandings, nil
	}
	if r := server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT
						network_abuse_state.network_id,
						network_abuse_state.state
					FROM network_abuse_state
					WHERE network_abuse_state.state <> $1
					LIMIT $2
				`,
				networkAbuseStateRestricted,
				maxCount+1,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var networkId server.Id
					var state string
					server.Raise(result.Scan(&networkId, &state))
					networkIdStandings[networkId] = AbuseStanding(state)
				}
			})
		})
	}); r != nil {
		return nil, errors.New("network_abuse_state's held networks did not load")
	}
	if maxCount < len(networkIdStandings) {
		return nil, errors.New("network_abuse_state holds more networks than the hard deny reads")
	}
	return networkIdStandings, nil
}

// Tunables of NewAbuseGate's cache.
type AbuseGateSettings struct {
	// An entry older than this still answers and asks for a refresh: 60 s, network_verified's Redis window
	// (NetworkVerifiedSettings.CacheTtl), so a gate is no staler than api's own reads. Non-positive refreshes at
	// every read.
	RefreshAfter time.Duration
	// An entry older than this answers not loaded: 5 min, so a loader cut off from Postgres stops passing what it
	// cannot check. Non-positive keeps an entry however old.
	MaxAge time.Duration
	// How long an answer that the network does not exist is kept before a read asks again: 5 s, so a network
	// created after its first read reaches the gates soon.
	AbsentRefreshAfter time.Duration
	// The most networks cached: 65,536, a few megabytes. Past it, entries unread within MaxAge are dropped first,
	// then any, never more than a load needs.
	MaxNetworkCount int
	// How far a read must move an entry's read time before it is written: a second, so concurrent reads of one
	// network seldom write its shared line, which eviction reads only to the minute. Non-positive writes at every read.
	ReadTimeResolution time.Duration
	// The most networks one load reads, in one statement: 256.
	LoadBatchSize int
	// One load's bound: 4 s.
	LoadTimeout time.Duration
	// How long the loader waits after a failed load before the next: a second, so an outage costs one statement a
	// second and not one per read.
	LoadRetryInterval time.Duration
	// The least time between two default-level reports of a failed load: a minute.
	ReportInterval time.Duration
	// The snapshot's settings, for a gate that makes its own. nil takes the defaults.
	HardDeny *AbuseHardDenySettings
	// The standing's numbers (ProbationAge). nil takes DefaultAbuseSettings.
	Abuse *AbuseSettings
	// Called after each load on the loader's goroutine, which it must not hold, once its entries are installed: the
	// networks it read and why it failed, nil when it read cleanly. What a host counts and a test waits on. Nil calls
	// nothing.
	OnLoad func(networkIds []server.Id, err error)
}

// The cache's numbers.
func DefaultAbuseGateSettings() *AbuseGateSettings {
	return &AbuseGateSettings{
		RefreshAfter:       60 * time.Second,
		MaxAge:             5 * time.Minute,
		AbsentRefreshAfter: 5 * time.Second,
		MaxNetworkCount:    65536,
		ReadTimeResolution: time.Second,
		LoadBatchSize:      256,
		LoadTimeout:        4 * time.Second,
		LoadRetryInterval:  time.Second,
		ReportInterval:     time.Minute,
	}
}

// Where a gate reads networks' facts at now, keyed by network, a network that does not exist having no entry. Must
// return its failures, never panic.
type abuseGateLoad func(ctx context.Context, networkIds []server.Id, now time.Time) (map[server.Id]*networkAbuseFacts, error)

// One network as the gate holds it, never changed once installed but for its read time.
type abuseGateEntry struct {
	// false for an answer that the network does not exist
	found bool
	// the row's state when it is neither restricted nor absent: suspended, or one this build does not know
	heldStanding AbuseStanding
	// when probation ends, zero for no probation
	probationEnd time.Time
	restricted   bool
	// when the restriction ends, zero for no end
	restrictedUntil time.Time
	// every entry condition of trusted held at the load (ABUSE-M6-01)
	trusted  bool
	verified bool
	loadTime        time.Time
	// unix nanoseconds of the last read, which eviction reads
	readTime atomic.Int64
}

// The entry's standing at now, past a hold, in QD-194's order.
func (self *abuseGateEntry) standing(now time.Time) AbuseStanding {
	switch {
	case now.Before(self.probationEnd):
		return AbuseStandingProbation
	case self.restricted && (self.restrictedUntil.IsZero() || now.Before(self.restrictedUntil)):
		return AbuseStandingRestricted
	case self.trusted:
		return AbuseStandingTrusted
	}
	return AbuseStandingStandard
}

// ServiceHost.Abuse on Postgres. See the file header.
type NetworkAbuseGate struct {
	ctx          context.Context
	cancel       context.CancelFunc
	hardDeny     *AbuseHardDenySnapshot
	ownsHardDeny bool
	load         abuseGateLoad
	now          func() time.Time
	settings     *AbuseGateSettings
	abuse        *AbuseSettings
	failures     *abuseGateReport

	stateLock        sync.RWMutex
	networkIdEntries map[server.Id]*abuseGateEntry

	// asked networks, waiting for the loader or in its current load
	loadLock         sync.Mutex
	askedNetworkIds  map[server.Id]bool
	queuedNetworkIds []server.Id
	loadAsked        chan struct{}
	// networks loaded since the gate began
	loadCount atomic.Uint64
}

// The gate on Postgres over hardDeny, a snapshot of its own when nil, its loader running until ctx ends or Close.
// settings may be nil for the defaults.
func NewAbuseGate(ctx context.Context, hardDeny *AbuseHardDenySnapshot, settings *AbuseGateSettings) *NetworkAbuseGate {
	return newAbuseGate(ctx, hardDeny, func(ctx context.Context, networkIds []server.Id, now time.Time) (networkIdFacts map[server.Id]*networkAbuseFacts, returnErr error) {
		if r := server.HandleError(func() {
			server.Db(ctx, func(conn server.PgConn) {
				networkIdFacts = loadNetworkAbuseFactsWithConn(ctx, conn, networkIds, now, true)
			})
		}); r != nil {
			return nil, errors.New("the networks' standing did not load")
		}
		return networkIdFacts, nil
	}, nil, settings)
}

func newAbuseGate(
	ctx context.Context,
	hardDeny *AbuseHardDenySnapshot,
	load abuseGateLoad,
	now func() time.Time,
	settings *AbuseGateSettings,
) *NetworkAbuseGate {
	if settings == nil {
		settings = DefaultAbuseGateSettings()
	}
	abuse := settings.Abuse
	if abuse == nil {
		abuse = DefaultAbuseSettings()
	}
	if now == nil {
		now = server.NowUtc
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	ownsHardDeny := false
	if hardDeny == nil {
		hardDeny = NewAbuseHardDenySnapshot(cancelCtx, settings.HardDeny)
		ownsHardDeny = true
	}
	self := &NetworkAbuseGate{
		ctx:              cancelCtx,
		cancel:           cancel,
		hardDeny:         hardDeny,
		ownsHardDeny:     ownsHardDeny,
		load:             load,
		now:              now,
		settings:         settings,
		abuse:            abuse,
		failures:         newAbuseGateReport(settings.ReportInterval),
		networkIdEntries: map[server.Id]*abuseGateEntry{},
		askedNetworkIds:  map[server.Id]bool{},
		loadAsked:        make(chan struct{}, 1),
	}
	go self.run()
	return self
}

// Stops the loader, and the snapshot when the gate made it. What is cached keeps answering until it passes MaxAge.
func (self *NetworkAbuseGate) Close() {
	self.cancel()
	if self.ownsHardDeny {
		self.hardDeny.Close()
	}
}

// See sidecar.AbuseGate and the file header. No I/O and no allocation.
func (self *NetworkAbuseGate) Standing(networkId connect.Id) (sidecar.AbuseStanding, bool) {
	id := server.Id(networkId)
	now := self.now()
	set := self.hardDeny.current(now)
	if set != nil {
		if standing, denied := set.networkIdStandings[id]; denied {
			return standing, true
		}
	}
	entry := self.entry(id, now)
	if set == nil || entry == nil || !entry.found {
		// a set must be in force to say the network is not held
		return "", false
	}
	if entry.heldStanding != "" {
		if entry.loadTime.Before(set.loadTime) {
			// held when cached and not by the newer set: the hold ended, and what followed it is not cached
			self.ask(id)
			return "", false
		}
		// held after the set loaded: refused either way
		return entry.heldStanding, true
	}
	return entry.standing(now), true
}

// See sidecar.AbuseGate and the file header. No I/O and no allocation.
func (self *NetworkAbuseGate) Verified(networkId connect.Id) (bool, bool) {
	entry := self.entry(server.Id(networkId), self.now())
	if entry == nil || !entry.found {
		return false, false
	}
	return entry.verified, true
}

// The network's entry when it may answer at now, else nil; either way a load is asked for an entry that is missing,
// too old to answer, or old enough to refresh.
func (self *NetworkAbuseGate) entry(networkId server.Id, now time.Time) *abuseGateEntry {
	self.stateLock.RLock()
	entry := self.networkIdEntries[networkId]
	self.stateLock.RUnlock()
	if entry == nil {
		self.ask(networkId)
		return nil
	}
	age := now.Sub(entry.loadTime)
	if 0 < self.settings.MaxAge && self.settings.MaxAge <= age {
		self.ask(networkId)
		return nil
	}
	refreshAfter := self.settings.RefreshAfter
	if !entry.found {
		refreshAfter = self.settings.AbsentRefreshAfter
	}
	if refreshAfter <= age {
		self.ask(networkId)
	}
	nowNanos := now.UnixNano()
	if entry.readTime.Load() <= nowNanos-int64(self.settings.ReadTimeResolution) {
		entry.readTime.Store(nowNanos)
	}
	return entry
}

// Asks the loader for the network unless it is asked already, without blocking. At most MaxNetworkCount networks wait.
func (self *NetworkAbuseGate) ask(networkId server.Id) {
	asked := func() bool {
		self.loadLock.Lock()
		defer self.loadLock.Unlock()
		if self.askedNetworkIds[networkId] || self.settings.MaxNetworkCount <= len(self.askedNetworkIds) {
			return false
		}
		self.askedNetworkIds[networkId] = true
		self.queuedNetworkIds = append(self.queuedNetworkIds, networkId)
		return true
	}()
	if !asked {
		return
	}
	select {
	case self.loadAsked <- struct{}{}:
	default:
	}
}

// Loads what is asked, a batch at a time, until the gate closes.
func (self *NetworkAbuseGate) run() {
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-self.loadAsked:
		}
		for {
			networkIds := func() []server.Id {
				self.loadLock.Lock()
				defer self.loadLock.Unlock()
				batchSize := min(max(1, self.settings.LoadBatchSize), len(self.queuedNetworkIds))
				networkIds := self.queuedNetworkIds[:batchSize:batchSize]
				self.queuedNetworkIds = self.queuedNetworkIds[batchSize:]
				return networkIds
			}()
			if len(networkIds) == 0 {
				break
			}
			err := self.loadBatch(networkIds)
			func() {
				self.loadLock.Lock()
				defer self.loadLock.Unlock()
				for _, networkId := range networkIds {
					delete(self.askedNetworkIds, networkId)
				}
			}()
			if onLoad := self.settings.OnLoad; onLoad != nil {
				onLoad(networkIds, err)
			}
			if err == nil {
				continue
			}
			if suppressedCount, report := self.failures.Allow(time.Now()); report {
				glog.Infof("[abuse]the gate's load failed, and its networks answer from what is cached until it succeeds: %v; %d more since the last report\n", err, suppressedCount)
			}
			select {
			case <-self.ctx.Done():
				return
			case <-time.After(self.settings.LoadRetryInterval):
			}
		}
	}
}

// One load of networkIds, installing an entry for each, an absent one for a network that does not exist.
func (self *NetworkAbuseGate) loadBatch(networkIds []server.Id) error {
	ctx := self.ctx
	if 0 < self.settings.LoadTimeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(self.ctx, self.settings.LoadTimeout)
		defer cancel()
	}
	now := self.now()
	networkIdFacts, err := self.load(ctx, networkIds, now)
	if err != nil {
		return err
	}
	entries := make([]*abuseGateEntry, len(networkIds))
	for index, networkId := range networkIds {
		entry := &abuseGateEntry{loadTime: now}
		if facts, found := networkIdFacts[networkId]; found {
			entry.found = true
			entry.verified = facts.verified
			entry.trusted = facts.trusted
			if 0 < self.abuse.ProbationAge {
				entry.probationEnd = facts.createTime.Add(self.abuse.ProbationAge)
			}
			if row := facts.row; row != nil {
				if row.state == networkAbuseStateRestricted {
					entry.restricted = true
					if row.untilTime != nil {
						entry.restrictedUntil = *row.untilTime
					}
				} else {
					entry.heldStanding = AbuseStanding(row.state)
				}
			}
		}
		entry.readTime.Store(now.UnixNano())
		entries[index] = entry
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for index, networkId := range networkIds {
		if _, cached := self.networkIdEntries[networkId]; !cached {
			self.makeRoomWithLock(now)
		}
		self.networkIdEntries[networkId] = entries[index]
	}
	self.loadCount.Add(uint64(len(networkIds)))
	return nil
}

// Drops entries until one more fits: those unread within MaxAge first, then any.
func (self *NetworkAbuseGate) makeRoomWithLock(now time.Time) {
	if len(self.networkIdEntries) < self.settings.MaxNetworkCount {
		return
	}
	if 0 < self.settings.MaxAge {
		idleNanos := now.Add(-self.settings.MaxAge).UnixNano()
		for networkId, entry := range self.networkIdEntries {
			if entry.readTime.Load() < idleNanos {
				delete(self.networkIdEntries, networkId)
			}
		}
	}
	for networkId := range self.networkIdEntries {
		if len(self.networkIdEntries) < self.settings.MaxNetworkCount {
			return
		}
		// map order is unspecified, which serves as a random choice
		delete(self.networkIdEntries, networkId)
	}
}

// How many networks the gate holds, and how many it has loaded since it began, for a host's metrics and a test.
func (self *NetworkAbuseGate) Stats() (cachedCount int, loadedCount uint64) {
	self.stateLock.RLock()
	cachedCount = len(self.networkIdEntries)
	self.stateLock.RUnlock()
	return cachedCount, self.loadCount.Load()
}

// A default-level report that logs at most once per interval and counts what it held back. Safe for concurrent use.
type abuseGateReport struct {
	interval time.Duration

	stateLock       sync.Mutex
	nextReportTime  time.Time
	suppressedCount int
}

func newAbuseGateReport(interval time.Duration) *abuseGateReport {
	return &abuseGateReport{interval: interval}
}

// Whether to report at now, and how many reports were held back since the last.
func (self *abuseGateReport) Allow(now time.Time) (suppressedCount int, report bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if now.Before(self.nextReportTime) {
		self.suppressedCount += 1
		return 0, false
	}
	self.nextReportTime = now.Add(self.interval)
	suppressedCount = self.suppressedCount
	self.suppressedCount = 0
	return suppressedCount, true
}
