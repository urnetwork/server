//go:build sidecar_pending

package tlsedgehost

// FS-41's and FS-42's M5 steps for tlsedge on T3 (TLS-M5-26; §19.10, §21.10, FI-12), on tlsedge_t3_test.go's world:
// the block on the private stack's Postgres and Redis, with network_tls_suspension restored as a dump holds it.
//
//   - FS-42, Postgres lost and restored from a dump older than the abuse mail: a kill and a suspension mailed after the
//     dump lose their rows and a lift mailed after it gets its row back, while Redis keeps what the mail wrote. tlsedge's
//     first reload after the restore keeps the kill and the suspension from their {tes_} fields, the restored rows
//     lacking them, and keeps the lifted host killed from its restored row, the closed direction (a reader drops a kill
//     only once neither its row nor its key holds it); §19.10's abuse step then replays the mail, and from the next reload
//     the kill and the suspension hold on their rows again and the lifted host routes.
//   - FS-41, Redis lost with Postgres intact: every {tes_} key gone, the rows alone keep each kill through the next
//     reload, and a visitor of a killed host still gets unrecognized_name with no Find.
//   - The label map's restore (§12.6): a restore that raises the root's restore serial makes the engine take a new
//     snapshot, and the network's names map as the restored rows hold them.
//
// The certificate re-order of FS-42's M5 step (a chain newer than the dump ordered again with replaces, plan reading 7)
// is the certificate manager's, held on T2 by TestTlsCertManagerReordersALostRowWithReplaces; its T3 through server's
// certs routes is not here. Time moves only on the world's fake clock, by the kill switch's reload interval.

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// network_tls_suspension's rows of one network, as a dump holds them.
type t3EdgeSuspensionDump struct {
	networkId server.Id
	rows      []*sidecar.TlsSuspendArgs
}

// The network's rows now, as a dump taken now would hold them.
func t3EdgeDumpSuspensions(t testing.TB, ctx context.Context, networkId sidecar.Id) *t3EdgeSuspensionDump {
	t.Helper()
	rows, err := model.GetTlsSuspensions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	dump := &t3EdgeSuspensionDump{networkId: server.Id(networkId)}
	for _, row := range rows {
		if row.NetworkId == networkId {
			kept := *row
			dump.rows = append(dump.rows, &kept)
		}
	}
	return dump
}

// Puts the network's rows back as the dump holds them, as a Postgres restore does, and leaves Redis as it is: rows
// written after the dump are gone and rows deleted after it are back.
func (self *t3EdgeSuspensionDump) restore(t testing.TB, ctx context.Context) {
	t.Helper()
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_tls_suspension WHERE network_id = $1`, self.networkId))
		for _, row := range self.rows {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_tls_suspension (network_id, host, reason, until_time) VALUES ($1, $2, $3, $4)`,
				self.networkId,
				row.Host,
				row.Reason,
				t3EdgeUntil(row.Until),
			))
		}
	})
}

// A row's until as the table takes it: null for none.
func t3EdgeUntil(until time.Time) *time.Time {
	if until.IsZero() {
		return nil
	}
	utc := until.UTC()
	return &utc
}

// Moves the clock past the kill switch's reload and waits for the reload, so the engine has read every row and key.
func (self *t3EdgeWorld) reloadKills() {
	self.t.Helper()
	count, _ := self.host.engine.KillLoads()
	self.clock.Advance(self.settings.TlsEdge.KillReloadInterval)
	hostTestAwait(self.t, "the kill switch's reload", count+1, self.host.engine.KillLoads)
}

// Whether a visitor of host gets unrecognized_name with no Find, as a killed host's does, or reaches the device.
func (self *t3EdgeWorld) killed(host string) bool {
	self.t.Helper()
	if !self.host.engine.Kills().Kills(self.networkId, host) {
		self.visit(self.serverName(host), []string{"h2"}, []byte("routes"))
		return false
	}
	dials := len(self.dial.Requests())
	answer := self.answer(t3EdgeHello(self.t, self.serverName(host), []string{"h2"}))
	if !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName)) {
		self.t.Fatalf("a killed host answered %x", answer)
	}
	if len(self.dial.Requests()) != dials {
		self.t.Fatal("a killed host reached Find")
	}
	return true
}

