//go:build sidecar_pending

package model

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// §12.7's edge grants and their trigger (dns_edge_grants_model.go): which hosts set * and @, the sync that writes a
// transition once and draws a zone only for a first publish, and the hourly pass over every network's published
// names, which removes what expired, adds what is missing, writes nothing where the flags hold, and stops, writing
// nothing more, where it cannot read a network's grants.

// A network with no label, as a new network is before anything needs its zone.
func testDnsEdgeNetwork(ctx context.Context) server.Id {
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "edge-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	return networkId
}

// The zone's edge flags in root, nil for no zone.
func testDnsEdgeState(ctx context.Context, root string, networkId server.Id) (state *DnsZoneState) {
	server.Db(ctx, func(conn server.PgConn) {
		state = getDnsZoneStateInTx(ctx, conn, dnsLabelRoot(root), networkId)
	})
	return
}

// Whether root serves an edge row at owner of each of A and AAAA, and none at either when want is false.
func testDnsEdgeServed(t testing.TB, served map[dnsservice.DnsRecordKey]dnsservice.DnsRecordSet, owner string, want bool) {
	t.Helper()
	for _, rrType := range []uint16{dns.TypeA, dns.TypeAAAA} {
		set, ok := served[dnsservice.DnsRecordKey{Owner: owner, Type: rrType}]
		if ok != want || ok && set.Kind != dnsservice.DnsRecordKindEdge {
			t.Errorf("%s %s served %t (%+v), want %t", owner, dns.TypeToString[rrType], ok, set, want)
		}
	}
}

// A one-label host or a glob sets *, @ sets @, a scope host sets neither, and the system publish sets * only while
// the network has a mail_network row, which is asked only where nothing else sets *.
func TestDnsEdgeGrantsOfHostsReadsEachHost(t *testing.T) {
	cases := []struct {
		hosts       []string
		mailNetwork bool
		want        DnsEdgeGrants
		asked       bool
	}{
		{hosts: nil, want: DnsEdgeGrants{}},
		{hosts: []string{"web"}, want: DnsEdgeGrants{Wildcard: true}},
		{hosts: []string{"@"}, want: DnsEdgeGrants{Apex: true}},
		{hosts: []string{"@", "web", "shop"}, want: DnsEdgeGrants{Wildcard: true, Apex: true}},
		{hosts: []string{"*"}, want: DnsEdgeGrants{Wildcard: true}},
		{hosts: []string{"*-preview"}, want: DnsEdgeGrants{Wildcard: true}},
		{hosts: []string{"web.eng", "*.eng"}, want: DnsEdgeGrants{}},
		{hosts: []string{""}, want: DnsEdgeGrants{}},
		{hosts: []string{"mta-sts"}, mailNetwork: false, want: DnsEdgeGrants{}, asked: true},
		{hosts: []string{"mta-sts"}, mailNetwork: true, want: DnsEdgeGrants{Wildcard: true}, asked: true},
		{hosts: []string{"mta-sts", "web"}, mailNetwork: false, want: DnsEdgeGrants{Wildcard: true}, asked: false},
	}
	for _, c := range cases {
		asked := false
		got := DnsEdgeGrantsOfHosts(c.hosts, func() bool {
			asked = true
			return c.mailNetwork
		})
		if got != c.want || asked != c.asked {
			t.Errorf("%v with mail %t: %+v asking %t, want %+v asking %t", c.hosts, c.mailNetwork, got, asked, c.want, c.asked)
		}
	}
	if got := DnsEdgeGrantsOfHosts([]string{"mta-sts"}, nil); got != (DnsEdgeGrants{}) {
		t.Errorf("the system publish with no mail answer: %+v", got)
	}
}

