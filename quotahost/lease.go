// Package quotahost is server's sidecar.Quota (§20.6, §20.7): the one implementation every engine host gets
// as ServiceHost.Quota. lease*.go hold its edge leases (QUOTA-M2-07, DESIGN-DETAILS 13.2.2); quota*.go the
// rest (QUOTA-M2-11). It sits beside sidecarhost, since server and model stay free of engine wiring and
// engines never import server.
package quotahost

import (
	"cmp"
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Edge leases (§20.6 as DESIGN-DETAILS 13.2.2 gives them). A lease is one process's hold on one (network,
// meter): one entry a process and key, its handles ref-counted, the last Close settling it
// (plan-QUOTA-small-readings reading 16). An entry owns reservation tokens lease/<lease_id>/<n> on the
// payer's balances, one a draw, each in the balance's escrow hash (quota_lease_reservation.go):
//
//	Spend    a compare-and-swap on available and an add to spent: no lock, no I/O, no allocation (§22.5's
//	         T1 row), false when available is short
//	draw     the payer's balances in draw order, one reserve a balance under a new token name, repeated
//	         under the same name when unanswered and abandoned after ReserveRetryCount tries; a draw's
//	         bytes join available only when its answer arrives
//	renew    the entry's goroutine, woken by Spend at the renew mark: the larger of a quarter of the
//	         last draw and RenewAhead of the spend rate measured over the last second; a draw is
//	         DrawByteCount (CommunityDrawByteCount on Community) or, when more, DrawAhead of that rate
//	         rounded up to a multiple of it, at most DrawMaxMultiple of it
//	expiry   each draw is spendable until ExpiryMargin before its token could lapse, counted from the send
//	         time of its last answered reserve, extend or carve, and extended every ExtendInterval; a draw
//	         past that, or whose extend answers 0, is dropped: its unspent bytes leave available and its
//	         spent bytes not yet planned into a batch are given up, counted in
//	         urnetwork_quota_lease_unbilled_bytes_total
//	close    the last handle's Close zeroes available and settles each draw, the highest rank first, to
//	         the bytes spent and not yet carved that are attributed or attributable to it, releasing one
//	         that keeps none; the entry stays until its last batch has carved what is left
//
// Spent bytes are attributed to draws only when a batch is planned (lease_batch.go), lowest rank first and
// within a rank the oldest draw, a rank being the balance's place in draw order (credit line last, then end
// time, then id), so what a lease spends lands on its balances in draw order and its unspent rest returns
// in reverse (E-a, E-b). The invariant the entry keeps: its draws' amounts sum to available, plus the bytes
// spent and not yet planned, plus the bytes planned and not yet carved.
//
// The operations that name one token (reserve, extend, carve, settle, release) are sent one at a time, each
// only when the one before was answered: every Redis call on a key's tokens holds that key's
// operationLock, which Spend never takes and which is never held with a stateLock. A carve sent and not
// answered is repeated as it was before any other operation names its tokens; one abandoned at its batch's
// horizon makes the token's next operation an extend, whose answer is the token's amount. No token carries
// a pending or recovery marker, and none of this runs Postgres on a host (I-12): balances come from the
// BalanceSource, reservations from the QuotaRateRedis the host passes. No log line or metric names a
// network, balance or lease (13.3's I-13 row): they carry the meter alone.

type LeaseSettings struct {
	// Bytes a draw reserves: §20.6's 64 MiB.
	DrawByteCount int64
	// On Community: §20.6's 16 MiB.
	CommunityDrawByteCount int64
	// A draw is at most this many times DrawByteCount (or CommunityDrawByteCount): 16, so 1 GiB, 256 MiB on
	// Community (QD-654).
	DrawMaxMultiple int
	// A fast lease draws this much of its rate when that is more than a draw: 4 s.
	DrawAhead time.Duration
	// A lease renews with this much of its rate left, when that is more than RenewFraction of its last draw:
	// 2 s, the time a renewal has at 1,000 Mbit/s.
	RenewAhead time.Duration
	// Or when this fraction of the last draw is left: §20.6's 75% used.
	RenewFraction float64
	// A lease token's lifetime, extended while the lease lives: 15 min (QD-653).
	TokenTtl time.Duration
	// How often a live draw is extended: 5 min.
	ExtendInterval time.Duration
	// A draw stops being spendable this long before its token could lapse: 30 s.
	ExpiryMargin time.Duration
	// A carved token's lifetime: model's 24 h.
	CarvedTokenTtl time.Duration
	// A balance reading younger than this is used as it is: 5 s.
	BalanceRefreshAge time.Duration
	// No reading older than this is used, from the moment it was asked for: 30 s.
	MaxCreditAge time.Duration
	// A floor's lifetime once lowered: model's 2 min, at least twice MaxCreditAge.
	FloorTtl time.Duration
	// Tries of one reservation under one token name before the name is abandoned: 3.
	ReserveRetryCount int
	// Bounds each Redis or balance call: 1 s, redisContractAdmissionTimeout.
	CallTimeout time.Duration
	// How often an entry's goroutine samples its rate, extends what is due and drops what expired: 1 s.
	TickInterval time.Duration
	// The wait after a renewal Redis did not answer, doubling to RenewRetryMaxTimeout: 1 s and 30 s.
	RenewRetryTimeout    time.Duration
	RenewRetryMaxTimeout time.Duration
	// How long a batch's plan is kept after its last ReportUsage, and a closed entry's spent bytes after
	// its close, before they are given up: past the batcher's 10 min retry horizon.
	PlanHorizon time.Duration
}

func DefaultLeaseSettings() *LeaseSettings {
	reservation := model.DefaultLeaseReservationSettings()
	return &LeaseSettings{
		DrawByteCount:          64 * 1024 * 1024,
		CommunityDrawByteCount: 16 * 1024 * 1024,
		DrawMaxMultiple:        16,
		DrawAhead:              4 * time.Second,
		RenewAhead:             2 * time.Second,
		RenewFraction:          0.25,
		TokenTtl:               15 * time.Minute,
		ExtendInterval:         5 * time.Minute,
		ExpiryMargin:           30 * time.Second,
		CarvedTokenTtl:         reservation.CarvedTokenTtl,
		BalanceRefreshAge:      5 * time.Second,
		MaxCreditAge:           30 * time.Second,
		FloorTtl:               reservation.FloorTtl,
		ReserveRetryCount:      3,
		CallTimeout:            time.Second,
		TickInterval:           time.Second,
		RenewRetryTimeout:      time.Second,
		RenewRetryMaxTimeout:   30 * time.Second,
		PlanHorizon:            12 * time.Minute,
	}
}

// What the design's bounds need of the settings: a floor outliving every reading (FloorTtl at least twice
// MaxCreditAge), a draw spendable for a while before its margin, and extends well inside a token's life.
func (self *LeaseSettings) Validate() error {
	switch {
	case self.DrawByteCount <= 0 || self.CommunityDrawByteCount <= 0 || self.DrawMaxMultiple < 1:
		return errors.New("quota lease: draws must be positive")
	case self.FloorTtl < 2*self.MaxCreditAge:
		return fmt.Errorf("quota lease: FloorTtl %s is under twice MaxCreditAge %s", self.FloorTtl, self.MaxCreditAge)
	case self.MaxCreditAge <= 0 || self.BalanceRefreshAge <= 0 || self.MaxCreditAge < self.BalanceRefreshAge:
		return errors.New("quota lease: a reading must be used for a positive time, refreshed before it is too old")
	case self.TokenTtl <= self.ExpiryMargin || self.ExpiryMargin <= 0:
		return errors.New("quota lease: a token must outlive its margin")
	case self.ExtendInterval <= 0 || self.TokenTtl-self.ExpiryMargin <= self.ExtendInterval:
		return errors.New("quota lease: a draw must be extended before it stops being spendable")
	case self.CallTimeout <= 0 || self.TickInterval <= 0 || self.RenewRetryTimeout <= 0 || self.ReserveRetryCount < 1:
		return errors.New("quota lease: timeouts and tries must be positive")
	case self.RenewFraction <= 0 || 1 <= self.RenewFraction:
		return errors.New("quota lease: RenewFraction must lie in (0, 1)")
	}
	return nil
}

var quotaLeaseUnbilledByteCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_quota_lease_unbilled_bytes_total",
	Help: "Lease bytes spent and never debited because their reservation was gone, lapsed or lost with Redis, before a carve held them (QD-653); alerts.",
}, []string{"meter"})

