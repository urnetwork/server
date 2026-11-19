//go:build sidecar_pending

package main

// TLS-M6-07's custom-host rows through the real splitter (FS-122, FS-42's M6 step; FI-11, FI-12, §13.4, §12.6), on
// tlsedge_split_t3_test.go's split: real nginx, warp's lb process with lbmap, the dns API on Postgres and tlsedge.
//
//   - FS-122's window clause: three adds and a remove inside one load window load in one reload, lbmap reporting the
//     newest version and none between, each added host routing and the removed one not, and each added host ready
//     once that reload's version passes its route probes.
//   - FS-42's M6 step: custom hosts added or removed after the dump leave or return to the map the restore regenerates
//     (DnsRaiseCustomHostMapForRestore, 2^24 above), lbmap loads it, the hosts route as the restored rows say, the
//     restore lists each, and each stays pending until every interface is current for the restore's version: on two lb
//     interfaces, one stopped across the loss, every host stands at the restore's version with no ready time, then
//     pending naming the stopped one alone while the other passes its probes, and ready once the stopped one starts on
//     the restore's version and passes its own.
//
// The tests step on lbmap's applied reports; lbmap's own load window is the only wait, a real one inside the lb. The
// route probes are tlsedge_map_stale_t3_test.go's edge stand-in.

import (
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A host name of the test's own under a documentation domain drawn per run.
func t3CustomHost(t testing.TB, label string) string {
	t.Helper()
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%s.corp-%x.example", label, suffix)
}

func TestT3Fs122ChangesInsideOneWindowLoadInOneReload(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		hosts := []string{t3CustomHost(t, "app"), t3CustomHost(t, "shop"), t3CustomHost(t, "docs"), t3CustomHost(t, "blog")}
		first := world.mapCustomHost(hosts[0])
		world.reloadNames()
		split := newT3Split(t, world, hosts)
		// a load window long against the four writes below, short against the test
		split.setEnv("WARP_LBMAP_LOAD_DELAY", "2s")
		split.startLb(first)

		var last uint64
		for _, host := range hosts[1:] {
			last = world.mapCustomHost(host)
		}
		last = world.unmapCustomHost(hosts[0])
		world.reloadNames()
		split.awaitLoaded(last)
		for _, reported := range split.reports.reported() {
			if first < reported && reported < last {
				t.Fatalf("lbmap reported %d between %d and %d: the window's changes took more than one reload", reported, first, last)
			}
		}
		for _, host := range hosts[1:] {
			if !split.reachesDevice(host, []string{"h2"}, split.customPool) {
				t.Errorf("%s did not route after the window's reload", host)
			}
		}
		if !split.reachesPlatform(hosts[0]) {
			t.Error("the host removed inside the window still routed")
		}
		// the one reload's version passes its probes, and each host it added is ready at it
		split.api.probeRound(t, world, []*t3Split{split})
		for _, host := range hosts[1:] {
			t3SplitAwaitReady(t, world, host)
		}
	})
}

// A row of network_dns_custom_host as a dump holds it.
type t3CustomHostRow struct {
	host       string
	root       string
	networkId  server.Id
	owner      string
	state      string
	caaMode    string
	chainOk    bool
	createTime time.Time
}

// The network's custom-host rows now, as a dump taken now would hold them.
func t3DumpCustomHosts(t testing.TB, world *t3EdgeWorld) []*t3CustomHostRow {
	t.Helper()
	rows := []*t3CustomHostRow{}
	server.Db(world.ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			world.ctx,
			`SELECT host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time FROM network_dns_custom_host WHERE network_id = $1`,
			server.Id(world.networkId),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &t3CustomHostRow{}
				server.Raise(result.Scan(&row.host, &row.root, &row.networkId, &row.owner, &row.state, &row.caaMode, &row.chainOk, &row.createTime))
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// Puts the network's custom-host rows back as dump holds them, as a Postgres restore does.
func t3RestoreCustomHosts(t testing.TB, world *t3EdgeWorld, dump []*t3CustomHostRow) {
	t.Helper()
	server.Tx(world.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(world.ctx, `DELETE FROM network_dns_custom_host WHERE network_id = $1`, server.Id(world.networkId)))
		for _, row := range dump {
			server.RaisePgResult(tx.Exec(
				world.ctx,
				`
					INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				`,
				row.host, row.root, row.networkId, row.owner, row.state, row.caaMode, row.chainOk, row.createTime,
			))
		}
	}, server.TxReadCommitted)
}

