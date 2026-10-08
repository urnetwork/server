package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/session"
)

type renewingCacheTestEntry struct {
	value   string
	expires time.Time
}

// The expiry clock is real Go time inside a synctest bubble. Advancing it
// exercises actual timers and deadlines without sleeping for real seconds.
type renewingCacheTestStore struct {
	mu           sync.Mutex
	entries      map[string]renewingCacheTestEntry
	renewCalls   int
	publishCalls int
	failAt       int
	fault        string
}

func newRenewingCacheTestStore() *renewingCacheTestStore {
	return &renewingCacheTestStore{entries: map[string]renewingCacheTestEntry{}}
}

func (self *renewingCacheTestStore) getLocked(key string) (string, bool) {
	entry, ok := self.entries[key]
	if ok && !time.Now().Before(entry.expires) {
		delete(self.entries, key)
		return "", false
	}
	return entry.value, ok
}

func (self *renewingCacheTestStore) get(_ context.Context, key string) (string, bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	value, ok := self.getLocked(key)
	return value, ok, nil
}

func (self *renewingCacheTestStore) set(_ context.Context, key string, value string, ttl time.Duration, onlyIfAbsent bool) error {
	self.mu.Lock()
	defer self.mu.Unlock()
	if _, ok := self.getLocked(key); !onlyIfAbsent || !ok {
		self.entries[key] = renewingCacheTestEntry{value, time.Now().Add(ttl)}
	}
	return nil
}

func (self *renewingCacheTestStore) acquire(_ context.Context, key string, token string, ttl time.Duration) (bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if _, ok := self.getLocked(key); ok {
		return false, nil
	}
	self.entries[key] = renewingCacheTestEntry{token, time.Now().Add(ttl)}
	return true, nil
}

func (self *renewingCacheTestStore) publish(_ context.Context, keys handlerCacheKeys, token string, value string, ttl time.Duration) (bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	self.publishCalls++
	if current, ok := self.getLocked(keys.fill); !ok || current != token {
		return false, nil
	}
	self.entries[keys.value] = renewingCacheTestEntry{value, time.Now().Add(ttl)}
	delete(self.entries, keys.fill)
	return true, nil
}

func (self *renewingCacheTestStore) release(_ context.Context, key string, token string) error {
	self.mu.Lock()
	defer self.mu.Unlock()
	if current, ok := self.getLocked(key); ok && current == token {
		delete(self.entries, key)
	}
	return nil
}

func (self *renewingCacheTestStore) renew(ctx context.Context, key string, token string, ttl time.Duration) (bool, error) {
	self.mu.Lock()
	self.renewCalls++
	fault := ""
	if self.renewCalls == self.failAt {
		fault = self.fault
	}
	current, ok := self.getLocked(key)
	if fault == "replacement" {
		self.entries[key] = renewingCacheTestEntry{"replacement-owner", time.Now().Add(ttl)}
		self.mu.Unlock()
		return false, nil
	}
	if !ok || current != token {
		self.mu.Unlock()
		return false, nil
	}
	if fault == "error" {
		self.mu.Unlock()
		return false, errors.New("synthetic renewal error")
	}
	if fault != "timeout" {
		self.entries[key] = renewingCacheTestEntry{token, time.Now().Add(ttl)}
	}
	self.mu.Unlock()
	if fault == "timeout" || fault == "applied-timeout" {
		<-ctx.Done()
		return fault == "applied-timeout", ctx.Err()
	}
	return true, nil
}

func (self *renewingCacheTestStore) counts() (int, int) {
	self.mu.Lock()
	defer self.mu.Unlock()
	return self.renewCalls, self.publishCalls
}