func init() {
	prometheus.MustRegister(quotaLeaseUnbilledByteCounter)
}

// The meters a lease spends (§20.6): tlsedge's, mxedge's and smtpedge's. evs's bytes draw at apply.
func isLeaseMeter(meter string) bool {
	switch meter {
	case sidecar.QuotaMeterTls, sidecar.QuotaMeterMx, sidecar.QuotaMeterSmtp:
		return true
	}
	return false
}

// A lease's (network, meter).
type quotaLeaseKey struct {
	networkId server.Id
	meter     string
}

// Every entry of one key: the open one and closed ones still owed a carve, with the lock that sends their
// tokens' operations one at a time.
type quotaLeaseLane struct {
	key           quotaLeaseKey
	operationLock sync.Mutex
	// under the set's stateLock
	open    *quotaLeaseEntry
	closing []*quotaLeaseEntry
}

// One process's leases. Safe for concurrent use.
type quotaLeaseSet struct {
	ctx      context.Context
	cancel   context.CancelFunc
	redis    model.QuotaRateRedis
	balances BalanceSource
	// the network's tier, from the Quota's 5 s cache: a draw's size on Community
	tier     func(ctx context.Context, networkId server.Id) (string, error)
	clock    sidecar.Clock
	settings *LeaseSettings

	stateLock    sync.Mutex
	lanes        map[quotaLeaseKey]*quotaLeaseLane
	balanceReads map[server.Id]*leaseBalanceRead
	plans        map[server.Id]*leaseBatchPlan
	// the entries' goroutines, for Drain
	entryGroup sync.WaitGroup
	// a test drives each entry's rounds itself (tick), so no goroutine races it. False in production.
	manualTicks bool
}

