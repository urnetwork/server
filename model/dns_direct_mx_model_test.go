//go:build sidecar_pending

package model

// §12.8's direct MX (dns_direct_mx_model.go; DNS-M7-03, FS-33's dns side): addresses §12.7's A rules refuse,
// mail off and a network that is not a team refused with nothing written; a set and a clear through the journal,
// each one write, the TLSA set standing throughout and reconciles writing nothing; a receiver's removal dropping
// its address; mail off taking the direct rows with mx; and an operator's write redirecting a URnetwork-zone
// direct MX, which is why it carries no strict claim.

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Invented public addresses, as the record API's tests use: no documentation range passes §12.7's A rules.
var (
	testDirectMxFirst  = netip.MustParseAddr("11.22.33.4")
	testDirectMxSecond = netip.MustParseAddr("11.22.33.5")
	testDirectMxIpv6   = netip.MustParseAddr("2a0b:1111:2222::5")
)

// A team world with mail on and a receiver's TLSA at mx.
func newTestDirectMxWorld(t testing.TB, ctx context.Context) *testDnsRecordsWorld {
	t.Helper()
	world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
	testDnsSetTeam(t, map[server.Id]bool{world.networkId: true})
	if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
		t.Fatal(err)
	}
	if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: world.root, NetworkId: world.networkId, Upserts: []dnsservice.DnsRecordSet{{
		Owner: "_25._tcp.mx." + world.apex, Type: dns.TypeTLSA, Ttl: 300, Kind: dnsservice.DnsRecordKindKey,
		Rdata: []string{"3 1 1 " + strings.Repeat("cd", 32)},
	}}}); err != nil {
		t.Fatal(err)
	}
	return world
}

// What the zone holds at mx's A and AAAA: kind and values each.
func testDirectMxRows(t testing.TB, ctx context.Context, world *testDnsRecordsWorld) map[uint16]string {
	t.Helper()
	rows := map[uint16]string{}
	for key, set := range testDnsSnapshotRows(t, ctx, world.root) {
		if key.Owner == "mx."+world.apex && (key.Type == dns.TypeA || key.Type == dns.TypeAAAA || key.Type == dns.TypeTLSA) {
			rows[key.Type] = string(set.Kind) + " " + strings.Join(set.Rdata, ",")
		}
	}
	return rows
}

func testSetDirectMx(ctx context.Context, world *testDnsRecordsWorld, addresses ...netip.Addr) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsSetDirectMxInTx(ctx, tx, world.networkId, world.root, addresses, nil)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func testClearDirectMx(ctx context.Context, world *testDnsRecordsWorld) (result *DnsWriteResult, returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		result, returnErr = DnsClearDirectMxInTx(ctx, tx, world.networkId, world.root, nil)
	}, server.TxReadCommitted)
	return
}

// §12.7's A rules refuse a private, a documentation and a repeated address, and none at all; mail off and a
// network that is not a team refuse too. Nothing is written.
func TestDnsDirectMxRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDirectMxWorld(t, ctx)
		before := world.serial(t, ctx)
		// more addresses of a family than §12.7's RRset cap
		tooMany := []netip.Addr{}
		for index := range 9 {
			tooMany = append(tooMany, netip.AddrFrom4([4]byte{11, 22, 33, byte(20 + index)}))
		}
		for _, addresses := range [][]netip.Addr{
			{netip.MustParseAddr("10.1.2.3")},
			{netip.MustParseAddr("192.0.2.9")},
			{netip.MustParseAddr("2001:db8::9")},
			{testDirectMxFirst, testDirectMxFirst},
			{},
			tooMany,
		} {
			if _, err := testSetDirectMx(ctx, world, addresses...); !errors.Is(err, dnsservice.ErrDnsDirectMxRefused) {
				t.Errorf("%v: %v", addresses, err)
			}
		}
		requireDnsSerial(t, ctx, world.root, before)

		mailOff := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		testDnsSetTeam(t, map[server.Id]bool{mailOff.networkId: true})
		if _, err := testSetDirectMx(ctx, mailOff, testDirectMxFirst); !errors.Is(err, ErrDnsDirectMxMailOff) {
			t.Errorf("mail off: %v", err)
		}
		notTeam := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSetMailRecords(ctx, notTeam.networkId, notTeam.root, true); err != nil {
			t.Fatal(err)
		}
		var quotaError *QuotaRefusalError
		if _, err := testSetDirectMx(ctx, notTeam, testDirectMxFirst); !errors.As(err, &quotaError) {
			t.Errorf("not a team: %v", err)
		}
	})
}

