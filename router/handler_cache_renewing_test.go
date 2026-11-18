package router

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server"

	"github.com/urnetwork/server/session"
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

// Models the fence script: only the current token's lease, only toward
// expiry, marked fenced.
func (self *renewingCacheTestStore) fence(_ context.Context, key string, token string, ttl time.Duration) (bool, error) {
	self.mu.Lock()
	defer self.mu.Unlock()
	current, ok := self.getLocked(key)
	if !ok || current != token {
		return false, nil
	}
	expires := self.entries[key].expires
	if fenced := time.Now().Add(ttl); fenced.Before(expires) {
		expires = fenced
	}
	self.entries[key] = renewingCacheTestEntry{handlerCacheFenceToken(token), expires}
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
// the result for four times its 70-second fill (renewingHandlerCacheValueTtl):
// lease renewal itself extends no freshness.
func TestHandlerCacheRenewingSlowFillPairedControl(t *testing.T) {
	// The exact 30s lifetime below needs the value ttl jitter pinned to zero;
	// TestHandlerCacheValueTtlJitterBound covers the jitter itself.
	prior := handlerCacheValueTtlJitter
	defer func() { handlerCacheValueTtlJitter = prior }()
	handlerCacheValueTtlJitter = func(time.Duration) time.Duration { return 0 }
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
				begin := time.Now()
				time.Sleep(31 * time.Second)
				// Overlapping misses wait for the fill in progress: a renewed
				// lease outlasts their wait, and they answer 503 after it.
				var overlapping sync.WaitGroup
				results := make([]handlerCacheTestCallResult, 8)
				for i := range results {
					overlapping.Add(1)
					go func() {
						defer overlapping.Done()
						value, err := call(func(context.Context) (*handlerCacheTestResult, error) {
							fills.Add(1)
							return &handlerCacheTestResult{Message: "replacement"}, nil
						})
						results[i] = handlerCacheTestCallResult{value, err}
					}()
				}
				overlapping.Wait()
				for _, result := range results {
					if renewing {
						var unavailable *handlerCacheUnavailableError
						if result.value != nil || !errors.As(result.err, &unavailable) {
							t.Fatalf("renewing overlapping result = (%v, %v)", result.value, result.err)
						}
					} else if result.err != nil || result.value == nil || result.value.Message != "replacement" {
						t.Fatalf("baseline replacement = (%v, %v)", result.value, result.err)
					}
				}
				want := int64(2)
				if renewing {
					want = 1
				}
				if fills.Load() != want {
					t.Fatalf("at 31s: fills=%d want=%d", fills.Load(), want)
				}
				time.Sleep(70*time.Second - time.Since(begin))
				releaseOwner()
				result := <-owner
				if renewing {
					if result.err != nil || result.value == nil || result.value.Message != "owner" {
						t.Fatalf("70s owner = (%v, %v)", result.value, result.err)
					}
					before, publishes := store.counts()
					time.Sleep(4*70*time.Second - time.Second)
					if _, exists, _ := store.get(ctx, newHandlerCacheKeys(key).value); !exists {
						t.Fatal("response expired before four times its 70s fill")
					}
					time.Sleep(2 * time.Second)
					if _, exists, _ := store.get(ctx, newHandlerCacheKeys(key).value); exists {
						t.Fatal("response freshness was extended past its fill cost by fill renewal")
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

// The owner budget, not the caller, bounds a fill. A caller that cancels or
// whose own deadline passes stops waiting at once, while its fill keeps the
// lease to the end of the budget, leaves the failure fence and joins its
// renewal; a miss after the fence recovers.
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
				fillEnded := make(chan time.Duration, 1)
				value, err := cacheJsonWithRenewingFill(ctx, store, "bounded", 30*time.Second, 2*time.Minute,
					func(fillCtx context.Context) (*handlerCacheTestResult, error) {
						<-fillCtx.Done()
						fillEnded <- time.Since(start)
						return nil, fillCtx.Err()
					})
				if value != nil || !errors.Is(err, control.wantErr) || time.Since(start) != control.elapsed {
					t.Fatalf("bounded result=(%v,%v) elapsed=%v", value, err, time.Since(start))
				}
				time.Sleep(2*time.Minute - time.Since(start))
				synctest.Wait()
				select {
				case ended := <-fillEnded:
					if ended != 2*time.Minute {
						t.Fatalf("fill ended after %v, want its 2m owner budget", ended)
					}
				default:
					t.Fatal("fill outlived its owner budget")
				}
				before, _ := store.counts()
				if _, exists, _ := store.get(context.Background(), newHandlerCacheKeys("bounded").fill); !exists {
					t.Fatal("ended owner released its retry fence")
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
		original := &session.ClientSession{Ctx: ctx, Cancel: cancel, ByJwt: &session.ByJwt{NetworkId: server.NewId()}}
		other := &session.ClientSession{Ctx: ctx, Cancel: cancel, ByJwt: &session.ByJwt{NetworkId: server.NewId()}}
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

// A fill that takes the given time unless its context ends first.
func renewingCacheTestFillTaking(fills *atomic.Int64, d time.Duration, message string) func(context.Context) (*handlerCacheTestResult, error) {
	return func(fillCtx context.Context) (*handlerCacheTestResult, error) {
		fills.Add(1)
		select {
		case <-time.After(d):
			return &handlerCacheTestResult{Message: message}, nil
		case <-fillCtx.Done():
			return nil, fillCtx.Err()
		}
	}
}

// One request of a caller that gives up after patience, and how long it took.
func renewingCacheTestCall(
	store handlerCacheRenewingStore,
	key string,
	patience time.Duration,
	fill func(context.Context) (*handlerCacheTestResult, error),
) (*handlerCacheTestResult, error, time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), patience)
	defer cancel()
	start := time.Now()
	value, err := cacheJsonWithRenewingFill(ctx, store, key, 30*time.Second, 2*time.Minute, fill)
	return value, err, time.Since(start)
}

// The root cause of GET /stats/providers answering 503 to every request of a
// large network (2026-11-16..18): a provider list slower than its callers'
// patience was canceled with each caller and never published. Here every
// caller gives up after 10 s (as a client does; the lb cuts at 30 s) and the
// list takes 20 s. The fill must outlive its first caller and publish, so a
// later caller is answered at once from that one fill.
func TestHandlerCacheRenewingFillOutlivesItsCaller(t *testing.T) {
	prior := handlerCacheValueTtlJitter
	defer func() { handlerCacheValueTtlJitter = prior }()
	handlerCacheValueTtlJitter = func(time.Duration) time.Duration { return 0 }
	synctest.Test(t, func(t *testing.T) {
		store := newRenewingCacheTestStore()
		var fills atomic.Int64
		fill := renewingCacheTestFillTaking(&fills, 20*time.Second, "providers")
		value, err, elapsed := renewingCacheTestCall(store, "slow-provider-list", 10*time.Second, fill)
		if value != nil || !errors.Is(err, context.DeadlineExceeded) || elapsed != 10*time.Second {
			t.Fatalf("first caller = (%v,%v) after %v", value, err, elapsed)
		}
		time.Sleep(15 * time.Second)
		value, err, elapsed = renewingCacheTestCall(store, "slow-provider-list", 10*time.Second, fill)
		if err != nil || value == nil || value.Message != "providers" || elapsed != 0 {
			t.Fatalf("caller after the fill = (%v,%v) after %v", value, err, elapsed)
		}
		if fills.Load() != 1 {
			t.Fatalf("fills = %d, want the first caller's fill only", fills.Load())
		}
	})
}

// Once a provider list has been published, a refresh slower than a miss's wait
// answers the last published list, to its owner and to overlapping misses,
// instead of 503; the refresh still publishes. A refresh within the wait is
// answered fresh, a fill's own refusal keeps its error, and the last good list
// expires handlerCacheLastGoodTtl after its publication.
func TestHandlerCacheRenewingLastGoodAnswersSlowRefresh(t *testing.T) {
	prior := handlerCacheValueTtlJitter
	defer func() { handlerCacheValueTtlJitter = prior }()
	handlerCacheValueTtlJitter = func(time.Duration) time.Duration { return 0 }
	synctest.Test(t, func(t *testing.T) {
		store := newRenewingCacheTestStore()
		const key = "provider-list-refresh"
		var fills atomic.Int64
		expect := func(name string, value *handlerCacheTestResult, err error, elapsed time.Duration, message string, wantElapsed time.Duration) {
			t.Helper()
			if err != nil || value == nil || value.Message != message || elapsed != wantElapsed {
				t.Fatalf("%s = (%v,%v) after %v, want %q after %v", name, value, err, elapsed, message, wantElapsed)
			}
		}

		value, err, elapsed := renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, time.Second, "first"))
		expect("first list", value, err, elapsed, "first", time.Second)

		// Five minutes later the list has expired and its refresh takes 20 s.
		time.Sleep(5 * time.Minute)
		owner := make(chan handlerCacheTestCallResult, 1)
		go func() {
			value, err, elapsed := renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, 20*time.Second, "second"))
			if elapsed != handlerCacheFillWaitLimit {
				err = errors.Join(err, errors.New("refresh owner did not answer at the wait limit"))
			}
			owner <- handlerCacheTestCallResult{value, err}
		}()
		time.Sleep(time.Second)
		value, err, elapsed = renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, time.Second, "duplicate"))
		expect("overlapping miss", value, err, elapsed, "first", handlerCacheFillWaitLimit)
		result := <-owner
		expect("refresh owner", result.value, result.err, handlerCacheFillWaitLimit, "first", handlerCacheFillWaitLimit)

		// The refresh still publishes: the next miss is answered fresh at once.
		time.Sleep(15 * time.Second)
		value, err, elapsed = renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, time.Second, "unused"))
		expect("after the refresh", value, err, elapsed, "second", 0)
		if fills.Load() != 2 {
			t.Fatalf("fills = %d, want the first list and one refresh", fills.Load())
		}

		// A refresh within the wait is answered fresh, not from the last good list.
		// The 20 s refresh's list lived four times its fill.
		time.Sleep(2 * time.Minute)
		value, err, elapsed = renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, 2*time.Second, "third"))
		expect("quick refresh", value, err, elapsed, "third", 2*time.Second)

		// A fill's own refusal keeps its error; a miss inside its failure fence
		// answers the last good list at once.
		time.Sleep(time.Minute)
		refusal := errors.New("synthetic payout attribution refusal")
		value, err, _ = renewingCacheTestCall(store, key, 10*time.Second, func(context.Context) (*handlerCacheTestResult, error) {
			return nil, refusal
		})
		if value != nil || !errors.Is(err, refusal) {
			t.Fatalf("refused refresh = (%v,%v), want the refusal", value, err)
		}
		value, err, elapsed = renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, time.Second, "fenced"))
		expect("miss in the fence", value, err, elapsed, "third", 0)

		// The last good list lasts handlerCacheLastGoodTtl from its publication.
		time.Sleep(handlerCacheLastGoodTtl)
		value, err, elapsed = renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, 20*time.Second, "late"))
		if value != nil || !errors.Is(err, context.DeadlineExceeded) || elapsed != 10*time.Second {
			t.Fatalf("miss after the last good list expired = (%v,%v) after %v", value, err, elapsed)
		}
		// Join the late fill, which publishes after its caller left.
		time.Sleep(15 * time.Second)
		value, err, elapsed = renewingCacheTestCall(store, key, 10*time.Second, renewingCacheTestFillTaking(&fills, time.Second, "unused"))
		expect("after the late fill", value, err, elapsed, "late", 0)
	})
}

