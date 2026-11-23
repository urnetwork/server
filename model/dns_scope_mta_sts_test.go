//go:build sidecar_pending

package model

// QD-1206 on Postgres (dns_scope_model.go's DnsScopeRecords; dns_zone_model.go's ReconcileNetworkZoneMailPolicy): a
// Team scope's zone publishes no _mta-sts, since no receiver serves a scope's MTA-STS policy (§13.9 names no scope
// publish, and sidecar.MailMtaStsHandler answers a scope's host 404), so no sender finds a record there pointing at
// a policy it cannot fetch. A verified network with mail on publishes its own zone's _mta-sts as DNS-M8-01 wrote it
// and none at any scope, whichever writer writes the scope's set, the scope keeping its mail and its TLSA; an
// _mta-sts a scope's rows still hold from before QD-1206 reads stale and goes at the network's next refresh, alone,
// in one journal row; and a scope a suspension withholds keeps such a leftover until the suspension lifts, without
// holding up its zone's reconcile.

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A receiver's TLSA at _25._tcp.mx.<scope>, as POST /dns/key-record writes it.
func testDnsScopeTlsa(scopeApex string) dnsservice.DnsRecordSet {
	return dnsservice.DnsRecordSet{
		Owner: "_25._tcp.mx." + scopeApex,
		Type:  dns.TypeTLSA,
		Ttl:   300,
		Kind:  dnsservice.DnsRecordKindKey,
		Rdata: []string{"3 1 1 " + strings.Repeat("ab", 32)},
	}
}

// The _mta-sts a scope's set held before QD-1206, when it was DNS-M8-01's at the scope's apex: the id of the policy
// for the scope's mail host.
func testDnsScopeLeftoverMtaSts(scopeApex string) dnsservice.DnsRecordSet {
	id := sidecar.MtaStsPolicyId(sidecar.MtaStsPolicy(dnsMailHostLabel+"."+scopeApex, nil), nil)
	return dnsSystemTxt(dnsMtaStsLabel+"."+scopeApex, "v=STSv1; id="+id, dnsZoneTtl().MailPolicy)
}

// Whether a reconcile would write the network's _mta-sts in root now.
func testDnsMtaStsStale(t testing.TB, ctx context.Context, root string, networkId server.Id) bool {
	t.Helper()
	stale := false
	server.Tx(ctx, func(tx server.PgTx) {
		stale = isDnsZoneMailPolicyStaleInTx(ctx, tx, root, networkId, server.NowUtc())
	}, server.TxReadCommitted)
	return stale
}

