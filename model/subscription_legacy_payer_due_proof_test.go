// A successful cold due-only entry must supply its own positive proof. No
// optional registration or manually seeded full-index cache may hide the gap.
package model

import (
	"context"
	"crypto/sha256"
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
