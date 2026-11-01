package router

// handler_cache_fence_test.go pins how long a failed fill closes its key, that
// a fill in progress keeps its whole lease, and that values written together
// do not expire together. Expiry runs on the synctest bubble's clock, so
// leases of minutes pass instantly and deterministically.

import (
	"context"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type handlerCacheExpiringTestEntry struct {
	value   string
	expires time.Time
}

// Models Redis expiry, the token-guarded fence and renew scripts, and the
// faults of a struggling Redis: a SETNX that applies but loses its response,
// failing reads, and a publication that fails without applying.
type handlerCacheExpiringTestStore struct {
	mu      sync.Mutex
	entries map[string]handlerCacheExpiringTestEntry

	acquireLostResponse error
	// Per physical key, the outcome of each next read; nil reads normally.
	readFaults map[string][]error
	publishErr error

	acquireTtls []time.Duration
	publishTtls []time.Duration
	setTtls     []time.Duration
}

func newHandlerCacheExpiringTestStore() *handlerCacheExpiringTestStore {
	return &handlerCacheExpiringTestStore{
		entries:    map[string]handlerCacheExpiringTestEntry{},
		readFaults: map[string][]error{},
	}
}

func (self *handlerCacheExpiringTestStore) liveLocked(key string) (handlerCacheExpiringTestEntry, bool) {
	entry, ok := self.entries[key]
	if ok && !time.Now().Before(entry.expires) {
		delete(self.entries, key)
		return handlerCacheExpiringTestEntry{}, false
	}
	return entry, ok
}

func (self *handlerCacheExpiringTestStore) get(_ context.Context, key string) (string, bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if faults := self.readFaults[key]; 0 < len(faults) {
		self.readFaults[key] = faults[1:]
		if faults[0] != nil {
			return "", false, faults[0]
		}
	}
	entry, ok := self.liveLocked(key)
	return entry.value, ok, nil
}

func (self *handlerCacheExpiringTestStore) set(_ context.Context, key string, value string, ttl time.Duration, onlyIfAbsent bool) error {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.setTtls = append(self.setTtls, ttl)
	if _, ok := self.liveLocked(key); !onlyIfAbsent || !ok {
		self.entries[key] = handlerCacheExpiringTestEntry{value, time.Now().Add(ttl)}
	}
	return nil
}

func (self *handlerCacheExpiringTestStore) acquire(_ context.Context, key string, token string, ttl time.Duration) (bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.acquireTtls = append(self.acquireTtls, ttl)
	if _, ok := self.liveLocked(key); ok {
		return false, nil
	}
	self.entries[key] = handlerCacheExpiringTestEntry{token, time.Now().Add(ttl)}
	if err := self.acquireLostResponse; err != nil {
		self.acquireLostResponse = nil
		return false, err
	}
	return true, nil
}

func (self *handlerCacheExpiringTestStore) publish(_ context.Context, keys handlerCacheKeys, token string, value string, ttl time.Duration) (bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if err := self.publishErr; err != nil {
		self.publishErr = nil
		return false, err
	}
	if entry, ok := self.liveLocked(keys.fill); !ok || entry.value != token {
		return false, nil
	}
	self.publishTtls = append(self.publishTtls, ttl)
	self.entries[keys.value] = handlerCacheExpiringTestEntry{value, time.Now().Add(ttl)}
	delete(self.entries, keys.fill)
	return true, nil
}

func (self *handlerCacheExpiringTestStore) release(_ context.Context, key string, token string) error {
	self.mu.Lock()
	defer self.mu.Unlock()
	if entry, ok := self.liveLocked(key); ok && entry.value == token {
		delete(self.entries, key)
	}
	return nil
}

// Like the script, it changes only the token's own lease and only toward
// expiry. A canceled context fails the call without applying it, as a Redis
// command on a canceled context would.
func (self *handlerCacheExpiringTestStore) fence(ctx context.Context, key string, token string, ttl time.Duration) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	self.mu.Lock()
	defer self.mu.Unlock()
	entry, ok := self.liveLocked(key)
	if !ok || entry.value != token {
		return false, nil
	}
	if expires := time.Now().Add(ttl); expires.Before(entry.expires) {
		self.entries[key] = handlerCacheExpiringTestEntry{token, expires}
	}
	return true, nil
}