// A running lease set on redis and balances, reading a network's tier through tier (nil: every network
// draws the larger size), on clock (nil: real time), with settings (nil: the defaults). It runs until ctx
// ends or Drain.
func newQuotaLeaseSet(ctx context.Context, redis model.QuotaRateRedis, balances BalanceSource, tier func(ctx context.Context, networkId server.Id) (string, error), clock sidecar.Clock, settings *LeaseSettings) (*quotaLeaseSet, error) {
	if settings == nil {
		settings = DefaultLeaseSettings()
	}
	if err := settings.Validate(); err != nil {
		return nil, err
	}
	if redis == nil || balances == nil {
		return nil, errors.New("quota lease: a lease set needs Redis and a balance source")
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	cancelCtx, cancel := context.WithCancel(ctx)
	return &quotaLeaseSet{
		ctx:          cancelCtx,
		cancel:       cancel,
		redis:        redis,
		balances:     balances,
		tier:         tier,
		clock:        clock,
		settings:     settings,
		lanes:        map[quotaLeaseKey]*quotaLeaseLane{},
		balanceReads: map[server.Id]*leaseBalanceRead{},
		plans:        map[server.Id]*leaseBatchPlan{},
	}, nil
}

// One draw: one token on one balance.
type quotaLeaseDraw struct {
	balanceId server.Id
	// the balance's place in draw order: credit line last, then end time, then id
	isCredit bool
	endTime  time.Time
	// counts the lease's draws from 1, the token's n
	number int
	token  string
	// what the token holds as the host last learned
	amount int64
	// bytes of amount planned into batches and not yet carved
	pendingByteCount int64
	// bytes of amount the host gave up, an abandoned carve's that never ran: never spent or attributed,
	// returned at close
	deadByteCount int64
	// the send time of the token's last answered reserve, extend or carve that set its expiry
	refreshTime    time.Time
	spendableUntil time.Time
	// a carve sent and not answered, repeated before any other operation names the token
	unanswered *leaseBatchPart
	// an operation was abandoned: the token's next is an extend, whose answer is its amount
	needsExtend bool
}

// The token's bytes neither planned nor given up: available's and the spent unplanned bytes', between them.
func (self *quotaLeaseDraw) freeByteCount() int64 {
	return max(self.amount-self.pendingByteCount-self.deadByteCount, 0)
}

// The draw order of two draws, for attribution: the lowest rank first, within a rank the oldest draw.
func compareQuotaLeaseDraws(a *quotaLeaseDraw, b *quotaLeaseDraw) int {
	if a.isCredit != b.isCredit {
		if !a.isCredit {
			return -1
		}
		return 1
	}
	if c := a.endTime.Compare(b.endTime); c != 0 {
		return c
	}
	if c := a.balanceId.Cmp(b.balanceId); c != 0 {
		return c
	}
	return cmp.Compare(a.number, b.number)
}

// One process's hold on one key.
type quotaLeaseEntry struct {
	set     *quotaLeaseSet
	lane    *quotaLeaseLane
	key     quotaLeaseKey
	leaseId server.Id
	// a draw's size before the rate: DrawByteCount or, on Community, CommunityDrawByteCount
	baseDrawByteCount int64

	// what Spend may take
	available atomic.Int64
	// every byte Spend took, ever
	spent atomic.Int64
	// Spend wakes the renewal when available falls below it
	renewMark atomic.Int64
	// set when Spend first answered false, for MarkSpent
	exhausted atomic.Bool
	// Spend's non-blocking wake of the entry's goroutine
	wake chan struct{}

	// the rest under stateLock, never held across I/O and never taken by Spend
	stateLock sync.Mutex
	draws     []*quotaLeaseDraw
	drawCount int
	// bytes of spent attributed to a batch's plan, or given up
	carved      int64
	handleCount int
	closed      bool
	closeTime   time.Time
	settled     bool
	forgotten   bool
	// the last draw's size and the rate measured over the last tick
	lastDrawByteCount int64
	rateSampleTime    time.Time
	rateSampleSpent   int64
	byteRate          float64
	// the renewal's state: Redis answered and reserved nothing, so a run dry marks spent
	renewalRefused bool
	spentMarked    bool
	renewRetryTime time.Time
	renewRetryWait time.Duration
}

// The lane of key, made when absent. Under the set's stateLock.
func (self *quotaLeaseSet) laneWithLock(key quotaLeaseKey) *quotaLeaseLane {
	lane, ok := self.lanes[key]
	if !ok {
		lane = &quotaLeaseLane{key: key}
		self.lanes[key] = lane
	}
	return lane
}

// 16 random bytes: a lease id no other process draws.
func newQuotaLeaseId() server.Id {
	var leaseId server.Id
	// never fails: since Go 1.24 a broken system generator crashes the program instead
	cryptorand.Read(leaseId[:])
	return leaseId
}

// One handle on a process's lease. Spend and Close are safe for concurrent use.
type quotaLeaseHandle struct {
	entry  *quotaLeaseEntry
	closed atomic.Bool
}

// See the file header: the inline path's one call, with no lock, no I/O and no allocation.
func (self *quotaLeaseHandle) Spend(byteCount int64) bool {
	if byteCount < 0 || self.closed.Load() {
		return false
	}
	entry := self.entry
	for {
		available := entry.available.Load()
		if available < byteCount {
			if !entry.exhausted.Swap(true) {
				select {
				case entry.wake <- struct{}{}:
				default:
				}
			}
			return false
		}
		if entry.available.CompareAndSwap(available, available-byteCount) {
			entry.spent.Add(byteCount)
			if available-byteCount <= entry.renewMark.Load() {
				select {
				case entry.wake <- struct{}{}:
				default:
				}
			}
			return true
		}
	}
}

// The last handle's Close settles the entry (see the file header). Spend then refuses on this handle.
func (self *quotaLeaseHandle) Close() {
	if self.closed.Swap(true) {
		return
	}
	entry := self.entry
	last := false
	func() {
		entry.set.stateLock.Lock()
		defer entry.set.stateLock.Unlock()
		entry.stateLock.Lock()
		defer entry.stateLock.Unlock()
		entry.handleCount -= 1
		if 0 < entry.handleCount || entry.closed {
			return
		}
		last = true
		entry.closeWithLock()
	}()
	if last {
		entry.settle(entry.set.ctx)
	}
}

func (self *quotaLeaseHandle) String() string {
	return fmt.Sprintf("lease on %s", self.entry.key.meter)
}

// Marks the entry closed and zeroes available, moving it from its lane's open slot to closing. Under the
// set's stateLock and the entry's.
func (self *quotaLeaseEntry) closeWithLock() {
	self.closed = true
	self.closeTime = self.set.clock.Now()
	self.available.Swap(0)
	self.renewMark.Store(0)
	if self.lane.open == self {
		self.lane.open = nil
	}
	self.lane.closing = append(self.lane.closing, self)
}

// Opens a lease on networkId and meter for this process (§20.7's Quota.Lease): an open entry that holds
// bytes gives a new handle with no I/O; one that holds none makes one draw now; an absent one is opened with
// its first draw. A draw that reserves nothing while Redis answered is a refusal, limit_key data with
// upgrade, and marks the network spent; Redis or the balances unanswered is an error, with no refusal and
// no mark.
func (self *quotaLeaseSet) Lease(ctx context.Context, networkId connect.Id, meter string) (sidecar.QuotaLease, *sidecar.QuotaRefusal, error) {
	if networkId == (connect.Id{}) {
		return nil, nil, errors.New("quota lease: a zero network id")
	}
	if !isLeaseMeter(meter) {
		return nil, nil, fmt.Errorf("quota lease: no lease spends meter %q", meter)
	}
	key := quotaLeaseKey{networkId: server.Id(networkId), meter: meter}
	var entry *quotaLeaseEntry
	opened := false
	err := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.ctx.Err() != nil {
			return errors.New("quota lease: the lease set is drained")
		}
		lane := self.laneWithLock(key)
		entry = lane.open
		if entry == nil {
			entry = &quotaLeaseEntry{
				set:     self,
				lane:    lane,
				key:     key,
				leaseId: newQuotaLeaseId(),
				wake:    make(chan struct{}, 1),
			}
			lane.open = entry
			opened = true
		}
		func() {
			entry.stateLock.Lock()
			defer entry.stateLock.Unlock()
			entry.handleCount += 1
		}()
		return nil
	}()
	if err != nil {
		return nil, nil, err
	}
	if opened {
		baseDrawByteCount := self.settings.DrawByteCount
		if self.tier != nil {
			if tier, err := self.tier(ctx, key.networkId); err == nil && tier == string(model.NetworkTierFree) {
				baseDrawByteCount = self.settings.CommunityDrawByteCount
			}
		}
		func() {
			entry.stateLock.Lock()
			defer entry.stateLock.Unlock()
			entry.baseDrawByteCount = baseDrawByteCount
			entry.lastDrawByteCount = baseDrawByteCount
		}()
		if !self.manualTicks {
			self.entryGroup.Add(1)
			go func() {
				defer self.entryGroup.Done()
				entry.run()
			}()
		}
	}
	handle := &quotaLeaseHandle{entry: entry}
	if 0 < entry.available.Load() {
		return handle, nil, nil
	}
	drawn, answered, err := entry.drawNow(ctx, entry.drawSize(), true)
	if err == nil && drawn == 0 && 0 < entry.available.Load() {
		// another caller's draw landed while this one waited
		return handle, nil, nil
	}
	if err != nil || drawn == 0 {
		handle.Close()
		if err != nil {
			return nil, nil, err
		}
		if answered {
			refusal := &sidecar.QuotaRefusal{LimitKey: "data", Upgrade: true}
			markCtx, cancel := context.WithTimeout(ctx, self.settings.CallTimeout)
			defer cancel()
			if err := self.balances.MarkSpent(markCtx, key.networkId); err != nil {
				glog.Infof("[quota][lease]a %s refusal's spent mark not written: %s\n", key.meter, err)
			}
			return nil, refusal, nil
		}
		return nil, nil, errors.New("quota lease: the draw was not answered")
	}
	func() {
		entry.stateLock.Lock()
		defer entry.stateLock.Unlock()
		entry.spentMarked = false
	}()
	return handle, nil, nil
}

