// A hosted owner bounds the derived-client registrations its devices run at
// once. One registration is a database transaction that writes the child client
// and its credential state. A proxy process hosts more than a thousand devices
// behind one small connection pool, so without a bound a burst of window
// re-dials (a deploy, a provider outage, a slow database) queued enough mints
// to exhaust the pool for every other control operation on the host, and each
// timed-out mint was retried. Past the bound a registration is refused before
// any credential or database read, as a rate limit: the SDK answers it with its
// jittered rate-limit backoff instead of retrying.
//
// Two refinements keep the bound from turning away a new device's first exits
// (server proxy/P90SPEED.md §3.3). From 2026-11-14 the proxy's two slots refused
// 30-60% of registrations under sweep load, connect answered each refusal with
// its 5 s doubling backoff, and one, two or three refusals cost a new proxy's
// first CONNECT about 4, 11 or 27 s in WaitForRoute.
//   - A registration that finds the bound full waits for a slot, first come
//     first served, for at most MaxWait and with at most MaxWaiterCount others;
//     past either it is refused as before. A waiter has read no credential and
//     holds no database work, so the wait spends only the slot time that a
//     refusal and its retry would have left idle.
//   - A device that cannot route a new flow yet (its window has added no exit)
//     may take one of ReservedConcurrentCount further slots when the shared
//     ones are full, for at most ReservedCountPerDevice registrations in its
//     life. A device past that registers through the shared slots, so the
//     reserved slots cannot carry a churning device's replacements (the
//     2026-10-22 loop): their registrations are bounded by the devices opened,
//     not by how often a device's candidates fail.
package localclient

import (
	"context"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/connect"
)

var mintAdmissionRefusals = prometheus.NewCounter(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "mint_admission_refusals_total",
		Help:      "Hosted derived-client registrations refused because the owner's concurrent registration bound was full, at once or after waiting for a slot",
	},
)

var mintAdmissionAdmissions = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "mint_admission_admissions_total",
		Help:      "Hosted derived-client registrations the owner's concurrent registration bound admitted, by lane: shared, or reserved for a device that cannot route yet",
	},
	[]string{"lane"},
)

var mintAdmissionWaits = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "mint_admission_waits_total",
		Help:      "Hosted derived-client registrations that found the owner's concurrent registration bound full and waited for a slot, by how the wait ended: admitted, refused when it ran out, or canceled by the caller",
	},
	[]string{"outcome"},
)

var mintAdmissionWaitSeconds = prometheus.NewHistogram(
	prometheus.HistogramOpts{
		Namespace: "urnetwork",
		Subsystem: "hosted",
		Name:      "mint_admission_wait_seconds",
		Help:      "How long hosted derived-client registrations that waited for a slot of the owner's concurrent registration bound waited before they were admitted",
		Buckets:   []float64{0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1},
	},
)

var (
	mintAdmissionSharedAdmissions   = mintAdmissionAdmissions.WithLabelValues("shared")
	mintAdmissionReservedAdmissions = mintAdmissionAdmissions.WithLabelValues("reserved")
	mintAdmissionWaitsAdmitted      = mintAdmissionWaits.WithLabelValues("admitted")
	mintAdmissionWaitsRefused       = mintAdmissionWaits.WithLabelValues("refused")
	mintAdmissionWaitsCanceled      = mintAdmissionWaits.WithLabelValues("canceled")
)

func init() {
	prometheus.MustRegister(mintAdmissionRefusals)
	prometheus.MustRegister(mintAdmissionAdmissions)
	prometheus.MustRegister(mintAdmissionWaits)
	prometheus.MustRegister(mintAdmissionWaitSeconds)
}

// A registration bound's slots, its reserved lane and its wait.
type MintAdmissionSettings struct {
	// Registrations of any device in flight at once. Nonpositive builds no
	// admission, which admits everything.
	MaxConcurrentCount int
	// Further registrations in flight at once, taken only when the shared
	// slots are full and only by a device that cannot route yet. Nonpositive
	// reserves none.
	ReservedConcurrentCount int
	// The reserved registrations one device takes in its life.
	ReservedCountPerDevice int
	// The reserved registrations one device of a network on the free plan
	// takes in its life, in place of ReservedCountPerDevice: a device whose
	// parent credential carries no pro claim. Nonpositive reserves none for
	// such a device.
	FreeReservedCountPerDevice int
	// Registrations waiting for a slot at once. Past it a registration that
	// finds the bound full is refused at once. Nonpositive waits none.
	MaxWaiterCount int
	// The longest a registration waits for a slot before it is refused.
	// Nonpositive waits none.
	MaxWait time.Duration
}

