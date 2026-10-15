package router

// handler_cache_test.go pins the distributed cold-cache boundary without a
// live Redis server or scheduler timing. Explicit state transitions model
// leases, expiry, and ambiguous network responses.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type handlerCacheTestStore struct {
	stateLock sync.Mutex

	values     map[string]string
	fillTokens map[string]string

	acquireErr     error
	acquireApplied bool
	publishErr     error
	publishApplied bool
	setErr         error

	acquireTtls []time.Duration
}

// Starts with empty value and lease namespaces.
func newHandlerCacheTestStore() *handlerCacheTestStore {
	return &handlerCacheTestStore{
		values:     map[string]string{},
		fillTokens: map[string]string{},
	}
}

// Returns a consistent snapshot of one namespace entry.
func (self *handlerCacheTestStore) get(
	_ context.Context,
	key string,
) (string, bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if value, ok := self.values[key]; ok {
		return value, true, nil
	}
	value, ok := self.fillTokens[key]
	return value, ok, nil
}

// Models SET and SETNX for explicit warm calls.
func (self *handlerCacheTestStore) set(
	_ context.Context,
	key string,
	value string,
	_ time.Duration,
	onlyIfAbsent bool,
) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.setErr != nil {
		return self.setErr
	}
	if _, ok := self.values[key]; !onlyIfAbsent || !ok {
		self.values[key] = value
	}
	return nil
}

// Models a unique distributed lease and can reproduce SETNX applying while
// its response is lost.
func (self *handlerCacheTestStore) acquire(
	_ context.Context,
	key string,
	token string,
	ttl time.Duration,
) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.acquireTtls = append(self.acquireTtls, ttl)
	if _, ok := self.fillTokens[key]; ok {
		return false, nil
	}
	if self.acquireErr != nil {
		if self.acquireApplied {
			self.fillTokens[key] = token
		}
		return false, self.acquireErr
	}
	self.fillTokens[key] = token
	return true, nil
}

// Models the token-guarded atomic publish script and its ambiguous-response
// boundary.
func (self *handlerCacheTestStore) publish(
	_ context.Context,
	keys handlerCacheKeys,
	token string,
	value string,
	_ time.Duration,
) (bool, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.fillTokens[keys.fill] != token {
		return false, nil
	}
	if self.publishErr != nil {
		if self.publishApplied {
			self.values[keys.value] = value
			delete(self.fillTokens, keys.fill)
		}
		return false, self.publishErr
	}
	self.values[keys.value] = value
	delete(self.fillTokens, keys.fill)
	return true, nil
}

// Removes only the lease still owned by this request.
func (self *handlerCacheTestStore) release(
	_ context.Context,
	key string,
	token string,
) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.fillTokens[key] == token {
		delete(self.fillTokens, key)
	}
	return nil
}

// Simulates Redis ttl expiry without a wall-clock wait.
func (self *handlerCacheTestStore) expireFill(key string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.fillTokens, key)
}

// Reports whether a fill fence remains active.
func (self *handlerCacheTestStore) hasFill(key string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	_, ok := self.fillTokens[key]
	return ok
}

type handlerCacheTestResult struct {
	Message string `json:"message"`
}

type handlerCacheTestCallResult struct {
	value *handlerCacheTestResult
	err   error
}

// A held fill is the deterministic concurrency barrier. Every overlapping
// miss must skip the database-producing callback, while the winner publishes
// one value that all later requests reuse.
func TestHandlerCacheCoalescesConcurrentMisses(t *testing.T) {
	ctx := context.Background()
	store := newHandlerCacheTestStore()
	logicalKey := "synthetic-cache-key"
	fillStarted := make(chan struct{})
	allowFill := make(chan struct{})
	winnerResult := make(chan handlerCacheTestCallResult, 1)
	var fillCount atomic.Int64

	go func() {
		value, err := cacheJson(ctx, store, logicalKey, 30*time.Second, func() (*handlerCacheTestResult, error) {
			fillCount.Add(1)
			close(fillStarted)
			<-allowFill
			return &handlerCacheTestResult{Message: "published"}, nil
		})
		winnerResult <- handlerCacheTestCallResult{value: value, err: err}
	}()

	<-fillStarted
	loserValue, loserErr := cacheJson(ctx, store, logicalKey, 30*time.Second, func() (*handlerCacheTestResult, error) {
		fillCount.Add(1)
		return &handlerCacheTestResult{Message: "duplicate"}, nil
	})
	if loserValue != nil {
		t.Fatalf("overlapping loser value = %#v, want nil", loserValue)
	}
	var unavailableErr *handlerCacheUnavailableError
	if !errors.As(loserErr, &unavailableErr) {
		t.Fatalf("overlapping loser error = %v, want cache-unavailable error", loserErr)
	}

	close(allowFill)
	winner := <-winnerResult
	if winner.err != nil || winner.value == nil || winner.value.Message != "published" {
		t.Fatalf("winner result = (%#v, %v), want published value", winner.value, winner.err)
	}

	cachedValue, cachedErr := cacheJson(ctx, store, logicalKey, 30*time.Second, func() (*handlerCacheTestResult, error) {
		fillCount.Add(1)
		return &handlerCacheTestResult{Message: "unexpected-refill"}, nil
	})
	if cachedErr != nil || cachedValue == nil || cachedValue.Message != "published" {
		t.Fatalf("cached result = (%#v, %v), want published value", cachedValue, cachedErr)
	}
	if got := fillCount.Load(); got != 1 {
		t.Fatalf("fill calls = %d, want 1", got)
	}
	if len(store.acquireTtls) != 2 || store.acquireTtls[0] != 30*time.Second || store.acquireTtls[1] != 30*time.Second {
		t.Fatalf("acquire ttls = %v, want two 30s attempts", store.acquireTtls)
	}
}

