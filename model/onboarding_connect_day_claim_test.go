// The fleet-wide connect.day claim admits one writer per network day, and the
// PostgreSQL guard keeps the day exact whenever the claim cannot.
package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
)

// A writer in another process holds the day's claim. This process must not
// write, and must check again on a later connect: when that writer stops
// without recording the day, its claim expires and the next connect records it.
func TestRecordConnectDayLeavesAHeldClaimToItsWriter(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, clientId := connectDayClaimTestClient(ctx)
		connectAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		day := ConnectDayStart(connectAt)
		defer useConnectDayProcess()()
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Set(ctx, connectDayKey(networkId, day), "synthetic-other-writer", connectDayClaimTtl).Err())
		})

		waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, connectAt))
		connect.AssertEqual(t, 0, connectDayEventCount(t, ctx, networkId))
		holder, _ := connectDayRedisState(t, ctx, networkId, day)
		connect.AssertEqual(t, "synthetic-other-writer", holder)
		if !connectDaySeen.remember(networkId, day) {
			t.Fatal("a day claimed by another writer stayed remembered, so this process would never check it again")
		}
		connectDaySeen.forget(networkId, day)

		// the other writer stopped and its claim expired
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.Del(ctx, connectDayKey(networkId, day)).Err())
		})
		waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, connectAt.Add(time.Hour)))
		connect.AssertEqual(t, 1, connectDayEventCount(t, ctx, networkId))
		holder, ttl := connectDayRedisState(t, ctx, networkId, day)
		connect.AssertEqual(t, connectDayRecordedValue, holder)
		if ttl <= 24*time.Hour || connectDayRecordedTtl < ttl {
			t.Fatalf("recorded marker expires in %s, want within (24h, %s]", ttl, connectDayRecordedTtl)
		}
	})
}

// A claim that Redis cannot answer falls back to the PostgreSQL guard: the day
// is still recorded, and a second process without the claim adds no event.
func TestRecordConnectDayWritesWhenTheClaimFails(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, clientId := connectDayClaimTestClient(ctx)
		connectAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		day := ConnectDayStart(connectAt)
		// a value of another type makes the claim script fail for this key only
		breakClaim := func() {
			server.Redis(ctx, func(r server.RedisClient) {
				key := connectDayKey(networkId, day)
				server.Raise(r.Del(ctx, key).Err())
				server.Raise(r.HSet(ctx, key, "synthetic", "1").Err())
			})
		}
		defer useConnectDayProcess()()

		for process := range 2 {
			breakClaim()
			connectDaySeen = &connectDayCache{}
			waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, connectAt.Add(time.Duration(process)*time.Hour)))
			if count := connectDayEventCount(t, ctx, networkId); count != 1 {
				t.Fatalf("process %d without a claim left %d connect.day events, want 1", process, count)
			}
		}
		holder, _ := connectDayRedisState(t, ctx, networkId, day)
		connect.AssertEqual(t, connectDayRecordedValue, holder)
	})
}

// A failed write releases its claim at once, so the next connect of the
// network in another process records the day instead of waiting out the claim.
func TestRecordConnectDayFailedWriteReleasesTheClaim(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, clientId := connectDayClaimTestClient(ctx)
		connectAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		day := ConnectDayStart(connectAt)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				CREATE FUNCTION synthetic_reject_connect_day() RETURNS trigger LANGUAGE plpgsql AS $$
				BEGIN
					RAISE EXCEPTION 'synthetic connect.day rejection';
				END;
				$$;
				CREATE TRIGGER synthetic_reject_connect_day
				BEFORE INSERT ON network_onboarding_event
				FOR EACH ROW EXECUTE FUNCTION synthetic_reject_connect_day();
			`))
		})
		defer useConnectDayProcess()()

		waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, connectAt))
		connect.AssertEqual(t, 0, connectDayEventCount(t, ctx, networkId))
		holder, _ := connectDayRedisState(t, ctx, networkId, day)
		connect.AssertEqual(t, "", holder)

		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_reject_connect_day ON network_onboarding_event`))
		})
		// another process
		connectDaySeen = &connectDayCache{}
		waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, connectAt.Add(time.Hour)))
		connect.AssertEqual(t, 1, connectDayEventCount(t, ctx, networkId))
		holder, _ = connectDayRedisState(t, ctx, networkId, day)
		connect.AssertEqual(t, connectDayRecordedValue, holder)
	})
}

