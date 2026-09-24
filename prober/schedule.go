package prober

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"
)

// The scheduler: one pass over a batch of providers, with bounded
// concurrency, a success cache, and capped per-error logging.

// Reports one scheduler run.
type Summary struct {
	Attempted int
	Submitted int
	Skipped   int
	Failed    int
	// Counts the probes, among Failed, whose run measured nothing
	// because the tunnel died and could not be re-created in time (see
	// ErrNotMeasured): the prober's lost path, not the providers' traffic, and
	// worth watching apart from ordinary failures -- a pass full of them is a
	// churning fleet or a prober that cannot hold tunnels up.
	NotMeasured int
}

// Probes a set of providers with bounded concurrency, skipping any
// provider probed within CacheTtl. Only successful probes are cached, so a
// failure is retried on the next run.
type Scheduler struct {
	Prober      *Prober
	Concurrency int
	CacheTtl    time.Duration
	// Defaults to time.Now; tests override it to advance the clock.
	Now func() time.Time

	stateLock sync.Mutex
	probed    map[string]time.Time
}

// Returns the time on the scheduler's clock: Now, or the wall clock.
func (self *Scheduler) now() time.Time {
	if self.Now != nil {
		return self.Now()
	}
	return time.Now()
}

// Reports whether id was probed successfully within CacheTtl. The clock is
// read before the lock is taken: Now is the caller's function.
func (self *Scheduler) recentlyProbed(id string) bool {
	now := self.now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	last, ok := self.probed[id]
	if !ok {
		return false
	}
	return now.Sub(last) < self.CacheTtl
}

// Records a successful probe of id at the current time.
func (self *Scheduler) markProbed(id string) {
	now := self.now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.probed == nil {
		self.probed = map[string]time.Time{}
	}
	self.probed[id] = now
}

// Evicts entries from probed older than CacheTtl (M2). Without this,
// probed only ever grows: markProbed adds an entry per successfully probed
// provider and nothing ever removed one, so a long-lived process (this
// scheduler is driven from an infinite loop in cmd/egress-prober) would
// accumulate one map entry per provider ever seen, forever. An entry past
// CacheTtl no longer affects recentlyProbed's decision anyway (its age
// already exceeds the cache window), so evicting it changes no behavior --
// it only bounds memory. Pruning is O(n) over probed and runs once per Run
// call, which is cheap relative to the network calls Run is about to make.
func (self *Scheduler) prune() {
	now := self.now()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.probed == nil {
		return
	}
	for id, last := range self.probed {
		if self.CacheTtl <= now.Sub(last) {
			delete(self.probed, id)
		}
	}
}

// Probes each provider that is not cached, with at most Concurrency
// tunnels open at once.
//
// Per-provider failures are logged as they occur (I2): before this, ProbeOne's
// error was discarded entirely (only aggregate counters ever left this
// function), so a wrong -platform-url, a revoked jwt, and a pin mismatch all
// produced an identical `failed=N` with nothing to distinguish them --
// making a broken prober running unattended on a VPS undebuggable without
// adding print statements and redeploying. To avoid flooding the log when
// every provider fails the same way, only the first Prober.MaxLoggedDistinctErrors
// distinct error messages are logged in detail (each with the provider id
// that first produced it); beyond that, one notice is logged noting further
// detail is suppressed. The total failure count -- and among it the runs that
// measured nothing -- is unaffected and always visible via the returned
// Summary.
func (self *Scheduler) Run(ctx context.Context, providers []Provider) Summary {
	self.prune()
	// Re-arm the prober's per-pass error-log gates (and report what the last
	// pass withheld). Their cap is only safe because every pass starts clean:
	// a permanent cap would let ten transient errors silence a later fault
	// that breaks every provider.
	if self.Prober != nil {
		self.Prober.ResetErrorLogging()
	}

	concurrency := self.Concurrency
	if concurrency < 1 {
		concurrency = 1
	}

	maxLoggedDistinctErrors := self.Prober.maxLoggedDistinctErrors()

	// Guards the tallies and the per-pass log dedup below. Nothing is logged
	// with it held.
	var stateLock sync.Mutex
	var sum Summary
	loggedErrors := map[string]bool{}
	suppressedNoted := false
	skip := func(n int) {
		stateLock.Lock()
		defer stateLock.Unlock()
		sum.Skipped += n
	}

	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup

	// recentlyProbed only becomes true once a probe completes, so a duplicate
	// id inside one batch would otherwise open two tunnels to the same
	// provider simultaneously and pay the contract cost twice. The enumeration
	// path de-duplicates before it gets here; the due path is whatever the
	// server sent.
	seen := map[string]bool{}

	for i, provider := range providers {
		id := provider.ClientId
		if seen[id] {
			skip(1)
			continue
		}
		seen[id] = true

		// A dead context stops the pass here, before any further tunnel is
		// built. providertunnel.Open constructs a full netstack before it
		// ever consults the context, so without this check every remaining
		// provider in the batch would get a real tunnel built and torn down
		// just so its probe could fail instantly -- a 500-provider batch
		// reporting hundreds of spurious failures (and, in single-shot mode,
		// exiting non-zero blaming the providers) when the truth is that the
		// operator sent SIGTERM. The explicit Err check runs first because
		// select chooses randomly among ready cases: with the semaphore free
		// and the context dead, the select below may still pick the
		// semaphore.
		cancelled := ctx.Err() != nil
		if !cancelled {
			if self.recentlyProbed(id) {
				skip(1)
				continue
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				cancelled = true
			}
		}
		if cancelled {
			remaining := len(providers) - i
			skip(remaining)
			log.Printf("prober: run cancelled (%v); skipping the %d remaining provider(s) in this pass", ctx.Err(), remaining)
			break
		}

		wg.Add(1)
		go func(provider Provider) {
			defer wg.Done()
			defer func() { <-sem }()
			id := provider.ClientId

			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				sum.Attempted++
			}()

			err := self.Prober.ProbeOne(ctx, provider)

			// The tallies and the dedup decision under the lock, the log line
			// after it.
			logDetail, logSuppressed := false, false
			func() {
				stateLock.Lock()
				defer stateLock.Unlock()
				if err == nil {
					sum.Submitted++
					return
				}
				sum.Failed++
				if errors.Is(err, ErrNotMeasured) {
					sum.NotMeasured++
				}
				msg := err.Error()
				if loggedErrors[msg] {
					return
				}
				if len(loggedErrors) < maxLoggedDistinctErrors {
					loggedErrors[msg] = true
					logDetail = true
				} else if !suppressedNoted {
					suppressedNoted = true
					logSuppressed = true
				}
			}()
			switch {
			case logDetail:
				log.Printf("prober: probe failed provider=%s: %s", id, err)
			case logSuppressed:
				log.Printf("prober: %d+ distinct probe errors this pass; suppressing further per-error detail (see the pass's failed count for the total)", maxLoggedDistinctErrors)
			}

			if err == nil {
				self.markProbed(id)
			}
		}(provider)
	}
	wg.Wait()
	return sum
}