// A failed producer retains its fence until Redis expires it. That bounds a
// failing database query to one attempt per cache ttl instead of one per
// incoming request.
func TestHandlerCacheFailedFillRetriesOnlyAfterLeaseExpiry(t *testing.T) {
	ctx := context.Background()
	store := newHandlerCacheTestStore()
	logicalKey := "synthetic-failure-key"
	keys := newHandlerCacheKeys(logicalKey)
	fillErr := errors.New("synthetic fill failure")
	fillCount := 0

	_, err := cacheJson(ctx, store, logicalKey, 15*time.Second, func() (*handlerCacheTestResult, error) {
		fillCount++
		return nil, fillErr
	})
	if !errors.Is(err, fillErr) {
		t.Fatalf("first fill error = %v, want synthetic failure", err)
	}
	if !store.hasFill(keys.fill) {
		t.Fatal("failed fill released its retry-suppression lease")
	}

	_, err = cacheJson(ctx, store, logicalKey, 15*time.Second, func() (*handlerCacheTestResult, error) {
		fillCount++
		return &handlerCacheTestResult{Message: "too-early"}, nil
	})
	var unavailableErr *handlerCacheUnavailableError
	if !errors.As(err, &unavailableErr) {
		t.Fatalf("pre-expiry error = %v, want cache-unavailable error", err)
	}
	if fillCount != 1 {
		t.Fatalf("pre-expiry fill calls = %d, want 1", fillCount)
	}

	store.expireFill(keys.fill)
	value, err := cacheJson(ctx, store, logicalKey, 15*time.Second, func() (*handlerCacheTestResult, error) {
		fillCount++
		return &handlerCacheTestResult{Message: "recovered"}, nil
	})
	if err != nil || value == nil || value.Message != "recovered" {
		t.Fatalf("post-expiry result = (%#v, %v), want recovered", value, err)
	}
	if fillCount != 2 {
		t.Fatalf("post-expiry fill calls = %d, want 2", fillCount)
	}
}

// A publication failure must not turn an unshared in-process value into a
// successful response. The retained fence protects the database until retry.
func TestHandlerCachePublishFailureReturnsUnavailableAndRetainsLease(t *testing.T) {
	ctx := context.Background()
	store := newHandlerCacheTestStore()
	store.publishErr = errors.New("synthetic publish failure")
	logicalKey := "synthetic-publish-key"
	keys := newHandlerCacheKeys(logicalKey)
	fillCount := 0

	value, err := cacheJson(ctx, store, logicalKey, time.Minute, func() (*handlerCacheTestResult, error) {
		fillCount++
		return &handlerCacheTestResult{Message: "not-published"}, nil
	})
	var unavailableErr *handlerCacheUnavailableError
	if value != nil || !errors.As(err, &unavailableErr) {
		t.Fatalf("publication failure result = (%#v, %v), want unavailable", value, err)
	}
	if fillCount != 1 || !store.hasFill(keys.fill) {
		t.Fatalf("publication failure state: fills=%d lease=%v, want 1 and retained", fillCount, store.hasFill(keys.fill))
	}

	_, err = cacheJson(ctx, store, logicalKey, time.Minute, func() (*handlerCacheTestResult, error) {
		fillCount++
		return &handlerCacheTestResult{Message: "duplicate"}, nil
	})
	if !errors.As(err, &unavailableErr) || fillCount != 1 {
		t.Fatalf("guarded retry = (fills=%d, err=%v), want one fill and unavailable", fillCount, err)
	}
}

