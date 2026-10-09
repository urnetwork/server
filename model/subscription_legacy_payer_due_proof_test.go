// A successful cold due-only entry must supply its own positive proof. No
// optional registration or manually seeded full-index cache may hide the gap.
package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestLegacyPayerDueProofColdSuccessAvoidsLaterProbeRefusal(t *testing.T) {
	full, due := &legacySettlementPayerIndexCache{}, &legacySettlementPayerIndexCache{}
	identity := sha256.Sum256([]byte("synthetic due-only resource"))
	started := time.Unix(100, 0)
	instant := started
	now := func() time.Time { return instant }
	resource := func(context.Context) ([sha256.Size]byte, error) { return identity, nil }
	reads := 0
	read := func(ctx context.Context) (bool, error) {
		reads++
		deadline, ok := ctx.Deadline()
		if !ok || deadline.After(time.Now().Add(legacySettlementPayerIndexBudget)) {
			t.Fatal("due proof lost the existing bounded probe")
		}
		if reads == 1 {
			instant = instant.Add(37 * time.Millisecond)
			return true, nil
		}
		return false, context.DeadlineExceeded
	}
	first := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	if first.Outcome != "ready" || first.Cached || reads != 1 || full.ready {
		t.Fatal("cold due-only success changed probe count or promoted full-index authority", first, reads)
	}
	second := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	if second.Outcome != "ready" || !second.Cached || reads != 1 {
		t.Fatal("successful due-only entry did not publish its own proof", second, reads)
	}
	if full.ready || due.expires != started.Add(legacySettlementPayerIndexCacheLifetime) {
		t.Fatal("due proof authorized the missing index or extended its admission expiry")
	}
	instant = due.expires
	expired := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	if expired.Outcome != "deadline" || expired.Cached || reads != 2 {
		t.Fatal("expired due proof suppressed the ordinary fresh probe", expired, reads)
	}
}

func TestLegacyPayerDueProofRejectsUnknownAndLateObservations(t *testing.T) {
	for _, kind := range []string{"invalid", "read_error", "deadline", "canceled", "late", "resource_error", "resource_changed"} {
		full, due := &legacySettlementPayerIndexCache{}, &legacySettlementPayerIndexCache{}
		identity := sha256.Sum256([]byte("synthetic refused due resource"))
		current := identity
		instant := time.Unix(100, 0)
		now := func() time.Time { return instant }
		ctx, cancel := context.WithCancel(t.Context())
		reads := 0
		observed := observeLegacySettlementPayerDueIndexWithProof(ctx, full, due,
			func(context.Context) ([sha256.Size]byte, error) {
				if kind == "resource_error" {
					return current, errors.New("synthetic resource refusal")
				}
				return current, nil
			}, now, func(context.Context) (bool, error) {
				reads++
				switch kind {
				case "invalid":
					return false, nil
				case "read_error":
					return false, errors.New("synthetic catalog refusal")
				case "deadline":
					return false, context.DeadlineExceeded
				case "canceled":
					cancel()
				case "late":
					instant = instant.Add(legacySettlementPayerIndexCacheLifetime)
				case "resource_changed":
					current = sha256.Sum256([]byte("synthetic replacement resource"))
				}
				return true, nil
			})
		cancel()
		if full.ready || due.ready || observed.Cached || reads > 1 {
			t.Fatal("unknown or late due observation published authority or retried", kind, observed, reads)
		}
		secondReads := 0
		second := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due,
			func(context.Context) ([sha256.Size]byte, error) { return identity, nil }, now,
			func(context.Context) (bool, error) { secondReads++; return false, context.DeadlineExceeded })
		if second.Outcome != "deadline" || second.Cached || secondReads != 1 {
			t.Fatal("a nonproof suppressed a later caller's original probe", kind, second, secondReads)
		}
	}
}