// What the db layer raises when the context of a query ends (server.Db,
// through dbContextDoneCause): its done stop joined with the driver's error
// and the context's own, as the api logged it on 2026-11-18: "Done",
// "timeout: context deadline exceeded", "context deadline exceeded".
func renewingCacheTestDbStop(ctx context.Context) error {
	return errors.Join(server.DbContextDoneError, errors.New("timeout: context deadline exceeded"), ctx.Err())
}

// A fill whose database read outlasts the context it runs under, which
// queryContext derives from the fill context, and raises the db layer's stop
// when that context ends.
func renewingCacheTestDbReadOutlasting(
	queryContext func(context.Context) (context.Context, context.CancelFunc),
) func(context.Context) (*handlerCacheTestResult, error) {
	return func(fillCtx context.Context) (*handlerCacheTestResult, error) {
		ctx, cancel := queryContext(fillCtx)
		defer cancel()
		<-ctx.Done()
		panic(renewingCacheTestDbStop(ctx))
	}
}

// The root cause of "[h]renewing cache fill panic: Done" every ~3 minutes on
// 2026-11-18: the largest network's provider list reached its 2-minute owner
// budget inside a database read, and the db layer ended the read by raising
// its done stop. The fill ends with the budget's error instead: a caller that
// waits past the budget is answered context.DeadlineExceeded, not the raise,
// and the lease is cut to its failure fence with renewal joined. A lost
// renewal ends the same read as unavailable. The control: the same stop for a
// narrower context of the fill's own, while the fill context is live, is
// still raised unchanged.
func TestHandlerCacheRenewingDbStopAtFillEndReturnsError(t *testing.T) {
	call := func(store *renewingCacheTestStore, key string, fill func(context.Context) (*handlerCacheTestResult, error)) (value *handlerCacheTestResult, err error, raised any) {
		defer func() { raised = recover() }()
		value, err = cacheJsonWithRenewingFill(context.Background(), store, key, 30*time.Second, 2*time.Minute, fill)
		return
	}
	fillContext := func(ctx context.Context) (context.Context, context.CancelFunc) { return context.WithCancel(ctx) }
	t.Run("budget", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := newRenewingCacheTestStore()
			start := time.Now()
			value, err, raised := call(store, "db-budget", renewingCacheTestDbReadOutlasting(fillContext))
			if raised != nil || value != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 2*time.Minute {
				t.Fatalf("db read at the fill budget = (%v,%v) raised=%v after %v, want context.DeadlineExceeded after 2m", value, err, raised, time.Since(start))
			}
			token, exists, _ := store.get(context.Background(), newHandlerCacheKeys("db-budget").fill)
			if !exists || !isHandlerCacheFence(token) {
				t.Fatalf("budget-ended fill left lease %q (exists=%v), want its failure fence", token, exists)
			}
			renews, publishes := store.counts()
			time.Sleep(time.Minute)
			if after, _ := store.counts(); after != renews || publishes != 0 {
				t.Fatal("budget-ended fill left a renewal or publication running")
			}
		})
	})
	t.Run("lost-renewal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := newRenewingCacheTestStore()
			store.failAt, store.fault = 2, "replacement"
			value, err, raised := call(store, "db-lost-renewal", renewingCacheTestDbReadOutlasting(fillContext))
			var unavailable *handlerCacheUnavailableError
			if raised != nil || value != nil || !errors.As(err, &unavailable) {
				t.Fatalf("db read ended by a lost renewal = (%v,%v) raised=%v, want unavailable", value, err, raised)
			}
		})
	})
	t.Run("narrower-context", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			store := newRenewingCacheTestStore()
			value, err, raised := call(store, "db-own-timeout", renewingCacheTestDbReadOutlasting(
				func(ctx context.Context) (context.Context, context.CancelFunc) {
					return context.WithTimeout(ctx, 10*time.Second)
				}))
			stop, ok := raised.(error)
			if !ok || !errors.Is(stop, server.DbContextDoneError) || value != nil || err != nil {
				t.Fatalf("stop of the fill's own narrower context = (%v,%v) raised=%v, want it raised unchanged", value, err, raised)
			}
		})
	})
}