// The default settings around `maxConcurrentCount` shared slots. The values
// come from the measurements on Main of 2026-11-18 (server proxy/P90SPEED.md
// §6 #2): the registrations offered to one proxy process's two slots came to
// 1.4-2.0 Erlang, with a mean hold of 0.05-0.12 s (Erlang figures at 0.1 s).
//   - A 1 s wait is about ten holds. At 1.5 Erlang 64% of registrations find
//     both slots busy and wait 0.2 s on average, and 0.4% wait past 1 s, where
//     the instant refusal turned away 31-40%. Past 2 Erlang no wait helps, and
//     refusals return at the bound.
//   - 32 waiters is more than two slots drain in 1 s at that hold (about 20),
//     so the waiter bound refuses only a pile-up.
//   - A device that cannot route yet registers about five window clients
//     before its first exit answers: 0.15-0.45 Erlang per process at the
//     day's 0.5-0.75 device builds a second. Two reserved slots alone admit
//     those at once 93-99% of the time (Erlang B); they come after the shared
//     slots and before the wait.
//   - Six is a hosted device's whole first window: five quality exits and one
//     speed exit. A free device's window is smaller (see
//     defaultFreeReservedCountPerDevice).
func DefaultMintAdmissionSettings(maxConcurrentCount int) MintAdmissionSettings {
	return MintAdmissionSettings{
		MaxConcurrentCount:         maxConcurrentCount,
		ReservedConcurrentCount:    2,
		ReservedCountPerDevice:     6,
		FreeReservedCountPerDevice: defaultFreeReservedCountPerDevice,
		MaxWaiterCount:             32,
		MaxWait:                    1 * time.Second,
	}
}

// The whole first window of a free device: two quality exits and one speed
// exit (proxy ProxyDeviceSettings.FreeQualityWindowSize and
// FreeSpeedWindowSize; owner, 2026-11-24). It halves the reserved lane's
// registrations per free build.
const defaultFreeReservedCountPerDevice = 3

// The slot a registration holds.
type mintLane int

const (
	// held from no bound: a nil admission admits everything
	mintLaneUnbounded mintLane = iota
	mintLaneShared
	mintLaneReserved
)

// The concurrent registration bound shared by the authorities of one owner. A
// nil admission admits everything. Safe for concurrent use.
type MintAdmission struct {
	settings MintAdmissionSettings

	stateLock     sync.Mutex
	sharedInUse   int
	reservedInUse int
	// Registrations waiting for a slot, oldest first. A released shared slot
	// goes to the oldest waiter, and a released reserved slot to the oldest
	// waiter that may take one, so a shared slot is free only while nobody
	// waits.
	waiters []*mintWaiter
}

type mintWaiter struct {
	// the reserved registrations left to the waiter's device; nil when the
	// device could route as the waiter arrived
	reserved *atomic.Int64
	// the slot handed to the waiter, sent once under the admission's lock
	granted chan mintLane
}

// Nonpositive `maxConcurrentCount` returns nil, which admits everything. The
// reserved lane and the wait take their defaults.
func NewMintAdmission(maxConcurrentCount int) *MintAdmission {
	return NewMintAdmissionWithSettings(DefaultMintAdmissionSettings(maxConcurrentCount))
}

// Nonpositive `settings.MaxConcurrentCount` returns nil, which admits
// everything.
func NewMintAdmissionWithSettings(settings MintAdmissionSettings) *MintAdmission {
	if settings.MaxConcurrentCount <= 0 {
		return nil
	}
	return &MintAdmission{
		settings: settings,
	}
}

// The reserved registrations a new authority's device takes in its life, by
// its parent credential's pro claim.
func (self *MintAdmission) reservedCountPerDevice(pro bool) int64 {
	if self == nil || self.settings.ReservedConcurrentCount <= 0 {
		return 0
	}
	if !pro {
		return int64(max(0, self.settings.FreeReservedCountPerDevice))
	}
	return int64(max(0, self.settings.ReservedCountPerDevice))
}

// Admits one registration, waiting for a slot while the bound is full. A
// device whose `noRoute` reports true may take a reserved slot while
// `reserved`, its own remaining count, lasts. `noRoute` is read only when the
// shared slots are full, and never under the admission's lock. A nil error
// must be paired with release of the returned lane. A refusal reads as a rate
// limit; a caller whose ctx ends while it waits gets ctx's error.
func (self *MintAdmission) acquire(ctx context.Context, noRoute func() bool, reserved *atomic.Int64) (mintLane, error) {
	if self == nil {
		return mintLaneUnbounded, nil
	}
	if self.tryShared() {
		mintAdmissionSharedAdmissions.Inc()
		return mintLaneShared, nil
	}
	var claim *atomic.Int64
	if reserved != nil && 0 < reserved.Load() && noRoute != nil && noRoute() {
		claim = reserved
	}
	lane, waiter := self.admitOrWait(claim)
	switch {
	case lane != mintLaneUnbounded:
		countMintAdmission(lane)
		return lane, nil
	case waiter == nil:
		mintAdmissionRefusals.Inc()
		return mintLaneUnbounded, mintAdmissionRefusedError()
	}

	waitStart := time.Now()
	waitTimer := time.NewTimer(self.settings.MaxWait)
	defer waitTimer.Stop()
	select {
	case lane = <-waiter.granted:
	case <-waitTimer.C:
	case <-ctx.Done():
	}
	if lane == mintLaneUnbounded {
		// a slot handed over as the wait ended is kept
		lane = self.leave(waiter)
	}
	if lane != mintLaneUnbounded {
		mintAdmissionWaitsAdmitted.Inc()
		mintAdmissionWaitSeconds.Observe(time.Since(waitStart).Seconds())
		countMintAdmission(lane)
		return lane, nil
	}
	if err := ctx.Err(); err != nil {
		mintAdmissionWaitsCanceled.Inc()
		return mintLaneUnbounded, err
	}
	mintAdmissionWaitsRefused.Inc()
	mintAdmissionRefusals.Inc()
	return mintLaneUnbounded, mintAdmissionRefusedError()
}