// A verified network with mail on publishes _mta-sts at its own zone, as DNS-M8-01 wrote it, and none at any Team
// scope, through every writer of a scope's set: the edge records' sync, the mail toggle off and on again, and a scope
// write. The edge answers no TXT at a scope's _mta-sts, the scope's * answering the name; each scope keeps its mail
// and eng its receiver's TLSA; the zone then reads current, so the next refresh writes nothing.
func TestDnsScopeMtaStsVerifiedNetworkPublishesTheZonesAlone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		testDnsScopeSource(t, networkId, "eng", "ops")
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		networkMtaSts := map[string]string{dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex)}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, networkMtaSts) {
			t.Fatalf("the verified network's mail on wrote _mta-sts %v, want %v", rows, networkMtaSts)
		}
		engApex := "eng." + apex
		opsApex := "ops." + apex
		if _, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants([]string{"eng", "ops"}, "web.eng")); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsScopeTlsa(engApex)}}); err != nil {
			t.Fatal(err)
		}
		tlsaLines := testDnsZoneLinesBelow(t, ctx, root, networkId, "_25._tcp.mx."+engApex)
		if len(tlsaLines) != 1 {
			t.Fatalf("eng's TLSA reads %v", tlsaLines)
		}
		requireZonesAlone := func(writer string) {
			t.Helper()
			if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, networkMtaSts) {
				t.Fatalf("after %s, _mta-sts %v, want the zone's alone %v", writer, rows, networkMtaSts)
			}
			if lines := testDnsZoneLinesBelow(t, ctx, root, networkId, "_25._tcp.mx."+engApex); !slices.Equal(lines, tlsaLines) {
				t.Errorf("after %s, eng's TLSA reads %v, want %v", writer, lines, tlsaLines)
			}
			view, healthy := testDnsEdgeView(t, ctx, root)
			requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+apex, dns.TypeTXT, dns.RcodeSuccess, testDnsMtaStsValue(apex))
			for _, scopeApex := range []string{engApex, opsApex} {
				requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+scopeApex, dns.TypeTXT, dns.RcodeSuccess)
				requireDnsViewAnswer(t, view, healthy, scopeApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+scopeApex)
				requireDnsViewAnswer(t, view, healthy, "mx."+scopeApex, dns.TypeA, dns.RcodeSuccess, "192.0.2.2")
				requireDnsViewAnswer(t, view, healthy, "b."+scopeApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+scopeApex)
			}
		}
		requireZonesAlone("the sync")

		for _, mail := range []bool{false, true} {
			if _, err := DnsSetMailRecords(ctx, networkId, root, mail); err != nil {
				t.Fatal(err)
			}
		}
		requireZonesAlone("the mail toggle")

		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "ops", EdgeWildcard: testBool(true)}); err != nil {
			t.Fatal(err)
		}
		requireZonesAlone("a scope write")

		if testDnsMtaStsStale(t, ctx, root, networkId) {
			t.Fatal("the zone reads stale")
		}
		serial := testDnsSerialNow(t, ctx, root)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("the network read unverified")
		}
		requireDnsSerial(t, ctx, root, serial)
		requireZonesAlone("a refresh")
	})
}

// An _mta-sts a scope's rows still hold from before QD-1206, when every scope of a verified network with mail on had
// one, reads stale though the network is verified, and the network's next refresh takes it away with the copy it kept,
// in one journal row holding only those two deletes; the network's own _mta-sts, the scope's mail and its TLSA stand,
// and the refresh after writes nothing.
func TestDnsMtaStsRefreshTakesAScopesLeftoverRecord(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		testDnsScopeSource(t, networkId, "eng")
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		engApex := "eng." + apex
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "eng", MailOfZone: true}); err != nil {
			t.Fatal(err)
		}
		if testDnsMtaStsStale(t, ctx, root, networkId) {
			t.Fatal("the zone reads stale before any leftover")
		}
		leftover := testDnsScopeLeftoverMtaSts(engApex)
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsScopeTlsa(engApex), leftover}}); err != nil {
			t.Fatal(err)
		}
		withLeftover := testDnsZoneLinesBelow(t, ctx, root, networkId, engApex)
		leftoverLines := []string{}
		kept := []string{}
		for _, line := range withLeftover {
			if strings.HasPrefix(line, leftover.Owner+" ") {
				leftoverLines = append(leftoverLines, line)
			} else {
				kept = append(kept, line)
			}
		}
		// the leftover keeps *.<scope>'s records through a copy, as the earlier build's write left it
		if want := testDnsWithCopyLines(dnsSystemRecordLines([]dnsservice.DnsRecordSet{leftover}), leftover.Owner); !slices.Equal(leftoverLines, want) {
			t.Fatalf("the leftover reads %v, want %v", leftoverLines, want)
		}
		networkMtaSts := map[string]string{dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex)}
		withLeftoverMtaSts := maps.Clone(networkMtaSts)
		withLeftoverMtaSts[leftover.Owner] = testDnsMtaStsValue(engApex)
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, withLeftoverMtaSts) {
			t.Fatalf("with the leftover, _mta-sts %v, want %v", rows, withLeftoverMtaSts)
		}
		if !testDnsMtaStsStale(t, ctx, root, networkId) {
			t.Fatal("a verified network's scope holding _mta-sts reads current")
		}

		before := testDnsSerialNow(t, ctx, root)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("the network read unverified")
		}
		sortedKeys := func(keys []dnsservice.DnsRecordKey) []dnsservice.DnsRecordKey {
			keys = slices.Clone(keys)
			slices.SortFunc(keys, func(first dnsservice.DnsRecordKey, second dnsservice.DnsRecordKey) int {
				return int(first.Type) - int(second.Type)
			})
			return keys
		}
		wantDeletes := sortedKeys([]dnsservice.DnsRecordKey{leftover.Key(), {Owner: leftover.Owner, Type: dnsservice.DnsCopyType}})
		journal := testDnsJournal(t, ctx, root, before)
		if len(journal) != 1 || len(journal[0].Upserts) != 0 || !slices.Equal(sortedKeys(journal[0].Deletes), wantDeletes) {
			t.Fatalf("the refresh wrote %+v, want one row deleting %v", journal, wantDeletes)
		}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, networkMtaSts) {
			t.Fatalf("after the refresh _mta-sts %v, want %v", rows, networkMtaSts)
		}
		if got := testDnsZoneLinesBelow(t, ctx, root, networkId, engApex); !slices.Equal(got, kept) {
			t.Fatalf("the scope's rows\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(kept, "\n"))
		}

		after := testDnsSerialNow(t, ctx, root)
		UpdateNetworkVerified(ctx, networkId)
		requireDnsSerial(t, ctx, root, after)
	})
}