// A fill that panics after its caller left cannot take the process down. The
// panic is logged, and the lease is cut to the failure fence as for a panic
// its caller sees.
func TestHandlerCacheRenewingPanicAfterItsCallerLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := newRenewingCacheTestStore()
		value, err, elapsed := renewingCacheTestCall(store, "late-panic", time.Second, func(context.Context) (*handlerCacheTestResult, error) {
			time.Sleep(3 * time.Second)
			panic("synthetic late fill panic")
		})
		if value != nil || !errors.Is(err, context.DeadlineExceeded) || elapsed != time.Second {
			t.Fatalf("caller = (%v,%v) after %v", value, err, elapsed)
		}
		time.Sleep(3 * time.Second)
		token, exists, _ := store.get(context.Background(), newHandlerCacheKeys("late-panic").fill)
		if !exists || !isHandlerCacheFence(token) {
			t.Fatalf("late panic left lease %q (exists=%v), want its failure fence", token, exists)
		}
		renews, publishes := store.counts()
		time.Sleep(time.Minute)
		if after, _ := store.counts(); after != renews || publishes != 0 {
			t.Fatal("panicked fill left a renewal or publication running")
		}
	})
}

// The root cause of back-to-back fills of the largest provider lists
// (2026-11-18): a value lived the key's 30 s ttl however long its fill took,
// so a once-a-minute poller started a new minute-long fill on every request.
// A value now lives four times as long as its fill took, at most
// handlerCacheRenewingValueTtlLimit; a quick fill keeps the ttl (the control).
// Just inside that lifetime a request is answered at once without a fill.
// Just past it a refresh starts, and while the refresh is slower than the
// wait the last good list answers at the wait limit, not 503.
func TestHandlerCacheRenewingValueLivesWithItsFillCost(t *testing.T) {
	prior := handlerCacheValueTtlJitter
	defer func() { handlerCacheValueTtlJitter = prior }()
	handlerCacheValueTtlJitter = func(time.Duration) time.Duration { return 0 }
	for _, control := range []struct {
		name     string
		fill     time.Duration
		lifetime time.Duration
	}{
		{"quick-fill-keeps-ttl", time.Second, 30 * time.Second},
		{"minute-fill", time.Minute, 4 * time.Minute},
		{"limited", 100 * time.Second, handlerCacheRenewingValueTtlLimit},
	} {
		t.Run(control.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				// joins a fill still running when an assertion ends the test early
				defer time.Sleep(3 * time.Minute)
				store := newRenewingCacheTestStore()
				const key = "provider-list-fill-cost"
				var fills atomic.Int64
				value, err, elapsed := renewingCacheTestCall(store, key, 3*time.Minute, renewingCacheTestFillTaking(&fills, control.fill, "first"))
				if err != nil || value == nil || value.Message != "first" || elapsed != control.fill {
					t.Fatalf("first list = (%v,%v) after %v", value, err, elapsed)
				}

				time.Sleep(control.lifetime - time.Second)
				value, err, elapsed = renewingCacheTestCall(store, key, 3*time.Minute, renewingCacheTestFillTaking(&fills, control.fill, "unused"))
				if err != nil || value == nil || value.Message != "first" || elapsed != 0 || fills.Load() != 1 {
					t.Fatalf("1s inside its %v lifetime = (%v,%v) after %v, fills=%d; want the list at once from its one fill", control.lifetime, value, err, elapsed, fills.Load())
				}

				time.Sleep(2 * time.Second)
				value, err, elapsed = renewingCacheTestCall(store, key, 3*time.Minute, renewingCacheTestFillTaking(&fills, 20*time.Second, "refreshed"))
				if err != nil || value == nil || value.Message != "first" || elapsed != handlerCacheFillWaitLimit || fills.Load() != 2 {
					t.Fatalf("1s past its %v lifetime = (%v,%v) after %v, fills=%d; want the last good list at the wait limit from a refresh", control.lifetime, value, err, elapsed, fills.Load())
				}
				// join the refresh, which publishes after its caller left
				time.Sleep(20 * time.Second)
				value, err, elapsed = renewingCacheTestCall(store, key, 3*time.Minute, renewingCacheTestFillTaking(&fills, control.fill, "unused"))
				if err != nil || value == nil || value.Message != "refreshed" || elapsed != 0 {
					t.Fatalf("after the refresh = (%v,%v) after %v", value, err, elapsed)
				}
			})
		})
	}
}