// The size of the entry's next draw: its base, or DrawAhead of its rate rounded up to a multiple of the
// base when that is more, at most DrawMaxMultiple bases.
func (self *quotaLeaseEntry) drawSize() int64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.drawSizeWithLock()
}

func (self *quotaLeaseEntry) drawSizeWithLock() int64 {
	base := max(self.baseDrawByteCount, 1)
	ahead := int64(self.byteRate * self.set.settings.DrawAhead.Seconds())
	size := base
	if base < ahead {
		size = (ahead + base - 1) / base * base
	}
	return min(size, base*int64(self.set.settings.DrawMaxMultiple))
}

// The renew mark after a draw of drawByteCount: the larger of RenewFraction of it and RenewAhead of the
// rate. Under the entry's stateLock.
func (self *quotaLeaseEntry) updateRenewMarkWithLock() {
	if self.closed {
		self.renewMark.Store(0)
		return
	}
	mark := int64(self.set.settings.RenewFraction * float64(self.lastDrawByteCount))
	mark = max(mark, int64(self.byteRate*self.set.settings.RenewAhead.Seconds()))
	self.renewMark.Store(mark)
}

// One draw of up to drawByteCount: the payer's balances in draw order, one reservation a balance, each
// granted against a reading no older than MaxCreditAge. answered is whether Redis answered every call it
// was asked: a draw that reserved nothing while answered means the payer has nothing left. onlyIfEmpty
// draws nothing when another draw filled available while this one waited for the lane.
func (self *quotaLeaseEntry) drawNow(ctx context.Context, drawByteCount int64, onlyIfEmpty bool) (drawn int64, answered bool, returnErr error) {
	set := self.set
	settings := set.settings
	balances, askTime, err := set.readBalances(ctx, self.key.networkId)
	if err != nil {
		return 0, false, err
	}
	self.lane.operationLock.Lock()
	defer self.lane.operationLock.Unlock()
	closed := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		closed = self.closed
	}()
	if closed || (onlyIfEmpty && 0 < self.available.Load()) {
		return 0, true, nil
	}
	answered = true
	for _, balance := range balances {
		if drawByteCount <= drawn {
			break
		}
		if balance.CreditByteCount <= 0 {
			continue
		}
		if settings.MaxCreditAge <= set.clock.Now().Sub(askTime) {
			// the reading aged past its use: refresh it before the next balance
			fresh, freshAskTime, err := set.readBalances(ctx, self.key.networkId)
			if err != nil {
				return drawn, false, err
			}
			index := slices.IndexFunc(fresh, func(other *LeaseBalance) bool { return other.BalanceId == balance.BalanceId })
			if index < 0 {
				continue
			}
			balance, askTime = fresh[index], freshAskTime
		}
		var number int
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.drawCount += 1
			number = self.drawCount
		}()
		token := model.QuotaLeaseToken(self.leaseId, number)
		amount := int64(0)
		var sendTime time.Time
		reserved := false
		for range settings.ReserveRetryCount {
			sendTime = set.clock.Now()
			callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
			amount, err = model.ReserveLease(callCtx, set.redis, balance.BalanceId, token, balance.CreditByteCount, drawByteCount-drawn, settings.TokenTtl, settings.FloorTtl)
			cancel()
			if err == nil {
				reserved = true
				break
			}
		}
		if !reserved {
			// the name is abandoned: its token, if any, lapses at its lifetime with no debit
			if glog.V(1) {
				glog.Infof("[quota][lease]a %s reservation went unanswered: %s\n", self.key.meter, err)
			}
			return drawn, false, err
		}
		if amount == 0 {
			continue
		}
		draw := &quotaLeaseDraw{
			balanceId:      balance.BalanceId,
			isCredit:       balance.IsCredit,
			endTime:        balance.EndTime,
			number:         number,
			token:          token,
			amount:         amount,
			refreshTime:    sendTime,
			spendableUntil: sendTime.Add(settings.TokenTtl - settings.ExpiryMargin),
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.draws = append(self.draws, draw)
			// its bytes join available only now, with its answer
			if !self.closed {
				self.available.Add(amount)
			}
			self.lastDrawByteCount = drawByteCount
			self.updateRenewMarkWithLock()
		}()
		drawn += amount
	}
	if 0 < drawn {
		self.exhausted.Store(false)
	}
	return drawn, answered, nil
}

