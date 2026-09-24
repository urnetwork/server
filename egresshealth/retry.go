package egresshealth

import (
	"context"
	"fmt"
	"math/rand"
	"sync"
	"time"
)

// One run's retry machinery: the capped exponential retry delay, the state a
// run shares between its loads, every load's retry chain, and the record of
// what the warm-ups saw.

// Caps one drawn retry delay at this many means. It is
// part of the spacing's definition (GEOMAP §11.3: exponential, capped at three
// times the mean), not a tunable: the mean is the setting, and the cap is what
// keeps one unlucky draw from holding a load, and the tunnel under it, open
// for an hour.
const retryDelayCapFactor = 3

// Turns one draw of a unit-mean exponential (rand.ExpFloat64) into
// a retry spacing with the given mean, capped at retryDelayCapFactor times it.
//
// Exponential because it is memoryless: from outside, the next request to a
// site is as likely at any moment as at any other, which is what a person
// coming back to a page looks like and what a fixed back-off does not. The cap
// is applied to the draw before it is scaled: ExpFloat64 has an unbounded
// tail, and scaling a large draw into a Duration first would overflow int64
// and come back negative.
//
// The capped distribution's mean is mean * (1 - e^-3), about 0.95 * mean; the
// cap is reached by e^-3, about 5 %, of draws. TestRetryDelayMeanAndCap holds
// both.
func retryDelay(unitDraw float64, mean time.Duration) time.Duration {
	if mean <= 0 || unitDraw <= 0 {
		return 0
	}
	if retryDelayCapFactor <= unitDraw {
		return retryDelayCapFactor * mean
	}
	return time.Duration(unitDraw * float64(mean))
}

// Serializes draws from one generator. Every load's retry chain is
// its own goroutine, and math/rand.Rand is not safe for concurrent use; one
// shared generator behind a lock keeps a caller-supplied seed meaningful,
// where a generator per goroutine would need seeds of its own.
type lockedRand struct {
	stateLock sync.Mutex
	r         *rand.Rand
}

// Draws one unit-mean exponential.
func (self *lockedRand) expFloat64() float64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.r.ExpFloat64()
}

// One run's shared state: every load's retry chain reads it, and none
// writes it except through the slots, the path tracker and the exit record,
// each of which is safe for that.
type run struct {
	path              *pathTracker
	profile           RequestProfile
	perRequestTimeout time.Duration
	loadAttempts      int
	retryMeanInterval time.Duration
	// When the run's budget ends, on the Options clock. The
	// context carries the same deadline on the wall clock; this copy is what
	// lets a retry be fitted inside it without mixing the two clocks, so a
	// test's injected clock and sleeper stay self-consistent.
	deadline time.Time
	// Bounds simultaneous fetches, not simultaneous loads. A load
	// waiting out its retry spacing holds no slot, which is what lets the
	// loads of a run interleave: a site that keeps failing does not keep the
	// rest of the sample queued behind its ten-minute chain.
	slots chan struct{}
	rng   *lockedRand
	exit  *exitRecord
	opts  Options
}

// Resolves opts into one run's shared state: its path tracker, whose re-opened
// tunnels are warmed up by this run, its budget's deadline on the Options
// clock, and concurrency slots.
func newRun(path Path, opts Options, concurrency int, budget time.Duration, rng *rand.Rand, exit *exitRecord) *run {
	r := &run{
		path:              &pathTracker{path: path, reopenLimit: opts.tunnelRecreateAttempts()},
		profile:           opts.profile(),
		perRequestTimeout: opts.perRequestTimeout(),
		loadAttempts:      opts.loadAttempts(),
		retryMeanInterval: opts.loadRetryMeanInterval(),
		deadline:          opts.now().Add(budget),
		slots:             make(chan struct{}, max(1, concurrency)),
		rng:               &lockedRand{r: rng},
		exit:              exit,
		opts:              opts,
	}
	r.path.warm = r.warmClient
	return r
}

// Derives a context that also ends when signal does -- the tunnel under
// an attempt being lost -- so the fetch in flight stops at once instead of
// burning its timeout against a tunnel that is gone.
func bound(ctx context.Context, signal context.Context) (context.Context, func()) {
	if signal == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	stop := context.AfterFunc(signal, func() {
		cancel(context.Cause(signal))
	})
	return ctx, func() {
		stop()
		cancel(nil)
	}
}

