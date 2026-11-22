//go:build sidecar_pending

package model

// QD-1190 on Postgres (dns_write_model.go, dns_withhold_model.go; dnsservice.PlanCopyRows): every writer that plans
// copy rows replans when a user record comes or goes at an owner with no address of its own, and takes the copy once
// the owner no longer needs it. Through the record API, §12.6's write path commits a published host's copy in the
// journal row of the TXT written there, so the host keeps answering the edge's addresses through the edge's answer
// path, and the TXT's removal takes it; an address of its own takes the copy away and its removal brings it back; a
// CNAME takes none. The downgrade's withholding plans over what stays served, and so does the write path, so neither
// undoes the other's copy. A suspension's rows stay as it left them under every other write, and Restore serves the
// host's copy again.

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Whether rows hold a copy row at owner.
func testDnsHoldsCopy(rows []dnsservice.DnsRecordSet, owner string) bool {
	return slices.ContainsFunc(rows, func(row dnsservice.DnsRecordSet) bool {
		return row.Kind == dnsservice.DnsRecordKindCopy && row.Owner == owner
	})
}

// Whether keys name the copy row at owner.
func testDnsDeletesCopy(keys []dnsservice.DnsRecordKey, owner string) bool {
	return slices.Contains(keys, dnsservice.DnsRecordKey{Owner: owner, Type: dnsservice.DnsCopyType})
}

// The zone's copy row at owner as stored: whether it stands, and whether it is withheld.
func (self *testDnsRecordsWorld) copyAt(ctx context.Context, owner string) (held bool, withheld bool) {
	server.Db(ctx, func(conn server.PgConn) {
		for _, row := range getDnsZoneRowsInTx(ctx, conn, self.root, self.networkId) {
			if row.set.Kind == dnsservice.DnsRecordKindCopy && row.set.Owner == owner {
				held, withheld = true, row.withheld
			}
		}
	})
	return held, withheld
}

// The one journal row of the root above before: the write just made.
func (self *testDnsRecordsWorld) journalRow(t testing.TB, ctx context.Context, before uint64) dnsservice.DnsJournalRow {
	t.Helper()
	journal := testDnsJournal(t, ctx, self.root, before)
	if len(journal) != 1 {
		t.Fatalf("%d journal rows above %d, want one: %+v", len(journal), before, journal)
	}
	return journal[0]
}

// A record API write that must be answered.
func (self *testDnsRecordsWorld) mustWrite(t testing.TB, ctx context.Context, request *sidecar.DnsRecordsRequest) {
	t.Helper()
	if _, err := self.write(ctx, request); err != nil {
		t.Fatal(err)
	}
}

// One delete of type at owner.
func testDnsDelete(owner string, rrType string) *sidecar.DnsRecordsRequest {
	return &sidecar.DnsRecordsRequest{Deletes: []sidecar.DnsRecordDelete{{Name: owner, Type: rrType}}}
}

// The world's edge interfaces, as the view's healthy set answers them for *.
var testDnsCopyEdgeAddresses = []string{"192.0.2.1", "192.0.2.2"}

// With an edge grant at *, a site-verification TXT at web, a published host, commits web's copy in its own journal
// row, and web answers its TXT and the edge's addresses; an A written beside the TXT takes the copy and answers
// alone, and its removal brings the copy back; the TXT's removal takes the copy in its own row, nothing staying at
// web, which answers from * as any name does. A CNAME at a host takes no copy.
func TestDnsUserRecordAtAPublishedHostKeepsItsAddresses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		web := "web." + world.apex
		txt := `"site-verification=token"`

		before := world.serial(t, ctx)
		world.mustWrite(t, ctx, testDnsUpsert(web, "TXT", txt))
		if row := world.journalRow(t, ctx, before); !testDnsHoldsCopy(row.Upserts, web) {
			t.Fatalf("the TXT's journal row %+v holds no copy at the host", row)
		}
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeTXT, dns.RcodeSuccess, txt)

		before = world.serial(t, ctx)
		world.mustWrite(t, ctx, testDnsUpsert(web, "A", "11.22.33.44"))
		if row := world.journalRow(t, ctx, before); !testDnsDeletesCopy(row.Deletes, web) {
			t.Fatalf("the A's journal row %+v keeps the host's copy", row)
		}
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeA, dns.RcodeSuccess, "11.22.33.44")
		before = world.serial(t, ctx)
		world.mustWrite(t, ctx, testDnsDelete(web, "A"))
		if row := world.journalRow(t, ctx, before); !testDnsHoldsCopy(row.Upserts, web) {
			t.Fatalf("the A's removal %+v brings no copy back", row)
		}

		before = world.serial(t, ctx)
		world.mustWrite(t, ctx, testDnsDelete(web, "TXT"))
		if row := world.journalRow(t, ctx, before); !testDnsDeletesCopy(row.Deletes, web) {
			t.Fatalf("the TXT's removal %+v keeps the host's copy", row)
		}
		if held, _ := world.copyAt(ctx, web); held {
			t.Fatal("a copy stayed at a host with nothing of its own")
		}
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeTXT, dns.RcodeSuccess)

		shop := "shop." + world.apex
		before = world.serial(t, ctx)
		world.mustWrite(t, ctx, testDnsUpsert(shop, "CNAME", "target.example."))
		if row := world.journalRow(t, ctx, before); len(row.Upserts) != 1 || row.Upserts[0].Type != dns.TypeCNAME || len(row.Deletes) != 0 {
			t.Fatalf("the CNAME's journal row %+v", row)
		}
	})
}