// A network with no grant gets no zone; its first publish draws its label and writes * in one row; the same grants
// again write nothing; @ follows the apex; and the grants' end deletes what they wrote.
func TestSyncDnsEdgeRecordsWritesEachTransitionOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId := testDnsEdgeNetwork(ctx)
		result, err := SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{})
		if err != nil || result != nil {
			t.Fatalf("no grant wrote %+v, %v", result, err)
		}
		if label := GetNetworkLabel(ctx, networkId, root); label != "" || testDnsEdgeState(ctx, root, networkId) != nil {
			t.Fatalf("no grant drew label %q and zone %+v", label, testDnsEdgeState(ctx, root, networkId))
		}
		requireDnsSerial(t, ctx, root, 0)

		result, err = SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{Wildcard: true})
		if err != nil || result == nil {
			t.Fatalf("a first publish wrote %+v, %v", result, err)
		}
		label := GetNetworkLabel(ctx, networkId, root)
		if label == "" {
			t.Fatal("a first publish drew no label")
		}
		apex := label + "." + root
		if state := testDnsEdgeState(ctx, root, networkId); state == nil || !state.EdgeWildcard || state.EdgeApex {
			t.Fatalf("after a first publish the zone reads %+v", state)
		}
		served := testDnsSnapshotRows(t, ctx, root)
		testDnsEdgeServed(t, served, "*."+apex, true)
		testDnsEdgeServed(t, served, apex, false)
		serial := result.Serial

		result, err = SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{Wildcard: true})
		if err != nil || result != nil {
			t.Fatalf("the same grants wrote %+v, %v", result, err)
		}
		requireDnsSerial(t, ctx, root, serial)

		result, err = SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{Wildcard: true, Apex: true})
		if err != nil || result == nil || result.Serial != serial+1 {
			t.Fatalf("publishing the apex wrote %+v, %v", result, err)
		}
		testDnsEdgeServed(t, testDnsSnapshotRows(t, ctx, root), apex, true)

		result, err = SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{})
		if err != nil || result == nil || result.Serial != serial+2 {
			t.Fatalf("the grants' end wrote %+v, %v", result, err)
		}
		served = testDnsSnapshotRows(t, ctx, root)
		testDnsEdgeServed(t, served, "*."+apex, false)
		testDnsEdgeServed(t, served, apex, false)
		if state := testDnsEdgeState(ctx, root, networkId); state == nil || state.EdgeWildcard || state.EdgeApex {
			t.Fatalf("after the grants' end the zone reads %+v", state)
		}
		// the zone stays, with its mail-off set, once drawn
		if _, ok := served[dnsservice.DnsRecordKey{Owner: apex, Type: dns.TypeMX}]; !ok {
			t.Error("the zone's own set went with its edge records")
		}
	})
}

// api's sync reads the state its sequencer holds: before base 1 a network publishes nothing and gets no zone; a base
// that publishes nothing takes away the * records a lost sync left; and a sync of the same state writes nothing.
func TestSyncDnsEdgeRecordsAtTipFollowsTheSequencersState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		defer Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
		cluster, err := blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		world := sidecartest.NewConfigLogSuiteWorld(ctx, cluster)
		networkId := server.Id(world.NetworkId())
		Testing_CreateNetwork(ctx, networkId, "edge-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
		settings := DefaultConfigLogSettings()
		settings.Clock = world.Clock()
		sequencer := NewConfigLogSequencer(cluster, settings, nil)

		SyncDnsEdgeRecordsAtTip(ctx, sequencer, networkId)
		if state := testDnsEdgeState(ctx, root, networkId); state != nil {
			t.Fatalf("a network with no log holds a zone %+v", state)
		}
		requireDnsSerial(t, ctx, root, 0)

		// the * records of a sync whose state a crash lost
		if _, err := SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{Wildcard: true}); err != nil {
			t.Fatal(err)
		}
		signedHeader, rows, genesis, configPushes := world.BaseOne()
		if _, err := sequencer.AppendBase(ctx, networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes}); err != nil {
			t.Fatal(err)
		}
		SyncDnsEdgeRecordsAtTip(ctx, sequencer, networkId)
		if state := testDnsEdgeState(ctx, root, networkId); state == nil || state.EdgeWildcard || state.EdgeApex {
			t.Fatalf("after a base that publishes nothing the zone reads %+v", state)
		}
		testDnsEdgeServed(t, testDnsSnapshotRows(t, ctx, root), "*."+GetNetworkLabel(ctx, networkId, root)+"."+root, false)
		serial := testDnsRootSerial(t, ctx, root)
		SyncDnsEdgeRecordsAtTip(ctx, sequencer, networkId)
		requireDnsSerial(t, ctx, root, serial)
	})
}

