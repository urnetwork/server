//go:build sidecar_pending

package model

// Labels in the label map's serial order (§12.5, §12.6; DNS-M5-10's map deltas): a label is drawn with its zone, so a
// draw in a transaction that writes nothing else still takes a serial a reader following the map since its cursor
// gets, and a draw inside a zone writer's transaction takes that writer's one serial. Before, a label drawn outside a
// zone write kept change serial 0, which no read since a serial returns, so a running tlsedge never routed it.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server"
)

// A network for one test, with no label yet.
func testDnsLabelNetwork(t testing.TB, ctx context.Context) (networkId server.Id, userId server.Id) {
	t.Helper()
	networkId = server.NewId()
	userId = server.NewId()
	Testing_CreateNetwork(ctx, networkId, "serial-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), userId)
	return
}

// The network's entries in a read of the map.
func testDnsLabelMapEntries(labelMap *DnsLabelMap, networkId server.Id) []*DnsLabelMapEntry {
	entries := []*DnsLabelMapEntry{}
	for _, entry := range labelMap.Entries {
		if entry.NetworkId != nil && *entry.NetworkId == networkId {
			entries = append(entries, entry)
		}
	}
	return entries
}

// A label drawn alone, as GET /sidecar/label's and GET /dns/zone's first need draws it, after a reader took its
// cursor, comes back from the reader's next read at the serial of its zone's one journal write, which holds the
// zone's §12.7 set under the label's apex.
func TestDnsLabelDrawnAloneTakesItsZonesSerial(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		firstNetworkId, _ := testDnsNetwork(t, ctx, root)
		cursor, err := DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		first := testDnsLabelMapEntries(cursor, firstNetworkId)
		if len(first) != 1 || first[0].ChangeSerial == 0 || first[0].ChangeSerial != cursor.Serial {
			t.Fatalf("the first label drawn alone reads %+v in a map at %d", first, cursor.Serial)
		}

		secondNetworkId, secondApex := testDnsNetwork(t, ctx, root)
		since, err := DnsLabelMapSince(ctx, root, cursor.Serial)
		if err != nil {
			t.Fatal(err)
		}
		if len(since.Entries) != 1 {
			t.Fatalf("the read since %d: %+v", cursor.Serial, since.Entries)
		}
		entry := since.Entries[0]
		if entry.NetworkId == nil || *entry.NetworkId != secondNetworkId || entry.Kind != DnsLabelKindLabel || entry.State != DnsLabelStateLive || entry.Label+"."+root != secondApex {
			t.Fatalf("the second label reads %+v", entry)
		}
		if entry.ChangeSerial <= cursor.Serial || since.Serial != entry.ChangeSerial {
			t.Fatalf("the second label at %d in a read at %d, from %d", entry.ChangeSerial, since.Serial, cursor.Serial)
		}
		if serial := testDnsRootSerial(t, ctx, root); serial != entry.ChangeSerial {
			t.Fatalf("the root is at %d, the label at %d", serial, entry.ChangeSerial)
		}
		rows := testDnsJournal(t, ctx, root, cursor.Serial)
		if len(rows) != 1 || rows[0].Serial != entry.ChangeSerial || len(rows[0].Upserts) == 0 || 0 < len(rows[0].Deletes) {
			t.Fatalf("the draw's journal %+v", rows)
		}
		for _, set := range rows[0].Upserts {
			if set.Kind != dnsservice.DnsRecordKindSystem || !dnsOwnerAtOrBelow(set.Owner, secondApex) {
				t.Fatalf("the draw wrote %+v", set)
			}
		}
		// a second need draws nothing and takes no serial
		again, err := EnsureNetworkLabel(ctx, secondNetworkId, root)
		if err != nil || again+"."+root != secondApex {
			t.Fatalf("a second need gave %q, %v", again, err)
		}
		requireDnsSerial(t, ctx, root, entry.ChangeSerial)
	})
}