func (self *handlerCacheExpiringTestStore) renew(_ context.Context, key string, token string, ttl time.Duration) (bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry, ok := self.liveLocked(key)
	if !ok || entry.value != token {
		return false, nil
	}
	self.entries[key] = handlerCacheExpiringTestEntry{token, time.Now().Add(ttl)}
	return true, nil
}

func (self *handlerCacheExpiringTestStore) expiry(key string) (time.Time, bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	entry, ok := self.liveLocked(key)
	return entry.expires, ok
}

func (self *handlerCacheExpiringTestStore) acquired() []time.Duration {
	self.mu.Lock()
	defer self.mu.Unlock()
	return append([]time.Duration(nil), self.acquireTtls...)
}

func isHandlerCacheUnavailable(err error) bool {
	var unavailable *handlerCacheUnavailableError
	return errors.As(err, &unavailable)
}

// A failed fill used to keep its lease, the same lease a fill in progress
// holds, for min(ttl, 2m). Every API process then answered the key with 503
// for that whole lease, so a brief Redis or database stall closed it across
// the fleet for up to two minutes. Every failure after a request takes the
// lease must now close the key for the short failure fence only, and for no
// less: retries stay suppressed until the fence expires.
func TestHandlerCacheFailedFillFencesOnlyBriefly(t *testing.T) {
	errFill := errors.New("synthetic fill failure")
	errRedis := errors.New("synthetic redis failure")
	succeed := func(context.CancelFunc) (*handlerCacheTestResult, error) {
		return &handlerCacheTestResult{Message: "unshared"}, nil
	}
	unavailable := func(err error, panicked any) bool {
		return panicked == nil && isHandlerCacheUnavailable(err)
	}
	failures := []struct {
		name   string
		inject func(*handlerCacheExpiringTestStore, handlerCacheKeys)
		fill   func(cancel context.CancelFunc) (*handlerCacheTestResult, error)
		failed func(err error, panicked any) bool
	}{
		{
			name: "fill-error",
			fill: func(context.CancelFunc) (*handlerCacheTestResult, error) { return nil, errFill },
			failed: func(err error, panicked any) bool {
				return panicked == nil && errors.Is(err, errFill)
			},
		},
		{
			name:   "nil-result",
			fill:   func(context.CancelFunc) (*handlerCacheTestResult, error) { return nil, nil },
			failed: unavailable,
		},
		{
			// Model calls raise storage failures as panics.
			name:   "fill-panic",
			fill:   func(context.CancelFunc) (*handlerCacheTestResult, error) { panic(errFill) },
			failed: func(_ error, panicked any) bool { return panicked == errFill },
		},
		{
			// The caller went away mid-fill; its fence must not need its context.
			name: "canceled-caller",
			fill: func(cancel context.CancelFunc) (*handlerCacheTestResult, error) {
				cancel()
				return nil, context.Canceled
			},
			failed: func(err error, panicked any) bool {
				return panicked == nil && errors.Is(err, context.Canceled)
			},
		},
		{
			name: "value-recheck-error",
			inject: func(store *handlerCacheExpiringTestStore, keys handlerCacheKeys) {
				store.readFaults[keys.value] = []error{nil, errRedis}
			},
			fill:   succeed,
			failed: unavailable,
		},
		{
			name: "publish-error",
			inject: func(store *handlerCacheExpiringTestStore, _ handlerCacheKeys) {
				store.publishErr = errRedis
			},
			fill:   succeed,
			failed: unavailable,
		},
		{
			// SETNX applied but lost its response, and the ownership read failed
			// too: the request answers 503 while its token holds a lease that
			// nobody fills under.
			name: "lost-acquire-response",
			inject: func(store *handlerCacheExpiringTestStore, keys handlerCacheKeys) {
				store.acquireLostResponse = errRedis
				store.readFaults[keys.fill] = []error{errRedis}
			},
			fill:   succeed,
			failed: unavailable,
		},
	}
	// The ttls of the current callers: network reliability, provider stats,
	// transfer stats and provider status.
	for _, ttl := range []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute} {
		fence := handlerCacheFailureFenceTtl(ttl)
		if fence <= 0 || ttl < fence || 10*time.Second < fence {
			t.Fatalf("failure fence for ttl %v = %v, want positive and at most min(ttl, 10s)", ttl, fence)
		}
		for _, failure := range failures {
			t.Run(fmt.Sprintf("%v/%s", ttl, failure.name), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					store := newHandlerCacheExpiringTestStore()
					key := "synthetic-fenced-key"
					if failure.inject != nil {
						failure.inject(store, newHandlerCacheKeys(key))
					}
					fills := 0
					call := func(ctx context.Context, fill func() (*handlerCacheTestResult, error)) (value *handlerCacheTestResult, err error, panicked any) {
						defer func() { panicked = recover() }()
						value, err = cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
							fills++
							return fill()
						})
						return
					}
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					value, err, panicked := call(ctx, func() (*handlerCacheTestResult, error) { return failure.fill(cancel) })
					if value != nil || !failure.failed(err, panicked) {
						t.Fatalf("failed request = (%v, %v, panic %v)", value, err, panicked)
					}
					failedFills := fills
					retry := func() (*handlerCacheTestResult, error) {
						return &handlerCacheTestResult{Message: "recovered"}, nil
					}
					start := time.Now()
					for _, at := range []time.Duration{0, fence - time.Millisecond} {
						time.Sleep(at - time.Since(start))
						if value, err, _ := call(context.Background(), retry); value != nil || !isHandlerCacheUnavailable(err) || fills != failedFills {
							t.Fatalf("retry %v after the failure = (%v, %v) with %d new fills, want 503 and none", at, value, err, fills-failedFills)
						}
					}
					time.Sleep(fence - time.Since(start))
					if value, err, _ := call(context.Background(), retry); err != nil || value == nil || value.Message != "recovered" {
						t.Fatalf("retry at the %v failure fence = (%v, %v), want a new fill; the key stayed closed toward its %v fill lease", fence, value, err, min(ttl, 2*time.Minute))
					}
				})
			})
		}
	}
}