func TestT3Fs42CustomHostsAfterTheDumpLeaveOrReturnToTheRestoredMap(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		kept, returned, left := t3CustomHost(t, "app"), t3CustomHost(t, "shop"), t3CustomHost(t, "docs")
		hosts := []string{kept, returned, left}
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		api := newT3SplitApi(t, world, interfaces)
		world.mapCustomHost(kept)
		version := world.mapCustomHost(returned)
		world.reloadNames()
		split := newT3SplitOn(t, world, api, interfaces[0], hosts)
		lagging := newT3SplitOn(t, world, api, interfaces[1], hosts)
		split.startLb(version)
		lagging.startLb(version)
		splits := []*t3Split{split, lagging}
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, kept)
		t3SplitAwaitReady(t, world, returned)
		dump := t3DumpCustomHosts(t, world)

		// after the dump: one host added, one removed, each loaded on both interfaces
		world.mapCustomHost(left)
		version = world.unmapCustomHost(returned)
		world.reloadNames()
		split.awaitLoaded(version)
		lagging.awaitLoaded(version)
		if !split.reachesDevice(left, []string{"h2"}, split.customPool) || !split.reachesPlatform(returned) {
			t.Fatal("the changes after the dump did not load")
		}
		// one interface's lb is down across the loss
		lagging.stopLb()

		// Postgres lost and restored from the dump; the restore regenerates the map above every version
		t3RestoreCustomHosts(t, world, dump)
		incidentId := server.NewId()
		restored, err := model.DnsRaiseCustomHostMapForRestore(world.ctx, world.root, incidentId, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		if restored < version+(1<<24) {
			t.Fatalf("the restore's version %d is not 2^24 above %d", restored, version)
		}
		listing, err := model.ListDnsRestoreCustomHosts(world.ctx, world.root, incidentId, []string{returned}, []string{left}, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		listed := []string{}
		for _, entry := range listing.Entries {
			listed = append(listed, fmt.Sprintf("%+v", entry))
		}
		for _, host := range []string{returned, left} {
			if !slices.ContainsFunc(listed, func(entry string) bool { return strings.Contains(entry, host) }) {
				t.Errorf("the restore lists no %s: %v", host, listed)
			}
		}
		world.reloadNames()
		split.awaitLoaded(restored)
		if !split.reachesDevice(kept, []string{"h2"}, split.customPool) || !split.reachesDevice(returned, []string{"h2"}, split.customPool) {
			t.Fatal("a host the dump holds did not route at the restore's version")
		}
		if !split.reachesPlatform(left) {
			t.Fatal("a host added after the dump still routed after the restore")
		}
		readiness, err := model.DnsCustomHostReadiness(world.ctx, world.root, server.Id(world.networkId))
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range readiness {
			if row.MapVersion != restored || row.ReadyTime != nil {
				t.Errorf("%s at version %d, ready %v, after the restore", row.Host, row.MapVersion, row.ReadyTime)
			}
		}
		if len(readiness) != 2 {
			t.Fatalf("%d hosts after the restore", len(readiness))
		}

		// the running interface passes its probes at the restore's version: each host stays pending, naming the other
		api.probeRound(t, world, []*t3Split{split})
		for _, host := range []string{kept, returned} {
			if row := t3SplitReadiness(t, world, host); row.ReadyTime != nil || !slices.Equal(row.PendingInterfaces, []string{interfaces[1]}) {
				t.Fatalf("%s with one interface current for the restore: %+v", host, row)
			}
		}
		// the other starts on the restore's version and passes its own: every interface is current, each host ready
		lagging.startLb(restored)
		if !lagging.reachesDevice(returned, []string{"h2"}, lagging.customPool) || !lagging.reachesPlatform(left) {
			t.Fatal("the restarted interface does not route the restored map")
		}
		api.probeRound(t, world, splits)
		t3SplitAwaitReady(t, world, kept)
		t3SplitAwaitReady(t, world, returned)
	})
}
