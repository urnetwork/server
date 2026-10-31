// The per-client move cooldown (connect_move_cooldown.go): set with a TTL,
// read for many clients at once, withdrawn only by the move that set it, and
// never set without an expiry.
package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// The cooldown's whole life: set, read, withdraw, expiry and many at once.
func TestConnectMoveCooldown(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {
		ctx := connectMoveTestContext(t)

		clientId := server.NewId()
		otherClientId := server.NewId()
		moveId := server.NewId()

		cooling, err := GetConnectMoveCooldowns(ctx, []server.Id{clientId, otherClientId})
		if err != nil {
			t.Fatal(err)
		}
		if len(cooling) != 0 {
			t.Fatalf("cooling before any move: %v", cooling)
		}

		if err := SetConnectMoveCooldown(ctx, clientId, moveId, 6*time.Hour); err != nil {
			t.Fatal(err)
		}
		cooling, err = GetConnectMoveCooldowns(ctx, []server.Id{clientId, otherClientId})
		if err != nil {
			t.Fatal(err)
		}
		if !cooling[clientId] || cooling[otherClientId] || len(cooling) != 1 {
			t.Fatalf("cooling = %v, want only the moved client", cooling)
		}
		server.Redis(ctx, func(r server.RedisClient) {
			key := "move_cooldown_" + clientId.String()
			ttl, err := r.TTL(ctx, key).Result()
			if err != nil {
				t.Fatal(err)
			}
			if ttl < 6*time.Hour-time.Minute || 6*time.Hour < ttl {
				t.Fatalf("cooldown ttl = %s", ttl)
			}
			value, err := r.Get(ctx, key).Result()
			if err != nil || value != moveId.String() {
				t.Fatalf("cooldown value = %q (%v), want the move id", value, err)
			}
		})

		// only the move that set it withdraws it
		if err := RemoveConnectMoveCooldown(ctx, clientId, server.NewId()); err != nil {
			t.Fatal(err)
		}
		cooling, err = GetConnectMoveCooldowns(ctx, []server.Id{clientId})
		if err != nil || !cooling[clientId] {
			t.Fatalf("another move withdrew the cooldown (%v)", err)
		}
		if err := RemoveConnectMoveCooldown(ctx, clientId, moveId); err != nil {
			t.Fatal(err)
		}
		cooling, err = GetConnectMoveCooldowns(ctx, []server.Id{clientId})
		if err != nil || cooling[clientId] {
			t.Fatalf("the move's withdraw left the cooldown (%v)", err)
		}

		// a non-positive ttl sets nothing, never a key without expiry
		for _, ttl := range []time.Duration{0, -time.Second} {
			if err := SetConnectMoveCooldown(ctx, otherClientId, moveId, ttl); err != nil {
				t.Fatal(err)
			}
		}
		cooling, err = GetConnectMoveCooldowns(ctx, []server.Id{otherClientId})
		if err != nil || cooling[otherClientId] {
			t.Fatalf("a non-positive ttl set a cooldown (%v)", err)
		}

		// the cooldown ends with its ttl
		if err := SetConnectMoveCooldown(ctx, otherClientId, moveId, 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(300 * time.Millisecond):
		}
		cooling, err = GetConnectMoveCooldowns(ctx, []server.Id{otherClientId})
		if err != nil || cooling[otherClientId] {
			t.Fatalf("the cooldown outlived its ttl (%v)", err)
		}

		// many clients in one read
		clientIds := []server.Id{}
		for i := 0; i < 50; i += 1 {
			clientIds = append(clientIds, server.NewId())
			if i%3 == 0 {
				if err := SetConnectMoveCooldown(ctx, clientIds[i], moveId, time.Hour); err != nil {
					t.Fatal(err)
				}
			}
		}
		cooling, err = GetConnectMoveCooldowns(ctx, clientIds)
		if err != nil {
			t.Fatal(err)
		}
		for i, clientId := range clientIds {
			if cooling[clientId] != (i%3 == 0) {
				t.Errorf("client %d cooling = %t", i, cooling[clientId])
			}
		}
	})
}