// The fence cuts only the failed request's own lease, and only toward expiry.
// A replacement owner that took over after this request's lease expired keeps
// its whole fill lease, and a lease due to expire before the fence would is
// never extended, so no key stays closed longer than it did before.
func TestHandlerCacheFenceNeverExtendsOrTakesALease(t *testing.T) {
	ttl := 10 * time.Second
	lease := min(ttl, 2*time.Minute)
	fence := handlerCacheFailureFenceTtl(ttl)
	ctx := context.Background()
	key := "synthetic-live-lease"
	probe := func(store *handlerCacheExpiringTestStore) (*handlerCacheTestResult, error, int) {
		fills := 0
		value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
			fills++
			return &handlerCacheTestResult{Message: "probe"}, nil
		})
		return value, err, fills
	}

	t.Run("replacement-owner", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := newHandlerCacheExpiringTestStore()
			expiredStarted, expiredFail := make(chan struct{}), make(chan struct{})
			expiredOwner := make(chan handlerCacheTestCallResult, 1)
			go func() {
				value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
					close(expiredStarted)
					<-expiredFail
					return nil, errors.New("synthetic slow failure")
				})
				expiredOwner <- handlerCacheTestCallResult{value, err}
			}()
			<-expiredStarted
			time.Sleep(lease + 500*time.Millisecond)

			replacementStarted, replacementFinish := make(chan struct{}), make(chan struct{})
			replacement := make(chan handlerCacheTestCallResult, 1)
			go func() {
				value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
					close(replacementStarted)
					<-replacementFinish
					return &handlerCacheTestResult{Message: "replacement"}, nil
				})
				replacement <- handlerCacheTestCallResult{value, err}
			}()
			<-replacementStarted
			time.Sleep(500 * time.Millisecond)
			close(expiredFail)
			if result := <-expiredOwner; result.value != nil || result.err == nil {
				t.Fatalf("expired owner = (%v, %v), want its fill failure", result.value, result.err)
			}

			time.Sleep(fence)
			if value, err, fills := probe(store); value != nil || !isHandlerCacheUnavailable(err) || fills != 0 {
				t.Fatalf("%v after the expired owner failed = (%v, %v) with %d fills; its fence cut the replacement's lease", fence, value, err, fills)
			}
			close(replacementFinish)
			if result := <-replacement; result.err != nil || result.value == nil || result.value.Message != "replacement" {
				t.Fatalf("replacement owner = (%v, %v), want its published value", result.value, result.err)
			}
		})
	})

	t.Run("lease-due-before-the-fence", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := newHandlerCacheExpiringTestStore()
			// The lease has less time left than a fence when the fill fails.
			failAt := lease - fence/2
			_, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
				time.Sleep(failAt)
				return nil, errors.New("synthetic late failure")
			})
			if err == nil {
				t.Fatal("late failure returned no error")
			}
			time.Sleep(lease - failAt - time.Millisecond)
			if value, err, fills := probe(store); value != nil || !isHandlerCacheUnavailable(err) || fills != 0 {
				t.Fatalf("before the lease's own expiry = (%v, %v) with %d fills, want 503 and none", value, err, fills)
			}
			time.Sleep(time.Millisecond)
			if value, err, fills := probe(store); err != nil || value == nil || fills != 1 {
				t.Fatalf("at the lease's own expiry = (%v, %v) with %d fills, want a new fill; the fence extended the lease", value, err, fills)
			}
		})
	})
}