// Sets create_time of the RRset at owner and rrType, so the withholding orders it.
func (self *testDnsRecordsWorld) setCreateTime(ctx context.Context, owner string, rrType uint16, createTime time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_dns_record SET create_time = $4 WHERE root = $1 AND owner = $2 AND rrtype = $3`,
			self.root, owner, int32(rrType), createTime,
		))
	})
}

// The downgrade's withholding and §12.6's write path both plan copies over what edges serve, so neither undoes the
// other's (QD-1190): web's TXT, the older, stays served while its newer A is withheld, so the pass gives web a copy
// and web answers the edge's addresses; a write elsewhere in the zone leaves that copy standing; the release serves
// the A again and takes the copy. A host whose only record is withheld loses its copy with it and has it back with
// the release.
func TestDnsWithholdAndTheWritePathKeepOneCopy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		web := "web." + world.apex
		world.mustWrite(t, ctx, testDnsUpsert(web, "TXT", `"text"`))
		world.mustWrite(t, ctx, testDnsUpsert(web, "A", "11.22.33.44"))
		start := server.NowUtc().Add(-time.Hour)
		world.setCreateTime(ctx, web, dns.TypeTXT, start)
		world.setCreateTime(ctx, web, dns.TypeA, start.Add(time.Minute))
		if held, _ := world.copyAt(ctx, web); held {
			t.Fatal("a host with an A of its own holds a copy")
		}

		world.quota(t, ctx, 1, false)
		if held, withheld := world.copyAt(ctx, web); !held || withheld {
			t.Fatalf("withholding the host's A left its copy held %t, withheld %t", held, withheld)
		}
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)

		news := "news." + world.apex
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: world.root, NetworkId: world.networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt(news, "news")}}); err != nil {
			t.Fatal(err)
		}
		world.setCreateTime(ctx, news, dns.TypeTXT, start.Add(2*time.Minute))
		if held, _ := world.copyAt(ctx, web); !held {
			t.Fatal("a write elsewhere took the copy the withholding gave")
		}

		world.quota(t, ctx, -1, true)
		if held, _ := world.copyAt(ctx, web); held {
			t.Fatal("the copy stayed once the host's A was served again")
		}
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeA, dns.RcodeSuccess, "11.22.33.44")

		shop := "shop." + world.apex
		world.mustWrite(t, ctx, testDnsUpsert(shop, "TXT", `"shop"`))
		world.setCreateTime(ctx, shop, dns.TypeTXT, start.Add(3*time.Minute))
		world.quota(t, ctx, 3, false)
		if held, _ := world.copyAt(ctx, shop); held {
			t.Fatal("withholding a host's only record left its copy")
		}
		world.quota(t, ctx, -1, true)
		if held, withheld := world.copyAt(ctx, shop); !held || withheld {
			t.Fatalf("the release left the host's copy held %t, withheld %t", held, withheld)
		}
	})
}

// A suspension's rows stay as it left them under every other write (§12.9): web, holding a TXT and its copy, is
// suspended, and a write elsewhere neither deletes its withheld copy nor writes one beside the withheld owner; shop,
// suspended with nothing of its own, takes no copy from such a write; Restore serves web's TXT and copy again, and
// web answers the edge's addresses, as shop does from * once restored.
func TestDnsSuspensionKeepsAHostsCopyUnderOtherWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		web := "web." + world.apex
		shop := "shop." + world.apex
		txt := `"site-verification=token"`
		world.mustWrite(t, ctx, testDnsUpsert(web, "TXT", txt))
		for _, host := range []string{"web", "shop"} {
			if err := SuspendHost(ctx, world.networkId, host); err != nil {
				t.Fatal(err)
			}
		}
		if held, withheld := world.copyAt(ctx, web); !held || !withheld {
			t.Fatalf("the suspension left web's copy held %t, withheld %t", held, withheld)
		}

		before := world.serial(t, ctx)
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: world.root, NetworkId: world.networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("news."+world.apex, "news")}}); err != nil {
			t.Fatal(err)
		}
		row := world.journalRow(t, ctx, before)
		for _, host := range []string{web, shop} {
			if testDnsHoldsCopy(row.Upserts, host) || testDnsDeletesCopy(row.Deletes, host) {
				t.Errorf("a write elsewhere changed %s's copy: %+v", host, row)
			}
		}
		if held, withheld := world.copyAt(ctx, web); !held || !withheld {
			t.Fatalf("a write elsewhere left web's copy held %t, withheld %t", held, withheld)
		}
		if held, _ := world.copyAt(ctx, shop); held {
			t.Fatal("a write elsewhere gave a suspended host a copy")
		}
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeA, dns.RcodeSuccess)
		requireDnsViewAnswer(t, view, healthy, shop, dns.TypeA, dns.RcodeSuccess)

		for _, host := range []string{"web", "shop"} {
			if err := Restore(ctx, world.networkId, host); err != nil {
				t.Fatal(err)
			}
		}
		if held, withheld := world.copyAt(ctx, web); !held || withheld {
			t.Fatalf("Restore left web's copy held %t, withheld %t", held, withheld)
		}
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
		requireDnsViewAnswer(t, view, healthy, web, dns.TypeTXT, dns.RcodeSuccess, txt)
		requireDnsViewAnswer(t, view, healthy, shop, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
	})
}
