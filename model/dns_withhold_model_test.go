//go:build sidecar_pending

package model

// §20.7's downgrade column, dns side (dns_withhold_model.go; DNS-M6-07), FS-51's dns side on Postgres: the newest
// user RRsets past the limit withheld through the journal with their rows kept, MX and TLSA untouched; a second
// pass writing nothing; releases oldest first; a withheld host's children keeping the wildcard by a copy row; a
// suspension's changes leaving a downgrade's withholding alone; and the custom-host half (QD-840): web hosts
// withheld out of the map and tlsedge's rows with their rows kept, while the zone, its MX, TLSA and router rows,
// a strict-mode MX host and a host pending certs' checks stay as they were, and a release bringing each web host
// back at the next map version, serving once every interface is current. FS-51's 14 days are QUOTA-M6-05's
// clock, which calls these once they are past.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Writes a TXT at each host in order, each a minute after the last by its first write, so newest is last.
func (self *testDnsRecordsWorld) writeAged(t testing.TB, ctx context.Context, hosts ...string) {
	t.Helper()
	start := server.NowUtc().Add(-time.Hour)
	for index, host := range hosts {
		owner := host + "." + self.apex
		if _, err := self.write(ctx, testDnsUpsert(owner, "TXT", `"`+host+`"`)); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE network_dns_record SET create_time = $3 WHERE root = $1 AND owner = $2`,
				self.root, owner, start.Add(time.Duration(index)*time.Minute),
			))
		})
	}
}

// The user RRsets' owners withheld and served, by host.
func (self *testDnsRecordsWorld) withheldHosts(t testing.TB, ctx context.Context) (withheld []string, served []string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getDnsZoneRowsInTx(ctx, conn, self.root, self.networkId) {
			if row.set.Kind != dnsservice.DnsRecordKindUser {
				continue
			}
			host := strings.TrimSuffix(row.set.Owner, "."+self.apex)
			if row.withheld {
				withheld = append(withheld, host)
			} else {
				served = append(served, host)
			}
		}
	})
	slices.Sort(withheld)
	slices.Sort(served)
	return
}

// Runs one convergence in a transaction of its own.
func (self *testDnsRecordsWorld) quota(t testing.TB, ctx context.Context, limit int64, release bool) *DnsWriteResult {
	t.Helper()
	var result *DnsWriteResult
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		if release {
			result, returnErr = DnsReleaseWithheldInTx(ctx, tx, self.networkId, self.root, limit, false)
		} else {
			result, returnErr = DnsWithholdOverQuotaInTx(ctx, tx, self.networkId, self.root, limit)
		}
	}, server.TxReadCommitted)
	if returnErr != nil {
		t.Fatal(returnErr)
	}
	return result
}

// The newest past the limit leave the journal with their rows kept, the entries count unchanged; the zone's mail
// and key rows stand; a second pass writes nothing; a raised limit serves the oldest withheld first, and no limit
// serves all.
func TestDnsWithholdOverQuotaWithholdsTheNewest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
			t.Fatal(err)
		}
		tlsa := dnsservice.DnsRecordSet{Owner: "_25._tcp.mx." + world.apex, Type: dns.TypeTLSA, Ttl: 300, Kind: dnsservice.DnsRecordKindKey, Rdata: []string{"3 1 1 " + strings.Repeat("ab", 32)}}
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: world.root, NetworkId: world.networkId, Upserts: []dnsservice.DnsRecordSet{tlsa}}); err != nil {
			t.Fatal(err)
		}
		world.writeAged(t, ctx, "h1", "h2", "h3", "h4", "h5")
		before := world.serial(t, ctx)
		result := world.quota(t, ctx, 3, false)
		if result == nil || result.Serial != before+1 {
			t.Fatalf("withhold %+v", result)
		}
		if withheld, served := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"h4", "h5"}) || !slices.Equal(served, []string{"h1", "h2", "h3"}) {
			t.Fatalf("withheld %v served %v", withheld, served)
		}
		journal := testDnsJournal(t, ctx, world.root, before)
		if len(journal) != 1 || len(journal[0].Deletes) != 2 {
			t.Fatalf("journal %+v", journal)
		}
		if count := DnsEntryCount(ctx, world.networkId); count != 5 {
			t.Errorf("%d entries, the withheld still count", count)
		}
		// MX and TLSA, mail's and the key's, are no user rows and stand
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range getDnsZoneRowsInTx(ctx, conn, world.root, world.networkId) {
				if row.withheld && row.set.Kind != dnsservice.DnsRecordKindUser {
					t.Errorf("withheld %s %s", row.set.Owner, dns.TypeToString[row.set.Type])
				}
			}
		})
		if again := world.quota(t, ctx, 3, false); again != nil {
			t.Fatalf("a second pass wrote %+v", again)
		}
		world.quota(t, ctx, 4, true)
		if withheld, _ := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"h5"}) {
			t.Fatalf("a raised limit served %v", withheld)
		}
		world.quota(t, ctx, -1, true)
		if withheld, served := world.withheldHosts(t, ctx); len(withheld) != 0 || len(served) != 5 {
			t.Fatalf("no limit: withheld %v", withheld)
		}
		testDnsAdminResultsContain(t, ctx, world.networkId, AdminEventResultDnsWithheld, AdminEventResultDnsReleased)
	})
}

// Each result among the network's quota events.
func testDnsAdminResultsContain(t testing.TB, ctx context.Context, networkId server.Id, results ...string) {
	t.Helper()
	got := testDnsAdminEventResults(ctx, networkId, AdminEventActionDnsQuotaWithhold)
	for _, result := range results {
		if !slices.Contains(got, result) {
			t.Errorf("no %s among %v", result, got)
		}
	}
}

// A withheld host whose child stays served becomes an empty non-terminal, which keeps answering the wildcard by a
// copy row written with the withholding, and the child, no longer behind the host's own record, takes one too.
// Served again, the host holds only its TXT and so keeps its copy (QD-1190), while the child, which the host blocks
// the wildcard from once more, loses its own.
func TestDnsWithholdKeepsTheWildcardAtAWithheldHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		blog := "blog." + world.apex
		note := "_note.blog." + world.apex
		world.writeAged(t, ctx, "_note.blog", "blog")
		world.quota(t, ctx, 1, false)
		if withheld, _ := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"blog"}) {
			t.Fatalf("withheld %v", withheld)
		}
		for _, owner := range []string{blog, note} {
			if held, withheld := world.copyAt(ctx, owner); !held || withheld {
				t.Fatalf("withholding the host left %s's copy held %t, withheld %t", owner, held, withheld)
			}
		}
		world.quota(t, ctx, -1, true)
		if held, withheld := world.copyAt(ctx, blog); !held || withheld {
			t.Errorf("serving the host's TXT again left its copy held %t, withheld %t", held, withheld)
		}
		if held, _ := world.copyAt(ctx, note); held {
			t.Error("the child's copy stayed once the host's own record was served again")
		}
	})
}

// A suspension's changes leave a downgrade's withholding alone: suspending and lifting another host keeps the
// withheld RRset withheld; and a zone suspended is left to its Restore.
func TestDnsWithholdSurvivesASuspensionsChanges(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		world.writeAged(t, ctx, "h1", "h2", "h3")
		world.quota(t, ctx, 2, false)
		if err := SuspendHost(ctx, world.networkId, "h1"); err != nil {
			t.Fatal(err)
		}
		if err := Restore(ctx, world.networkId, "h1"); err != nil {
			t.Fatal(err)
		}
		if withheld, _ := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"h3"}) {
			t.Fatalf("after another host's suspension withheld %v", withheld)
		}
		if err := SuspendZone(ctx, world.networkId, "test", nil); err != nil {
			t.Fatal(err)
		}
		if result := world.quota(t, ctx, 0, false); result != nil {
			t.Fatalf("a suspended zone was written %+v", result)
		}
	})
}

// The custom-host half's round (QD-840): each active web host, weak or strong, goes withheld, out of the map at a
// new version and routed by no edge, its row kept, while a strict-mode MX host stays active; certs' verdict does
// not touch a withheld host; a pass with nothing to move writes nothing; and DnsReleaseWithheldInTx with its
// custom hosts, the renewal's call, brings each back active at the next version with no ready time yet.
func TestDnsWithholdCustomHostsAndReleaseThem(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "a." + world.domain, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}
		world.strongHost(t, ctx, "s."+world.domain, "shop."+world.apex)
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: "s." + world.domain, Pass: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "mx." + world.domain, Owner: "mx." + world.apex}); err != nil {
			t.Fatal(err)
		}
		before := world.customHostMap(t, ctx)
		withhold := func() int {
			t.Helper()
			var count int
			var returnErr error
			server.Tx(ctx, func(tx server.PgTx) {
				count, returnErr = DnsWithholdCustomHostsInTx(ctx, tx, world.networkId)
			}, server.TxReadCommitted)
			if returnErr != nil {
				t.Fatal(returnErr)
			}
			return count
		}
		if count := withhold(); count != 2 {
			t.Fatalf("withheld %d", count)
		}
		withheld := world.customHostMap(t, ctx)
		if withheld.Version != before.Version+1 || len(withheld.Hosts) != 0 {
			t.Fatalf("the map after withholding %+v", withheld)
		}
		for _, host := range []string{"a." + world.domain, "s." + world.domain} {
			if row := world.row(ctx, host); row == nil || row.State != DnsCustomHostStateWithheld || row.MapVersion != 0 {
				t.Fatalf("%s %+v", host, row)
			}
			if testDnsCustomHostMapped(ctx, host) {
				t.Fatalf("%s still routed", host)
			}
		}
		if row := world.row(ctx, "mx."+world.domain); row.State != sidecar.DnsCustomHostStateActive {
			t.Fatalf("the strict-mode MX host %+v", row)
		}
		// certs' verdict leaves a withheld host as it is
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: "s." + world.domain, Pass: true}); !errors.Is(err, ErrDnsCustomHostUnknown) {
			t.Fatalf("a verdict on a withheld host: %v", err)
		}
		if count := withhold(); count != 0 {
			t.Fatalf("a second pass withheld %d", count)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := DnsReleaseWithheldInTx(ctx, tx, world.networkId, world.root, -1, true)
			server.Raise(err)
		}, server.TxReadCommitted)
		released := world.customHostMap(t, ctx)
		if released.Version != withheld.Version+1 || len(released.Hosts) != 2 {
			t.Fatalf("the map after the release %+v", released)
		}
		for _, host := range []string{"a." + world.domain, "s." + world.domain} {
			if row := world.row(ctx, host); row.State != sidecar.DnsCustomHostStateActive || row.MapVersion != released.Version || row.ReadyTime != nil {
				t.Fatalf("%s after the release %+v", host, row)
			}
		}
		testDnsAdminResultsContain(t, ctx, world.networkId, AdminEventResultDnsWithheld, AdminEventResultDnsReleased)
	})
}

// Withholds the world network's custom web hosts in a transaction of its own, answering how many went withheld.
func (self *testDnsCustomHostWorld) withholdCustomHosts(t testing.TB, ctx context.Context) int {
	t.Helper()
	var count int
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		count, returnErr = DnsWithholdCustomHostsInTx(ctx, tx, self.networkId)
	}, server.TxReadCommitted)
	if returnErr != nil {
		t.Fatal(returnErr)
	}
	return count
}

// Releases the world network's withheld custom web hosts in a transaction of its own, answering how many went
// active.
func (self *testDnsCustomHostWorld) releaseCustomHosts(t testing.TB, ctx context.Context) int {
	t.Helper()
	var count int
	var returnErr error
	server.Tx(ctx, func(tx server.PgTx) {
		count, returnErr = DnsReleaseCustomHostsInTx(ctx, tx, self.networkId)
	}, server.TxReadCommitted)
	if returnErr != nil {
		t.Fatal(returnErr)
	}
	return count
}

// The custom host GET /network/dns shows for host, which must be there.
func (self *testDnsCustomHostWorld) shownCustomHost(t testing.TB, ctx context.Context, host string) sidecar.DnsCustomHost {
	t.Helper()
	networkDns, err := GetNetworkDns(ctx, self.root, self.networkId)
	if err != nil {
		t.Fatal(err)
	}
	for _, customHost := range networkDns.CustomHosts {
		if customHost.Host == host {
			return customHost
		}
	}
	t.Fatalf("GET /network/dns shows no %s", host)
	return sidecar.DnsCustomHost{}
}

// The zone's rows, one line each in owner and type order: what edges serve and what a downgrade keeps.
func (self *testDnsCustomHostWorld) zoneLines(ctx context.Context) []string {
	lines := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getDnsZoneRowsInTx(ctx, conn, self.root, self.networkId) {
			lines = append(lines, fmt.Sprintf("%s %s %s %d %v withheld=%t", row.set.Owner, dns.TypeToString[row.set.Type], row.set.Kind, row.set.Ttl, row.set.Rdata, row.withheld))
		}
	})
	return lines
}

// FS-51's custom-host half (QD-840): a Team lapse past its 14 days withholds the network's web hosts, which leave
// the map lbmap loads and the rows tlsedge routes by while their rows, CAA mode and chain check stay, and GET
// /network/dns shows them withheld, naming no interface; and mail still arrives: the zone takes no write, so its
// MX, TLSA and router rows answer as they did, and the strict-mode MX host stays mapped for mxedge. A host pending
// certs' checks stays pending, out of the map as it was.
func TestDnsWithholdCustomHostsKeepsMailArriving(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		t.Cleanup(Testing_SetDnsUserRoot(world.root))
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
			t.Fatal(err)
		}
		tlsa := dnsservice.DnsRecordSet{Owner: "_25._tcp.mx." + world.apex, Type: dns.TypeTLSA, Ttl: 300, Kind: dnsservice.DnsRecordKindKey, Rdata: []string{"3 1 1 " + strings.Repeat("cd", 32)}}
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: world.root, NetworkId: world.networkId, Upserts: []dnsservice.DnsRecordSet{tlsa}}); err != nil {
			t.Fatal(err)
		}
		weak := "a." + world.domain
		strong := "s." + world.domain
		pending := "p." + world.domain
		strictMx := "mx1." + world.domain
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: weak, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}
		world.strongHost(t, ctx, strong, "shop."+world.apex)
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: strong, Pass: true}); err != nil {
			t.Fatal(err)
		}
		world.strongHost(t, ctx, pending, "shop."+world.apex)
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: strictMx, Owner: "mx." + world.apex}); err != nil {
			t.Fatal(err)
		}
		zone := world.zoneLines(ctx)
		mailLines := 0
		for _, line := range zone {
			if (strings.Contains(line, " MX ") && strings.Contains(line, "mx."+world.apex)) || strings.Contains(line, " TLSA ") {
				mailLines += 1
			}
		}
		if mailLines < 2 {
			t.Fatalf("the zone holds no MX into mx and TLSA to keep: %v", zone)
		}
		serial := world.serial(t, ctx)
		before := world.customHostMap(t, ctx)
		if !slices.Contains(before.Hosts, weak) || !slices.Contains(before.Hosts, strong) {
			t.Fatalf("the map before %+v", before)
		}

		if count := world.withholdCustomHosts(t, ctx); count != 2 {
			t.Fatalf("withheld %d", count)
		}

		after := world.customHostMap(t, ctx)
		if after.Version != before.Version+1 || slices.Contains(after.Hosts, weak) || slices.Contains(after.Hosts, strong) {
			t.Fatalf("the map lbmap loads after %+v", after)
		}
		for _, host := range []string{weak, strong} {
			if testDnsCustomHostMapped(ctx, host) {
				t.Errorf("tlsedge's rows still route %s", host)
			}
			if shown := world.shownCustomHost(t, ctx, host); shown.State != DnsCustomHostStateWithheld || shown.Ready || len(shown.PendingInterfaces) != 0 {
				t.Errorf("GET /network/dns shows %+v", shown)
			}
		}
		if row := world.row(ctx, strong); row == nil || row.State != DnsCustomHostStateWithheld || row.CaaMode != sidecar.DnsCustomHostModeStrong || !row.EffectiveCaaChainOk {
			t.Errorf("the strong host's row %+v", row)
		}
		if row := world.row(ctx, weak); row == nil || row.State != DnsCustomHostStateWithheld || row.CaaMode != sidecar.DnsCustomHostModeWeak {
			t.Errorf("the weak host's row %+v", row)
		}
		if row := world.row(ctx, pending); row == nil || row.State != sidecar.DnsCustomHostStatePending || testDnsCustomHostMapped(ctx, pending) {
			t.Errorf("the pending host %+v", row)
		}
		// mail still arrives: mxedge's strict-mode MX row stands, and the zone took no write
		if strict := DnsStrictMxHostRows(ctx, world.networkId); len(strict) != 1 || strict[0].Host != strictMx || strict[0].State != sidecar.DnsCustomHostStateActive {
			t.Errorf("mxedge's rows %+v", strict)
		}
		if afterSerial := world.serial(t, ctx); afterSerial != serial {
			t.Errorf("the zone took serial %d after %d", afterSerial, serial)
		}
		if afterZone := world.zoneLines(ctx); !slices.Equal(afterZone, zone) {
			t.Errorf("the zone moved from %v to %v", zone, afterZone)
		}
	})
}

// A renewal's release (QD-840): the withheld web host goes active at the next map version with its CAA mode and
// chain check as they stood and no zone write, so no new walk's canary; it is pending, naming every interface,
// until each reports the version and passes its probes, and then serves; certs judges it again as an active host;
// a host pending certs' checks stays pending through the withholding and the release, so no release activates a
// host no walk passed; and a second release moves and raises nothing.
func TestDnsReleasedCustomHostsServeOnceEveryInterfaceIsCurrent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		t.Cleanup(Testing_SetDnsUserRoot(world.root))
		interfaces := []string{"edge-1-eth0", "edge-2-eth0"}
		if err := SyncDnsCustomHostMapInterfaces(ctx, interfaces); err != nil {
			t.Fatal(err)
		}
		host := "s." + world.domain
		pending := "p." + world.domain
		world.strongHost(t, ctx, host, "shop."+world.apex)
		if _, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: host, Pass: true}); err != nil {
			t.Fatal(err)
		}
		world.strongHost(t, ctx, pending, "shop."+world.apex)
		if count := world.withholdCustomHosts(t, ctx); count != 1 {
			t.Fatalf("withheld %d", count)
		}
		withheld := world.customHostMap(t, ctx)
		serial := world.serial(t, ctx)

		if count := world.releaseCustomHosts(t, ctx); count != 1 {
			t.Fatalf("released %d", count)
		}

		released := world.customHostMap(t, ctx)
		if released.Version != withheld.Version+1 || !slices.Equal(released.Hosts, []string{host}) {
			t.Fatalf("the map after the release %+v", released)
		}
		if row := world.row(ctx, host); row.State != sidecar.DnsCustomHostStateActive || row.MapVersion != released.Version || row.ReadyTime != nil || row.CaaMode != sidecar.DnsCustomHostModeStrong || !row.EffectiveCaaChainOk {
			t.Fatalf("the released row %+v", row)
		}
		if row := world.row(ctx, pending); row.State != sidecar.DnsCustomHostStatePending || row.MapVersion != 0 {
			t.Fatalf("the pending host after the release %+v", row)
		}
		if afterSerial := world.serial(t, ctx); afterSerial != serial {
			t.Fatalf("the release wrote the zone at serial %d", afterSerial)
		}
		if shown := world.shownCustomHost(t, ctx, host); shown.Ready || !slices.Equal(shown.PendingInterfaces, interfaces) {
			t.Fatalf("before any report %+v", shown)
		}
		settings := dnsservice.DefaultDnsSettings()
		now := server.NowUtc()
		for index, name := range interfaces {
			err := RecordDnsCustomHostMapReport(ctx, &dnsservice.DnsCustomHostMapReport{
				Format: dnsservice.DnsFormat, Interface: name, Version: released.Version, Digest: released.Digest,
				Master: &dnsservice.DnsMapMaster{Pid: 100, StartTimeMillis: 1}, Added: []string{host},
			}, now, settings.Probe.RouteHostCount, settings.Map.ReportHostMaxCount)
			if err != nil {
				t.Fatal(err)
			}
			err = RecordDnsRouteProbes(ctx, &dnsservice.DnsRouteProbeReport{Format: dnsservice.DnsFormat, Interfaces: []dnsservice.DnsRouteProbeResult{{
				Name: name, Version: released.Version, ProbeName: dnsservice.DnsCustomHostMapProbeName(released.Digest), ProbePassed: true,
				Hosts: []dnsservice.DnsRouteProbeHost{{Host: host, Passed: true}},
			}}}, now)
			if err != nil {
				t.Fatal(err)
			}
			state, err := GetDnsCustomHostMapState(ctx, world.root)
			if err != nil {
				t.Fatal(err)
			}
			if err := SetDnsCustomHostReadiness(ctx, world.root, settings.Map.Judge(state, now).Ready, now); err != nil {
				t.Fatal(err)
			}
			shown := world.shownCustomHost(t, ctx, host)
			if index == 0 && (shown.Ready || !slices.Equal(shown.PendingInterfaces, interfaces[1:])) {
				t.Fatalf("one interface current %+v", shown)
			}
			if index == 1 && (!shown.Ready || len(shown.PendingInterfaces) != 0) {
				t.Fatalf("both current %+v", shown)
			}
		}
		if row := world.row(ctx, host); row.ReadyTime == nil {
			t.Fatalf("the released host serves with no ready time %+v", row)
		}
		// an active host again, which certs' later schedule judges
		if outcome, err := DnsCustomHostResultApply(ctx, &DnsCustomHostResult{Host: host, Pass: true}); err != nil || outcome != DnsCustomHostOutcomeChecked {
			t.Fatalf("a verdict after the release: %s %v", outcome, err)
		}
		if count := world.releaseCustomHosts(t, ctx); count != 0 {
			t.Fatalf("a second release moved %d", count)
		}
		if again := world.customHostMap(t, ctx); again.Version != released.Version {
			t.Fatalf("a second release raised the map to %d", again.Version)
		}
	})
}

// The restore's map raise (DNS-M6-07, QD-257): 2^24 above the newest of the counter, every host's map_version and
// every interface's report, every active host with it and none ready; a rerun for the incident answers the same
// version; the hosts lbmap's load added and dropped are listed once each, by network, a dropped one by its
// verified domain, with one admin event a network.
func TestDnsRestoreRaisesTheCustomHostMapAndListsItsLoad(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "a." + world.domain, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}
		if err := SyncDnsCustomHostMapInterfaces(ctx, []string{"lb-1"}); err != nil {
			t.Fatal(err)
		}
		// lbmap ran the lost primary's version 9
		if err := RecordDnsCustomHostMapReport(ctx, &dnsservice.DnsCustomHostMapReport{
			Format: dnsservice.DnsFormat, Interface: "lb-1", Version: 9, Digest: dnsservice.DnsCustomHostMapDigest(nil),
			Master: &dnsservice.DnsMapMaster{Pid: 1, StartTimeMillis: 1},
		}, server.NowUtc(), 16, 16); err != nil {
			t.Fatal(err)
		}
		incidentId := server.NewId()
		version, err := DnsRaiseCustomHostMapForRestore(ctx, world.root, incidentId, server.NowUtc())
		if err != nil || version != 9+dnsservice.DnsRestoreSerialJump {
			t.Fatalf("raised %d %v", version, err)
		}
		if row := world.row(ctx, "a."+world.domain); row.MapVersion != version || row.ReadyTime != nil {
			t.Fatalf("host %+v", row)
		}
		if again, err := DnsRaiseCustomHostMapForRestore(ctx, world.root, incidentId, server.NowUtc()); err != nil || again != version {
			t.Fatalf("a rerun raised %d %v", again, err)
		}
		if customHostMap := world.customHostMap(t, ctx); customHostMap.Version != version {
			t.Fatalf("the map %+v", customHostMap)
		}
		listing, err := ListDnsRestoreCustomHosts(ctx, world.root, incidentId, []string{"a." + world.domain}, []string{"late." + world.domain, "x.unknown.example"}, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		lines := []string{}
		for _, entry := range listing.Entries {
			network := "none"
			if entry.NetworkId != nil && server.Id(*entry.NetworkId) == world.networkId {
				network = "net"
			}
			lines = append(lines, entry.CustomHost+" "+entry.CustomHostChange+" "+network)
		}
		slices.Sort(lines)
		want := []string{"a." + world.domain + " added net", "late." + world.domain + " dropped net", "x.unknown.example dropped none"}
		if !slices.Equal(lines, want) {
			t.Fatalf("listed %v", lines)
		}
		listing, err = ListDnsRestoreCustomHosts(ctx, world.root, incidentId, []string{"a." + world.domain}, nil, server.NowUtc())
		if err != nil || len(listing.Entries) != 3 {
			t.Fatalf("a rerun listed %d %v", len(listing.Entries), err)
		}
		events := 0
		for _, event := range GetNetworkAdminEvents(ctx, world.networkId, 0, 0) {
			if event.Action == AdminEventActionDnsRestore && event.TargetId != nil && *event.TargetId == incidentId {
				events += 1
			}
		}
		if events != 1 {
			t.Fatalf("%d admin events", events)
		}
	})
}