// A label drawn inside a zone writer's transaction takes that writer's one serial: on a network with no label, the
// zone, its edge records and its mail records are each one journal write, and the label's change serial is its
// serial.
func TestDnsLabelDrawnInAZoneWriteTakesThatWritesSerial(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		writers := []struct {
			name  string
			write func(networkId server.Id) (*DnsWriteResult, error)
		}{
			{name: "the zone", write: func(networkId server.Id) (*DnsWriteResult, error) { return EnsureNetworkZone(ctx, networkId, root) }},
			{name: "edge records", write: func(networkId server.Id) (*DnsWriteResult, error) {
				return DnsSyncEdgeRecords(ctx, networkId, root, true, false)
			}},
			{name: "mail records", write: func(networkId server.Id) (*DnsWriteResult, error) {
				return DnsSetMailRecords(ctx, networkId, root, true)
			}},
		}
		for _, writer := range writers {
			networkId, _ := testDnsLabelNetwork(t, ctx)
			before := testDnsRootSerial(t, ctx, root)
			result, err := writer.write(networkId)
			if err != nil {
				t.Fatalf("%s: %v", writer.name, err)
			}
			if result == nil || result.Serial != before+1 {
				t.Fatalf("%s wrote %+v from %d", writer.name, result, before)
			}
			if rows := testDnsJournal(t, ctx, root, before); len(rows) != 1 {
				t.Fatalf("%s made %d journal writes", writer.name, len(rows))
			}
			labelMap, err := DnsLabelMapSince(ctx, root, before)
			if err != nil {
				t.Fatal(err)
			}
			entries := testDnsLabelMapEntries(labelMap, networkId)
			if len(entries) != 1 || entries[0].Kind != DnsLabelKindLabel || entries[0].ChangeSerial != result.Serial {
				t.Fatalf("%s's label reads %+v, its write at %d", writer.name, entries, result.Serial)
			}
		}
	})
}

// Two labels drawn alone at once, the first holding its zone's serial uncommitted: the second waits at the root's
// row lock and takes the next serial, and a reader that reads while the first is held, then while the second is,
// then after both, gets each label once, in serial order, from its cursor.
func TestDnsLabelsDrawnAtOnceComeInSerialOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		firstNetworkId, _ := testDnsLabelNetwork(t, ctx)
		secondNetworkId, _ := testDnsLabelNetwork(t, ctx)
		start, err := DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}

		firstHeld := make(chan struct{})
		firstRelease := make(chan struct{})
		secondBefore := make(chan struct{})
		secondHeld := make(chan struct{})
		secondRelease := make(chan struct{})
		var stateLock sync.Mutex
		heldCount := 0
		beforeCount := 0
		Testing_DnsWriteBarrier = func(name string, barrierRoot string) {
			if barrierRoot != root {
				return
			}
			switch name {
			case DnsWriteBarrier:
				stateLock.Lock()
				heldCount += 1
				count := heldCount
				stateLock.Unlock()
				switch count {
				case 1:
					close(firstHeld)
					<-firstRelease
				case 2:
					close(secondHeld)
					<-secondRelease
				}
			case DnsWriteBarrierBefore:
				stateLock.Lock()
				beforeCount += 1
				second := beforeCount == 2
				stateLock.Unlock()
				if second {
					close(secondBefore)
				}
			}
		}
		defer func() { Testing_DnsWriteBarrier = nil }()

		drawn := make(chan error, 2)
		go func() {
			_, err := EnsureNetworkLabel(ctx, firstNetworkId, root)
			drawn <- err
		}()
		<-firstHeld
		// the first's label and zone are uncommitted: nothing new
		held, err := DnsLabelMapSince(ctx, root, start.Serial)
		if err != nil {
			t.Fatal(err)
		}
		if len(held.Entries) != 0 || held.Serial != start.Serial {
			t.Fatalf("a read while the first is held: %+v at %d", held.Entries, held.Serial)
		}
		go func() {
			_, err := EnsureNetworkLabel(ctx, secondNetworkId, root)
			drawn <- err
		}()
		// the second is at the root's row lock while the first holds its serial
		<-secondBefore
		close(firstRelease)
		// the second holds the next serial, so the first has committed
		<-secondHeld
		between, err := DnsLabelMapSince(ctx, root, held.Serial)
		if err != nil {
			t.Fatal(err)
		}
		if len(between.Entries) != 1 || between.Entries[0].NetworkId == nil || *between.Entries[0].NetworkId != firstNetworkId {
			t.Fatalf("a read between the commits: %+v", between.Entries)
		}
		if between.Entries[0].ChangeSerial != start.Serial+1 || between.Serial != start.Serial+1 {
			t.Fatalf("the first label at %d in a read at %d, from %d", between.Entries[0].ChangeSerial, between.Serial, start.Serial)
		}
		close(secondRelease)
		for range 2 {
			if err := <-drawn; err != nil {
				t.Fatal(err)
			}
		}
		after, err := DnsLabelMapSince(ctx, root, between.Serial)
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Entries) != 1 || after.Entries[0].NetworkId == nil || *after.Entries[0].NetworkId != secondNetworkId || after.Entries[0].ChangeSerial != between.Serial+1 {
			t.Fatalf("a read after both: %+v", after.Entries)
		}
	})
}

