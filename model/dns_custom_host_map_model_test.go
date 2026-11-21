//go:build sidecar_pending

package model

// §13.4's custom-host map, server side (dns_custom_host_map_model.go; DNS-M6-03) on Postgres: each change to the
// active rows raising a version, FS-35's removal leaving the next one, an unchanged list raising nothing, a
// pending host out of the map until certs activates it; the answer warp's lbmap reads, digest and probe name
// alike; a list that moved with no raise raised on the spot; readiness and pending interfaces from reports and
// probes, which GET /network/dns shows; and versions purged at 7 days, the newest kept.

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/warp"
)

// The newest version of the world's map, which must exist.
func (self *testDnsCustomHostWorld) customHostMap(t testing.TB, ctx context.Context) *dnsservice.DnsCustomHostMap {
	t.Helper()
	customHostMap, err := GetDnsCustomHostMap(ctx, self.root)
	if err != nil || customHostMap == nil {
		t.Fatalf("map %+v %v", customHostMap, err)
	}
	return customHostMap
}

// Each change to the active rows raises a version: a weak host entering at once with that version as its
// map_version, a strong host only when certs activates it, a removal leaving the host out of the next version
// (FS-35); a refused pending host, a strict-mode MX host and a change that leaves the list as it was raise none.
func TestDnsCustomHostMapRaisesOnEachChange(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		if customHostMap, err := GetDnsCustomHostMap(ctx, world.root); err != nil || customHostMap != nil {
			t.Fatalf("before any host: %+v %v", customHostMap, err)
		}
		weak := func(host string) {
			t.Helper()
			if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
				t.Fatal(err)
			}
		}
		weak("a." + world.domain)
		first := world.customHostMap(t, ctx)
		if first.Version != 1 || !slices.Equal(first.Hosts, []string{"a." + world.domain}) || world.row(ctx, "a."+world.domain).MapVersion != 1 {
			t.Fatalf("first %+v", first)
		}
		weak("b." + world.domain)
		if second := world.customHostMap(t, ctx); second.Version != 2 || len(second.Hosts) != 2 || world.row(ctx, "a."+world.domain).MapVersion != 1 || world.row(ctx, "b."+world.domain).MapVersion != 2 {
			t.Fatalf("second %+v", second)
		}
		// a strong host stays out of the map while pending, and enters when certs activates it
		world.strongHost(t, ctx, "s."+world.domain, "shop."+world.apex)
		if version := world.customHostMap(t, ctx).Version; version != 2 {
			t.Fatalf("a pending host raised %d", version)
		}
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: "s." + world.domain, Pass: true}); err != nil {
			t.Fatal(err)
		}
		if activated := world.customHostMap(t, ctx); activated.Version != 3 || !slices.Contains(activated.Hosts, "s."+world.domain) || world.row(ctx, "s."+world.domain).MapVersion != 3 {
			t.Fatalf("activated %+v", activated)
		}
		world.strongHost(t, ctx, "r."+world.domain, "shop."+world.apex)
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: "r." + world.domain, Pass: false}); err != nil {
			t.Fatal(err)
		}
		// FS-35: a removal withdraws the route at the next version
		if _, err := RemoveDnsCustomHost(ctx, &DnsCustomHostRequest{Root: world.root, NetworkId: world.networkId, Remove: &sidecar.DnsCustomHostRemoveRequest{Host: "a." + world.domain}}); err != nil {
			t.Fatal(err)
		}
		removed := world.customHostMap(t, ctx)
		if removed.Version != 4 || slices.Contains(removed.Hosts, "a."+world.domain) {
			t.Fatalf("after the removal %+v", removed)
		}
		var unchanged uint64
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			unchanged, err = DnsRaiseCustomHostMapInTx(ctx, tx, world.root)
			server.Raise(err)
		})
		if unchanged != 4 {
			t.Fatalf("an unchanged list raised %d", unchanged)
		}
	})
}