// The unchanged generic cache is the causal control: at 31 seconds a second
// database-producing callback starts while its first callback remains held.
// The opt-in path keeps exactly one callback through 70 seconds and publishes
// the result for the original 30-second response lifetime.
func TestHandlerCacheRenewingSlowFillPairedControl(t *testing.T) {
	for _, renewing := range []bool{false, true} {
		name := "baseline"
		if renewing {
			name = "renewing"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newRenewingCacheTestStore()
				ctx := context.Background()
				key := "provider-list-same-network-same-arguments"
				call := func(fill func(context.Context) (*handlerCacheTestResult, error)) (*handlerCacheTestResult, error) {
					if renewing {
						return cacheJsonWithRenewingFill(ctx, store, key, 30*time.Second, 2*time.Minute, fill)
					}
					return cacheJson(ctx, store, key, 30*time.Second, func() (*handlerCacheTestResult, error) { return fill(ctx) })
				}
				started, finish := make(chan struct{}), make(chan struct{})
				releaseOwner := sync.OnceFunc(func() { close(finish) })
				defer releaseOwner()
				owner := make(chan handlerCacheTestCallResult, 1)
				var fills atomic.Int64
				go func() {
					value, err := call(func(context.Context) (*handlerCacheTestResult, error) {
						fills.Add(1)
						close(started)
						<-finish
						return &handlerCacheTestResult{Message: "owner"}, nil
					})
					owner <- handlerCacheTestCallResult{value, err}
				}()
				<-started
				time.Sleep(31 * time.Second)
				for range 8 {
					value, err := call(func(context.Context) (*handlerCacheTestResult, error) {
						fills.Add(1)
						return &handlerCacheTestResult{Message: "replacement"}, nil
					})
					if renewing {
						var unavailable *handlerCacheUnavailableError
						if value != nil || !errors.As(err, &unavailable) {
							t.Fatalf("renewing overlapping result = (%v, %v)", value, err)
						}
					} else if err != nil || value == nil || value.Message != "replacement" {
						t.Fatalf("baseline replacement = (%v, %v)", value, err)
					}
				}
				want := int64(2)
				if renewing {
					want = 1
				}
				if fills.Load() != want {
					t.Fatalf("at 31s: fills=%d want=%d", fills.Load(), want)
				}
				time.Sleep(39 * time.Second)
				releaseOwner()
				result := <-owner
				if renewing {
					if result.err != nil || result.value == nil || result.value.Message != "owner" {
						t.Fatalf("70s owner = (%v, %v)", result.value, result.err)
					}
					before, publishes := store.counts()
					time.Sleep(29 * time.Second)
					if _, exists, _ := store.get(ctx, newHandlerCacheKeys(key).value); !exists {
						t.Fatal("response expired before its unchanged 30s lifetime")
					}
					time.Sleep(2 * time.Second)
					if _, exists, _ := store.get(ctx, newHandlerCacheKeys(key).value); exists {
						t.Fatal("response freshness was extended by fill renewal")
					}
					if after, finalPublishes := store.counts(); before != after || publishes != 1 || finalPublishes != 1 {
						t.Fatal("successful fill left renewal or publication running")
					}
				} else if result.value != nil || result.err == nil {
					t.Fatal("expired baseline owner published after its replacement also expired")
				}
			})
		})
	}
}

func TestHandlerCacheRenewingFailureCancelsFillAndRejectsStaleValue(t *testing.T) {
	for _, fault := range []string{"replacement", "error", "timeout", "applied-timeout"} {
		t.Run(fault, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newRenewingCacheTestStore()
				store.failAt, store.fault = 2, fault
				start := time.Now()
				canceled := false
				value, err := cacheJsonWithRenewingFill(context.Background(), store, "failure", 30*time.Second, 2*time.Minute,
					func(ctx context.Context) (*handlerCacheTestResult, error) {
						<-ctx.Done()
						canceled = true
						// Even a callback that ignores its cancellation cannot publish.
						return &handlerCacheTestResult{Message: "stale"}, nil
					})
				var unavailable *handlerCacheUnavailableError
				if value != nil || !errors.As(err, &unavailable) || !canceled || time.Since(start) > 12*time.Second {
					t.Fatalf("failure=%s result=(%v,%v), canceled=%v elapsed=%v", fault, value, err, canceled, time.Since(start))
				}
				renews, publishes := store.counts()
				if renews != 2 || publishes != 0 {
					t.Fatalf("failure continued work: renew=%d publish=%d", renews, publishes)
				}
				if token, exists, _ := store.get(context.Background(), newHandlerCacheKeys("failure").fill); !exists || (fault == "replacement" && token != "replacement-owner") {
					t.Fatal("failed owner removed the retry fence or replacement token")
				}
			})
		})
	}
}

func TestHandlerCacheRenewingInitialFailureDoesNotStartFill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRenewingCacheTestStore()
		store.failAt, store.fault = 1, "applied-timeout"
		called := false
		value, err := cacheJsonWithRenewingFill(context.Background(), store, "initial-failure", 30*time.Second, 2*time.Minute,
			func(context.Context) (*handlerCacheTestResult, error) { called = true; return nil, nil })
		if value != nil || err == nil || called {
			t.Fatalf("uncertain initial lease started work: (%v,%v) called=%v", value, err, called)
		}
	})
}

