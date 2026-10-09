package session

import (
	"testing"
	"time"
)

func TestSessionMintClockFailsClosedOutsideMeasuredBound(t *testing.T) {
	start := time.Unix(1700000000, 0)
	end := start.Add(100 * time.Millisecond)
	for _, sample := range []struct {
		authority time.Time
		safe      bool
	}{
		{start, true}, {end, true}, {end.Add(-SessionClockDisagreementLimit), true},
		{start.Add(-SessionClockDisagreementLimit - time.Nanosecond), false},
		{start.Add(SessionClockDisagreementLimit), true}, {end.Add(SessionClockDisagreementLimit + time.Nanosecond), false},
	} {
		if safe, _ := sessionClockSampleSafe(start, end, sample.authority); safe != sample.safe {
			t.Fatal("unsafe clock admitted or bounded sample refused", sample)
		}
	}
	if safe, _ := sessionClockSampleSafe(end, start, start); safe {
		t.Fatal("backward local clock admitted")
	}
	// A two-second response interval plus an apparent 14-second offset can
	// conceal 16 seconds of skew. The nearest endpoint must not admit it.
	if safe, distance := sessionClockSampleSafe(start, start.Add(2*time.Second), start.Add(16*time.Second)); safe || distance != 16*time.Second {
		t.Fatal("request latency hid a clock bound violation", safe, distance)
	}
}