// FS-42's M5 TLS step: a kill, a suspension and a lift mailed after the dump hold through tlsedge's first reloads, the
// kill's and the suspension's {tes_} fields keeping what the restored rows lack, and once the abuse step replays the
// mail the lift lets its host route.
func TestT3TlsedgeKillsHoldThroughAPostgresRestore(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		// before the dump: old is killed, the case the lift will end
		world.kill("old")
		dump := t3EdgeDumpSuspensions(t, world.ctx, world.networkId)
		if len(dump.rows) != 1 {
			t.Fatalf("the dump holds %d rows", len(dump.rows))
		}

		// mailed after the dump: a kill, a suspension to a time and a lift
		world.kill("shop")
		suspensionUntil := world.clock.Now().Add(48 * time.Hour)
		count, _ := world.host.engine.KillChanges()
		if err := model.SetTlsSuspension(world.ctx, &sidecar.TlsSuspendArgs{NetworkId: world.networkId, Host: "store", Reason: "t3", Until: suspensionUntil}); err != nil {
			t.Fatal(err)
		}
		hostTestAwait(t, "the suspension's key event", count+1, world.host.engine.KillChanges)
		world.lift("old")
		for host, want := range map[string]bool{"old": false, "shop": true, "store": true} {
			if got := world.killed(host); got != want {
				t.Fatalf("before the loss %s killed %t", host, got)
			}
		}

		// Postgres lost and restored from the dump: shop's and store's rows are gone, old's is back
		dump.restore(t, world.ctx)
		world.reloadKills()
		for host, want := range map[string]bool{"old": true, "shop": true, "store": true} {
			if got := world.killed(host); got != want {
				t.Fatalf("after the restore %s killed %t", host, got)
			}
		}

		// §19.10's abuse step replays what was mailed after the dump
		if err := model.SetTlsSuspension(world.ctx, &sidecar.TlsSuspendArgs{NetworkId: world.networkId, Host: "shop", Reason: "t3"}); err != nil {
			t.Fatal(err)
		}
		if err := model.SetTlsSuspension(world.ctx, &sidecar.TlsSuspendArgs{NetworkId: world.networkId, Host: "store", Reason: "t3", Until: suspensionUntil}); err != nil {
			t.Fatal(err)
		}
		if err := model.LiftTlsSuspension(world.ctx, server.Id(world.networkId), "old"); err != nil {
			t.Fatal(err)
		}
		world.reloadKills()
		for host, want := range map[string]bool{"old": false, "shop": true, "store": true} {
			if got := world.killed(host); got != want {
				t.Fatalf("after the replay %s killed %t", host, got)
			}
		}
		replayed := t3EdgeDumpSuspensions(t, world.ctx, world.networkId)
		if len(replayed.rows) != 2 {
			t.Fatalf("after the replay %d rows", len(replayed.rows))
		}
	})
}

// FS-41's M5 TLS step on the kill switch: Redis lost with Postgres intact leaves every kill on its row, so a killed host
// stays refused through the next reload with no {tes_} key behind it.
func TestT3TlsedgeKillsHoldThroughARedisLoss(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		world.kill("shop")
		if err := world.stack.Delete(world.ctx, []string{sidecar.TlsKillKey(world.networkId), sidecar.TlsKillTriggerKey(world.networkId)}); err != nil {
			t.Fatal(err)
		}
		fields, err := world.stack.HashGetAll(world.ctx, sidecar.TlsKillKey(world.networkId))
		if err != nil || 0 < len(fields) {
			t.Fatalf("the wipe left %d fields, %v", len(fields), err)
		}
		world.reloadKills()
		if !world.killed("shop") {
			t.Fatal("a kill whose key was lost stopped holding")
		}
		if world.killed("blog") {
			t.Fatal("a host never killed was refused")
		}
	})
}

// §12.6's restore of the label map: a restore that raises the root's restore serial makes the engine take a new
// snapshot rather than follow its old cursor, and the network's names map as the restored rows hold them.
func TestT3TlsedgeTakesASnapshotAfterALabelMapRestore(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		_, restoreSerial := world.host.engine.Names().Serials()
		root := world.root + "."
		if err := model.SetDnsRestoring(world.ctx, root, true); err != nil {
			t.Fatal(err)
		}
		if _, err := model.CommitDnsRestore(world.ctx, nil, root, server.NewId(), 0); err != nil {
			t.Fatal(err)
		}
		if err := model.SetDnsRestoring(world.ctx, root, false); err != nil {
			t.Fatal(err)
		}
		count, _ := world.host.engine.NameLoads()
		world.clock.Advance(world.settings.TlsEdge.NameReloadInterval)
		hostTestAwait(t, "the label map's read after the restore", count+1, world.host.engine.NameLoads)
		if _, newRestoreSerial := world.host.engine.Names().Serials(); newRestoreSerial == restoreSerial {
			t.Fatal("the engine did not take the restore's snapshot")
		}
		world.visit(world.serverName("blog"), []string{"h2"}, []byte("after the restore"))
	})
}
