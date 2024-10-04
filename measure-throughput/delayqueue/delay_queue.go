package delayqueue

import (
	"context"
	"time"
)

type delayedFn struct {
	deadline time.Time
	fn       func()
}

type DelayQueue struct {
	queue chan delayedFn
}

func New(ctx context.Context, size int) *DelayQueue {
	dq := &DelayQueue{
		queue: make(chan delayedFn, size),
	}
	go dq.run(ctx)
	return dq
}

func (d *DelayQueue) run(ctx context.Context) {
	for ctx.Err() == nil {
		select {
		case <-ctx.Done():
			return
		case df := <-d.queue:
			time.Sleep(df.deadline.Sub(time.Now()))
			df.fn()
		}
	}
}

func (d *DelayQueue) GoDelayed(delay time.Duration, fn func()) {
	if delay <= 0 {
		fn()
		return
	}
	d.queue <- delayedFn{
		deadline: time.Now().Add(delay),
		fn:       fn,
	}
}
