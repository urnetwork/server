package router

import (
	"context"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Only explicitly selected expensive handlers renew their fill ownership.
// Ordinary cache callers retain their existing lease-expiry behavior.
type handlerCacheRenewingStore interface {
	handlerCacheStore
	renew(context.Context, string, string, time.Duration) (bool, error)
}

// An expired or replaced token cannot be revived by its previous owner.
func (self redisHandlerCacheStore) renew(
	ctx context.Context,
	key string,
	token string,
	ttl time.Duration,
) (renewed bool, returnErr error) {
	const script = `
		if redis.call('GET', KEYS[1]) ~= ARGV[1] then
			return 0
		end
		return redis.call('PEXPIRE', KEYS[1], ARGV[2])
	`
	// The ordinary pool does not apply caller deadlines to socket I/O. This
	// one-key EVAL uses the existing no-retry, deadline-aware pool instead.
	operationCtx, cancel := context.WithTimeout(ctx, min(2*time.Second, ttl/6))
	defer cancel()
	returnErr = server.RedisWithDeadline(operationCtx, func(client server.RedisClient) error {
		result, err := client.Eval(operationCtx, script, []string{key}, token, max(int64(1), ttl.Milliseconds())).Int64()
		renewed = result == 1
		return err
	})
	return
}

// Captures the original cache acquisition without changing its lookup,
// ambiguous-acquisition, retry-fence or guarded-publication behavior.
type handlerCacheRenewingAttempt struct {
	handlerCacheRenewingStore
	key   string
	token string
	ttl   time.Duration
}

func (self *handlerCacheRenewingAttempt) acquire(
	ctx context.Context,
	key string,
	token string,
	ttl time.Duration,
) (bool, error) {
	self.key, self.token, self.ttl = key, token, ttl
	return self.handlerCacheRenewingStore.acquire(ctx, key, token, ttl)
}

// Renew within both the old lease's conservative lifetime and a short Redis
// call budget. The start of the successful call bounds the new lease below;
// a delayed acknowledgement never adds assumed ownership time.
func (self *handlerCacheRenewingAttempt) renewBefore(ctx context.Context, before time.Time) (time.Time, error) {
	started := time.Now()
	if !started.Before(before) {
		return time.Time{}, &handlerCacheUnavailableError{}
	}
	deadline := minTime(before, started.Add(min(2*time.Second, self.ttl/6)))
	callCtx, cancel := context.WithDeadline(ctx, deadline)
	renewed, err := self.renew(callCtx, self.key, self.token, self.ttl)
	callErr := callCtx.Err()
	cancel()
	if err != nil || callErr != nil || !renewed || !time.Now().Before(deadline) {
		return time.Time{}, &handlerCacheUnavailableError{}
	}
	return started.Add(self.ttl), nil
}

func minTime(a time.Time, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (self *handlerCacheRenewingAttempt) keep(
	ctx context.Context,
	fillCtx context.Context,
	leaseBefore time.Time,
) error {
	timer := time.NewTimer(self.ttl / 3)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			// Normal joined shutdown cancels only the renewal context.
			return fillCtx.Err()
		case <-timer.C:
			var err error
			leaseBefore, err = self.renewBefore(ctx, leaseBefore)
			if ctx.Err() != nil {
				return fillCtx.Err()
			}
			if err != nil {
				return err
			}
			timer.Reset(self.ttl / 3)
		}
	}
}

// A lost or uncertain renewal cancels the exact context used by the expensive
// fill. The callback must return before this owner returns; it is never left as
// a detached database operation. Failures retain the existing expiring fence.
func renewingHandlerCacheFill[R any](
	ctx context.Context,
	attempt *handlerCacheRenewingAttempt,
	fillTimeout time.Duration,
	fill func(context.Context) (*R, error),
) (*R, error) {
	fillCtx, cancelFill := context.WithTimeout(ctx, fillTimeout)
	defer cancelFill()
	leaseBefore, err := attempt.renewBefore(fillCtx, time.Now().Add(attempt.ttl))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, err
	}
	renewCtx, cancelRenew := context.WithCancel(fillCtx)
	joined := make(chan error, 1)
	go func() {
		err := attempt.keep(renewCtx, fillCtx, leaseBefore)
		if err != nil {
			cancelFill()
		}
		joined <- err
	}()
	var renewalErr error
	stopped := false
	stop := func() {
		if !stopped {
			cancelRenew()
			renewalErr = <-joined
			stopped = true
		}
	}
	defer stop()
	value, fillErr := fill(fillCtx)
	stop()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if fillCtx.Err() == context.DeadlineExceeded {
		return nil, context.DeadlineExceeded
	}
	if renewalErr != nil {
		return nil, &handlerCacheUnavailableError{}
	}
	return value, fillErr
}

func cacheJsonWithRenewingFill[R any](
	ctx context.Context,
	store handlerCacheRenewingStore,
	key string,
	ttl time.Duration,
	fillTimeout time.Duration,
	fill func(context.Context) (*R, error),
) (*R, error) {
	if handlerCacheFillLeaseTtl(ttl) < time.Second || fillTimeout <= 0 {
		return nil, &handlerCacheUnavailableError{}
	}
	attempt := &handlerCacheRenewingAttempt{handlerCacheRenewingStore: store}
	return cacheJson(ctx, attempt, key, ttl, func() (*R, error) {
		return renewingHandlerCacheFill(ctx, attempt, fillTimeout, fill)
	})
}

// Opt-in ownership for an expensive network-scoped cache fill. Response TTL,
// key scope and failure policy match CacheWithNetworkAuth. A lease loss cancels
// only the copied session's fill context; it cannot mutate the caller session.
func CacheWithNetworkAuthRenewingFill[R any](
	impl ImplFunction[*R],
	key string,
	ttl time.Duration,
	fillTimeout time.Duration,
) ImplFunction[*R] {
	return func(clientSession *session.ClientSession) (*R, error) {
		store, ok := defaultHandlerCacheStore.(handlerCacheRenewingStore)
		if !ok {
			return nil, &handlerCacheUnavailableError{}
		}
		return cacheJsonWithRenewingFill(clientSession.Ctx, store, KeyWithNetworkAuth(clientSession, key), ttl, fillTimeout,
			func(ctx context.Context) (*R, error) {
				owned := *clientSession
				owned.Ctx, owned.Cancel = context.WithCancel(ctx)
				defer owned.Cancel()
				return impl(&owned)
			})
	}
}

// The input key uses the same network and serialized-argument scope as the
// existing cache. No distinct lookback requests are silently combined.
func CacheWithNetworkAuthInputRenewingFill[T any, R any](
	impl ImplWithInputFunction[T, *R],
	key string,
	ttl time.Duration,
	fillTimeout time.Duration,
) ImplWithInputFunction[T, *R] {
	return func(input T, clientSession *session.ClientSession) (*R, error) {
		return CacheWithNetworkAuthRenewingFill(func(owned *session.ClientSession) (*R, error) {
			return impl(input, owned)
		}, inputCacheKey(key, input), ttl, fillTimeout)(clientSession)
	}
}
