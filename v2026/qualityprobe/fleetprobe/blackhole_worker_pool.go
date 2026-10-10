// A pass may overlap guard cohorts without multiplying its active check workers.
package fleetprobe

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrBlackholeWorkerPoolClosed = errors.New("fleetprobe: blackhole worker pool closed")

// Owns a fixed worker set, shared only by cohorts of one caller's pass. Work
// submission is concurrent-safe and unbuffered. Each accepted job is joined by
// its batch; CloseAndWait belongs to the outer owner after those batches join.
// It must not be called from a check or progress callback.
type BlackholeWorkerPool struct {
	ctx         context.Context
	cancel      context.CancelFunc
	jobs        chan func()
	workerCount int
	workers     sync.WaitGroup
}

// Starts the instance's workers immediately. Independent passes construct
// independent pools; no process-wide admission or transport budget is shared.
func NewBlackholeWorkerPool(ctx context.Context, concurrency int) (*BlackholeWorkerPool, error) {
	if ctx == nil || concurrency <= 0 {
		return nil, fmt.Errorf("fleetprobe: blackhole pool requires a context and positive concurrency")
	}
	poolCtx, cancel := context.WithCancel(ctx)
	pool := &BlackholeWorkerPool{ctx: poolCtx, cancel: cancel, jobs: make(chan func()), workerCount: concurrency}
	for range concurrency {
		pool.workers.Add(1)
		go func() {
			defer pool.workers.Done()
			for {
				if pool.ctx.Err() != nil {
					return
				}
				select {
				case <-pool.ctx.Done():
					return
				case job := <-pool.jobs:
					// An accepted job owns its batch's wait-group entry even if
					// cancellation won immediately after this receive.
					job()
				}
			}
		}()
	}
	return pool, nil
}

// Admission may wait for this pass's worker, but never outlives either owner or
// its admission-only cutoff. The job rechecks cancellation at execution.
func (self *BlackholeWorkerPool) submit(ctx context.Context, admissionDone, additionalAdmissionDone <-chan struct{}, job func()) bool {
	if ctx.Err() != nil || self.ctx.Err() != nil {
		return false
	}
	select {
	case <-admissionDone:
		return false
	case <-additionalAdmissionDone:
		return false
	default:
	}
	select {
	case self.jobs <- job:
		return true
	case <-ctx.Done():
		return false
	case <-self.ctx.Done():
		return false
	case <-admissionDone:
		return false
	case <-additionalAdmissionDone:
		return false
	}
}

// Stops admission and joins every accepted job. Active checks retain their own
// contexts; callers must cancel those contexts separately when ending a pass.
func (self *BlackholeWorkerPool) CloseAndWait() {
	self.cancel()
	self.workers.Wait()
}