// The event is written for the connecting client's own network only. A client
// outside the authenticated network records nothing and must not hold back
// the network's own clients, in this process or another.
func TestRecordConnectDayRecordsNothingForAClientOutsideTheNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		otherNetworkId, otherClientId := connectDayClaimTestClient(ctx)
		networkId, clientId := connectDayClaimTestClient(ctx)
		connectAt := time.Date(2026, 9, 10, 12, 30, 0, 0, time.UTC)
		day := ConnectDayStart(connectAt)
		defer useConnectDayProcess()()

		waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, otherClientId, connectAt))
		connect.AssertEqual(t, 0, connectDayEventCount(t, ctx, networkId))
		connect.AssertEqual(t, 0, connectDayEventCount(t, ctx, otherNetworkId))
		holder, _ := connectDayRedisState(t, ctx, networkId, day)
		connect.AssertEqual(t, "", holder)

		waitConnectDayWrites(recordConnectDayForTest(ctx, networkId, clientId, connectAt.Add(time.Hour)))
		connect.AssertEqual(t, 1, connectDayEventCount(t, ctx, networkId))
		connect.AssertEqual(t, 0, connectDayEventCount(t, ctx, otherNetworkId))
	})
}

// A claim is short, so a writer that stops releases the day within a minute;
// the recorded marker covers the rest of the day; neither is taken over.
func TestClaimConnectDayLifetimes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		networkId := server.NewId()
		day := time.Date(2026, 9, 10, 0, 0, 0, 0, time.UTC)

		token, holder, err := claimConnectDay(ctx, networkId, day)
		if err != nil || token == "" || holder != token {
			t.Fatalf("first claim returned token %q holder %q: %v", token, holder, err)
		}
		_, ttl := connectDayRedisState(t, ctx, networkId, day)
		if ttl <= 0 || connectDayClaimTtl < ttl {
			t.Fatalf("claim expires in %s, want within (0, %s]", ttl, connectDayClaimTtl)
		}
		secondToken, secondHolder, err := claimConnectDay(ctx, networkId, day)
		if err != nil || secondToken != "" || secondHolder != token {
			t.Fatalf("second claim returned token %q holder %q: %v, want the first writer's claim", secondToken, secondHolder, err)
		}
		// the next day is its own claim
		nextToken, _, err := claimConnectDay(ctx, networkId, day.Add(24*time.Hour))
		if err != nil || nextToken == "" {
			t.Fatalf("next day's claim returned token %q: %v", nextToken, err)
		}

		if err := markConnectDayRecorded(ctx, networkId, day); err != nil {
			t.Fatal(err)
		}
		recordedToken, recordedHolder, err := claimConnectDay(ctx, networkId, day)
		if err != nil || recordedToken != "" || recordedHolder != connectDayRecordedValue {
			t.Fatalf("claim after recording returned token %q holder %q: %v", recordedToken, recordedHolder, err)
		}
		_, ttl = connectDayRedisState(t, ctx, networkId, day)
		if ttl <= 24*time.Hour || connectDayRecordedTtl < ttl {
			t.Fatalf("recorded marker expires in %s, want within (24h, %s]", ttl, connectDayRecordedTtl)
		}
	})
}

// A network with one client in PostgreSQL.
func connectDayClaimTestClient(ctx context.Context) (networkId server.Id, clientId server.Id) {
	networkId = server.NewId()
	clientId = server.NewId()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx,
			`INSERT INTO network_client (client_id, network_id, active) VALUES ($1, $2, true)`,
			clientId, networkId,
		))
	})
	return
}

// Gives the caller a process memory of its own until the returned restore runs.
func useConnectDayProcess() (restore func()) {
	processSeen := connectDaySeen
	connectDaySeen = &connectDayCache{}
	return func() {
		connectDaySeen = processSeen
	}
}

// The network day's Redis value, "" when absent, and its remaining lifetime.
func connectDayRedisState(t testing.TB, ctx context.Context, networkId server.Id, day time.Time) (value string, ttl time.Duration) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		key := connectDayKey(networkId, day)
		var err error
		value, err = r.Get(ctx, key).Result()
		if errors.Is(err, redis.Nil) {
			value = ""
		} else {
			server.Raise(err)
		}
		ttl, err = r.PTTL(ctx, key).Result()
		server.Raise(err)
	})
	return
}