func countMintAdmission(lane mintLane) {
	switch lane {
	case mintLaneShared:
		mintAdmissionSharedAdmissions.Inc()
	case mintLaneReserved:
		mintAdmissionReservedAdmissions.Inc()
	}
}

// Takes a free shared slot. Free shared slots mean nobody waits, so this
// never passes a waiter.
func (self *MintAdmission) tryShared() bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.sharedInUse < self.settings.MaxConcurrentCount {
		self.sharedInUse += 1
		return true
	}
	return false
}

// Takes a free shared slot, or a free reserved slot for `claim`, or queues a
// waiter while the wait and the waiter bound allow. Neither a lane nor a
// waiter is a refusal.
func (self *MintAdmission) admitOrWait(claim *atomic.Int64) (mintLane, *mintWaiter) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.sharedInUse < self.settings.MaxConcurrentCount {
		self.sharedInUse += 1
		return mintLaneShared, nil
	}
	if claim != nil && self.reservedInUse < self.settings.ReservedConcurrentCount && takeReservedMint(claim) {
		self.reservedInUse += 1
		return mintLaneReserved, nil
	}
	if self.settings.MaxWait <= 0 || self.settings.MaxWaiterCount <= len(self.waiters) {
		return mintLaneUnbounded, nil
	}
	waiter := &mintWaiter{
		reserved: claim,
		granted:  make(chan mintLane, 1),
	}
	self.waiters = append(self.waiters, waiter)
	return mintLaneUnbounded, waiter
}

// Withdraws a waiter whose wait ended and returns the slot handed to it before
// the withdrawal, if any.
func (self *MintAdmission) leave(waiter *mintWaiter) mintLane {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	select {
	case lane := <-waiter.granted:
		return lane
	default:
	}
	if i := slices.Index(self.waiters, waiter); 0 <= i {
		self.waiters = slices.Delete(self.waiters, i, i+1)
	}
	return mintLaneUnbounded
}

// Returns a slot, handing it to the oldest waiter that may take it.
func (self *MintAdmission) release(lane mintLane) {
	if self == nil || lane == mintLaneUnbounded {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch lane {
	case mintLaneShared:
		if 0 < len(self.waiters) {
			waiter := self.waiters[0]
			self.waiters = slices.Delete(self.waiters, 0, 1)
			waiter.granted <- mintLaneShared
			return
		}
		self.sharedInUse -= 1
	case mintLaneReserved:
		for i, waiter := range self.waiters {
			if waiter.reserved != nil && takeReservedMint(waiter.reserved) {
				self.waiters = slices.Delete(self.waiters, i, i+1)
				waiter.granted <- mintLaneReserved
				return
			}
		}
		self.reservedInUse -= 1
	}
}

// Spends one of a device's reserved registrations, if any are left.
func takeReservedMint(reserved *atomic.Int64) bool {
	for {
		left := reserved.Load()
		if left <= 0 {
			return false
		}
		if reserved.CompareAndSwap(left, left-1) {
			return true
		}
	}
}

// The refusal reads as a rate limit to the SDK's window classifier, which
// backs off with jitter, and says its bound is busy: a slot frees within one
// registration's hold, so a window with no exit asks again on its short
// cadence (connect ClientRegistrationBusyError, which this matches by its
// method alone). No mint was attempted, so nothing needs retirement.
func mintAdmissionRefusedError() error {
	return &mintAdmissionRefusal{
		HttpStatusError: &connect.HttpStatusError{
			StatusCode: http.StatusTooManyRequests,
			Status:     "429 Too Many Requests",
			Body:       []byte("hosted client registration is at its concurrent bound"),
		},
	}
}

// A full bound's refusal: the 429 itself, through Unwrap, and its busy mark.
type mintAdmissionRefusal struct {
	*connect.HttpStatusError
}

func (self *mintAdmissionRefusal) ClientRegistrationBusy() bool {
	return true
}

func (self *mintAdmissionRefusal) Unwrap() error {
	return self.HttpStatusError
}

// The owner's bound this authority registers through, for owner wiring tests.
func (self *Authority) Testing_MintAdmission() *MintAdmission {
	return self.mintAdmission
}

// The device's route probe (SetRoutable), nil when unset, for owner wiring
// tests.
func (self *Authority) Testing_Routable() func() bool {
	if routable := self.routable.Load(); routable != nil {
		return *routable
	}
	return nil
}
