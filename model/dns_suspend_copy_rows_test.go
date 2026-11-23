//go:build sidecar_pending

package model

// QD-1190's copy rows through §12.9's hooks (dns_suspend_model.go): SuspendHost and Restore plan copies as every
// other writer does, through dnsservice.PlanCopyRows over what edges serve. A multi-label host's withheld owner
// makes the names above it exist, so each takes its copy in the suspension's journal row and drops it in
// Restore's; and Restore plans over the rows it returns, so a copy the zone stopped needing while it was withheld
// is deleted rather than returned, and one it started needing comes back with them.

import (
	"context"
	"slices"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Holds what the root's snapshot serves at apex to the copy rows PlanCopyRows plans over it: none to add and
// none to drop.
func requireDnsCopiesAsPlanned(t testing.TB, ctx context.Context, root string, apex string) {
	t.Helper()
	served := []dnsservice.DnsRecordSet{}
	for _, set := range testDnsSnapshotRows(t, ctx, root) {
		served = append(served, set)
	}
	upserts, deletes := dnsservice.PlanCopyRows(apex, served)
	if len(upserts) != 0 || len(deletes) != 0 {
		t.Fatalf("what edges serve wants copies added at %+v and dropped at %+v", upserts, deletes)
	}
}

// Whether rows hold an RRset at owner of rrType.
func testDnsHoldsRow(rows []dnsservice.DnsRecordSet, owner string, rrType uint16) bool {
	return slices.ContainsFunc(rows, func(row dnsservice.DnsRecordSet) bool {
		return row.Owner == owner && row.Type == rrType
	})
}

// One user A at owner.
func testDnsUserA(owner string, address string) dnsservice.DnsRecordSet {
	return dnsservice.DnsRecordSet{Owner: owner, Type: dns.TypeA, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{address}}
}

// A write through §12.6's path that must land.
func (self *testDnsRecordsWorld) mustDnsWrite(t testing.TB, ctx context.Context, upserts []dnsservice.DnsRecordSet, deletes []dnsservice.DnsRecordKey) {
	t.Helper()
	if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: self.root, NetworkId: self.networkId, Upserts: upserts, Deletes: deletes}); err != nil {
		t.Fatal(err)
	}
}

// Suspending x.y.shop, with nothing at or below shop, makes y.shop and shop empty non-terminals over its
// withheld owner, which would block *: the suspension's own journal row adds both copies, so both keep answering
// the edge's addresses at once while x.y.shop answers nothing, and what edges serve is as PlanCopyRows plans it.
// Restore's journal row takes both copies with the withheld owner, none staying stored, and shop answers from *
// itself.
func TestDnsSuspendMultiLabelHostPlansItsParentsCopies(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		shop := "shop." + world.apex
		middle := "y." + shop
		host := "x." + middle

		before := world.serial(t, ctx)
		if err := SuspendHost(ctx, world.networkId, "x.y.shop"); err != nil {
			t.Fatal(err)
		}
		row := world.journalRow(t, ctx, before)
		if !testDnsHoldsRow(row.Upserts, host, dnsservice.DnsWithheldType) {
			t.Fatalf("the suspension's journal row %+v holds no withheld owner at the host", row)
		}
		for _, owner := range []string{shop, middle} {
			if !testDnsHoldsCopy(row.Upserts, owner) {
				t.Errorf("the suspension's journal row %+v adds no copy at %s", row, owner)
			}
			if held, withheld := world.copyAt(ctx, owner); !held || withheld {
				t.Errorf("the suspension left %s's copy held %t, withheld %t", owner, held, withheld)
			}
		}
		if testDnsHoldsCopy(row.Upserts, host) {
			t.Errorf("the suspension's journal row %+v adds a copy beside the withheld owner", row)
		}
		requireDnsCopiesAsPlanned(t, ctx, world.root, world.apex)
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, shop, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
		requireDnsViewAnswer(t, view, healthy, middle, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
		requireDnsViewAnswer(t, view, healthy, host, dns.TypeA, dns.RcodeSuccess)

		before = world.serial(t, ctx)
		if err := Restore(ctx, world.networkId, "x.y.shop"); err != nil {
			t.Fatal(err)
		}
		row = world.journalRow(t, ctx, before)
		if len(row.Upserts) != 0 || !slices.Contains(row.Deletes, dnsservice.DnsRecordKey{Owner: host, Type: dnsservice.DnsWithheldType}) {
			t.Fatalf("Restore's journal row %+v", row)
		}
		for _, owner := range []string{shop, middle} {
			if !testDnsDeletesCopy(row.Deletes, owner) {
				t.Errorf("Restore's journal row %+v keeps %s's copy", row, owner)
			}
			if held, _ := world.copyAt(ctx, owner); held {
				t.Errorf("a copy stayed at %s with nothing below it", owner)
			}
		}
		requireDnsCopiesAsPlanned(t, ctx, world.root, world.apex)
		view, healthy = world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, shop, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
		requireDnsViewAnswer(t, view, healthy, host, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
	})
}

