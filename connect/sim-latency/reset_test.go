package main

// Pure checks that the reset's destructive steps run only after the endpoint
// gate accepts every pool endpoint. The steps are recording fakes, and the pool
// resources are synthetic address literals, so nothing reaches a service, the
// host resolver, or a loopback tunnel.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/urnetwork/server"
)

// Pushes synthetic pool resources naming the given authorities.
func pushResetTestPoolResources(t *testing.T, postgresAuthority string, redisAuthority string) {
	t.Helper()
	t.Cleanup(server.Vault.PushSimpleResource("pg.yml", fmt.Appendf(nil,
		"authority: %q\nuser: synthetic\npassword: synthetic\ndb: synthetic\n", postgresAuthority,
	)))
	t.Cleanup(server.Config.PushSimpleResource("db.yml", []byte("min_connections: 0\nmax_connections: 1\n")))
	t.Cleanup(server.Vault.PushSimpleResource("redis.yml", fmt.Appendf(nil,
		"cluster: false\nauthority: %q\npassword: \"\"\ndb: 0\n", redisAuthority,
	)))
}

// The reported exposure: the local-only reset truncates tables with CASCADE and
// runs FLUSHALL on db 0, which sends no SELECT, so with an endpoint on a
// production data tunnel it would empty a production Redis node. Loopback
// endpoints (a forwarded production port) and other unattested endpoints are
// refused by the real gate before either step runs. This holds in every
// environment the test binary can start in: outside the local environment, in
// it without an attestation, and attested to a launcher, since none attests
// these endpoints.
func TestResetRefusesLoopbackAndUnattestedEndpointsBeforeDestructiveSteps(t *testing.T) {
	cases := []struct {
		name              string
		postgresAuthority string
		redisAuthority    string
	}{
		{name: "tunnel ports on loopback", postgresAuthority: "127.0.0.1:5432", redisAuthority: "127.0.0.1:6379"},
		{name: "unattested postgres and loopback redis", postgresAuthority: "192.0.2.10:5432", redisAuthority: "127.0.0.1:6380"},
		{name: "pgbouncer port on loopback", postgresAuthority: "127.0.0.1:6432", redisAuthority: "192.0.2.10:6379"},
		{name: "IPv6 loopback", postgresAuthority: "[::1]:5432", redisAuthority: "[::1]:6379"},
		{name: "unattested addresses", postgresAuthority: "198.51.100.7:5432", redisAuthority: "198.51.100.7:6379"},
	}
	for _, c := range cases {
		pushResetTestPoolResources(t, c.postgresAuthority, c.redisAuthority)
		steps := []string{}
		err := resetLocalStateWith(
			t.Context(),
			server.RequireAttestedLocalEndpoints,
			func(context.Context) { steps = append(steps, "truncate") },
			func(context.Context) { steps = append(steps, "flushall") },
		)
		if len(steps) != 0 {
			t.Errorf("%s: the reset ran %q before its endpoints were attested", c.name, steps)
		}
		if err == nil || !strings.Contains(err.Error(), "reset refused: destructive local operation refused") {
			t.Errorf("%s: reset error = %v; want the gate's refusal", c.name, err)
		}
	}
}

// The gate decides before anything is sent: a refusal stops both steps, and
// only an accepted gate runs them, truncate before flush.
func TestResetRunsDestructiveStepsOnlyAfterGateAccepts(t *testing.T) {
	refusal := errors.New("synthetic refusal")
	steps := []string{}
	truncate := func(context.Context) { steps = append(steps, "truncate") }
	flush := func(context.Context) { steps = append(steps, "flushall") }
	err := resetLocalStateWith(t.Context(), func(context.Context) error { return refusal }, truncate, flush)
	if !errors.Is(err, refusal) || len(steps) != 0 {
		t.Fatalf("refused reset = %v after steps %q; want the refusal and no step", err, steps)
	}
	if err := resetLocalStateWith(t.Context(), func(context.Context) error { return nil }, truncate, flush); err != nil {
		t.Fatalf("accepted reset = %v", err)
	}
	if !slices.Equal(steps, []string{"truncate", "flushall"}) {
		t.Fatalf("accepted reset ran %q; want truncate then flushall", steps)
	}
}

// The command's own reset goes through the gate. Its endpoints carry an invalid
// port, which the gate refuses in every environment and which no pool could
// dial either, so this exercises the real resetLocalState without any socket.
func TestResetLocalStateGatesItsOwnPools(t *testing.T) {
	pushResetTestPoolResources(t, "192.0.2.10:99999", "192.0.2.10:99999")
	err := resetLocalState(t.Context())
	if err == nil || !strings.Contains(err.Error(), "reset refused: destructive local operation refused") {
		t.Fatalf("reset error = %v; want the gate's refusal", err)
	}
}
