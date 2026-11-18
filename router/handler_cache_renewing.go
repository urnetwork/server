package router

import (
	"context"
	"errors"
	"runtime/debug"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// How long a renewing key's last published value stays answerable after its
// own lifetime ends. A miss whose refresh is slower than its wait answers with
// it instead of 503 (cacheJsonWithRenewingFill). It covers the pollers seen on
// 2026-11-18, which come back every 1 and every ~5 minutes; the provider list's
// created_time says how old the answer is.
const handlerCacheLastGoodTtl = 15 * time.Minute

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

// A publication also keeps its value as the key's last good answer. That write
// is best effort: losing it leaves the previous last good answer in place.
func (self *handlerCacheRenewingAttempt) publish(
	ctx context.Context,
	keys handlerCacheKeys,
	token string,
	value string,
	ttl time.Duration,
) (bool, error) {
	published, err := self.handlerCacheRenewingStore.publish(ctx, keys, token, value, ttl)
	if err == nil && published {
		_ = self.handlerCacheRenewingStore.set(ctx, keys.last, value, handlerCacheLastGoodTtl, false)
	}
	return published, err
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
// a detached database operation. Failures keep the lease, which cacheJson then
// cuts to the failure fence; renewal is joined first, so it cannot extend it.
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

// A miss of a renewing key is answered from a fill its caller cannot cancel.
//
// The coordinated attempt (cacheJson: take the lease, or wait for its holder)
// runs on its own goroutine under the caller's context without its
// cancellation, and an owner's fill keeps the owner budget (fillTimeout) and
// lease renewal of renewingHandlerCacheFill. The caller waits for that outcome
// until its own end; when it leaves first, the fill still publishes and the
// next request of the key finds the value.
//
// A miss that has no answer after handlerCacheFillWaitLimit, or whose attempt
// ends unavailable (a failure fence, a lost lease, Redis, the fill budget),
// answers the key's last published value while one is kept
// (handlerCacheLastGoodTtl) instead of 503. A fill that answers within the
// wait is answered fresh, as before, and other fill errors keep their status.
//
// Before 2026-11-18 the fill ran under its caller's context. A provider list
// slower than its callers' patience (the lb cuts an upstream at 30 s; clients
// give up after about 10 s) was canceled with each caller and never
// published, so every request of that network answered 503 or nothing. One
// client's list took 2 s to fill on 2026-11-14 and more than 10 s from the
// evening of 11-16, and none of its requests succeeded after that.
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
	keys := newHandlerCacheKeys(key)
	if cachedValue, ok, err := getCachedJson[R](ctx, store, keys.value); err != nil {
		return nil, &handlerCacheUnavailableError{}
	} else if ok {
		return cachedValue, nil
	}

	outcomes := make(chan handlerCacheRenewingOutcome[R])
	abandoned := make(chan struct{})
	defer close(abandoned)
	go runRenewingHandlerCacheAttempt(context.WithoutCancel(ctx), store, key, ttl, fillTimeout, fill, outcomes, abandoned)

	wait := time.NewTimer(handlerCacheFillWaitLimit)
	defer wait.Stop()
	waitDone := wait.C
	for {
		select {
		case outcome := <-outcomes:
			if outcome.panicked {
				panic(outcome.panicValue)
			}
			if outcome.err != nil && handlerCacheRenewingUnavailable(outcome.err) {
				if lastGood, ok, err := getCachedJson[R](ctx, store, keys.last); err == nil && ok {
					return lastGood, nil
				}
			}
			return outcome.value, outcome.err
		case <-waitDone:
			waitDone = nil
			if lastGood, ok, err := getCachedJson[R](ctx, store, keys.last); err == nil && ok {
				return lastGood, nil
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// One coordinated attempt's result, or the panic that ended it.
type handlerCacheRenewingOutcome[R any] struct {
	value      *R
	err        error
	panicked   bool
	panicValue any
}

// Runs one coordinated attempt and hands its outcome to the caller while the
// caller waits. A panic is logged with its own stack and handed over to be
// raised again in the caller; once the caller has left, it is only logged.
func runRenewingHandlerCacheAttempt[R any](
	ctx context.Context,
	store handlerCacheRenewingStore,
	key string,
	ttl time.Duration,
	fillTimeout time.Duration,
	fill func(context.Context) (*R, error),
	outcomes chan<- handlerCacheRenewingOutcome[R],
	abandoned <-chan struct{},
) {
	var outcome handlerCacheRenewingOutcome[R]
	defer func() {
		if recovered := recover(); recovered != nil {
			glog.Errorf("[h]renewing cache fill panic: %v\n%s", recovered, debug.Stack())
			outcome = handlerCacheRenewingOutcome[R]{panicked: true, panicValue: recovered}
		}
		select {
		case outcomes <- outcome:
		case <-abandoned:
		}
	}()
	attempt := &handlerCacheRenewingAttempt{handlerCacheRenewingStore: store}
	outcome.value, outcome.err = cacheJson(ctx, attempt, key, ttl, func() (*R, error) {
		return renewingHandlerCacheFill(ctx, attempt, fillTimeout, fill)
	})
}

// An attempt that ended without an answer: a fence, a lost lease or a Redis
// failure (503), or the owner budget. A fill's own refusal is not one.
func handlerCacheRenewingUnavailable(err error) bool {
	var unavailable *handlerCacheUnavailableError
	return errors.As(err, &unavailable) || errors.Is(err, context.DeadlineExceeded)
}

// Opt-in ownership for an expensive network-scoped cache fill. Response TTL,
// key scope and failure policy match CacheWithNetworkAuth. A lease loss cancels
// only the copied session's fill context; it cannot mutate the caller session.
// The fill's context does not end with the caller (cacheJsonWithRenewingFill).
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