// A fill in progress keeps its whole lease of min(ttl, 2m): overlapping misses
// across the fleet still get the retryable 503 rather than a duplicate fill,
// also once a failure fence would have passed. A successful fill still
// publishes, drops its lease and serves later requests from the cache.
func TestHandlerCacheFillInProgressKeepsWholeLease(t *testing.T) {
	for _, ttl := range []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute} {
		t.Run(ttl.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newHandlerCacheExpiringTestStore()
				ctx := context.Background()
				key := "synthetic-in-progress"
				lease := min(ttl, 2*time.Minute)
				var duplicates atomic.Int64
				overlapping := func() (*handlerCacheTestResult, error) {
					return cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
						duplicates.Add(1)
						return &handlerCacheTestResult{Message: "duplicate"}, nil
					})
				}
				started, finish := make(chan struct{}), make(chan struct{})
				owner := make(chan handlerCacheTestCallResult, 1)
				go func() {
					value, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) {
						close(started)
						<-finish
						return &handlerCacheTestResult{Message: "owner"}, nil
					})
					owner <- handlerCacheTestCallResult{value, err}
				}()
				<-started
				start := time.Now()
				for _, at := range []time.Duration{0, handlerCacheFailureFenceTtl(ttl) + time.Second, lease - time.Millisecond} {
					time.Sleep(at - time.Since(start))
					if value, err := overlapping(); value != nil || !isHandlerCacheUnavailable(err) || duplicates.Load() != 0 {
						t.Fatalf("overlapping miss %v into the fill = (%v, %v) with %d duplicate fills, want 503 and none", at, value, err, duplicates.Load())
					}
				}
				for _, acquired := range store.acquired() {
					if acquired != lease {
						t.Fatalf("fill leases = %v, want each %v", store.acquired(), lease)
					}
				}
				close(finish)
				if result := <-owner; result.err != nil || result.value == nil || result.value.Message != "owner" {
					t.Fatalf("owner = (%v, %v), want its published value", result.value, result.err)
				}
				if _, leased := store.expiry(newHandlerCacheKeys(key).fill); leased {
					t.Fatal("a published fill kept its lease")
				}
				if value, err := overlapping(); err != nil || value == nil || value.Message != "owner" || duplicates.Load() != 0 {
					t.Fatalf("after publication = (%v, %v) with %d duplicate fills, want the cached value", value, err, duplicates.Load())
				}
			})
		})
	}
}