// The entry's goroutine (see the file header): renewal, rate, extends, expiry, a run dry's spent mark, and
// leaving the set once closed and carved.
func (self *quotaLeaseEntry) run() {
	set := self.set
	for {
		select {
		case <-set.ctx.Done():
			return
		case <-self.wake:
		case <-set.clock.After(set.settings.TickInterval):
		}
		if self.tick(set.ctx) {
			return
		}
	}
}

// One round of the entry's goroutine. It answers whether the entry left the set.
func (self *quotaLeaseEntry) tick(ctx context.Context) (forgotten bool) {
	set := self.set
	settings := set.settings
	now := set.clock.Now()
	var renew, markSpent bool
	var drawByteCount int64
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		// the rate over the last TickInterval: a Spend's wake sooner than that reads the rate it has, so a
		// burst a few ms long never reads as a rate it cannot keep
		if self.rateSampleTime.IsZero() {
			self.rateSampleTime = now
			self.rateSampleSpent = self.spent.Load()
		} else if elapsed := now.Sub(self.rateSampleTime); settings.TickInterval <= elapsed {
			self.byteRate = float64(self.spent.Load()-self.rateSampleSpent) / elapsed.Seconds()
			self.rateSampleTime = now
			self.rateSampleSpent = self.spent.Load()
		}
		self.updateRenewMarkWithLock()
		if !self.closed && self.available.Load() <= self.renewMark.Load() && !now.Before(self.renewRetryTime) {
			renew = true
			drawByteCount = self.drawSizeWithLock()
		}
		markSpent = !self.closed && self.exhausted.Load() && self.renewalRefused && !self.spentMarked
	}()
	if renew {
		drawn, answered, err := self.drawNow(ctx, drawByteCount, false)
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			switch {
			case err != nil || !answered:
				// Redis or the balances did not answer: retry with backoff, mark nothing
				self.renewRetryWait = min(max(self.renewRetryWait*2, settings.RenewRetryTimeout), max(settings.RenewRetryMaxTimeout, settings.RenewRetryTimeout))
				self.renewRetryTime = now.Add(self.renewRetryWait)
			case drawn == 0:
				// answered with nothing: the lease runs dry, and a Spend refused marks the network spent
				self.renewalRefused = true
				self.renewRetryWait = settings.RenewRetryTimeout
				self.renewRetryTime = now.Add(self.renewRetryWait)
			default:
				self.renewalRefused = false
				self.spentMarked = false
				self.renewRetryWait = 0
				self.renewRetryTime = time.Time{}
			}
			markSpent = !self.closed && self.exhausted.Load() && self.renewalRefused && !self.spentMarked
		}()
	}
	if markSpent {
		callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
		err := set.balances.MarkSpent(callCtx, self.key.networkId)
		cancel()
		if err == nil {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				self.spentMarked = true
			}()
		}
	}
	self.extendAndExpire(ctx, now)
	return self.forgetIfDone(now)
}

