//go:build sidecar_pending

package model

// §20.7's downgrade column, dns side (dns_withhold_model.go; DNS-M6-07), FS-51's dns side on Postgres: the newest
// user RRsets past the limit withheld through the journal with their rows kept, MX and TLSA untouched; a second
// pass writing nothing; releases oldest first; a withheld host's children keeping the wildcard by a copy row; a
// suspension's changes leaving a downgrade's withholding alone; and the custom-host half refusing until its
// question is answered.

import (
	"context"
	"errors"
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
			result, returnErr = DnsReleaseWithheldInTx(ctx, tx, self.networkId, self.root, limit)
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
// copy row written with the withholding.
func TestDnsWithholdKeepsTheWildcardAtAWithheldHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		world.writeAged(t, ctx, "_note.blog", "blog")
		world.quota(t, ctx, 1, false)
		if withheld, _ := world.withheldHosts(t, ctx); !slices.Equal(withheld, []string{"blog"}) {
			t.Fatalf("withheld %v", withheld)
		}
		copied := false
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range getDnsZoneRowsInTx(ctx, conn, world.root, world.networkId) {
				copied = copied || (row.set.Kind == dnsservice.DnsRecordKindCopy && row.set.Owner == "blog."+world.apex && !row.withheld)
			}
		})
		if !copied {
			t.Fatal("no copy row at the withheld host")
		}
		world.quota(t, ctx, -1, true)
		server.Db(ctx, func(conn server.PgConn) {
			for _, row := range getDnsZoneRowsInTx(ctx, conn, world.root, world.networkId) {
				if row.set.Kind == dnsservice.DnsRecordKindCopy && row.set.Owner == "blog."+world.apex {
					t.Error("the copy stayed once the host was served again")
				}
			}
		})
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

// The custom-host half refuses until its question is answered, with nothing withheld.
func TestDnsWithholdCustomHostsRefusesUntilItsQuestionIsAnswered(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsCustomHostWorld(t, ctx)
		if _, err := world.add(ctx, &sidecar.DnsCustomHostAddRequest{Host: "a." + world.domain, Owner: "blog." + world.apex, Mode: sidecar.DnsCustomHostModeWeak, WeakDisclosureAccepted: true}); err != nil {
			t.Fatal(err)
		}
		var returnErr error
		server.Tx(ctx, func(tx server.PgTx) {
			_, returnErr = DnsWithholdCustomHostsInTx(ctx, tx, world.networkId)
		})
		if !errors.Is(returnErr, ErrDnsCustomHostWithholdUnavailable) {
			t.Fatalf("%v", returnErr)
		}
		if !testDnsCustomHostMapped(ctx, "a."+world.domain) {
			t.Fatal("a refusal withheld the host")
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
