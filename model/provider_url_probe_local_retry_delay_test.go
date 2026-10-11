package model

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// A visibly synthetic provider id whose low bytes carry index.
func testingLocalRetryClientId(index int) server.Id {
	var clientId server.Id
	binary.BigEndian.PutUint64(clientId[8:], uint64(index))
	return clientId
}

// Each consecutive earlier failure doubles the one-minute interval until the
// thirty-minute cap. One claim's jitter factor scales every step alike, so the
// steps are exact multiples of the first, up to microsecond truncation.
func TestUrlLocalRetryDelayDoublesToTheCap(t *testing.T) {
	clientId := testingLocalRetryClientId(7)
	first := providerUrlProbeLocalRetryDelay(clientId, 11, 1, time.Minute)
	if first < 54*time.Second || 66*time.Second < first {
		t.Fatalf("first local retry = %s, want one minute within 10%%", first)
	}
	for streak, multiple := range map[int]int{1: 1, 2: 2, 3: 4, 4: 8, 5: 16, 6: 30, 7: 30, 50: 30} {
		delay := providerUrlProbeLocalRetryDelay(clientId, 11, streak, time.Minute)
		if gap := delay - time.Duration(multiple)*first; gap < -64*time.Microsecond || 64*time.Microsecond < gap {
			t.Errorf("streak %d retried after %s, want %d times %s", streak, delay, multiple, first)
		}
	}
	// A streak below one is this failure alone.
	if delay := providerUrlProbeLocalRetryDelay(clientId, 11, 0, time.Minute); delay != first {
		t.Errorf("empty streak retried after %s, want %s", delay, first)
	}
}

// The factor stays inside [0.9, 1.1], is the same for the same issued claim,
// and differs across a provider's claims and across providers, so a burst of
// failures does not come back as a burst.
func TestUrlLocalRetryDelayJitterIsBoundedStableAndSpread(t *testing.T) {
	claimDelays := map[time.Duration]bool{}
	providerDelays := map[time.Duration]bool{}
	for index := range 1000 {
		claimDelay := providerUrlProbeLocalRetryDelay(testingLocalRetryClientId(1), int64(index+1), 3, time.Minute)
		providerDelay := providerUrlProbeLocalRetryDelay(testingLocalRetryClientId(index), 1, 3, time.Minute)
		for _, delay := range []time.Duration{claimDelay, providerDelay} {
			if delay < 216*time.Second || 264*time.Second < delay {
				t.Fatalf("third local retry = %s, want four minutes within 10%%", delay)
			}
		}
		if again := providerUrlProbeLocalRetryDelay(testingLocalRetryClientId(1), int64(index+1), 3, time.Minute); again != claimDelay {
			t.Fatalf("one claim's retry changed between reads: %s then %s", claimDelay, again)
		}
		claimDelays[claimDelay] = true
		providerDelays[providerDelay] = true
	}
	if len(claimDelays) < 500 || len(providerDelays) < 500 {
		t.Fatalf("1000 claims spread over %d delays and 1000 providers over %d, want at least 500 each", len(claimDelays), len(providerDelays))
	}
}

// A configured failure interval at or above the cap is never doubled, and an
// unset or zero interval schedules nothing extra.
func TestUrlLocalRetryDelayKeepsALongConfiguredInterval(t *testing.T) {
	if doublings := providerUrlProbeLocalRetryDoublings(time.Hour); doublings != 0 {
		t.Fatalf("an hour interval doubles %d times, want 0", doublings)
	}
	if doublings := providerUrlProbeLocalRetryDoublings(time.Minute); doublings != 5 {
		t.Fatalf("a one-minute interval doubles %d times before the cap, want 5", doublings)
	}
	for _, streak := range []int{1, 2, 10} {
		delay := providerUrlProbeLocalRetryDelay(testingLocalRetryClientId(3), 5, streak, time.Hour)
		if delay < 54*time.Minute || 66*time.Minute < delay {
			t.Fatalf("streak %d with an hour interval retried after %s, want an hour within 10%%", streak, delay)
		}
	}
	if delay := providerUrlProbeLocalRetryDelay(testingLocalRetryClientId(3), 5, 4, 0); delay != 0 {
		t.Fatalf("zero interval retried after %s, want 0", delay)
	}
}