// The pass over every network with a log: a published host gets *, a zone whose grant expired or ended loses it, an
// expired name and the system publish without mail make no zone, the system publish with mail sets *, a zone whose
// flags hold is not written, a bounded run resumes at its cursor, and a source that fails stops the run with nothing
// written past it.
func TestReconcileDnsEdgeRecordsFollowsEachNetworksGrants(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		defer Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		now := server.NowUtc()

		// each network with a log, then its label where it holds a zone already
		network := func() server.Id {
			networkId := testDnsEdgeNetwork(ctx)
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`INSERT INTO network_acl_head (network_id, version, chain) VALUES ($1, 1, $2)`,
					networkId,
					make([]byte, 32),
				))
			})
			return networkId
		}
		published := network()
		ended := network()
		expired := network()
		systemAlone := network()
		systemMail := network()
		held := network()
		quiet := network()
		for _, networkId := range []server.Id{ended, held} {
			if _, err := SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{Wildcard: true}); err != nil {
				t.Fatal(err)
			}
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO mail_network (network_id, create_time) VALUES ($1, $2)`, systemMail, now))
		})
		networkIdNames := map[server.Id][]*TlsPublishedName{
			published:   {{Host: "web", Port: 443}, {Host: "web", Port: 8443}},
			expired:     {{Host: "web", Port: 443, ExpireTime: now}},
			systemAlone: {{Host: "mta-sts", Port: 443}},
			systemMail:  {{Host: "mta-sts", Port: 443}},
			held:        {{Host: "shop", Port: 443, ExpireTime: now.Add(time.Hour)}},
		}
		var failing server.Id
		SetTlsPublishedNamesSource(func(ctx context.Context, networkId server.Id, sourceNow time.Time) ([]*TlsPublishedName, error) {
			if networkId == failing {
				return nil, errors.New("the gate cache holds no verified state of the network")
			}
			return networkIdNames[networkId], nil
		})
		defer SetTlsPublishedNamesSource(nil)

		// a source that fails at the first network writes nothing
		networkIds := []server.Id{published, ended, expired, systemAlone, systemMail, held, quiet}
		slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
			return bytes.Compare(a[:], b[:])
		})
		failing = networkIds[0]
		serial := testDnsRootSerial(t, ctx, root)
		if result, err := ReconcileDnsEdgeRecords(ctx, now, nil, DefaultDnsEdgeRecordsReconcileSettings()); err == nil {
			t.Fatalf("a failing source answered %+v", result)
		}
		requireDnsSerial(t, ctx, root, serial)
		failing = server.Id{}

		// in runs of two networks, each resuming where the last stopped
		settings := &DnsEdgeRecordsReconcileSettings{NetworkBatchCount: 1, RunNetworkMaxCount: 2}
		var after *server.Id
		total := &DnsEdgeRecordsReconcileResult{}
		for runCount := 0; ; runCount += 1 {
			if len(networkIds) < runCount {
				t.Fatalf("the pass never ended, at %v", after)
			}
			result, err := ReconcileDnsEdgeRecords(ctx, now, after, settings)
			if err != nil {
				t.Fatal(err)
			}
			if settings.RunNetworkMaxCount < result.NetworkCount {
				t.Fatalf("a run read %d networks", result.NetworkCount)
			}
			total.NetworkCount += result.NetworkCount
			total.WriteCount += result.WriteCount
			total.WaitingCount += result.WaitingCount
			total.FailedCount += result.FailedCount
			if result.AfterNetworkId == nil {
				break
			}
			after = result.AfterNetworkId
		}
		if total.NetworkCount != len(networkIds) || total.WriteCount != 3 || total.WaitingCount != 0 || total.FailedCount != 0 {
			t.Errorf("the pass did %+v", total)
		}
		want := map[server.Id]*DnsZoneState{
			published:  {EdgeWildcard: true},
			ended:      {EdgeWildcard: false},
			systemMail: {EdgeWildcard: true},
			held:       {EdgeWildcard: true},
		}
		for _, networkId := range networkIds {
			state := testDnsEdgeState(ctx, root, networkId)
			wanted := want[networkId]
			switch {
			case wanted == nil && state != nil:
				t.Errorf("%s holds a zone %+v, with no grant", networkId, state)
			case wanted != nil && (state == nil || state.EdgeWildcard != wanted.EdgeWildcard || state.EdgeApex):
				t.Errorf("%s's zone reads %+v, want %+v", networkId, state, wanted)
			}
		}
		served := testDnsSnapshotRows(t, ctx, root)
		for networkId, wanted := range want {
			testDnsEdgeServed(t, served, "*."+GetNetworkLabel(ctx, networkId, root)+"."+root, wanted.EdgeWildcard)
		}

		// a second pass finds every zone's flags held, and writes nothing
		serial = testDnsRootSerial(t, ctx, root)
		result, err := ReconcileDnsEdgeRecords(ctx, now, nil, DefaultDnsEdgeRecordsReconcileSettings())
		if err != nil || result.WriteCount != 0 || result.AfterNetworkId != nil {
			t.Fatalf("a second pass did %+v, %v", result, err)
		}
		requireDnsSerial(t, ctx, root, serial)

		// non-positive bounds still move the walk on, a network a run
		result, err = ReconcileDnsEdgeRecords(ctx, now, nil, &DnsEdgeRecordsReconcileSettings{})
		if err != nil || result.NetworkCount != 1 || result.AfterNetworkId == nil || *result.AfterNetworkId != networkIds[0] {
			t.Fatalf("a run with no bounds did %+v, %v", result, err)
		}
	})
}

// With no published-names source the pass reads nothing and writes nothing: it never takes unread grants for none.
func TestReconcileDnsEdgeRecordsNeedsASource(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		defer Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		networkId := testDnsEdgeNetwork(ctx)
		if _, err := SyncDnsEdgeRecords(ctx, networkId, root, DnsEdgeGrants{Wildcard: true}); err != nil {
			t.Fatal(err)
		}
		SetTlsPublishedNamesSource(nil)
		serial := testDnsRootSerial(t, ctx, root)
		if result, err := ReconcileDnsEdgeRecords(ctx, server.NowUtc(), nil, DefaultDnsEdgeRecordsReconcileSettings()); !errors.Is(err, ErrTlsPublishedNamesUnavailable) {
			t.Fatalf("a pass with no source answered %+v, %v", result, err)
		}
		requireDnsSerial(t, ctx, root, serial)
		if state := testDnsEdgeState(ctx, root, networkId); state == nil || !state.EdgeWildcard {
			t.Fatalf("a pass with no source left %+v", state)
		}
	})
}