// Extends each draw due its extend, and drops each that expired or whose extend answered 0.
func (self *quotaLeaseEntry) extendAndExpire(ctx context.Context, now time.Time) {
	settings := self.set.settings
	var dues []*quotaLeaseDraw
	var expireds []*quotaLeaseDraw
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, draw := range self.draws {
			switch {
			case !now.Before(draw.spendableUntil):
				expireds = append(expireds, draw)
			case settings.ExtendInterval <= now.Sub(draw.refreshTime) || draw.needsExtend:
				dues = append(dues, draw)
			}
		}
	}()
	for _, draw := range expireds {
		self.dropDraw(draw)
	}
	if len(dues) == 0 {
		return
	}
	self.lane.operationLock.Lock()
	defer self.lane.operationLock.Unlock()
	for _, draw := range dues {
		if !self.resolveUnansweredWithOperationLock(ctx, draw) {
			continue
		}
		sendTime := self.set.clock.Now()
		callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
		amount, err := model.ExtendLease(callCtx, self.set.redis, draw.balanceId, draw.token, settings.TokenTtl)
		cancel()
		if err != nil {
			// unanswered: the token's expiry is unknown, so the draw is spendable as before and asks again
			draw.needsExtend = true
			continue
		}
		if amount == 0 {
			self.dropDraw(draw)
			continue
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// the answer is the token's amount, settling a carve whose answer never came
			self.learnAmountWithLock(draw, amount)
			draw.needsExtend = false
			draw.refreshTime = sendTime
			draw.spendableUntil = sendTime.Add(settings.TokenTtl - settings.ExpiryMargin)
		}()
	}
}

