package connect

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
)

// The defaults are the ones MOVE.md §5.8 and §9 settled. A deploy changes no
// placement: the mover's default mode is off until an operator sets the Redis
// move control.
func TestDefaultExchangeMoveSettingsMatchDesign(t *testing.T) {
	settings := DefaultExchangeSettings()

	connect.AssertEqual(t, true, settings.EnableMove)
	connect.AssertEqual(t, true, settings.EnableDrainMoves)
	connect.AssertEqual(t, 3*time.Minute, settings.MoveArrivalTimeout)
	connect.AssertEqual(t, 1*time.Second, settings.MoveIntentPollInterval)
	connect.AssertEqual(t, 60*time.Second, settings.MoveCompulsoryTimeout)
	connect.AssertEqual(t, 6*time.Hour, settings.MovePreferDuration)

	connect.AssertEqual(t, "off", settings.MoveDefaultMode)
	connect.AssertEqual(t, 120, settings.MoveMaxPerMinute)
	connect.AssertEqual(t, 60, settings.MoveMaxPerMinutePerSourceHost)
	connect.AssertEqual(t, 40, settings.MoveMaxPerMinutePerTargetHost)
	connect.AssertEqual(t, 0.10, settings.MoveBalanceTolerance)
	connect.AssertEqual(t, 0.75, settings.MoveTargetMaxCpu)
	connect.AssertEqual(t, 10*time.Minute, settings.MoveIdleTimeout)
	connect.AssertEqual(t, 10*time.Minute, settings.MoveMinResidentAge)
	connect.AssertEqual(t, 6*time.Hour, settings.MoveClientCooldown)
	connect.AssertEqual(t, false, settings.EnableArrivalMoves)
	connect.AssertEqual(t, 0.2, settings.MoveTargetBreakerRatio)
	connect.AssertEqual(t, 20, settings.MoveTargetBreakerMinSends)
}

// The intent outlives the client's 60 s replacement connect timeout, so a
// client that connects slowly still hands off by intent, and a compulsory
// move's deadline falls inside the intent's life.
func TestMoveArrivalTimeoutCoversConnectAndDeadline(t *testing.T) {
	settings := DefaultExchangeSettings()
	clientConnectTimeout := 60 * time.Second
	connect.AssertEqual(t, true, clientConnectTimeout < settings.MoveArrivalTimeout)
	connect.AssertEqual(t, true, settings.MoveCompulsoryTimeout < settings.MoveArrivalTimeout)
}