// A lease can expire while its slow owner is still computing. The stale owner
// must return the replacement's published value and must never overwrite it.
func TestHandlerCacheExpiredOwnerCannotOverwriteReplacement(t *testing.T) {
	ctx := context.Background()
	store := newHandlerCacheTestStore()
	logicalKey := "synthetic-expired-owner"
	keys := newHandlerCacheKeys(logicalKey)
	firstFillStarted := make(chan struct{})
	allowFirstFill := make(chan struct{})
	firstResult := make(chan handlerCacheTestCallResult, 1)

	go func() {
		value, err := cacheJson(ctx, store, logicalKey, time.Minute, func() (*handlerCacheTestResult, error) {
			close(firstFillStarted)
			<-allowFirstFill
			return &handlerCacheTestResult{Message: "stale"}, nil
		})
		firstResult <- handlerCacheTestCallResult{value: value, err: err}
	}()

	<-firstFillStarted
	store.expireFill(keys.fill)
	replacement, err := cacheJson(ctx, store, logicalKey, time.Minute, func() (*handlerCacheTestResult, error) {
		return &handlerCacheTestResult{Message: "replacement"}, nil
	})
	if err != nil || replacement == nil || replacement.Message != "replacement" {
		t.Fatalf("replacement result = (%#v, %v), want replacement", replacement, err)
	}

	close(allowFirstFill)
	staleOwner := <-firstResult
	if staleOwner.err != nil || staleOwner.value == nil || staleOwner.value.Message != "replacement" {
		t.Fatalf("expired owner result = (%#v, %v), want authoritative replacement", staleOwner.value, staleOwner.err)
	}
}

// Explicit warm operations report a failed cache write instead of claiming a
// value was made available to other API processes.
func TestHandlerCacheWarmWriteFailureReturnsUnavailable(t *testing.T) {
	store := newHandlerCacheTestStore()
	store.setErr = errors.New("synthetic warm write failure")
	value, err := warmCacheJson(context.Background(), store, "synthetic-warm-key", time.Minute, true, func() (*handlerCacheTestResult, error) {
		return &handlerCacheTestResult{Message: "not-stored"}, nil
	})
	var unavailableErr *handlerCacheUnavailableError
	if value != nil || !errors.As(err, &unavailableErr) {
		t.Fatalf("warm write failure = (%#v, %v), want unavailable", value, err)
	}
}

// SETNX may apply even when its network response is lost. Reading back the
// unique owner token distinguishes ownership from an ordinary Redis failure.
func TestHandlerCacheRecoversAmbiguousLeaseAcquisition(t *testing.T) {
	ctx := context.Background()
	store := newHandlerCacheTestStore()
	store.acquireErr = errors.New("synthetic lost acquire response")
	store.acquireApplied = true

	value, err := cacheJson(ctx, store, "synthetic-ambiguous-acquire", time.Minute, func() (*handlerCacheTestResult, error) {
		return &handlerCacheTestResult{Message: "owned"}, nil
	})
	if err != nil || value == nil || value.Message != "owned" {
		t.Fatalf("ambiguous acquisition result = (%#v, %v), want owned value", value, err)
	}
}

// The atomic publish can likewise succeed before its response is lost. A
// cache read is the authority that makes returning the result safe.
func TestHandlerCacheRecoversAmbiguousPublication(t *testing.T) {
	ctx := context.Background()
	store := newHandlerCacheTestStore()
	store.publishErr = errors.New("synthetic lost publish response")
	store.publishApplied = true

	value, err := cacheJson(ctx, store, "synthetic-ambiguous-publish", time.Minute, func() (*handlerCacheTestResult, error) {
		return &handlerCacheTestResult{Message: "published"}, nil
	})
	if err != nil || value == nil || value.Message != "published" {
		t.Fatalf("ambiguous publication result = (%#v, %v), want published value", value, err)
	}
}

// Redis Cluster scripts require every key to carry the same hash tag. The
// digest also keeps the logical identity out of operational key listings.
func TestHandlerCacheKeysSharePrivateClusterHashTag(t *testing.T) {
	logicalKey := "synthetic-logical-identity"
	keys := newHandlerCacheKeys(logicalKey)
	hashTag := func(key string) string {
		open := strings.IndexByte(key, '{')
		close := strings.IndexByte(key, '}')
		if open < 0 || close <= open+1 {
			return ""
		}
		return key[open+1 : close]
	}
	if valueTag, fillTag := hashTag(keys.value), hashTag(keys.fill); valueTag == "" || valueTag != fillTag {
		t.Fatalf("cache key hash tags = (%q, %q), want one shared nonempty tag", valueTag, fillTag)
	}
	if strings.Contains(keys.value, logicalKey) || strings.Contains(keys.fill, logicalKey) {
		t.Fatalf("physical cache keys expose logical identity: %#v", keys)
	}
}

// HTTP clients receive an explicit transient status and bounded retry hint;
// a skipped fill must never be serialized as a successful null response.
func TestHandlerCacheUnavailableHttpResponse(t *testing.T) {
	w := httptest.NewRecorder()
	statusError := RaiseHttpError(&handlerCacheUnavailableError{}, w)
	if !statusError || w.Code != http.StatusServiceUnavailable {
		t.Fatalf("cache-unavailable status = (%v, %d), want (true, 503)", statusError, w.Code)
	}
	if got, want := w.Header().Get("Retry-After"), "5"; got != want {
		t.Fatalf("Retry-After = %q, want %q", got, want)
	}
	if got, want := strings.TrimSpace(w.Body.String()), "Cached response is temporarily unavailable."; got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}
}