// The answer is what warp's lbmap reads: its decoder takes it, its digest of the hosts is the answer's, and its
// probe name is the edges'.
func TestDnsCustomHostMapIsWhatLbmapReads(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		for _, host := range []string{"x." + world.domain, "a." + world.domain} {
			if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
				t.Fatal(err)
			}
		}
		customHostMap := world.customHostMap(t, ctx)
		data, err := json.Marshal(customHostMap)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := warp.DecodeLbCustomHostMap(data)
		if err != nil || decoded.Version != customHostMap.Version || decoded.Digest != customHostMap.Digest || !slices.Equal(decoded.Hosts, customHostMap.Hosts) {
			t.Fatalf("lbmap read %+v %v", decoded, err)
		}
		if digest := warp.LbCustomHostMapDigest(decoded.Hosts); digest != customHostMap.Digest {
			t.Fatalf("lbmap's digest %s, the map's %s", digest, customHostMap.Digest)
		}
		if warp.LbMapProbeName(customHostMap.Digest) != dnsservice.DnsCustomHostMapProbeName(customHostMap.Digest) {
			t.Fatal("lbmap's probe name is not the edges'")
		}
	})
}

// A list that moved with no raise is raised on the spot, so lbmap never loads hosts under another version's
// digest.
func TestDnsCustomHostMapRaisesAListThatMovedWithNoRaise(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "a." + world.domain, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time)
					VALUES ($1, $2, $3, $4, $5, $6, true, $7)
				`,
				"z."+world.domain, world.root, world.networkId, "blog."+world.apex, sidecar.DnsCustomHostStateActive, sidecar.DnsCustomHostModeWeak, server.NowUtc(),
			))
		})
		customHostMap := world.customHostMap(t, ctx)
		if customHostMap.Version != 2 || !slices.Contains(customHostMap.Hosts, "z."+world.domain) || customHostMap.Digest != dnsservice.DnsCustomHostMapDigest(customHostMap.Hosts) {
			t.Fatalf("map %+v", customHostMap)
		}
	})
}

// Readiness on Postgres: a host pending names both interfaces until each reports its version and passes its
// probes, then takes its ready time, which GET /network/dns shows; a host pending certs' checks names none.
func TestDnsCustomHostReadinessOnPostgres(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		t.Cleanup(Testing_SetDnsUserRoot(world.root))
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		if err := SyncDnsCustomHostMapInterfaces(ctx, interfaces); err != nil {
			t.Fatal(err)
		}
		host := "a." + world.domain
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}
		world.strongHost(t, ctx, "s."+world.domain, "shop."+world.apex)
		customHostMap := world.customHostMap(t, ctx)
		pendingOf := func(name string) (bool, []string) {
			t.Helper()
			networkDns, err := GetNetworkDns(ctx, world.root, world.networkId)
			if err != nil {
				t.Fatal(err)
			}
			for _, customHost := range networkDns.CustomHosts {
				if customHost.Host == name {
					return customHost.Ready, customHost.PendingInterfaces
				}
			}
			t.Fatalf("no %s", name)
			return false, nil
		}
		if ready, pending := pendingOf(host); ready || !slices.Equal(pending, interfaces) {
			t.Fatalf("before any report: %t %v", ready, pending)
		}
		if _, pending := pendingOf("s." + world.domain); len(pending) != 0 {
			t.Fatalf("a host pending certs names interfaces %v", pending)
		}
		settings := dnsservice.DefaultDnsSettings()
		now := server.NowUtc()
		judge := func() {
			t.Helper()
			state, err := GetDnsCustomHostMapState(ctx, world.root)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetDnsCustomHostReadiness(ctx, world.root, settings.Map.Judge(state, now).Ready, now); err != nil {
				t.Fatal(err)
			}
		}
		for index, name := range interfaces {
			err := RecordDnsCustomHostMapReport(ctx, &dnsservice.DnsCustomHostMapReport{
				Format: dnsservice.DnsFormat, Interface: name, Version: customHostMap.Version, Digest: customHostMap.Digest,
				Master: &dnsservice.DnsMapMaster{Pid: 100, StartTimeMillis: 1}, Added: []string{host},
			}, now, settings.Probe.RouteHostCount, settings.Map.ReportHostMaxCount)
			if err != nil {
				t.Fatal(err)
			}
			err = RecordDnsRouteProbes(ctx, &dnsservice.DnsRouteProbeReport{Format: dnsservice.DnsFormat, Interfaces: []dnsservice.DnsRouteProbeResult{{
				Name: name, Version: customHostMap.Version, ProbeName: dnsservice.DnsCustomHostMapProbeName(customHostMap.Digest), ProbePassed: true,
				Hosts: []dnsservice.DnsRouteProbeHost{{Host: host, Passed: true}},
			}}}, now)
			if err != nil {
				t.Fatal(err)
			}
			judge()
			ready, pending := pendingOf(host)
			if index == 0 && (ready || !slices.Equal(pending, interfaces[1:])) {
				t.Fatalf("one interface current: %t %v", ready, pending)
			}
			if index == 1 && (!ready || len(pending) != 0) {
				t.Fatalf("both current: %t %v", ready, pending)
			}
		}
		rows, err := DnsCustomHostReadiness(ctx, world.root, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		for _, row := range rows {
			if row.Host == host && (row.ReadyTime == nil || row.MapVersion != customHostMap.Version) {
				t.Fatalf("readiness %+v", row)
			}
		}
		// an interface that left the inventory takes its report with it
		if err := SyncDnsCustomHostMapInterfaces(ctx, interfaces[:1]); err != nil {
			t.Fatal(err)
		}
		state, err := GetDnsCustomHostMapState(ctx, world.root)
		if err != nil || len(state.Interfaces) != 1 || state.Interfaces[0].Name != interfaces[0] {
			t.Fatalf("after the inventory shrank %+v %v", state, err)
		}
	})
}

// Versions past their 7 days go, the newest whatever its age stays.
func TestDnsCustomHostMapPurgeKeepsTheNewest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		for _, host := range []string{"a." + world.domain, "b." + world.domain} {
			if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: host, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
				t.Fatal(err)
			}
		}
		purged, err := PurgeDnsCustomHostMapVersions(ctx, server.NowUtc().Add(8*24*time.Hour))
		if err != nil || purged != 1 {
			t.Fatalf("purged %d %v", purged, err)
		}
		state, err := GetDnsCustomHostMapState(ctx, world.root)
		if err != nil || len(state.Versions) != 1 || state.Versions[0].Version != 2 {
			t.Fatalf("kept %+v %v", state, err)
		}
		if customHostMap := world.customHostMap(t, ctx); customHostMap.Version != 2 {
			t.Fatalf("the newest %+v", customHostMap)
		}
	})
}

// A later poll or reload can replace the live probe sample before the incident is listed.
func TestDnsRestoreInventorySurvivesTheNextMapReport(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		root := testDnsRoot(t, ctx)
		now := server.NowUtc()
		if err := SyncDnsCustomHostMapInterfaces(ctx, []string{"lb-1"}); err != nil {
			t.Fatal(err)
		}
		target, err := DnsRaiseCustomHostMapForRestore(ctx, root, server.NewId(), now)
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 23)
		for i := range names {
			names[i] = fmt.Sprintf("host-%02d.example", i)
		}
		report := &dnsservice.DnsCustomHostMapReport{Interface: "lb-1", Version: target, Digest: dnsservice.DnsCustomHostMapDigest(names), Master: &dnsservice.DnsMapMaster{Pid: 7}, Added: names, Dropped: []string{"retired.example"}}
		if err := RecordDnsCustomHostMapReport(ctx, report, now, 16, 128); err != nil {
			t.Fatal(err)
		}
		report.Version++
		report.Added = []string{"later.example"}
		report.Dropped = nil
		report.Digest = dnsservice.DnsCustomHostMapDigest(report.Added)
		if err := RecordDnsCustomHostMapReport(ctx, report, now.Add(time.Second), 16, 128); err != nil {
			t.Fatal(err)
		}
		state, err := GetDnsCustomHostMapState(ctx, root)
		if err != nil || len(state.Interfaces) != 1 {
			t.Fatalf("interfaces: %+v %v", state, err)
		}
		row := state.Interfaces[0]
		if !slices.Equal(row.AddedHosts, report.Added) || row.Restore == nil || !row.Restore.Received || row.Restore.Version != target ||
			!slices.Equal(row.Restore.Added, names) || !slices.Equal(row.Restore.Dropped, []string{"retired.example"}) {
			t.Fatalf("ordinary report and retained inventory: %+v", row)
		}
	})
}
