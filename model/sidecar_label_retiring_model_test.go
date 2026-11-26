//go:build sidecar_pending

package model

// The zone view's retiring aliases (GetNetworkDnsRetiringAliasesInTx in getDnsZoneViewInTx; §12.5's alias and
// retirement rows, D-DNS-10): GET /dns/zone and GET /network/dns name each alias a change or a release retired, with
// the end the release's answer gives, in label order, beside the live alias; and once the 90-day job tombstones them
// they name none.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// The zone view of the world's network, failing t on an error.
func testDnsZoneViewOf(t testing.TB, ctx context.Context, world *testDnsRecordsWorld) *sidecar.DnsZone {
	t.Helper()
	zone, err := GetDnsZoneView(ctx, world.root, world.networkId)
	if err != nil {
		t.Fatal(err)
	}
	return zone
}

// Fails t unless retiring names want's aliases with want's ends, in order.
func requireDnsRetiringAliases(t testing.TB, retiring []sidecar.NetworkLabelRetiringAlias, want ...sidecar.NetworkLabelRetiringAlias) {
	t.Helper()
	if len(retiring) != len(want) {
		t.Fatalf("retiring aliases %+v, want %+v", retiring, want)
	}
	for index := range want {
		if retiring[index].Alias != want[index].Alias || !retiring[index].TombstoneTime.Equal(want[index].TombstoneTime) {
			t.Fatalf("retiring alias %d is %+v, want %+v", index, retiring[index], want[index])
		}
	}
}

func TestDnsZoneViewNamesTheRetiringAliases(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		if zone := testDnsZoneViewOf(t, ctx, world); zone.Alias != name || zone.RetiringAliases != nil {
			t.Fatalf("with a live alias alone the view is %+v", zone)
		}

		// the network's name changes and it claims the new one, which retires the old
		renamed := name + "-two"
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network SET network_name = $2 WHERE network_id = $1`, world.networkId, renamed))
		})
		if _, err := world.claimAlias(ctx, renamed); err != nil {
			t.Fatal(err)
		}
		window := DefaultDnsAliasSettings().RetiringWindow
		stateTimes := map[string]time.Time{}
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range getNetworkDnsAliasRowsInTx(ctx, conn, world.root, world.networkId) {
				stateTimes[row.name] = row.stateTime
			}
		})
		changedEnd := sidecar.NetworkLabelRetiringAlias{Alias: name, TombstoneTime: stateTimes[name].Add(window)}
		zone := testDnsZoneViewOf(t, ctx, world)
		if zone.Alias != renamed {
			t.Fatalf("after the change the view's alias is %q", zone.Alias)
		}
		requireDnsRetiringAliases(t, zone.RetiringAliases, changedEnd)
		console, err := GetNetworkDns(ctx, world.root, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		requireDnsRetiringAliases(t, console.RetiringAliases, changedEnd)

		// a release retires the new one too, until the end its answer gives
		released, err := world.releaseAlias(ctx, renamed)
		if err != nil {
			t.Fatal(err)
		}
		if released.TombstoneTime == nil {
			t.Fatalf("the release answered %+v", released)
		}
		zone = testDnsZoneViewOf(t, ctx, world)
		if zone.Alias != "" {
			t.Fatalf("after the release the view's alias is %q", zone.Alias)
		}
		requireDnsRetiringAliases(t, zone.RetiringAliases, changedEnd, sidecar.NetworkLabelRetiringAlias{Alias: renamed, TombstoneTime: *released.TombstoneTime})

		count, _, err := TombstoneRetiredDnsAliases(ctx, server.NowUtc().Add(window+time.Minute))
		if err != nil || count != 2 {
			t.Fatalf("past 90 days %d tombstoned, %v", count, err)
		}
		if zone := testDnsZoneViewOf(t, ctx, world); zone.RetiringAliases != nil {
			t.Fatalf("after the tombstones the view names %+v", zone.RetiringAliases)
		}
	})
}
