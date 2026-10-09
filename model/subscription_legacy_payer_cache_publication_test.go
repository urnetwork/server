// Modern dispatch must create its reusable proof through ordinary full-index
// registration. A manually warmed cache cannot exercise that ownership path.
package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"
)

// A cold due-only dispatch succeeds without publishing a full-index claim.
// Its registration check then supplies proof for payer and dispatcher reuse.
func TestLegacyPayerRegistrationPublishesColdFullIndexProof(t *testing.T) {
	ctx := t.Context()
	identity := sha256.Sum256([]byte("synthetic modern registration resource"))
	cache := &legacySettlementPayerIndexCache{}
	instant := time.Unix(100, 0)
	now := func() time.Time { return instant }
	cached := func(ctx context.Context) bool { return cache.readyObservation(ctx, identity, now()) }
	dueReads, fullReads := 0, 0
	readDue := func(context.Context) (bool, error) {
		dueReads++
		if dueReads == 1 {
			return true, nil
		}
		return false, context.DeadlineExceeded
	}
	readFull := func(ctx context.Context) (bool, error) {
		return readLegacySettlementPayerIndexesForCache(ctx, cache, func(context.Context) ([sha256.Size]byte, error) {
			return identity, nil
		}, now, func(context.Context) (bool, error) {
			fullReads++
			if fullReads != 1 {
				return false, context.DeadlineExceeded
			}
			instant = instant.Add(37 * time.Millisecond)
			return true, nil
		})
	}
	dispatch := observeLegacySettlementPayerIndexWithCache(ctx, cached, readDue, now)
	if dispatch.Outcome != "ready" || dispatch.Cached || dueReads != 1 || cached(ctx) {
		t.Fatal("due-only dispatch published unvalidated full-index proof", dispatch, dueReads)
	}
	started := instant
	registration := observeLegacySettlementPayerIndexWithCache(ctx, cached, readFull, now)
	if registration.Outcome != "ready" || registration.Cached || fullReads != 1 {
		t.Fatal("cold registration did not perform its one full-index read", registration, fullReads)
	}
	for _, read := range []func(context.Context) (bool, error){readDue, readFull, readDue} {
		observed := observeLegacySettlementPayerIndexWithCache(ctx, cached, read, now)
		if observed.Outcome != "ready" || !observed.Cached || dueReads != 1 || fullReads != 1 {
			t.Fatal("modern registration did not publish its fresh proof for ordinary callers", observed, dueReads, fullReads)
		}
	}
	if cache.expires != started.Add(legacySettlementPayerIndexCacheLifetime) {
		t.Fatal("read duration or proof reuse extended the original five-second expiry", cache.expires)
	}
	instant = cache.expires
	expired := observeLegacySettlementPayerIndexWithCache(ctx, cached, readDue, now)
	if expired.Outcome != "deadline" || expired.Cached || dueReads != 2 || fullReads != 1 {
		t.Fatal("expired registration proof suppressed the unchanged fresh due probe", expired, dueReads, fullReads)
	}
}

// Every nonproof leaves a future due consumer on its own bounded reader. The
// publisher preserves fresh read errors and never promotes a late valid row.
func TestLegacyPayerRegistrationUnknownFullIndexCannotPublish(t *testing.T) {
	for _, kind := range []string{"invalid", "error", "canceled", "late"} {
		cache := &legacySettlementPayerIndexCache{}
		identity := sha256.Sum256([]byte("synthetic refused registration resource"))
		instant := time.Unix(100, 0)
		now := func() time.Time { return instant }
		ctx, cancel := context.WithCancel(t.Context())
		physical := errors.New("synthetic full-index read refusal")
		reads := 0
		ready, err := readLegacySettlementPayerIndexesForCache(ctx, cache, func(context.Context) ([sha256.Size]byte, error) {
			return identity, nil
		}, now, func(context.Context) (bool, error) {
			reads++
			switch kind {
			case "invalid":
				return false, nil
			case "error":
				return false, physical
			case "canceled":
				cancel()
			case "late":
				instant = instant.Add(legacySettlementPayerIndexCacheLifetime)
			}
			return true, nil
		})
		cancel()
		if ready || reads != 1 || (kind == "error" && err != physical) || (kind != "error" && err != nil) {
			t.Fatal("unknown registration read changed its outcome or was repeated", kind, ready, reads, err)
		}
		dueReads := 0
		observed := observeLegacySettlementPayerIndexWithCache(t.Context(), func(ctx context.Context) bool {
			return cache.readyObservation(ctx, identity, now())
		}, func(context.Context) (bool, error) {
			dueReads++
			return false, context.DeadlineExceeded
		}, now)
		if observed.Outcome != "deadline" || observed.Cached || dueReads != 1 || cache.ready {
			t.Fatal("unknown registration supplied a positive proof", kind, observed, dueReads)
		}
	}
}