// Restore plans the copies over what it returns. a.shop holds a TXT and its copy when it is suspended; an address
// written at shop meanwhile stops * covering a.shop, so Restore returns the TXT and deletes the withheld copy rather
// than returning it. Suspended again, the address's removal lets * cover a.shop once more, and Restore's journal
// row writes a.shop's copy beside its TXT, so a.shop answers the edge's addresses at once.
func TestDnsRestorePlansTheCopiesItReturns(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		shop := "shop." + world.apex
		host := "a." + shop
		world.mustDnsWrite(t, ctx, []dnsservice.DnsRecordSet{testDnsTxt(host, "verify")}, nil)
		if held, _ := world.copyAt(ctx, host); !held {
			t.Fatal("a TXT under * took no copy")
		}

		if err := SuspendHost(ctx, world.networkId, "a.shop"); err != nil {
			t.Fatal(err)
		}
		world.mustDnsWrite(t, ctx, []dnsservice.DnsRecordSet{testDnsUserA(shop, "192.0.2.10")}, nil)
		if held, withheld := world.copyAt(ctx, host); !held || !withheld {
			t.Fatalf("a write above the suspension left a.shop's copy held %t, withheld %t", held, withheld)
		}
		before := world.serial(t, ctx)
		if err := Restore(ctx, world.networkId, "a.shop"); err != nil {
			t.Fatal(err)
		}
		row := world.journalRow(t, ctx, before)
		if !testDnsHoldsRow(row.Upserts, host, dns.TypeTXT) {
			t.Fatalf("Restore's journal row %+v returns no TXT", row)
		}
		if testDnsHoldsCopy(row.Upserts, host) || testDnsDeletesCopy(row.Deletes, host) {
			t.Fatalf("Restore's journal row %+v names a copy no edge held and none needs", row)
		}
		if held, _ := world.copyAt(ctx, host); held {
			t.Fatal("Restore kept a copy * no longer feeds")
		}
		requireDnsCopiesAsPlanned(t, ctx, world.root, world.apex)

		if err := SuspendHost(ctx, world.networkId, "a.shop"); err != nil {
			t.Fatal(err)
		}
		world.mustDnsWrite(t, ctx, nil, []dnsservice.DnsRecordKey{{Owner: shop, Type: dns.TypeA}})
		if held, _ := world.copyAt(ctx, host); held {
			t.Fatal("a write gave a suspended host a copy")
		}
		before = world.serial(t, ctx)
		if err := Restore(ctx, world.networkId, "a.shop"); err != nil {
			t.Fatal(err)
		}
		row = world.journalRow(t, ctx, before)
		if !testDnsHoldsRow(row.Upserts, host, dns.TypeTXT) || !testDnsHoldsCopy(row.Upserts, host) {
			t.Fatalf("Restore's journal row %+v returns the TXT without the copy * now feeds", row)
		}
		if held, withheld := world.copyAt(ctx, host); !held || withheld {
			t.Fatalf("Restore left a.shop's copy held %t, withheld %t", held, withheld)
		}
		requireDnsCopiesAsPlanned(t, ctx, world.root, world.apex)
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, host, dns.TypeA, dns.RcodeSuccess, testDnsCopyEdgeAddresses...)
		requireDnsViewAnswer(t, view, healthy, host, dns.TypeTXT, dns.RcodeSuccess, `"verify"`)
	})
}
