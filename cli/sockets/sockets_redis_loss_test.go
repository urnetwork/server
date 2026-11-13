// FS-41's and FS-42's M5 socket step on T3 (SOCK-M5-17; D-SOCK-3, D-SOCK-7): sockets keep nothing durable, so after
// Redis loses every key, a site's restore included, listeners come back by their next heartbeat with no other action
// (§9.5's Re-check: Redis loss heals in one interval). On the T3 world (sockets_t3_test.go), the test's own database on
// the private test Redis is emptied, as a lost master's keys are; a Find finds nothing; one heartbeat of the listener's
// whole set brings back its lease, the listing and the next Find's ticket.
//
// The step's names are application names here: tls/ registers only once TLS-M5-22's ValidateListen is the namespace's,
// and its pages take the same path. The restart itself, with every connection to the database cut, is
// sidecart3.RedisRestart's under sidecar_disruption (sockets_redis_restart_test.go), as §22.8's nightly runs it.

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// Empties the test's own database, as a lost master leaves it.
func t3EmptyRedis(t testing.TB, world *t3World) {
	t.Helper()
	server.Redis(world.ctx, func(client server.RedisClient) {
		if err := client.FlushDB(world.ctx).Err(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestT3RedisLossHealsByTheNextHeartbeat(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3World(t)
		names := []string{"app/db", "app/cache"}
		registrations := []*sidecar.SocketRegistration{}
		for _, name := range names {
			registrations = append(registrations, world.registration(world.listener, name))
		}
		world.registerOk(world.listener, 1, registrations...)
		for _, name := range names {
			if code := world.find(world.dialer, name); code != "" {
				t.Fatalf("before the loss a Find answered %s", code)
			}
		}

		t3EmptyRedis(t, world)
		for _, name := range names {
			if code := world.find(world.dialer, name); code != sidecar.SocketErrorCodeNoListener {
				t.Fatalf("after the loss a Find answered %s", code)
			}
		}

		// one heartbeat, the same set under a newer set_version as a restarted registrar's clock gives it
		world.world.clock.Advance(15 * time.Second)
		world.registerOk(world.listener, 2, registrations...)
		for _, name := range names {
			if code := world.find(world.dialer, name); code != "" {
				t.Fatalf("after one heartbeat a Find answered %s", code)
			}
		}
		recorder := hostTestServe(world.host, http.MethodGet, "/socket/list", world.listener.token, nil, "")
		if recorder.Code != http.StatusOK {
			t.Fatalf("the listing answered %d", recorder.Code)
		}
		listed := t3ListedNames(t, world, recorder.Body.Bytes())
		for _, name := range names {
			if !listed[name] {
				t.Fatalf("after one heartbeat the listing lacks %s", name)
			}
		}
	})
}