func TestHandlerCacheRenewingCancellationAndBudgetLeaveExpiringFence(t *testing.T) {
	for _, control := range []struct {
		name          string
		callerCancel  bool
		callerTimeout time.Duration
		elapsed       time.Duration
		wantErr       error
	}{
		{"fill-budget", false, 0, 2 * time.Minute, context.DeadlineExceeded},
		{"caller-cancel", true, 0, time.Second, context.Canceled},
		{"earlier-caller-deadline", false, 45 * time.Second, 45 * time.Second, context.DeadlineExceeded},
	} {
		t.Run(control.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				store := newRenewingCacheTestStore()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if control.callerTimeout != 0 {
					var deadlineCancel context.CancelFunc
					ctx, deadlineCancel = context.WithTimeout(ctx, control.callerTimeout)
					defer deadlineCancel()
				}
				if control.callerCancel {
					go func() { time.Sleep(time.Second); cancel() }()
				}
				start := time.Now()
				value, err := cacheJsonWithRenewingFill(ctx, store, "bounded", 30*time.Second, 2*time.Minute,
					func(fillCtx context.Context) (*handlerCacheTestResult, error) {
						<-fillCtx.Done()
						return nil, fillCtx.Err()
					})
				if value != nil || !errors.Is(err, control.wantErr) || time.Since(start) != control.elapsed {
					t.Fatalf("bounded result=(%v,%v) elapsed=%v", value, err, time.Since(start))
				}
				before, _ := store.counts()
				if _, exists, _ := store.get(context.Background(), newHandlerCacheKeys("bounded").fill); !exists {
					t.Fatal("canceled owner released its retry fence")
				}
				time.Sleep(31 * time.Second)
				after, _ := store.counts()
				if before != after {
					t.Fatal("renewal goroutine survived the joined fill")
				}
				value, err = cacheJsonWithRenewingFill(context.Background(), store, "bounded", 30*time.Second, 2*time.Minute,
					func(context.Context) (*handlerCacheTestResult, error) {
						return &handlerCacheTestResult{Message: "recovered"}, nil
					})
				if err != nil || value == nil || value.Message != "recovered" {
					t.Fatalf("expiry recovery = (%v,%v)", value, err)
				}
			})
		})
	}
}

func TestHandlerCacheRenewingColdEmptyAndNilResults(t *testing.T) {
	for _, nilResult := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty-providers", true: "nil-refused"}[nilResult], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				type response struct {
					Providers []string `json:"providers"`
				}
				store := newRenewingCacheTestStore()
				calls := 0
				for range 2 {
					value, err := cacheJsonWithRenewingFill(context.Background(), store, "cold-empty", 30*time.Second, 2*time.Minute,
						func(context.Context) (*response, error) {
							calls++
							if nilResult {
								return nil, nil
							}
							return &response{Providers: []string{}}, nil
						})
					if nilResult {
						var unavailable *handlerCacheUnavailableError
						if value != nil || !errors.As(err, &unavailable) {
							t.Fatalf("nil became a cold success: (%v,%v)", value, err)
						}
					} else if err != nil || value == nil || value.Providers == nil || len(value.Providers) != 0 {
						t.Fatalf("valid empty provider list changed: (%v,%v)", value, err)
					}
				}
				if calls != 1 {
					t.Fatalf("cold outcome retried within its response/failure lifetime: %d", calls)
				}
			})
		})
	}
}

func TestHandlerCacheRenewingPanicJoinsOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRenewingCacheTestStore()
		func() {
			defer func() {
				if recover() != "synthetic fill panic" {
					t.Fatal("fill panic boundary changed")
				}
			}()
			_, _ = cacheJsonWithRenewingFill(context.Background(), store, "panic", 30*time.Second, 2*time.Minute,
				func(context.Context) (*handlerCacheTestResult, error) { panic("synthetic fill panic") })
		}()
		before, publishes := store.counts()
		time.Sleep(time.Minute)
		after, _ := store.counts()
		if before != after || publishes != 0 {
			t.Fatal("panicked fill left a renewal or publication running")
		}
	})
}

func TestHandlerCacheRenewingNetworkInputAndSessionScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRenewingCacheTestStore()
		prior := defaultHandlerCacheStore
		defaultHandlerCacheStore = store
		defer func() { defaultHandlerCacheStore = prior }()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		original := &session.ClientSession{Ctx: ctx, Cancel: cancel, ByJwt: &jwt.ByJwt{NetworkId: server.NewId()}}
		other := &session.ClientSession{Ctx: ctx, Cancel: cancel, ByJwt: &jwt.ByJwt{NetworkId: server.NewId()}}
		fills := 0
		impl := CacheWithNetworkAuthInputRenewingFill(func(hours int, owned *session.ClientSession) (*handlerCacheTestResult, error) {
			fills++
			if owned == original || owned.Ctx == original.Ctx || owned.ByJwt == nil {
				t.Fatal("fill did not receive an isolated session context")
			}
			deadline, ok := owned.Ctx.Deadline()
			if !ok || deadline.Sub(time.Now()) != 2*time.Minute {
				t.Fatal("fill budget did not reach the actual session context")
			}
			owned.Cancel()
			return &handlerCacheTestResult{Message: "scoped"}, nil
		}, "provider-list", 30*time.Second, 2*time.Minute)
		for _, request := range []struct {
			hours int
			s     *session.ClientSession
		}{{24, original}, {24, original}, {48, original}, {24, other}} {
			value, err := impl(request.hours, request.s)
			if err != nil || value == nil {
				t.Fatalf("scoped result=(%v,%v)", value, err)
			}
		}
		if fills != 3 || original.Ctx != ctx || ctx.Err() != nil {
			t.Fatalf("scope/session mutated: fills=%d callerErr=%v", fills, ctx.Err())
		}
	})
}
