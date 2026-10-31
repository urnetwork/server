package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The onboarding campaign's connection facts (support inbox 8146).
// RemoveDisconnectedNetworkClients prunes a closed network_client_connection
// row 8 h after the disconnect, so a network that connected on earlier days
// keeps only the connect.day events the connection path wrote, and the
// connect.first an app sends (apps that predate it never do). The campaign
// told such a network it never connected. NetworkConnectDays counts each UTC
// day since sign-up that a connection row or a connect.day shows once, and a
// connect.first that is the only record as one day.
func TestNetworkConnectDaysCountsDurableConnectionRecords(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		since := now.Add(-6 * 24 * time.Hour)

		addClient := func(networkId server.Id) server.Id {
			clientId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
					clientId,
					networkId,
				))
			})
			return clientId
		}
		// the event the connection path writes for the UTC day of a connection
		recordDay := func(networkId server.Id, clientId server.Id, connectTime time.Time) {
			job := connectDayWriteJob{
				networkId:   networkId,
				clientId:    clientId,
				connectTime: connectTime,
				day:         ConnectDayStart(connectTime),
			}
			if !recordConnectDayEvent(ctx, job) {
				t.Fatalf("connect.day was not recorded for network %s at %s", networkId, connectTime)
			}
		}
		// a connection row that is not pruned yet
		addConnection := func(clientId server.Id, connectTime time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_client_connection (
							client_id, connection_id, connect_time, connection_host, connection_service,
							connection_block, client_address_hash, client_address_port, handler_id, connected
						)
						VALUES ($1, $2, $3, 'synthetic.example', 'test', 'test', $4, 10001, $5, true)
					`,
					clientId,
					server.NewId(),
					connectTime,
					make([]byte, 32),
					server.NewId(),
				))
			})
		}

		// Connected on three days, every connection row already pruned, and no
		// connect.first. A day before sign-up is not one of its first days.
		pruned := server.NewId()
		prunedClient := addClient(pruned)
		for _, day := range []int{0, 3, 4} {
			recordDay(pruned, prunedClient, since.Add(time.Duration(day)*24*time.Hour+time.Hour))
		}
		recordDay(pruned, prunedClient, since.Add(-2*24*time.Hour))

		// Connected now and two days ago: today's day is in both the
		// connection table and connect.day, and counts once.
		live := server.NewId()
		liveClient := addClient(live)
		liveAt := now.Add(-time.Minute)
		addConnection(liveClient, liveAt)
		recordDay(live, liveClient, liveAt)
		recordDay(live, liveClient, liveAt.Add(-2*24*time.Hour))

		// Only the app's connect.first is left. Its time is the app's clock, so
		// it proves one connection day without naming it.
		appOnly := server.NewId()
		server.Raise(AddOnboardingEvent(ctx, &OnboardingEvent{
			NetworkId: appOnly,
			Name:      EventConnectFirst,
			At:        since.Add(time.Hour),
			Platform:  "ios",
		}))

		// Never connected: other events, and a client that never connected.
		never := server.NewId()
		addClient(never)
		server.Raise(AddOnboardingEvent(ctx, &OnboardingEvent{NetworkId: never, Name: EventEmailOpened, At: since.Add(time.Hour)}))
		server.Raise(AddOnboardingEvent(ctx, &OnboardingEvent{NetworkId: never, Name: EventWidgetAdded, At: since.Add(time.Hour)}))

		for _, c := range []struct {
			name      string
			networkId server.Id
			days      int
			connected bool
		}{
			{"connect.day after the connection rows were pruned", pruned, 3, true},
			{"a live connection and connect.day", live, 2, true},
			{"connect.first only", appOnly, 1, true},
			{"never connected", never, 0, false},
		} {
			days, connected := NetworkConnectDays(ctx, c.networkId, since)
			if days != c.days || connected != c.connected {
				t.Errorf("%s: NetworkConnectDays = (%d, %t), want (%d, %t)", c.name, days, connected, c.days, c.connected)
			}
		}
	})
}
