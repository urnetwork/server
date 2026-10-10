// Test-only owners unwind before disposable pools and notification resources reset.
package work

import (
	"context"
	"errors"
	"sync"

	"github.com/jackc/pgx/v5"
	"github.com/urnetwork/server/v2026"
)

// Both the real maintenance lease and deterministic controls expose the same ownership boundary.
type privateLoadBarrierConn interface {
	Begin(context.Context) (server.PgTx, error)
	Release()
}

// Every successful acquisition has its cleanup in this frame before the next fallible operation.
func privateLoadWithBarrier(ctx context.Context, acquire func(context.Context) (privateLoadBarrierConn, error), query string, balanceId server.Id, body func() error, afterStage ...func(string)) (result error) {
	noteStage := func(stage string) {
		for _, note := range afterStage {
			note(stage)
		}
	}
	conn, err := acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	noteStage("acquired")
	held, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), server.PgRollbackTimeout)
		defer cancel()
		if err := held.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			result = errors.Join(result, err)
		}
	}()
	noteStage("transaction")
	tag, err := held.Exec(ctx, query, balanceId)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return errors.New("barrier row not held")
	}
	noteStage("locked")
	return body()
}

// Adapt the real maintenance pool without changing its configured capacity.
func privateLoadAcquireBarrier(ctx context.Context) (privateLoadBarrierConn, error) {
	return server.AcquireMaintenanceDbConn(ctx)
}

// An unpublished fixture still owns the notification worker it just created.
func privateLoadFinishControl(create func() (*providerEgressControl, error), closeNotifications func()) (control *providerEgressControl, closeOwner func(), err error) {
	closeOwner = sync.OnceFunc(closeNotifications)
	complete := false
	defer func() {
		if !complete {
			closeOwner()
		}
	}()
	control, err = create()
	if err != nil {
		return nil, closeOwner, err
	}
	complete = true
	return control, closeOwner, nil
}

// Gated local waves retain a single result and join on both normal and abnormal exits.
type privateLoadWave struct {
	cancel      context.CancelFunc
	ready       chan struct{}
	release     chan struct{}
	done        chan struct{}
	releaseOnce sync.Once
	report      privateLoadProcessReport
}

// Buffered readiness cannot strand a producer when its parent fails before receiving it.
func privateLoadStartWave(ctx context.Context, run func(context.Context, chan<- struct{}, <-chan struct{}) privateLoadProcessReport) *privateLoadWave {
	ctx, cancel := context.WithCancel(ctx)
	owner := &privateLoadWave{cancel: cancel, ready: make(chan struct{}, 1), release: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(owner.done)
		owner.report = run(ctx, owner.ready, owner.release)
	}()
	return owner
}

// Start may be called by normal execution or by deferred failure cleanup.
func (self *privateLoadWave) Start() { self.releaseOnce.Do(func() { close(self.release) }) }

// A setup failure may finish the wave without publishing readiness.
func (self *privateLoadWave) Ready(ctx context.Context, beforeWait ...func()) error {
	select {
	case <-self.ready:
		return nil
	case <-self.done:
		return errors.New("wave exited before readiness")
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	// The optional per-call control marks only a real transition into waiting.
	for _, note := range beforeWait {
		note()
	}
	select {
	case <-self.ready:
		return nil
	case <-self.done:
		return errors.New("wave exited before readiness")
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Wait observes the same published report without consuming it.
func (self *privateLoadWave) Wait() privateLoadProcessReport { <-self.done; return self.report }

// Close cancels requests, releases every gate, and joins the wave before fixture teardown.
func (self *privateLoadWave) Close() { self.cancel(); self.Start(); self.Wait() }

// Drain all sibling owners before reporting any one failure.
func privateLoadJoinResults(count int, results <-chan error) error {
	var result error
	for range count {
		result = errors.Join(result, <-results)
	}
	return result
}
