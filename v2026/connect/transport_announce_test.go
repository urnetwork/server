package connect

import (
	"testing"
	"time"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2026/model"
)

// passive speed sampling and the synthetic speed test gate are pure in-memory
// state while `connectionId` is nil (no speed rows are written), so they can
// be tested without the test env
func TestPassiveSpeedAndSyntheticGate(t *testing.T) {
	settings := DefaultConnectionAnnounceSettings()

	newAnnounce := func(age time.Duration) *ConnectionAnnounce {
		return &ConnectionAnnounce{
			settings:               settings,
			testConfig:             DefaultTestConfig(),
			startTime:              time.Now().Add(-age),
			passiveWindowStartTime: time.Now().Add(-time.Second),
		}
	}

	// young connection: synthetic deferred regardless of passive speed
	announce := newAnnounce(0)
	assert.Equal(t, false, announce.allowSyntheticSpeedWithLock())

	// aged connection without passive proof: synthetic allowed
	announce = newAnnounce(2 * settings.SyntheticSpeedTimeout)
	assert.Equal(t, true, announce.allowSyntheticSpeedWithLock())

	// a window below the minimum byte count is not a sample
	announce = newAnnounce(2 * settings.SyntheticSpeedTimeout)
	announce.ReceiveMessage(settings.PassiveSpeedMinByteCount / 2)
	announce.samplePassiveSpeed()
	assert.Equal(t, model.ByteCount(0), announce.passiveMaxBytesPerSecond)
	assert.Equal(t, true, announce.allowSyntheticSpeedWithLock())

	// a slow window samples but does not reach the synthetic threshold
	announce.passiveWindowStartTime = time.Now().Add(-time.Second)
	announce.ReceiveMessage(settings.PassiveSpeedMinByteCount)
	announce.samplePassiveSpeed()
	slowMax := announce.passiveMaxBytesPerSecond
	if slowMax <= 0 {
		t.Fatalf("expected a passive sample, got %d", slowMax)
	}
	assert.Equal(t, true, announce.allowSyntheticSpeedWithLock())

	// a fast window proves the connection speed and gates the synthetic test.
	// the window rate uses the max of the send and receive directions.
	announce.passiveWindowStartTime = time.Now().Add(-time.Second)
	announce.SendMessage(2 * settings.SyntheticSpeedBytesPerSecond)
	announce.samplePassiveSpeed()
	if announce.passiveMaxBytesPerSecond < settings.SyntheticSpeedBytesPerSecond {
		t.Fatalf(
			"expected passive max above the synthetic threshold, got %d < %d",
			announce.passiveMaxBytesPerSecond,
			settings.SyntheticSpeedBytesPerSecond,
		)
	}
	assert.Equal(t, false, announce.allowSyntheticSpeedWithLock())

	// the max is monotone: a later slower window does not lower it
	provenMax := announce.passiveMaxBytesPerSecond
	announce.passiveWindowStartTime = time.Now().Add(-time.Second)
	announce.ReceiveMessage(settings.PassiveSpeedMinByteCount)
	announce.samplePassiveSpeed()
	assert.Equal(t, provenMax, announce.passiveMaxBytesPerSecond)
	assert.Equal(t, false, announce.allowSyntheticSpeedWithLock())
}
