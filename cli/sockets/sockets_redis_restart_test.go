//go:build sidecar_disruption

// §22.8's Redis failover row for sockets (SOCK-M5-17): a master's restart, carried out for real by sidecart3.RedisRestart
// on the test's own database, cuts every connection the host holds, its key-event subscription included, and loses
// every key, since production's cluster runs no replicas. Sockets heal: the store's next calls reconnect, Finds answer
// no_listener rather than a stale ticket, and the listener's next heartbeat brings its lease back. Real time, as every
// disruption row is, so it runs in sidecar-t3.sh's nightly form under sidecar_disruption.

package main

import (
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecart3"
)

func TestT3RedisRestartHealsByTheNextHeartbeat(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3World(t)
		registration := world.registration(world.listener, "app/db")
		world.registerOk(world.listener, 1, registration)
		if code := world.find(world.dialer, "app/db"); code != "" {
			t.Fatalf("before the restart a Find answered %s", code)
		}
		restart := sidecart3.NewRedisRestart()
		if err := restart.Disrupt(world.ctx); err != nil {
			t.Fatal(err)
		}
		// a call cut mid-flight answers unavailable; the next reconnects and finds the group gone
		lost := false
		for range 20 {
			code := world.find(world.dialer, "app/db")
			if code == sidecar.SocketErrorCodeNoListener {
				lost = true
				break
			}
			if code != sidecar.SocketErrorCodeUnavailable {
				t.Fatalf("after the restart a Find answered %q", code)
			}
		}
		if !lost {
			t.Fatal("the store never answered after the restart")
		}
		world.world.clock.Advance(15 * time.Second)
		world.registerOk(world.listener, 2, registration)
		if code := world.find(world.dialer, "app/db"); code != "" {
			t.Fatalf("after one heartbeat a Find answered %s", code)
		}
	})
}