// A draw in a restoring root is refused with nothing written, as every write is (§12.6), and once the root is back
// the network's draw takes its serial.
func TestDnsLabelDrawWhileRestoringWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, _ := testDnsLabelNetwork(t, ctx)
		if err := SetDnsRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		if label, err := EnsureNetworkLabel(ctx, networkId, root); !errors.Is(err, dnsservice.ErrDnsRestoring) || label != "" {
			t.Fatalf("a draw while restoring gave %q, %v", label, err)
		}
		if label := GetNetworkLabel(ctx, networkId, root); label != "" {
			t.Fatalf("the refused draw left the label %q", label)
		}
		requireDnsSerial(t, ctx, root, 0)
		if err := SetDnsRestoring(ctx, root, false); err != nil {
			t.Fatal(err)
		}
		label, err := EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		requireDnsSerial(t, ctx, root, 1)
		labelMap, err := DnsLabelMapSince(ctx, root, 0)
		if err != nil {
			t.Fatal(err)
		}
		if entries := testDnsLabelMapEntries(labelMap, networkId); len(entries) != 1 || entries[0].Label != label || entries[0].ChangeSerial != 1 {
			t.Fatalf("the draw once back reads %+v", entries)
		}
	})
}

// A root with no dns_root row takes no serial, so a label drawn there keeps change serial 0 and has no zone: only a
// snapshot of the map shows it.
func TestDnsLabelDrawnWhereTheRootTakesNoSerialKeepsZero(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := "unrooted-" + strings.ReplaceAll(server.NewId().String()[24:], "-", "") + ".example."
		networkId, _ := testDnsLabelNetwork(t, ctx)
		label, err := EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if entries := testDnsLabelMapEntries(snapshot, networkId); len(entries) != 1 || entries[0].Label != label || entries[0].ChangeSerial != 0 {
			t.Fatalf("the label reads %+v", entries)
		}
		zoned := false
		server.Db(ctx, func(conn server.PgConn) {
			zoned = getDnsZoneStateInTx(ctx, conn, root, networkId) != nil
		})
		if zoned {
			t.Fatal("a root that takes no serial has a zone for the label")
		}
	})
}

// tlsedge's names (tlsedgeservice.TlsEdgeNames) over the Postgres store, as the engine's names loop runs them: a view
// snapshotted before the network had a label resolves <host>.<label>.<root> once the label is drawn alone and the
// next read since the view's serial is applied.
func TestDnsLabelDrawnAloneRoutesThroughTlsEdgeNames(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		edgeRoot := strings.TrimSuffix(root, ".")
		store := NewTlsEdgeStore(nil)
		names := tlsedgeservice.NewTlsEdgeNames(edgeRoot, nil)
		snapshot, err := store.NameSnapshot(ctx, edgeRoot)
		if err != nil {
			t.Fatal(err)
		}
		names.Apply(snapshot, true)

		networkId, apex := testDnsNetwork(t, ctx, root)
		label := strings.TrimSuffix(apex, "."+root)
		host := "web." + label + "." + edgeRoot
		if target, ok := names.Resolve(host, 443); ok {
			t.Fatalf("%s resolved before the view read it: %+v", host, target)
		}
		serial, restoreSerial := names.Serials()
		changes, err := store.NamesSince(ctx, edgeRoot, serial)
		if err != nil {
			t.Fatal(err)
		}
		// the engine applies a read as changes only under the same restore serial and a serial that did not go back
		if changes.RestoreSerial != restoreSerial || changes.Serial <= serial {
			t.Fatalf("the read since %d: %+v", serial, changes)
		}
		names.Apply(changes, false)
		target, ok := names.Resolve(host, 443)
		if !ok || target.NetworkId != sidecar.Id(networkId) || target.Label != label || target.Name != "tls/443/web" {
			t.Fatalf("%s resolves %+v, %t", host, target, ok)
		}
	})
}

// A network whose label was drawn alone is deleted: its zone's records leave in one journal write, and its label's
// tombstone comes back from a read since the reader's cursor, so a running tlsedge stops routing the name. Before, a
// label drawn alone had no zone, so its withdrawal wrote nothing and its tombstone kept change serial 0.
func TestDnsLabelDrawnAloneTombstonesInSerialOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, userId := testDnsLabelNetwork(t, ctx)
		label, err := EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		cursor, err := DnsLabelMapSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if success, _ := RemoveNetwork(ctx, networkId, &userId); !success {
			t.Fatal("the network was not removed")
		}
		rows := testDnsJournal(t, ctx, root, cursor.Serial)
		if len(rows) != 1 || len(rows[0].Upserts) != 0 || len(rows[0].Deletes) == 0 {
			t.Fatalf("the withdrawal's journal %+v", rows)
		}
		since, err := DnsLabelMapSince(ctx, root, cursor.Serial)
		if err != nil {
			t.Fatal(err)
		}
		entries := testDnsLabelMapEntries(since, networkId)
		if len(entries) != 1 || entries[0].Label != label || entries[0].State != DnsLabelStateTombstoned || entries[0].ChangeSerial != rows[0].Serial {
			t.Fatalf("the tombstone reads %+v, the withdrawal at %d", entries, rows[0].Serial)
		}
	})
}