// What the host learns a token holds, from an extend. More than it knew: an abandoned carve never ran, and
// the bytes it would have moved are dead, given up and returned at close. Less: bytes left the token unseen,
// taken from the dead bytes first, then the free, then the planned. Under the entry's stateLock.
func (self *quotaLeaseEntry) learnAmountWithLock(draw *quotaLeaseDraw, amount int64) {
	switch {
	case draw.amount < amount:
		draw.deadByteCount += amount - draw.amount
		draw.amount = amount
	case amount < draw.amount:
		lost := draw.amount - amount
		free := draw.freeByteCount()
		fromDead := min(lost, draw.deadByteCount)
		draw.deadByteCount -= fromDead
		lost -= fromDead
		fromFree := min(lost, free)
		self.removeFreeBytesWithLock(fromFree)
		lost -= fromFree
		draw.pendingByteCount = max(0, draw.pendingByteCount-lost)
		draw.amount = amount
	}
}

// Takes byteCount out of the lease's free bytes: from available while it holds them, the rest from the
// bytes spent and not yet planned, which are given up. Under the entry's stateLock.
func (self *quotaLeaseEntry) removeFreeBytesWithLock(byteCount int64) {
	if byteCount <= 0 {
		return
	}
	for {
		available := self.available.Load()
		take := min(available, byteCount)
		if self.available.CompareAndSwap(available, available-take) {
			if rest := byteCount - take; 0 < rest {
				self.carved += rest
				quotaLeaseUnbilledByteCounter.WithLabelValues(self.key.meter).Add(float64(rest))
			}
			return
		}
	}
}

// Drops a draw whose token is gone or about to lapse: its unspent bytes leave available, and the spent bytes
// attributable to it and not yet planned are given up and counted unbilled. Bytes already planned from it
// stay with their plan's part, whose carve answers what Redis holds.
func (self *quotaLeaseEntry) dropDraw(draw *quotaLeaseDraw) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	index := slices.Index(self.draws, draw)
	if index < 0 {
		return
	}
	// the unplanned spent bytes, attributed in draw order, the dropped draw's share given up
	unplanned := self.spent.Load() - self.carved
	free := int64(0)
	for _, other := range self.sortedDrawsWithLock() {
		otherFree := other.freeByteCount()
		absorbed := min(unplanned, otherFree)
		unplanned -= absorbed
		if other == draw {
			free = otherFree
			if 0 < absorbed {
				self.carved += absorbed
				quotaLeaseUnbilledByteCounter.WithLabelValues(self.key.meter).Add(float64(absorbed))
			}
			self.removeFreeBytesWithLock(free - absorbed)
			break
		}
	}
	self.draws = slices.Delete(self.draws, index, index+1)
	if glog.V(1) {
		glog.Infof("[quota][lease]a %s draw dropped with %d free bytes\n", self.key.meter, free)
	}
}

// The draws in attribution order. Under the entry's stateLock.
func (self *quotaLeaseEntry) sortedDrawsWithLock() []*quotaLeaseDraw {
	draws := slices.Clone(self.draws)
	slices.SortFunc(draws, compareQuotaLeaseDraws)
	return draws
}

// Repeats the carve sent and not answered on draw before another operation names its token. False: it went
// unanswered again, so nothing else may be sent on the token now. Holds the lane's operationLock.
func (self *quotaLeaseEntry) resolveUnansweredWithOperationLock(ctx context.Context, draw *quotaLeaseDraw) bool {
	var part *leaseBatchPart
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		part = draw.unanswered
	}()
	if part == nil {
		return true
	}
	return self.set.carvePartWithOperationLock(ctx, part) == nil
}