// A successful proof belongs to the resource that was read, including when a
// later registration uses another resource. Replacement never authorizes both.
func TestLegacyPayerRegistrationPublicationKeepsResourceIdentity(t *testing.T) {
	cache := &legacySettlementPayerIndexCache{}
	first := sha256.Sum256([]byte("synthetic first registration resource"))
	second := sha256.Sum256([]byte("synthetic second registration resource"))
	instant := time.Unix(100, 0)
	now := func() time.Time { return instant }
	reads := 0
	for _, identity := range [][sha256.Size]byte{first, second} {
		ready, err := readLegacySettlementPayerIndexesForCache(t.Context(), cache, func(context.Context) ([sha256.Size]byte, error) {
			return identity, nil
		}, now, func(context.Context) (bool, error) {
			reads++
			return true, nil
		})
		if err != nil || !ready || !cache.readyObservation(t.Context(), identity, now()) {
			t.Fatal("successful registration did not retain exact resource proof", ready, err)
		}
	}
	if reads != 2 || cache.readyObservation(t.Context(), first, now()) || !cache.readyObservation(t.Context(), second, now()) {
		t.Fatal("registration proof crossed resource identity", reads)
	}
}

// One existing loader owns a concurrent full-index check. A second registration
// refuses without waiting or querying; ordinary due-only discovery still runs.
func TestLegacyPayerRegistrationPublicationCoalescesConcurrentRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	cache := &legacySettlementPayerIndexCache{}
	identity := sha256.Sum256([]byte("synthetic concurrent registration resource"))
	instant := time.Unix(100, 0)
	now := func() time.Time { return instant }
	entered, release := make(chan struct{}), make(chan struct{})
	joined := make(chan struct{})
	var releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { releaseRead(); <-joined }()
	type result struct {
		ready bool
		err   error
	}
	finished := make(chan result, 1)
	go func() {
		defer close(joined)
		ready, err := readLegacySettlementPayerIndexesForCache(ctx, cache, func(context.Context) ([sha256.Size]byte, error) {
			return identity, nil
		}, now, func(context.Context) (bool, error) {
			close(entered)
			select {
			case <-release:
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		})
		finished <- result{ready: ready, err: err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("registration never entered the catalog barrier")
	}
	duplicateReads := 0
	ready, err := readLegacySettlementPayerIndexesForCache(ctx, cache, func(context.Context) ([sha256.Size]byte, error) {
		return identity, nil
	}, now, func(context.Context) (bool, error) {
		duplicateReads++
		return true, nil
	})
	dueReads := 0
	due := observeLegacySettlementPayerIndexWithCache(ctx, func(ctx context.Context) bool {
		return cache.readyObservation(ctx, identity, now())
	}, func(context.Context) (bool, error) {
		dueReads++
		return true, nil
	}, now)
	releaseRead()
	owner := <-finished
	if ready || err != nil || duplicateReads != 0 || due.Outcome != "ready" || due.Cached || dueReads != 1 {
		t.Fatal("registration refresh waited, duplicated a read, or supplied unfinished proof", ready, err, duplicateReads, due, dueReads)
	}
	if !owner.ready || owner.err != nil || !cache.readyObservation(ctx, identity, now()) {
		t.Fatal("completed registration did not publish its exact proof", owner)
	}
}

// A resource switch or unreadable identity during the one successful catalog
// read cannot publish proof under either the old or new resource digest.
func TestLegacyPayerRegistrationPublicationRejectsChangedResource(t *testing.T) {
	first := sha256.Sum256([]byte("synthetic admitted registration resource"))
	second := sha256.Sum256([]byte("synthetic replacement registration resource"))
	for _, kind := range []string{"changed", "unreadable"} {
		cache := &legacySettlementPayerIndexCache{}
		instant := time.Unix(100, 0)
		now := func() time.Time { return instant }
		resourceReads, catalogReads := 0, 0
		current := first
		physical := errors.New("synthetic unreadable registration resource")
		ready, err := readLegacySettlementPayerIndexesForCache(t.Context(), cache, func(context.Context) ([sha256.Size]byte, error) {
			resourceReads++
			if resourceReads == 2 && kind == "unreadable" {
				return [sha256.Size]byte{}, physical
			}
			return current, nil
		}, now, func(context.Context) (bool, error) {
			catalogReads++
			current = second
			return true, nil
		})
		if ready || err == nil || kind == "unreadable" && err != physical || resourceReads != 2 || catalogReads != 1 ||
			cache.readyObservation(t.Context(), first, now()) || cache.readyObservation(t.Context(), second, now()) {
			t.Fatal("changed resource authorized a catalog proof or caused a retry", kind, ready, err, resourceReads, catalogReads)
		}
	}
}
