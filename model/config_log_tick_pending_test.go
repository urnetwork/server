//go:build sidecar_pending

package model

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// §7.9's reap ticks at api (TEAM-M2-14), which need MG1's lease and bindings.

// The pending tick tasks of the network.
func testConfigLogTickTasks(t testing.TB, ctx context.Context, networkId server.Id) (tickTasks []*task.Task) {
	t.Helper()
	tickName := task.NewTaskTarget(AppendConfigLogTickTask).TargetFunctionName()
	for _, pending := range task.GetTasks(ctx, task.ListPendingTasks(ctx)...) {
		if pending.FunctionName == tickName && pending.ArgsJson == `{"network_id":"`+networkId.String()+`"}` {
			tickTasks = append(tickTasks, pending)
		}
	}
	return
}

// Every reap of a network in a minute schedules the one tick task of that network, a tick interval on, so
// their ids go in one tick; another network's reap has its own.
func TestConfigLogReapsCoalesceIntoOneTick(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		other := newClientOwnerFixture(t)
		before := server.NowUtc()
		for range 3 {
			ephemeral := newTestEphemeralClient(t, self, server.NowUtc())
			reaped, err := ReapEphemeralClient(self.ctx, ephemeral.clientId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, reaped, true)
		}
		otherEphemeral := newTestEphemeralClient(t, other, server.NowUtc())
		reaped, err := ReapEphemeralClient(other.ctx, otherEphemeral.clientId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, reaped, true)

		tickTasks := testConfigLogTickTasks(t, self.ctx, self.networkId)
		connect.AssertEqual(t, len(tickTasks), 1)
		interval := DefaultConfigLogTickSettings().TickInterval
		connect.AssertEqual(t, !tickTasks[0].RunAt.Before(before.Add(interval-time.Second)), true)
		connect.AssertEqual(t, len(testConfigLogTickTasks(t, other.ctx, other.networkId)), 1)
	})
}

// A tick names a sidecar only once every client bound to it is inactive or gone: a reaped ephemeral client
// leaves its sidecar so, a live one or a second live client does not.
func TestReapedEphemeralSidecarsAreThoseWithNoLiveClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newClientOwnerFixture(t)
		bind := func(sidecarId server.Id, generation int64, clientId server.Id) {
			server.Tx(self.ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					self.ctx,
					`
						INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
						VALUES ($1, $2, $3, $4, $5, $6)
					`,
					sidecarId, generation, clientId, make([]byte, 32), []byte("synthetic binding "+clientId.String()), server.NowUtc(),
				))
			})
		}
		reapedSidecar := server.NewId()
		reapedClient := newTestEphemeralClient(t, self, server.NowUtc())
		bind(reapedSidecar, 1, reapedClient.clientId)
		liveSidecar := server.NewId()
		liveClient := newTestEphemeralClient(t, self, server.NowUtc())
		bind(liveSidecar, 1, liveClient.clientId)
		mixedSidecar := server.NewId()
		mixedOld := newTestEphemeralClient(t, self, server.NowUtc())
		mixedNew := newTestEphemeralClient(t, self, server.NowUtc())
		bind(mixedSidecar, 1, mixedOld.clientId)
		bind(mixedSidecar, 2, mixedNew.clientId)
		unbound := server.NewId()

		for _, clientId := range []server.Id{reapedClient.clientId, mixedOld.clientId} {
			reaped, err := ReapEphemeralClient(self.ctx, clientId)
			connect.AssertEqual(t, err, nil)
			connect.AssertEqual(t, reaped, true)
		}
		got := reapedEphemeralSidecars(self.ctx, []sidecar.Id{
			sidecar.Id(reapedSidecar), sidecar.Id(liveSidecar), sidecar.Id(mixedSidecar), sidecar.Id(unbound),
		})
		connect.AssertEqual(t, slices.Equal(got, []sidecar.Id{sidecar.Id(reapedSidecar)}), true)
	})
}