// Fetches one destination until an attempt passes, an attempt fails TLS
// authentication, or it has had every attempt, and returns the outcome of the
// last attempt made with the attempt count and the last failure beside it.
//
// It passes if any attempt passed and fails only when every attempt failed:
// the stored result is per destination after retries, which is what the index
// and the one-in-ten rule count (GEOMAP §10.3), so a momentary block, a rate
// limit or a flapping path costs a retry rather than a failed site.
//
// Each attempt first asks the run's path for a live tunnel, re-creating a
// lost one (see Path): a provider that disconnects mid-run does not fail the
// loads it had not reached, it gets them on a new tunnel, with their remaining
// attempts and their spacing intact. An attempt that could not get a tunnel,
// or whose tunnel was lost under it, is spent like any other; and when the
// last attempt is one of those, the load is not measured -- neither a pass nor
// a failure -- because its tunnel could not be re-created within its attempts.
// A pass stands whenever it happened, and so does a TLS-authentication
// failure: that is an identity that did not authenticate, which a dying tunnel
// does not produce.
//
// A TLS-authentication failure ends the chain at once. A forged certificate is
// not a flake that a later attempt can wash out: it is a hard integrity
// failure whatever a retry would do, and retrying it would only give the
// interceptor a second look at the request.
//
// A retry is fitted inside what is left of the run's budget: every remaining
// attempt keeps one per-request timeout, and the waits before them share the
// rest evenly. An explicit budget shorter than the spacing can produce then
// shortens the waits rather than silently costing the load its last attempts
// -- clamping one wait to all the room left would starve the next. The default
// budget (Options.RunBudget) is sized so this never has to happen.
func (self *run) load(ctx context.Context, d Destination) CheckResult {
	result := CheckResult{Name: d.Name, Class: d.Class}
	lastFailure := ""
	tunnelGone := false
attempts:
	for attempt := 1; attempt <= self.loadAttempts; attempt++ {
		if 1 < attempt {
			delay := retryDelay(self.rng.expFloat64(), self.retryMeanInterval)
			remaining := time.Duration(self.loadAttempts - attempt + 1)
			if share := (self.deadline.Sub(self.opts.now()) - remaining*self.perRequestTimeout) / remaining; share < delay {
				if share <= 0 {
					break attempts
				}
				delay = share
			}
			if err := self.opts.sleep(ctx, delay); err != nil {
				break attempts
			}
		}

		// Checked before the slot is taken, because select picks at random
		// among ready cases: with a slot free and the run over, the send below
		// could still win and start an attempt the run no longer has time for.
		if ctx.Err() != nil {
			break attempts
		}
		select {
		case self.slots <- struct{}{}:
		case <-ctx.Done():
			break attempts
		}
		// Asked for only once the slot is held, so the client is the path's
		// current one at the moment of sending, not one that died while this
		// load queued.
		client, signal, err := self.path.live(ctx)
		if err != nil {
			<-self.slots
			if ctx.Err() != nil {
				break attempts
			}
			// No tunnel for this attempt: it is spent, keeping the load's
			// spacing, and says nothing about the site.
			result = CheckResult{Name: d.Name, Class: d.Class, Attempts: attempt, Err: fmt.Sprintf("no tunnel: %v", err)}
			lastFailure = result.Err
			tunnelGone = true
			continue
		}
		attemptCtx, stop := bound(ctx, signal)
		result = fetch(attemptCtx, client, d, self.perRequestTimeout, self.profile, self.opts.now)
		stop()
		<-self.slots
		result.Attempts = attempt

		if result.Ok || result.TlsAuthenticationFailure {
			tunnelGone = false
			if !result.Ok {
				lastFailure = result.Err
			}
			break attempts
		}
		lastFailure = result.Err
		tunnelGone = lost(signal)
	}
	if result.Attempts == 0 && result.Err == "" {
		// The run ended before this load's first attempt could start. On the
		// run's own budget it is still a failure -- nothing was carried -- but
		// the record says why, so it cannot be read as a provider that
		// answered nothing.
		result.Err = fmt.Sprintf("not attempted: the run ended first (%v)", context.Cause(ctx))
		lastFailure = result.Err
	}
	result.LastFailure = lastFailure
	result.NotMeasured = tunnelGone && !result.Ok
	return result
}

// Runs every destination's retry chain concurrently under the run's
// slots and returns the outcomes in the order given.
func (self *run) loadAll(ctx context.Context, dests []Destination) []CheckResult {
	results := make([]CheckResult, len(dests))
	var wg sync.WaitGroup
	for i := range dests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = self.load(ctx, dests[i])
		}(i)
	}
	wg.Wait()
	return results
}

// What the run's warm-ups saw: the first exit address an echo
// answered with, and why none did if none did. Warm-ups run on the first
// tunnel and again on every re-created one, so it is written concurrently;
// safe for concurrent use.
type exitRecord struct {
	stateLock  sync.Mutex
	ip         string
	observedAt time.Time
	errMessage string
	// Set by any echo whose peer did not authenticate the operator's
	// own api host: interception of the one host whose answer places the
	// provider, and the same hard signal a forged destination is.
	tlsFailure bool
}

// Records one warm-up's outcome. The first address an echo answered with
// stands; an error is kept only while no echo has answered, and a
// TLS-authentication failure is kept whatever else was seen.
func (self *exitRecord) record(ip string, at time.Time, err error) {
	if err != nil {
		errMessage := err.Error()
		tlsFailure := isTlsAuthenticationFailure(err)
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.ip == "" {
			self.errMessage = errMessage
		}
		if tlsFailure {
			self.tlsFailure = true
		}
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.ip == "" {
		self.ip, self.observedAt, self.errMessage = ip, at, ""
	}
}

// Returns what the warm-ups saw so far.
func (self *exitRecord) read() (ip string, observedAt time.Time, errMessage string, tlsFailure bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.ip, self.observedAt, self.errMessage, self.tlsFailure
}
