// A startup owner keeps temporary database/cache loss separate from a rejected
// deployment. It acquires one runtime only after the original queue is ready.
package taskworker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/router"
)

const taskworkerStartupReadTimeout = 300 * time.Second
const taskworkerStartupRetryDelay = 2 * time.Second

// Closing the owner's completion channel publishes all fields to the drain
// owner. A failed admission never owns an execution loop or metrics cohort.
type taskworkerStartupResult struct {
	worker       taskworkerRuntime
	flushStats   func()
	closeCapture func()
	err          error
}

// A bounded attempt can stop while the process remains available for a later
// read. This delay owns no transaction, lease or task retry allowance.
func waitTaskworkerStartup(ctx context.Context, delay time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return ctx.Err()
	}
}

// Only dependency reads and idempotent startup scheduling are retried. Real
// worker construction starts once; native work stays in its existing journals.
func startTaskworkerAfterReadiness(
	admission context.Context,
	lifetime context.Context,
	cancel context.CancelFunc,
	options RunOptions,
	readiness func(context.Context) error,
	startStatsPusher func(context.Context) func(),
	startRuntime func(context.Context, context.Context, context.CancelFunc, RunOptions) (taskworkerRuntime, error),
	wait func(context.Context, time.Duration) error,
) (result taskworkerStartupResult) {
	defer func() {
		if recovered := recover(); recovered != nil {
			if err, ok := recovered.(error); ok {
				result.err = fmt.Errorf("taskworker startup: %w", err)
			} else {
				result.err = fmt.Errorf("taskworker startup: %v", recovered)
			}
			readyGauge.Set(0)
			router.SetWarpStatusNotReady(result.err)
			if result.worker != nil {
				cancel()
			}
		}
	}()
	for {
		if err := admission.Err(); err != nil {
			result.err = errors.Join(result.err, err)
			return result
		}
		attempt, finish := context.WithTimeout(admission, taskworkerStartupReadTimeout)
		err := readiness(attempt)
		if err == nil {
			err = attempt.Err()
		}
		if err == nil && result.closeCapture == nil {
			config, configErr := server.LoadArinShadowRuntimeConfig()
			if configErr != nil {
				err = errors.Join(server.ErrArinShadowInput, configErr)
			} else {
				capture, captureErr := server.StartArinShadowRuntime(lifetime, config, "native", func(ctx context.Context) (server.ArinShadowRPCHandler, func(), error) {
					return model.NewArinShadowNativeRPC(ctx, config.Capacity)
				})
				if captureErr != nil {
					err = errors.Join(server.ErrArinShadowInput, captureErr)
				} else {
					result.closeCapture = func() { capture.Close() }
				}
			}
		}
		if err == nil {
			result.worker, err = startRuntime(attempt, lifetime, cancel, options)
			if err == nil && result.worker == nil {
				err = errors.New("taskworker startup returned no runtime owner")
			}
		}
		finish()
		if err == nil {
			// Construction transferred one real owner. A simultaneous caller
			// stop drains that owner and still receives its final metrics flush.
			result.err = nil
			result.flushStats = startStatsPusher(lifetime)
			if admission.Err() == nil {
				readyGauge.Set(1)
				router.SetWarpStatusReady()
			}
			return result
		}
		result.err = errors.Join(err, admission.Err())
		readyGauge.Set(0)
		router.SetWarpStatusNotReady(result.err)
		if result.worker != nil || !router.RetryableStartupReadinessError(result.err) {
			if result.worker != nil {
				cancel()
			}
			return result
		}
		if err := wait(admission, taskworkerStartupRetryDelay); err != nil {
			result.err = errors.Join(result.err, err, admission.Err())
			return result
		}
	}
}