// A scope a host suspension withholds keeps a leftover _mta-sts while the suspension stands, and does not hold up its
// zone's reconcile: the refresh that verifies the network publishes the zone's own _mta-sts, though the withheld
// scope's leftover is one the reconcile would take away and a write under the suspension is refused. Once the
// suspension lifts, the next refresh takes the leftover, the zone's standing.
func TestDnsMtaStsRefreshLeavesAWithheldScopesLeftoverToItsLift(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, adminUserId, apex := testDnsUnverifiedNetwork(t, ctx, root)
		testDnsScopeSource(t, networkId, "eng")
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		engApex := "eng." + apex
		if _, err := DnsSetScopeRecords(ctx, &DnsScopeArgs{Root: root, NetworkId: networkId, Scope: "eng", MailOfZone: true}); err != nil {
			t.Fatal(err)
		}
		leftover := testDnsScopeLeftoverMtaSts(engApex)
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{leftover}}); err != nil {
			t.Fatal(err)
		}
		if err := SuspendHost(ctx, networkId, "eng"); err != nil {
			t.Fatal(err)
		}

		testAddPasswordAuth(t, ctx, adminUserId, UserAuthTypeEmail, true)
		if !UpdateNetworkVerified(ctx, networkId) {
			t.Fatal("a verified email did not verify")
		}
		// the scope's leftover is stored withheld, as the suspension left it
		withheld := map[string]string{
			dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex),
			leftover.Owner:              testDnsMtaStsValue(engApex),
		}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, withheld) {
			t.Fatalf("under the suspension _mta-sts %v, want %v", rows, withheld)
		}
		if testDnsMtaStsStale(t, ctx, root, networkId) {
			t.Fatal("the zone reads stale under the suspension")
		}

		if err := Restore(ctx, networkId, "eng"); err != nil {
			t.Fatal(err)
		}
		UpdateNetworkVerified(ctx, networkId)
		want := map[string]string{dnsMtaStsLabel + "." + apex: testDnsMtaStsValue(apex)}
		if rows := testDnsMtaStsRows(t, ctx, root, networkId); !maps.Equal(rows, want) {
			t.Fatalf("after the lift and a refresh _mta-sts %v, want %v", rows, want)
		}
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+engApex, dns.TypeTXT, dns.RcodeSuccess)
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+engApex)
	})
}