// A value's lifetime grows with its fill but never past the limit, and stays a
// whole fill budget inside the last good value's 15 minutes, so a refresh
// that starts at its expiry can use its budget while the last good answers.
func TestRenewingHandlerCacheValueTtl(t *testing.T) {
	for _, test := range []struct {
		ttl, budget, fill, want time.Duration
	}{
		{30 * time.Second, 2 * time.Minute, 0, 30 * time.Second},
		{30 * time.Second, 2 * time.Minute, 7 * time.Second, 30 * time.Second},
		{30 * time.Second, 2 * time.Minute, 10 * time.Second, 40 * time.Second},
		{30 * time.Second, 2 * time.Minute, time.Minute, 4 * time.Minute},
		{30 * time.Second, 2 * time.Minute, 2 * time.Minute, handlerCacheRenewingValueTtlLimit},
		{30 * time.Second, 12 * time.Minute, 10 * time.Minute, handlerCacheLastGoodTtl - 12*time.Minute},
		{30 * time.Second, 20 * time.Minute, 10 * time.Minute, 30 * time.Second},
		{10 * time.Minute, 2 * time.Minute, time.Second, 10 * time.Minute},
	} {
		if got := renewingHandlerCacheValueTtl(test.ttl, test.budget, test.fill); got != test.want {
			t.Errorf("ttl=%v budget=%v fill=%v: lifetime %v, want %v", test.ttl, test.budget, test.fill, got, test.want)
		}
		if got := renewingHandlerCacheValueTtl(test.ttl, test.budget, test.fill); test.ttl < got && handlerCacheLastGoodTtl-test.budget < got {
			t.Errorf("ttl=%v budget=%v fill=%v: lifetime %v leaves a refresh less than its budget inside the last good value", test.ttl, test.budget, test.fill, got)
		}
	}
}