// A set points mx at the indexers in one journal write, kind direct, a family with no address having no row,
// and the TLSA stands; reconciles and mail on again write nothing; a call naming fewer drops the rest; a clear
// puts the router rows back in one write; mail off takes everything at mx.
func TestDnsDirectMxSetAndClear(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDirectMxWorld(t, ctx)
		tlsa := "key " + strings.Repeat("cd", 32)
		if rows := testDirectMxRows(t, ctx, world); rows[dns.TypeA] != "router " || rows[dns.TypeAAAA] != "router " {
			t.Fatalf("before, mx holds %v", rows)
		}
		before := world.serial(t, ctx)
		result, err := testSetDirectMx(ctx, world, testDirectMxSecond, testDirectMxIpv6, testDirectMxFirst)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || result.Serial != before+1 {
			t.Fatalf("the set wrote %+v", result)
		}
		requireDnsSerial(t, ctx, world.root, before+1)
		rows := testDirectMxRows(t, ctx, world)
		if rows[dns.TypeA] != "direct 11.22.33.4,11.22.33.5" || rows[dns.TypeAAAA] != "direct 2a0b:1111:2222::5" {
			t.Fatalf("after the set, mx holds %v", rows)
		}
		if got := testDnsSnapshotRows(t, ctx, world.root)[dnsservice.DnsRecordKey{Owner: "_25._tcp.mx." + world.apex, Type: dns.TypeTLSA}]; !strings.Contains(strings.Join(got.Rdata, ""), strings.Repeat("cd", 32)) {
			t.Fatalf("the TLSA set went: %+v (%s)", got, tlsa)
		}
		if again, err := testSetDirectMx(ctx, world, testDirectMxFirst, testDirectMxSecond, testDirectMxIpv6); err != nil || again != nil {
			t.Fatalf("the same set again wrote %+v, %v", again, err)
		}
		if reconciled, err := EnsureNetworkZone(ctx, world.networkId, world.root); err != nil || reconciled != nil {
			t.Fatalf("a reconcile wrote %+v, %v", reconciled, err)
		}
		if mailOn, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil || mailOn != nil {
			t.Fatalf("mail on again wrote %+v, %v", mailOn, err)
		}

		// the IPv6 indexer's receiver is removed (I-15): its address goes, and with it mx's AAAA
		if _, err := testSetDirectMx(ctx, world, testDirectMxFirst, testDirectMxSecond); err != nil {
			t.Fatal(err)
		}
		rows = testDirectMxRows(t, ctx, world)
		if _, held := rows[dns.TypeAAAA]; held || rows[dns.TypeA] != "direct 11.22.33.4,11.22.33.5" {
			t.Fatalf("after a removal, mx holds %v", rows)
		}
		if _, err := testSetDirectMx(ctx, world, testDirectMxFirst); err != nil {
			t.Fatal(err)
		}
		if rows := testDirectMxRows(t, ctx, world); rows[dns.TypeA] != "direct 11.22.33.4" {
			t.Fatalf("after a second removal, mx holds %v", rows)
		}

		before = world.serial(t, ctx)
		if cleared, err := testClearDirectMx(ctx, world); err != nil || cleared == nil {
			t.Fatalf("the clear wrote %+v, %v", cleared, err)
		}
		requireDnsSerial(t, ctx, world.root, before+1)
		if rows := testDirectMxRows(t, ctx, world); rows[dns.TypeA] != "router " || rows[dns.TypeAAAA] != "router " {
			t.Fatalf("after the clear, mx holds %v", rows)
		}
		if again, err := testClearDirectMx(ctx, world); err != nil || again != nil {
			t.Fatalf("clearing again wrote %+v, %v", again, err)
		}

		if _, err := testSetDirectMx(ctx, world, testDirectMxFirst); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, false); err != nil {
			t.Fatal(err)
		}
		if rows := testDirectMxRows(t, ctx, world); rows[dns.TypeA] != "" || rows[dns.TypeAAAA] != "" {
			t.Fatalf("mail off left mx holding %v", rows)
		}
	})
}

// FS-33's dns side: the direct MX's records are in URnetwork's zone, so a write by the operator alone, with no
// customer's signature, moves the address a validating sender reaches. The edge answers what the operator
// wrote, which is why §2.8 EX2 stays and no strict claim rides this path.
func TestDnsDirectMxOperatorCanRedirect(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDirectMxWorld(t, ctx)
		if _, err := testSetDirectMx(ctx, world, testDirectMxFirst); err != nil {
			t.Fatal(err)
		}
		view, healthy := world.view(t, ctx)
		requireDnsViewAnswer(t, view, healthy, "mx."+world.apex, dns.TypeA, dns.RcodeSuccess, testDirectMxFirst.String())
		substitute := netip.MustParseAddr("11.22.33.99")
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: world.root, NetworkId: world.networkId, Upserts: []dnsservice.DnsRecordSet{{
			Owner: "mx." + world.apex, Type: dns.TypeA, Ttl: 300, Kind: dnsservice.DnsRecordKindDirect, Rdata: []string{substitute.String()},
		}}}); err != nil {
			t.Fatal(err)
		}
		view, healthy = world.view(t, ctx)
		_, got := testDnsViewAnswer(t, view, healthy, "mx."+world.apex, dns.TypeA)
		if !slices.Equal(got, []string{substitute.String()}) {
			t.Fatalf("after the operator's write mx answers %v", got)
		}
	})
}
