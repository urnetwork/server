//go:build sidecar_pending

package model

import (
	"context"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A zone's first need allocates its label and writes §12.7's set in one journal row, moving the label
// map's cursor; a second need writes nothing.
func TestEnsureNetworkZoneWritesItsSetOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "zone-"+server.NewId().String()[:8], server.NewId())
		result, err := EnsureNetworkZone(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || result.Serial != 1 || len(result.Upserts) != 4 {
			t.Fatalf("first need %+v", result)
		}
		labelMap, err := DnsLabelMapSince(ctx, root, 0)
		if err != nil || len(labelMap.Entries) != 1 || labelMap.Entries[0].ChangeSerial != 1 {
			t.Fatalf("the label map %+v %v", labelMap, err)
		}
		again, err := EnsureNetworkZone(ctx, networkId, root)
		if err != nil || again != nil {
			t.Fatalf("a second need wrote %+v, %v", again, err)
		}
		requireDnsSerial(t, ctx, root, 1)
	})
}

// The edge toggles write one row a transition and none otherwise, and land on the same rows in either
// order.
func TestDnsSyncEdgeRecordsTransitions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		if _, err := EnsureNetworkZone(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		steps := []struct {
			wildcard bool
			apex     bool
			writes   bool
		}{
			{wildcard: true, writes: true},
			{wildcard: true, writes: false},
			{wildcard: true, apex: true, writes: true},
			{wildcard: false, apex: true, writes: true},
			{wildcard: true, apex: true, writes: true},
		}
		for index, step := range steps {
			state, _ := GetDnsRootState(ctx, root)
			result, err := DnsSyncEdgeRecords(ctx, networkId, root, step.wildcard, step.apex)
			if err != nil {
				t.Fatal(err)
			}
			if (result != nil) != step.writes {
				t.Fatalf("step %d wrote %+v", index, result)
			}
			if !step.writes {
				requireDnsSerial(t, ctx, root, state.Serial)
			}
		}
		served := testDnsSnapshotRows(t, ctx, root)
		for _, key := range []dnsservice.DnsRecordKey{
			{Owner: "*." + apex, Type: dns.TypeA}, {Owner: "*." + apex, Type: dns.TypeAAAA},
			{Owner: apex, Type: dns.TypeA}, {Owner: apex, Type: dns.TypeAAAA},
		} {
			if set, ok := served[key]; !ok || set.Kind != dnsservice.DnsRecordKindEdge {
				t.Errorf("%+v: %+v", key, set)
			}
		}
	})
}

// <root>'s reconcile writes its set once and nothing when run again, and a changed dns.yml writes only
// what changed.
func TestDnsReconcileRootRecordsIsIdempotent(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		settings := testDnsRootSettings(t, "")
		settings.Roots = []string{root}
		settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{root: {ReportMx: "rpt.report.example"}}
		if err := DnsReconcileRootRecords(ctx, settings); err != nil {
			t.Fatal(err)
		}
		requireDnsSerial(t, ctx, root, 1)
		if err := DnsReconcileRootRecords(ctx, settings); err != nil {
			t.Fatal(err)
		}
		requireDnsSerial(t, ctx, root, 1)
		settings.RootRecords[root].Psl = "psl-proof-token"
		if err := DnsReconcileRootRecords(ctx, settings); err != nil {
			t.Fatal(err)
		}
		rows := testDnsJournal(t, ctx, root, 1)
		if len(rows) != 1 || len(rows[0].Upserts) != 1 || rows[0].Upserts[0].Owner != "_psl."+root || len(rows[0].Deletes) != 0 {
			t.Fatalf("the change's row %+v", rows)
		}
		if served := testDnsSnapshotRows(t, ctx, root); len(served) == 0 {
			t.Fatal("nothing served")
		} else if _, ok := served[dnsservice.DnsRecordKey{Owner: root, Type: dns.TypeSOA}]; !ok {
			t.Fatal("no SOA")
		}
	})
}