// Values written at one instant used to share one expiry, so keys filled
// together after a fleet-wide restart expired and refilled together on every
// cycle. Each publication and each warm write now draws its own expiry from
// the last tenth of the ttl, never past the ttl itself.
func TestHandlerCacheValuesWrittenTogetherExpireApart(t *testing.T) {
	prior := handlerCacheValueTtlJitter
	defer func() { handlerCacheValueTtlJitter = prior }()
	random := mathrand.New(mathrand.NewPCG(2026, 1101))
	handlerCacheValueTtlJitter = func(span time.Duration) time.Duration {
		return time.Duration(random.Int64N(int64(span) + 1))
	}
	ttl := 30 * time.Second
	synctest.Test(t, func(t *testing.T) {
		store := newHandlerCacheExpiringTestStore()
		ctx := context.Background()
		value := func(key string) (*handlerCacheTestResult, error) {
			return &handlerCacheTestResult{Message: key}, nil
		}
		writes := []struct {
			name  string
			write func(key string) error
		}{
			{"publish", func(key string) error {
				_, err := cacheJson(ctx, store, key, ttl, func() (*handlerCacheTestResult, error) { return value(key) })
				return err
			}},
			{"renewing-publish", func(key string) error {
				_, err := cacheJsonWithRenewingFill(ctx, store, key, ttl, time.Minute, func(context.Context) (*handlerCacheTestResult, error) { return value(key) })
				return err
			}},
			{"warm", func(key string) error {
				_, err := warmCacheJson(ctx, store, key, ttl, true, func() (*handlerCacheTestResult, error) { return value(key) })
				return err
			}},
			{"warm-if-absent", func(key string) error {
				_, err := warmCacheJson(ctx, store, key, ttl, false, func() (*handlerCacheTestResult, error) { return value(key) })
				return err
			}},
		}
		start := time.Now()
		for _, write := range writes {
			keys := []string{write.name + "-a", write.name + "-b"}
			expiries := []time.Time{}
			for _, key := range keys {
				if err := write.write(key); err != nil {
					t.Fatalf("%s %s: %v", write.name, key, err)
				}
				expiry, ok := store.expiry(newHandlerCacheKeys(key).value)
				if !ok {
					t.Fatalf("%s %s stored no value", write.name, key)
				}
				if expiry.Before(start.Add(ttl-ttl/10)) || start.Add(ttl).Before(expiry) {
					t.Fatalf("%s %s expires %v after the write, want within [%v, %v]", write.name, key, expiry.Sub(start), ttl-ttl/10, ttl)
				}
				expiries = append(expiries, expiry)
			}
			if !time.Now().Equal(start) {
				t.Fatal("the writes were not simultaneous")
			}
			if expiries[0].Equal(expiries[1]) {
				t.Fatalf("%s: two values written at one instant both expire %v after it", write.name, expiries[0].Sub(start))
			}
		}
	})
}

// Jitter only ever shortens a value's life, and by at most a tenth of its ttl,
// whatever the random source returns. A value is never served staler than its
// caller's ttl.
func TestHandlerCacheValueTtlJitterBound(t *testing.T) {
	defaultJitter := handlerCacheValueTtlJitter
	defer func() { handlerCacheValueTtlJitter = defaultJitter }()
	for _, span := range []time.Duration{time.Nanosecond, time.Second, 3 * time.Second, 30 * time.Second} {
		for range 1000 {
			if offset := defaultJitter(span); offset < 0 || span < offset {
				t.Fatalf("default jitter for span %v = %v, want within [0, span]", span, offset)
			}
		}
	}
	ctx := context.Background()
	for _, ttl := range []time.Duration{10 * time.Second, 30 * time.Second, time.Minute, 5 * time.Minute} {
		span := ttl / 10
		for _, offset := range []time.Duration{0, span / 2, span, 2 * span, -time.Second} {
			handlerCacheValueTtlJitter = func(time.Duration) time.Duration { return offset }
			want := ttl - min(max(offset, 0), span)
			store := newHandlerCacheExpiringTestStore()
			if _, err := cacheJson(ctx, store, "synthetic-bound", ttl, func() (*handlerCacheTestResult, error) {
				return &handlerCacheTestResult{Message: "published"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := warmCacheJson(ctx, store, "synthetic-bound-warm", ttl, true, func() (*handlerCacheTestResult, error) {
				return &handlerCacheTestResult{Message: "warmed"}, nil
			}); err != nil {
				t.Fatal(err)
			}
			if len(store.publishTtls) != 1 || store.publishTtls[0] != want || len(store.setTtls) != 1 || store.setTtls[0] != want {
				t.Fatalf("ttl %v with jitter %v: published %v, warmed %v, want each %v", ttl, offset, store.publishTtls, store.setTtls, want)
			}
		}
	}
}
