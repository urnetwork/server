package monitor

import (
	"context"
	"sync"
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
}

type runSlotWaiter struct {
	ctx      context.Context
	ready    chan struct{}
	priority bool
	granted  bool
}

func newRunSlotPool(limit int) *runSlotPool { return &runSlotPool{limit: limit} }

func (p *runSlotPool) acquire(ctx context.Context, priority bool) (func(), error) {
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	w := &runSlotWaiter{ctx: ctx, ready: make(chan struct{}), priority: priority}
	p.waiting = append(p.waiting, w)
	p.dispatch()
	p.mu.Unlock()
	select {
	case <-w.ready:
		return p.release, nil
	case <-ctx.Done():
		p.mu.Lock()
		if w.granted {
			// Admission and cancellation raced. Return only our own grant.
			p.active--
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

func (p *runSlotPool) release() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.active--
	p.dispatch()
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
		p.waiting = append(p.waiting[:chosen], p.waiting[chosen+1:]...)
		w.granted = true
		p.active++
		if w.priority {
			p.priorityGrants++
		} else {
			p.priorityGrants = 0
		}
		close(w.ready)
	}
}

// Distinguish a scheduler deadline from a source command timeout without
// printing transport errors or claiming that a database request was sent.
type runSlotAdmissionError struct{ cause error }

func (e *runSlotAdmissionError) Error() string { return "monitor shared-slot admission unavailable" }
func (e *runSlotAdmissionError) Unwrap() error { return e.cause }
