package connect

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A move's arrival window (resident_move.go): its intent lives
// MoveArrivalTimeout past the move's migrate time, not past the send.
//
// The DB test requires the test DB env (WARP_ENV=local + postgres/redis/vault),
// like the rest of this package; skipped under -short.

// A move's window counts from its migrate time, or from now when that is past.
func TestResidentMoveExpireTimeCountsFromTheMigrateTime(t *testing.T) {
	now := time.Unix(1000, 0)
	connect.AssertEqual(t, now.Add(5*time.Minute), residentMoveExpireTime(now, now.Add(2*time.Minute), 3*time.Minute))
	connect.AssertEqual(t, now.Add(3*time.Minute), residentMoveExpireTime(now, now.Add(-time.Minute), 3*time.Minute))
	connect.AssertEqual(t, now.Add(3*time.Minute), residentMoveExpireTime(now, time.Time{}, 3*time.Minute))
}

// A client whose migrate time is late in a drain's jitter still finds its
// intent: the intent lives the arrival window past the migrate time, not past
// the send.
func TestResidentMoveIntentLivesPastALateMigrateTime(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		arrivalTimeout := 2 * time.Second
		migrateDelay := 3 * time.Second
		env := testing_newPeerDiscoveryEnvWithSettings(ctx, t, residentMoveTestExchangeSettings("alpha-edge-0", func(exchangeSettings *ExchangeSettings) {
			exchangeSettings.MoveArrivalTimeout = arrivalTimeout
		}))
		defer env.Close()
		<-env.exchange.EdgeInventory().Loaded()

		clientId, byClientJwt := env.authClient(&model.AuthNetworkClientArgs{
			Description: "late migrate",
			DeviceSpec:  "spec",
		})
		client := env.newClient(clientId)
		defer client.Close()
		transport := env.newTransport(byClientJwt, server.NewId(), client.RouteManager())
		defer transport.Close()
		resident := waitForResidentMoveTestResident(ctx, t, env.exchange, clientId, func(resident *Resident) bool {
			return 0 < resident.TransportCount()
		})
		resident.observeTransportEdge(ResidentTransportEdge{ResidentMoveVersion: residentMoveTargetVersion})

		sendTime := time.Now()
		_, err := env.exchange.SendResidentMove(ctx, ResidentMove{
			ClientId:    clientId,
			Kind:        model.ResidentMoveKindAdvisory,
			TargetHost:  "alpha-edge-1",
			Targets:     residentMoveTestTargets,
			MigrateTime: sendTime.Add(migrateDelay),
		})
		connect.AssertEqual(t, nil, err)

		// past the arrival window from the send, inside the one from the
		// migrate time
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(sendTime.Add(arrivalTimeout + time.Second))):
		}
		intent, err := model.GetResidentMoveIntent(ctx, clientId)
		connect.AssertEqual(t, nil, err)
		if intent == nil {
			t.Fatal("the intent ended an arrival window after the send, before the client's migrate time")
		}
		connect.AssertEqual(t, true, resident.move.Load() != nil)

		// and it ends an arrival window after the migrate time
		endTime := sendTime.Add(migrateDelay + arrivalTimeout + 10*time.Second)
		for resident.move.Load() != nil {
			if endTime.Before(time.Now()) {
				t.Fatal("the move outlived its arrival window")
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
		}
		intent, err = model.GetResidentMoveIntent(ctx, clientId)
		connect.AssertEqual(t, nil, err)
		connect.AssertEqual(t, true, intent == nil)
	})
}
