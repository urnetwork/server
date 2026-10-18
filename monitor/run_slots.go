package monitor

import (
	"context"
	"sync"
	"time"
)

// One request per signal loop can wait here. Finite-budget observations go
// ahead of ordinary queued work, with at most two priority grants before an
// ordinary waiter. FIFO holds within each class; active work is never evicted.
// This changes admission order, never the four-signal or host command caps.
type runSlotPool struct {
	mu             sync.Mutex
	limit, active  int
	priorityGrants int
	waiting        []*runSlotWaiter
	// Observation only: the scheduler still uses active, waiting and priority.
	holders          [4]*runSlotWaiter
	publicSignalKeys map[string]string
}

type runSlotWaiter struct {
	ctx       context.Context
	ready     chan struct{}
	priority  bool
	granted   bool
	signalKey string
	grantedAt time.Time
}

// The allowlist comes only from the code-owned catalog, never the caller's
// selected signal set. Unknown/custom keys cannot become receipt labels.
func newRunSlotPool(limit int) *runSlotPool {
	pool := &runSlotPool{limit: limit, publicSignalKeys: map[string]string{}}
	for _, signal := range NewSignals() {
		key := signal.Key()
		if key != "" && len(key) <= 64 {
			pool.publicSignalKeys[key] = key
		}
	}
	return pool
}

// The existing request context carries identity only to the local pool.
type runSlotSignalContextKey struct{}

func (p *runSlotPool) acquire(ctx context.Context, priority bool) (func(), error) {
	pgSampleTransportFromContext(ctx).runSlotRequested()
	signalKey := "unknown"
	if key, ok := ctx.Value(runSlotSignalContextKey{}).(string); ok && len(key) <= 64 {
		if public, known := p.publicSignalKeys[key]; known {
			signalKey = public
		}
	}
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	p.observePgHoldersWithLock(ctx, false)
	w := &runSlotWaiter{ctx: ctx, ready: make(chan struct{}), priority: priority, signalKey: signalKey}
	p.waiting = append(p.waiting, w)
	p.dispatch()
	p.mu.Unlock()
	select {
	case <-w.ready:
		return func() { p.release(w) }, nil
	case <-ctx.Done():
		p.mu.Lock()
		if w.granted {
			// Admission and cancellation raced. Return only our own grant.
			p.active--
			p.forgetHolderWithLock(w)
		} else {
			for i, candidate := range p.waiting {
				if candidate == w {
					p.waiting = append(p.waiting[:i], p.waiting[i+1:]...)
					break
				}
			}
		}
		p.dispatch()
		p.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (p *runSlotPool) release(w *runSlotWaiter) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
	p.forgetHolderWithLock(w)
	p.dispatch()
}

// Remove only the exact grant that ended, including cancellation after grant.
// Missing observation state never changes the scheduler's capacity accounting.
func (p *runSlotPool) forgetHolderWithLock(w *runSlotWaiter) {
	for i, holder := range p.holders {
		if holder == w {
			p.holders[i] = nil
			return
		}
	}
}

// Called only under mu. Canceled entries cannot consume a grant, even if
// their waiter goroutine has not yet resumed to remove itself.
func (p *runSlotPool) dispatch() {
	for p.active < p.limit && len(p.waiting) > 0 {
		live := p.waiting[:0]
		for _, w := range p.waiting {
			if w.ctx.Err() == nil {
				live = append(live, w)
			}
		}
		p.waiting = live
		if len(p.waiting) == 0 {
			return
		}
		chosen := 0
		wantPriority := p.priorityGrants < 2
		for i, w := range p.waiting {
			if w.priority == wantPriority {
				chosen = i
				break
			}
		}
		w := p.waiting[chosen]
		p.observePgHoldersWithLock(w.ctx, true)
		p.waiting = append(p.waiting[:chosen], p.waiting[chosen+1:]...)
		w.granted = true
		p.active++
		w.grantedAt = time.Now()
		for i, holder := range p.holders {
			if holder == nil {
				p.holders[i] = w
				break
			}
		}
		if w.priority {
			p.priorityGrants++
		} else {
			p.priorityGrants = 0
		}
		pgSampleTransportFromContext(w.ctx).runSlotGranted(w.ctx)
		close(w.ready)
	}
}

// Distinguish a scheduler deadline from a source command timeout without
// printing transport errors or claiming that a database request was sent.
type runSlotAdmissionError struct{ cause error }

func (e *runSlotAdmissionError) Error() string { return "monitor shared-slot admission unavailable" }
func (e *runSlotAdmissionError) Unwrap() error { return e.cause }