func TestLegacyPayerDueProofKeepsIdentityAndParentCancellation(t *testing.T) {
	full, due := &legacySettlementPayerIndexCache{}, &legacySettlementPayerIndexCache{}
	first := sha256.Sum256([]byte("synthetic first due resource"))
	identity := first
	resource := func(context.Context) ([sha256.Size]byte, error) { return identity, nil }
	now := func() time.Time { return time.Unix(100, 0) }
	reads := 0
	read := func(ctx context.Context) (bool, error) {
		reads++
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if reads == 1 {
			return true, nil
		}
		return false, context.DeadlineExceeded
	}
	observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	identity = sha256.Sum256([]byte("synthetic second due resource"))
	other := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	if other.Outcome != "deadline" || other.Cached || reads != 2 {
		t.Fatal("due proof crossed database resource identity", other, reads)
	}
	identity = first
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	canceled := observeLegacySettlementPayerDueIndexWithProof(ctx, full, due, resource, now, read)
	if canceled.Outcome != "canceled" || canceled.Cached {
		t.Fatal("positive due proof hid parent cancellation", canceled)
	}
}

func TestLegacyPayerDueProofInvalidReadDoesNotCacheRefusal(t *testing.T) {
	full, due := &legacySettlementPayerIndexCache{}, &legacySettlementPayerIndexCache{}
	identity := sha256.Sum256([]byte("synthetic repaired due resource"))
	resource := func(context.Context) ([sha256.Size]byte, error) { return identity, nil }
	now := func() time.Time { return time.Unix(100, 0) }
	reads := 0
	read := func(context.Context) (bool, error) { reads++; return reads > 1, nil }
	first := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	second := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	third := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, read)
	if first.Outcome != "catalog_invalid" || second.Outcome != "ready" || second.Cached || third.Outcome != "ready" || !third.Cached || reads != 2 || full.ready {
		t.Fatal("a negative due result suppressed fresh validation or promoted full proof", first, second, third, reads)
	}
}

func TestLegacyPayerDueProofConcurrentMissDoesNotWaitOrReplaceNewerProof(t *testing.T) {
	for _, firstFails := range []bool{false, true} {
		full, due := &legacySettlementPayerIndexCache{}, &legacySettlementPayerIndexCache{}
		identity := sha256.Sum256([]byte("synthetic concurrent due resource"))
		resource := func(context.Context) ([sha256.Size]byte, error) { return identity, nil }
		var clock atomic.Int64
		clock.Store(time.Unix(100, 0).UnixNano())
		now := func() time.Time { return time.Unix(0, clock.Load()) }
		entered, release := make(chan struct{}), make(chan struct{})
		firstDone := make(chan LegacySettlementPayerIndexReadiness, 1)
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		go func() {
			firstDone <- observeLegacySettlementPayerDueIndexWithProof(ctx, full, due, resource, now, func(ctx context.Context) (bool, error) {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
					return false, ctx.Err()
				}
				if firstFails {
					return false, context.DeadlineExceeded
				}
				return true, nil
			})
		}()
		<-entered
		clock.Store(time.Unix(101, 0).UnixNano())
		secondReads := 0
		second := observeLegacySettlementPayerDueIndexWithProof(ctx, full, due, resource, now, func(context.Context) (bool, error) {
			secondReads++
			return true, nil
		})
		close(release)
		first := <-firstDone
		cancel()
		if second.Outcome != "ready" || second.Cached || secondReads != 1 || due.expires != time.Unix(106, 0) || !due.ready || full.ready {
			t.Fatal("concurrent due miss waited, lost independent proof or changed its expiry", first, second, due.expires)
		}
		last := observeLegacySettlementPayerDueIndexWithProof(t.Context(), full, due, resource, now, func(context.Context) (bool, error) {
			t.Fatal("late reader erased the live independent proof")
			return false, nil
		})
		if last.Outcome != "ready" || !last.Cached {
			t.Fatal("concurrent due proof was not reusable", last)
		}
	}
}
