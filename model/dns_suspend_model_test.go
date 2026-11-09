//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Every row a snapshot of root serves, by key.
func testDnsSnapshotRows(t testing.TB, ctx context.Context, root string) map[dnsservice.DnsRecordKey]dnsservice.DnsRecordSet {
	t.Helper()
	snapshot, err := OpenDnsSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	rows := map[dnsservice.DnsRecordKey]dnsservice.DnsRecordSet{}
	for {
		set, err := snapshot.Next()
		if err != nil {
			return rows
		}
		rows[set.Key()] = *set
	}
}

// A network zone with a wildcard, an MX at the apex and a host's own record.
func testDnsSuspendZone(t testing.TB, ctx context.Context) (string, server.Id, string) {
	t.Helper()
	root := testDnsRoot(t, ctx)
	networkId, apex := testDnsNetwork(t, ctx, root)
	_, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{
		{Owner: "*." + apex, Type: dns.TypeA, Ttl: 60, Kind: dnsservice.DnsRecordKindEdge},
		{Owner: apex, Type: dns.TypeMX, Ttl: 300, Kind: dnsservice.DnsRecordKindSystem, Rdata: []string{"0 ."}},
		testDnsTxt("www."+apex, "own"),
		testDnsTxt("deep.shop."+apex, "below a host"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return root, networkId, apex
}

// A zone suspension takes every RRset out through the journal, leaves the snapshot with its apex's
// withheld owner alone, refuses writes, and Restore brings back exactly what was there.
func TestDnsSuspendZoneAndRestore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root, networkId, apex := testDnsSuspendZone(t, ctx)
		before := testDnsSnapshotRows(t, ctx, root)
		state, _ := GetDnsRootState(ctx, root)
		if err := SuspendZone(ctx, networkId, "case", nil); err != nil {
			t.Fatal(err)
		}
		rows := testDnsJournal(t, ctx, root, state.Serial)
		if len(rows) != 1 || len(rows[0].Deletes) != len(before) || len(rows[0].Upserts) != 1 || rows[0].Upserts[0].Kind != dnsservice.DnsRecordKindWithheld {
			t.Fatalf("the suspension's journal %+v", rows)
		}
		served := testDnsSnapshotRows(t, ctx, root)
		if _, ok := served[dnsservice.DnsRecordKey{Owner: apex, Type: dnsservice.DnsWithheldType}]; len(served) != 1 || !ok {
			t.Fatalf("a suspended zone serves %+v", served)
		}
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("new."+apex, "x")}}); !errors.Is(err, ErrDnsZoneSuspended) {
			t.Fatalf("a write to a suspended zone answered %v", err)
		}
		// a second call changes nothing
		suspendedState, _ := GetDnsRootState(ctx, root)
		if err := SuspendZone(ctx, networkId, "case", nil); err != nil {
			t.Fatal(err)
		}
		requireDnsSerial(t, ctx, root, suspendedState.Serial)

		if err := Restore(ctx, networkId, ""); err != nil {
			t.Fatal(err)
		}
		after := testDnsSnapshotRows(t, ctx, root)
		if len(after) != len(before) {
			t.Fatalf("restored %d rows of %d", len(after), len(before))
		}
		for key, set := range before {
			if got, ok := after[key]; !ok || !slices.Equal(got.Rdata, set.Rdata) || got.Ttl != set.Ttl || got.Kind != set.Kind {
				t.Errorf("%+v came back as %+v", set, got)
			}
		}
		restoredState, _ := GetDnsRootState(ctx, root)
		if err := Restore(ctx, networkId, ""); err != nil {
			t.Fatal(err)
		}
		requireDnsSerial(t, ctx, root, restoredState.Serial)
	})
}

// A host suspension puts a withheld owner at the host and withholds what is at and below it, so * no
// longer covers it; lifting it returns them.
func TestDnsSuspendHostAndRestore(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root, networkId, apex := testDnsSuspendZone(t, ctx)
		if err := SuspendHost(ctx, networkId, "shop"); err != nil {
			t.Fatal(err)
		}
		served := testDnsSnapshotRows(t, ctx, root)
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "shop." + apex, Type: dnsservice.DnsWithheldType}]; !ok {
			t.Fatalf("no withheld owner: %+v", served)
		}
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "deep.shop." + apex, Type: dns.TypeTXT}]; ok {
			t.Fatal("a row below the host still serves")
		}
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "www." + apex, Type: dns.TypeTXT}]; !ok {
			t.Fatal("another host's row went too")
		}
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("x.shop."+apex, "x")}}); !errors.Is(err, ErrDnsHostSuspended) {
			t.Fatalf("a write below a suspended host answered %v", err)
		}
		labelMap, err := DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if len(labelMap.Entries) != 1 || !slices.Equal(labelMap.Entries[0].WithheldHosts, []string{"shop." + apex}) {
			t.Fatalf("the label map %+v", labelMap.Entries)
		}
		state, _ := GetDnsRootState(ctx, root)
		if err := SuspendHost(ctx, networkId, "shop"); err != nil {
			t.Fatal(err)
		}
		requireDnsSerial(t, ctx, root, state.Serial)

		if err := Restore(ctx, networkId, "shop"); err != nil {
			t.Fatal(err)
		}
		served = testDnsSnapshotRows(t, ctx, root)
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "shop." + apex, Type: dnsservice.DnsWithheldType}]; ok {
			t.Fatal("the withheld owner stayed")
		}
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "deep.shop." + apex, Type: dns.TypeTXT}]; !ok {
			t.Fatal("the row below the host did not come back")
		}
	})
}

// While restoring a suspension sets only its rows and takes no serial, and the snapshot the edges
// reload carries it.
func TestDnsSuspendWhileRestoring(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root, networkId, apex := testDnsSuspendZone(t, ctx)
		state, _ := GetDnsRootState(ctx, root)
		if err := SetDnsRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		if err := SuspendHost(ctx, networkId, "www"); err != nil {
			t.Fatal(err)
		}
		requireDnsSerial(t, ctx, root, state.Serial)
		served := testDnsSnapshotRows(t, ctx, root)
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "www." + apex, Type: dns.TypeTXT}]; ok {
			t.Fatal("the snapshot serves a host suspended while restoring")
		}
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "www." + apex, Type: dnsservice.DnsWithheldType}]; !ok {
			t.Fatal("the snapshot lacks the withheld owner")
		}
	})
}

// A suspension's until lifts it at the pass after it, not before.
func TestDnsSuspensionUntilLifts(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root, networkId, apex := testDnsSuspendZone(t, ctx)
		until := server.NowUtc().Add(time.Hour)
		if err := SuspendZone(ctx, networkId, "case", &until); err != nil {
			t.Fatal(err)
		}
		if lifted, err := LiftExpiredDnsSuspensions(ctx, until.Add(-time.Minute)); err != nil || lifted != 0 {
			t.Fatalf("lifted %d early, %v", lifted, err)
		}
		if lifted, err := LiftExpiredDnsSuspensions(ctx, until.Add(time.Minute)); err != nil || lifted != 1 {
			t.Fatalf("lifted %d, %v", lifted, err)
		}
		served := testDnsSnapshotRows(t, ctx, root)
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "www." + apex, Type: dns.TypeTXT}]; !ok {
			t.Fatal("the lift did not return the rows")
		}
	})
}
