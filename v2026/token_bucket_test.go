package server

import (
	"testing"
	"time"
)

// The bucket admits its burst at once, then one per 1/rate, never more than
// its burst after an idle stretch, nothing for a clock that steps back, and
// nothing at all with a non-positive rate or burst.
func TestTokenBucketAdmitsItsBurstThenItsRate(t *testing.T) {
	start := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	bucket := NewTokenBucket(2, 3)
	admitted := 0
	for range 10 {
		if bucket.Take(start) {
			admitted += 1
		}
	}
	if admitted != 3 {
		t.Fatalf("admitted %d at once, want the burst of 3", admitted)
	}
	if bucket.Take(start.Add(400 * time.Millisecond)) {
		t.Fatal("admitted before a whole token refilled")
	}
	if !bucket.Take(start.Add(500 * time.Millisecond)) {
		t.Fatal("refused after a token refilled at 2 a second")
	}
	if bucket.Take(start.Add(500 * time.Millisecond)) {
		t.Fatal("admitted a second take on one refilled token")
	}
	// an idle hour refills only the burst
	idle := start.Add(time.Hour)
	admitted = 0
	for range 10 {
		if bucket.Take(idle) {
			admitted += 1
		}
	}
	if admitted != 3 {
		t.Fatalf("admitted %d after an idle hour, want the burst of 3", admitted)
	}
	// a clock that steps back refills nothing
	if bucket.Take(start) || bucket.Take(idle) {
		t.Fatal("a clock that stepped back refilled the bucket")
	}
	for _, closed := range []*TokenBucket{NewTokenBucket(0, 3), NewTokenBucket(2, 0), NewTokenBucket(-1, 3)} {
		if closed.Take(start) {
			t.Fatal("a non-positive rate or burst admitted")
		}
	}
}
