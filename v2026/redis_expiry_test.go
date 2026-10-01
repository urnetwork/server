package server

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

func TestRedisExpirySecondsUsesIntegerWireType(t *testing.T) {
	seconds := redisExpirySeconds(5 * time.Minute)

	connect.AssertEqual(t, seconds, int64(300))
}

func TestRedisExpirySecondsRoundsToNearestSecond(t *testing.T) {
	seconds := redisExpirySeconds(1500 * time.Millisecond)

	connect.AssertEqual(t, seconds, int64(2))
}

func TestRedisExpirySecondsDoesNotTriggerRawDurationWarning(t *testing.T) {
	seconds := redisExpirySeconds(5 * time.Minute)
	warning := redisCommandTtlWarning(
		[]any{"eval", "script", 1, "key", "old", "new", seconds},
		time.Now(),
	)

	connect.AssertEqual(t, warning, "")
}