// The close's settles: each draw, the highest rank first, shrunk to the spent bytes not yet carved that are
// attributed or attributable to it, or released when that is none, so the unspent rest returns in reverse
// draw order at once.
func (self *quotaLeaseEntry) settle(ctx context.Context) {
	settings := self.set.settings
	self.lane.operationLock.Lock()
	defer self.lane.operationLock.Unlock()
	type settleStep struct {
		draw *quotaLeaseDraw
		keep int64
	}
	var steps []settleStep
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.settled {
			return
		}
		self.settled = true
		unplanned := self.spent.Load() - self.carved
		sorted := self.sortedDrawsWithLock()
		keeps := make([]int64, len(sorted))
		for index, draw := range sorted {
			absorbed := min(unplanned, draw.freeByteCount())
			unplanned -= absorbed
			// a dead byte is not kept
			keeps[index] = draw.pendingByteCount + absorbed
		}
		if 0 < unplanned {
			// spent bytes no live draw holds: given up
			self.carved += unplanned
			quotaLeaseUnbilledByteCounter.WithLabelValues(self.key.meter).Add(float64(unplanned))
		}
		for index := len(sorted) - 1; 0 <= index; index -= 1 {
			steps = append(steps, settleStep{draw: sorted[index], keep: keeps[index]})
		}
	}()
	for _, step := range steps {
		draw := step.draw
		if !self.resolveUnansweredWithOperationLock(ctx, draw) {
			continue
		}
		if step.keep == draw.amount {
			continue
		}
		callCtx, cancel := context.WithTimeout(ctx, settings.CallTimeout)
		var err error
		var amount int64
		if step.keep == 0 {
			err = model.ReleaseLease(callCtx, self.set.redis, draw.balanceId, draw.token, settings.TokenTtl)
		} else {
			amount, err = model.SettleLease(callCtx, self.set.redis, draw.balanceId, draw.token, step.keep, settings.TokenTtl)
		}
		cancel()
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			switch {
			case err != nil:
				// unanswered: the token may or may not have shrunk, so its next operation is an extend
				draw.needsExtend = true
			case step.keep == 0:
				draw.amount = 0
				draw.deadByteCount = 0
				if index := slices.Index(self.draws, draw); 0 <= index {
					self.draws = slices.Delete(self.draws, index, index+1)
				}
			default:
				draw.amount = amount
				draw.deadByteCount = 0
			}
		}()
	}
}

// Leaves the set once the entry is closed and owes no carve: every spent byte planned, every plan's part
// carved or dropped. A closed entry still owed bytes past PlanHorizon gives them up and releases its tokens.
func (self *quotaLeaseEntry) forgetIfDone(now time.Time) bool {
	set := self.set
	var stale []*quotaLeaseDraw
	done := false
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.closed || !self.settled {
			return
		}
		owed := self.spent.Load() - self.carved
		pending := int64(0)
		for _, draw := range self.draws {
			pending += draw.pendingByteCount
		}
		if owed == 0 && pending == 0 {
			done = true
			stale = slices.Clone(self.draws)
			return
		}
		if set.settings.PlanHorizon <= now.Sub(self.closeTime) && pending == 0 {
			self.carved += owed
			quotaLeaseUnbilledByteCounter.WithLabelValues(self.key.meter).Add(float64(owed))
			done = true
			stale = slices.Clone(self.draws)
		}
	}()
	if !done {
		return false
	}
	// a draw left holds only bytes no batch will carve: return them
	if 0 < len(stale) {
		self.lane.operationLock.Lock()
		for _, draw := range stale {
			callCtx, cancel := context.WithTimeout(set.ctx, set.settings.CallTimeout)
			_ = model.ReleaseLease(callCtx, set.redis, draw.balanceId, draw.token, set.settings.TokenTtl)
			cancel()
		}
		self.lane.operationLock.Unlock()
	}
	set.stateLock.Lock()
	defer set.stateLock.Unlock()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.forgotten = true
	self.draws = nil
	self.lane.closing = slices.DeleteFunc(self.lane.closing, func(other *quotaLeaseEntry) bool { return other == self })
	if self.lane.open == nil && len(self.lane.closing) == 0 {
		delete(set.lanes, self.lane.key)
	}
	set.forgetOldBalanceReadsWithLock(now)
	return true
}

// Closes every entry, as the last handle's Close would, settles them, and stops the entries' goroutines.
// The engine's batcher drains first, so the batches it sends carve what was spent.
func (self *quotaLeaseSet) Drain(ctx context.Context) {
	var entries []*quotaLeaseEntry
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, lane := range self.lanes {
			if lane.open != nil {
				entry := lane.open
				func() {
					entry.stateLock.Lock()
					defer entry.stateLock.Unlock()
					entry.closeWithLock()
				}()
			}
			entries = append(entries, lane.closing...)
		}
	}()
	for _, entry := range entries {
		entry.settle(ctx)
	}
	self.cancel()
	self.entryGroup.Wait()
}

// Runs one round of every entry's goroutine work, open and closing, as their wakes or ticks would: the
// lever of a set whose rounds a test drives (manualTicks).
func (self *quotaLeaseSet) tickAll(ctx context.Context) {
	var entries []*quotaLeaseEntry
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, lane := range self.lanes {
			if lane.open != nil {
				entries = append(entries, lane.open)
			}
			entries = append(entries, lane.closing...)
		}
	}()
	for _, entry := range entries {
		entry.tick(ctx)
	}
}
