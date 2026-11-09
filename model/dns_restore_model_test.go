//go:build sidecar_pending

package model

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The admin events a restore wrote for a network.
func testDnsRestoreAdminEvents(t testing.TB, ctx context.Context, networkId server.Id) []*NetworkAdminEvent {
	t.Helper()
	events := []*NetworkAdminEvent{}
	for _, event := range GetNetworkAdminEvents(ctx, networkId, 0, 0) {
		if event.Action == AdminEventActionDnsRestore {
			events = append(events, event)
		}
	}
	return events
}

// The lists map each RRset to its network through dns_label, the root's own with none; a network with
// an open case is flagged, in the incident only; each RRset is listed once and each network gets one
// admin event an incident, naming the incident and nothing of the case; a second incident lists on its
// own.
func TestDnsRestoreListsMapFlagAndListOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		flaggedNetworkId, flaggedApex := testDnsNetwork(t, ctx, root)
		otherNetworkId, otherApex := testDnsNetwork(t, ctx, root)
		now := server.NowUtc()
		testOpenAbuseCase(t, ctx, testAbuseCaseArgs(flaggedNetworkId, "www."+flaggedApex), now)
		incidentId := server.NewId()
		keys := []dnsservice.DnsRecordKey{
			{Owner: "www." + flaggedApex, Type: dns.TypeA},
			{Owner: flaggedApex, Type: dns.TypeMX},
			{Owner: "api." + otherApex, Type: dns.TypeAAAA},
			{Owner: "_dmarc." + root, Type: dns.TypeTXT},
			{Owner: "www." + flaggedApex, Type: dns.TypeA},
		}
		listing, err := ListDnsRestoreChanges(ctx, root, incidentId, keys, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(listing.Entries) != 4 {
			t.Fatalf("listed %+v", listing.Entries)
		}
		for _, entry := range listing.Entries {
			switch {
			case entry.Owner == "_dmarc."+root:
				if entry.NetworkId != nil || entry.Flagged {
					t.Errorf("the root's own %+v", entry)
				}
			case entry.Owner == "api."+otherApex:
				if entry.NetworkId == nil || server.Id(*entry.NetworkId) != otherNetworkId || entry.Flagged {
					t.Errorf("the other network's %+v", entry)
				}
			default:
				if entry.NetworkId == nil || server.Id(*entry.NetworkId) != flaggedNetworkId || !entry.Flagged {
					t.Errorf("the flagged network's %+v", entry)
				}
			}
		}
		for _, networkId := range []server.Id{flaggedNetworkId, otherNetworkId} {
			events := testDnsRestoreAdminEvents(t, ctx, networkId)
			if len(events) != 1 || events[0].TargetId == nil || *events[0].TargetId != incidentId ||
				events[0].TargetKind != AdminEventTargetDnsRestore || events[0].Result != AdminEventResultDnsRestoreListed || events[0].ActorKind != AdminEventActorSystem {
				t.Errorf("network %s's events %+v", networkId, events)
			}
		}

		// a rerun lists nothing twice and writes no second event; a new key lists alone
		again, err := ListDnsRestoreChanges(ctx, root, incidentId, append(keys, dnsservice.DnsRecordKey{Owner: "mail." + otherApex, Type: dns.TypeMX}), now.Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if len(again.Entries) != 5 || len(testDnsRestoreAdminEvents(t, ctx, otherNetworkId)) != 1 {
			t.Errorf("a rerun listed %d", len(again.Entries))
		}
		read, err := GetDnsRestoreListing(ctx, root, incidentId)
		if err != nil || len(read.Entries) != 5 {
			t.Errorf("read back %+v %v", read, err)
		}
		// another incident lists the same RRsets on its own, with its own events
		secondId := server.NewId()
		second, err := ListDnsRestoreChanges(ctx, root, secondId, keys[:1], now.Add(2*time.Minute))
		if err != nil || len(second.Entries) != 1 || len(testDnsRestoreAdminEvents(t, ctx, flaggedNetworkId)) != 2 {
			t.Errorf("a second incident %+v %v", second, err)
		}
	})
}

// The lists and the posted diffs go after 400 days, the ones a day younger stay.
func TestDnsRestoreListsPurgeAfter400Days(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		_, apex := testDnsNetwork(t, ctx, root)
		old := server.NowUtc().Add(-401 * 24 * time.Hour)
		if _, err := ListDnsRestoreChanges(ctx, root, server.NewId(), []dnsservice.DnsRecordKey{{Owner: "old." + apex, Type: dns.TypeA}}, old); err != nil {
			t.Fatal(err)
		}
		keptId := server.NewId()
		if _, err := ListDnsRestoreChanges(ctx, root, keptId, []dnsservice.DnsRecordKey{{Owner: "new." + apex, Type: dns.TypeA}}, server.NowUtc().Add(-399*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if err := AddDnsReloadDiff(ctx, &dnsservice.DnsReloadDiff{Root: root, ReplacedSerial: 1, Serial: 2, Rrsets: []dnsservice.DnsRecordKey{{Owner: "new." + apex, Type: dns.TypeA}}}); err != nil {
			t.Fatal(err)
		}
		purged, err := PurgeDnsRestoreLists(ctx, server.NowUtc())
		if err != nil || purged < 1 {
			t.Fatalf("purged %d %v", purged, err)
		}
		listing, err := GetDnsRestoreListing(ctx, root, keptId)
		if err != nil || len(listing.Entries) != 1 {
			t.Errorf("the younger list %+v %v", listing, err)
		}
		diffs, err := GetDnsReloadDiffs(ctx, root)
		if err != nil || len(diffs) != 1 {
			t.Errorf("the diff just posted %+v %v", diffs, err)
		}
		// 400 days on, the diff goes too
		if _, err := PurgeDnsRestoreLists(ctx, server.NowUtc().Add(401*24*time.Hour)); err != nil {
			t.Fatal(err)
		}
		if diffs, _ := GetDnsReloadDiffs(ctx, root); len(diffs) != 0 {
			t.Errorf("diffs past 400 days %+v", diffs)
		}
	})
}

// A host suspended while restoring takes no serial, and the snapshot at the restore serial, the one
// every edge reloads, withholds it: the transaction lands above it, with the incident recorded.
func TestDnsRestoreSnapshotHoldsASuspensionPlacedWhileRestoring(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root, networkId, apex := testDnsSuspendZone(t, ctx)
		before, _ := GetDnsRootState(ctx, root)
		if err := SetDnsRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		if err := SuspendHost(ctx, networkId, "www"); err != nil {
			t.Fatal(err)
		}
		incidentId := server.NewId()
		restoreSerial, err := CommitDnsRestore(ctx, nil, root, incidentId, before.Serial+5)
		if err != nil {
			t.Fatal(err)
		}
		if want := before.Serial + 5 + dnsservice.DnsRestoreSerialJump; restoreSerial < want {
			t.Errorf("restore serial %d below %d", restoreSerial, want)
		}
		state, _ := GetDnsRootState(ctx, root)
		if !state.Restoring || state.Serial != restoreSerial || state.RestoreIncidentId == nil || server.Id(*state.RestoreIncidentId) != incidentId {
			t.Errorf("state %+v", state)
		}
		served := testDnsSnapshotRows(t, ctx, root)
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "www." + apex, Type: dns.TypeTXT}]; ok {
			t.Error("the restore's snapshot serves the suspended host")
		}
		if _, ok := served[dnsservice.DnsRecordKey{Owner: "www." + apex, Type: dnsservice.DnsWithheldType}]; !ok {
			t.Error("the restore's snapshot lacks the withheld owner")
		}
	})
}
