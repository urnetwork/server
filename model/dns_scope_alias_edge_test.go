//go:build sidecar_pending

package model

// §12.7's edge records beyond a network's own * and @ (dns_edge_grants_model.go; DNS-M6-06, DNS-M6-01, owner
// decision 335), through the edge's own answer path over the rows Postgres serves: a Team scope's zone serves its *
// A and AAAA, its mail and its _mta-sts once a host of the scope is published, and loses its addresses when the
// grant ends, its mail standing; a scope's first publish draws the network's zone; a scope label the zone already
// holds as a host is refused and counted, the others written; api's sync reads the scopes and grants of the state its
// sequencer holds, at the sequencer's clock; the hourly pass writes and takes away scope addresses from its source;
// a certified alias answers the label's and each scope's edge records, and loses them with the grants; the mail
// toggle turns each written scope's mail with the zone's, both ways, leaving a withheld scope to the sync after its
// lift; and POST /mail/enable writes the system publish's * and each scope's mail at once rather than at the hourly
// pass.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The edge's view of root, root's own records reconciled so the view answers, and a healthy set of one edge
// interface and one router interface, each with an IPv4 and an IPv6 address.
func testDnsEdgeView(t testing.TB, ctx context.Context, root string) (*dnsservice.ZoneView, *dnsservice.HealthySet) {
	t.Helper()
	settings := testDnsRootSettings(t, "")
	settings.Roots = []string{root}
	settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{root: {ReportMx: "rpt.report.example"}}
	if err := DnsReconcileRootRecords(ctx, settings); err != nil {
		t.Fatal(err)
	}
	snapshot, err := OpenDnsSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	view, err := dnsservice.NewZoneView(settings, snapshot)
	snapshot.Close()
	if err != nil {
		t.Fatal(err)
	}
	router := dnsservice.DnsLbInterface{Name: "lb-2", Ipv4: netip.MustParseAddr("192.0.2.2"), Ipv6: netip.MustParseAddr("2001:db8::2"), Router: true}
	healthy := dnsservice.NewHealthySet(
		[]dnsservice.DnsLbInterface{{Name: "lb-1", Ipv4: netip.MustParseAddr("192.0.2.1"), Ipv6: netip.MustParseAddr("2001:db8::1")}, router},
		[]dnsservice.DnsLbInterface{router},
	)
	return view, healthy
}

// Whether name answers the healthy edge's addresses for A and AAAA, or none of either.
func requireDnsEdgeAddresses(t testing.TB, view *dnsservice.ZoneView, healthy *dnsservice.HealthySet, name string, answers bool) {
	t.Helper()
	if answers {
		requireDnsViewAnswer(t, view, healthy, name, dns.TypeA, dns.RcodeSuccess, "192.0.2.1", "192.0.2.2")
		requireDnsViewAnswer(t, view, healthy, name, dns.TypeAAAA, dns.RcodeSuccess, "2001:db8::1", "2001:db8::2")
		return
	}
	requireDnsViewAnswer(t, view, healthy, name, dns.TypeA, dns.RcodeSuccess)
	requireDnsViewAnswer(t, view, healthy, name, dns.TypeAAAA, dns.RcodeSuccess)
}

// The grants of a state that publishes hosts on 443 with no end and lists scopes as its Team scopes.
func testDnsEdgeZoneGrants(scopes []string, hosts ...string) *DnsEdgeZoneGrants {
	names := []*sidecar.AclPublishedName{}
	for _, host := range hosts {
		names = append(names, &sidecar.AclPublishedName{Host: host, Port: 443})
	}
	return DnsEdgeZoneGrantsOf(&DnsEdgePublished{Names: names, ScopeLabels: scopes}, server.NowUtc(), nil)
}

// A network with a log, as api's append path leaves its head, and no label.
func testDnsEdgeNetworkWithLog(ctx context.Context) server.Id {
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

// A host of each listed scope, a glob label included, sets its scope's *; a bare * sets every listed scope's; a
// host of a scope the state does not list, a network host, @, a glob of one label and a name three labels deep set
// none.
func TestDnsEdgeScopeWildcardsOfHostsReadsEachHost(t *testing.T) {
	scopes := []string{"eng", "ops"}
	cases := []struct {
		hosts []string
		want  map[string]bool
	}{
		{hosts: nil, want: map[string]bool{"eng": false, "ops": false}},
		{hosts: []string{"web.eng"}, want: map[string]bool{"eng": true, "ops": false}},
		{hosts: []string{"*.ops"}, want: map[string]bool{"eng": false, "ops": true}},
		{hosts: []string{"web-*.eng", "api.ops"}, want: map[string]bool{"eng": true, "ops": true}},
		{hosts: []string{"*"}, want: map[string]bool{"eng": true, "ops": true}},
		{hosts: []string{"web.dev", "web", "@", "*-preview", "mta-sts", "a.web.eng", ".eng", "web."}, want: map[string]bool{"eng": false, "ops": false}},
	}
	for _, c := range cases {
		got := DnsEdgeScopeWildcardsOfHosts(c.hosts, scopes)
		if len(got) != len(c.want) {
			t.Errorf("%v: %v, want %v", c.hosts, got, c.want)
			continue
		}
		for scope, want := range c.want {
			if published, ok := got[scope]; !ok || published != want {
				t.Errorf("%v: %v, want %v", c.hosts, got, c.want)
			}
		}
	}
	if got := DnsEdgeScopeWildcardsOfHosts([]string{"web.eng", "*"}, nil); len(got) != 0 {
		t.Errorf("a state that lists no scope: %v", got)
	}
}

// What a state publishes reads as grants: a name past its end at now publishes nothing, a scope host sets its scope's
// * and not the zone's, and a state with no scopes gives no scope grant.
func TestDnsEdgeZoneGrantsOfDropsWhatEnded(t *testing.T) {
	now := server.NowUtc()
	published := &DnsEdgePublished{
		Names: []*sidecar.AclPublishedName{
			{Host: "web", Port: 443, ExpiresMs: now.UnixMilli()},
			{Host: "shop", Port: 443, ExpiresMs: now.Add(time.Hour).UnixMilli()},
			{Host: "web.eng", Port: 443, ExpiresMs: now.Add(-time.Second).UnixMilli()},
			{Host: "api.ops", Port: 8443},
			nil,
		},
		ScopeLabels: []string{"eng", "ops"},
	}
	grants := DnsEdgeZoneGrantsOf(published, now, nil)
	if grants.Network != (DnsEdgeGrants{Wildcard: true}) || grants.ScopeWildcards["eng"] || !grants.ScopeWildcards["ops"] {
		t.Errorf("grants %+v", grants)
	}
	ended := DnsEdgeZoneGrantsOf(published, now.Add(time.Hour), nil)
	if ended.Network != (DnsEdgeGrants{}) || ended.ScopeWildcards["eng"] || !ended.ScopeWildcards["ops"] {
		t.Errorf("an hour on, grants %+v", ended)
	}
	if none := DnsEdgeZoneGrantsOf(nil, now, nil); none.Network != (DnsEdgeGrants{}) || len(none.ScopeWildcards) != 0 {
		t.Errorf("nothing published: %+v", none)
	}
}

// A Team scope's zone, through the edge's answer path: once a host of it is published the scope serves its * A and
// AAAA, its mail host with the router interfaces and its _mta-sts, its apex no address; a scope that publishes nothing
// has its set and mail but no address; neither the network's own * nor a scope the state does not list takes
// anything; the same grants again write nothing; the grant's end takes the scope's addresses and leaves its mail; and
// a bare * publishes every scope's hosts. No scope row is an entry.
func TestSyncDnsEdgeZonesServesATeamScopesEdgeAndMail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		scopes := []string{"eng", "ops"}
		engApex := "eng." + apex
		opsApex := "ops." + apex

		synced, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes, "web.eng", "web.dev"))
		if err != nil || synced.WriteCount != 2 || synced.RefusedScopeCount != 0 || synced.WaitingScopeCount != 0 {
			t.Fatalf("the first sync did %+v, %v", synced, err)
		}
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web."+engApex, true)
		requireDnsEdgeAddresses(t, view, healthy, "any.thing."+engApex, true)
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+engApex)
		requireDnsViewAnswer(t, view, healthy, "maple-1234."+engApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+engApex)
		requireDnsViewAnswer(t, view, healthy, "mx."+engApex, dns.TypeA, dns.RcodeSuccess, "192.0.2.2")
		requireDnsViewAnswer(t, view, healthy, "mx."+engApex, dns.TypeAAAA, dns.RcodeSuccess, "2001:db8::2")
		requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+engApex, dns.TypeTXT, dns.RcodeSuccess, testDnsMtaStsValue(engApex))
		// no grant spells a scope's @
		requireDnsEdgeAddresses(t, view, healthy, engApex, false)
		// ops publishes nothing: its set and its mail, no address
		requireDnsViewAnswer(t, view, healthy, opsApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+opsApex)
		requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+opsApex, dns.TypeTXT, dns.RcodeSuccess, testDnsMtaStsValue(opsApex))
		requireDnsEdgeAddresses(t, view, healthy, "web."+opsApex, false)
		// a scope host is its scope's, never the zone's *; dev, which the state does not list, takes no set
		requireDnsEdgeAddresses(t, view, healthy, "web."+apex, false)
		if lines := testDnsZoneLinesBelow(t, ctx, root, networkId, "dev."+apex); len(lines) != 0 {
			t.Errorf("a scope the state does not list holds %v", lines)
		}

		serial := testDnsRootSerial(t, ctx, root)
		synced, err = SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes, "web.eng", "web.dev"))
		if err != nil || synced.WriteCount != 0 {
			t.Fatalf("the same grants did %+v, %v", synced, err)
		}
		requireDnsSerial(t, ctx, root, serial)

		synced, err = SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes))
		if err != nil || synced.WriteCount != 1 {
			t.Fatalf("the grant's end did %+v, %v", synced, err)
		}
		view, healthy = testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web."+engApex, false)
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+engApex)
		requireDnsViewAnswer(t, view, healthy, "mx."+engApex, dns.TypeA, dns.RcodeSuccess, "192.0.2.2")
		requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+engApex, dns.TypeTXT, dns.RcodeSuccess, testDnsMtaStsValue(engApex))

		// dns:* takes every host, a scope's too: the zone's * and both scopes' in three writes
		synced, err = SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes, "*"))
		if err != nil || synced.WriteCount != 3 {
			t.Fatalf("a bare * did %+v, %v", synced, err)
		}
		view, healthy = testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web."+apex, true)
		requireDnsEdgeAddresses(t, view, healthy, "web."+engApex, true)
		requireDnsEdgeAddresses(t, view, healthy, "web."+opsApex, true)
		if count := DnsEntryCount(ctx, networkId); count != 0 {
			t.Errorf("%d entries", count)
		}
	})
}

// A network with no zone gets none while nothing is published, its scopes listed or not; a scope host's first
// publish draws the label and the zone, its * at the zone's flags left off, and the scope's set with its addresses.
func TestSyncDnsEdgeZonesDrawsTheZoneAtAScopesFirstPublish(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId := testDnsEdgeNetwork(ctx)
		scopes := []string{"eng"}
		synced, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes))
		if err != nil || synced.WriteCount != 0 {
			t.Fatalf("no grant did %+v, %v", synced, err)
		}
		if label := GetNetworkLabel(ctx, networkId, root); label != "" || testDnsEdgeState(ctx, root, networkId) != nil {
			t.Fatalf("no grant drew label %q", label)
		}
		requireDnsSerial(t, ctx, root, 0)

		synced, err = SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes, "web.eng"))
		if err != nil || synced.WriteCount != 2 {
			t.Fatalf("a scope's first publish did %+v, %v", synced, err)
		}
		label := GetNetworkLabel(ctx, networkId, root)
		if label == "" {
			t.Fatal("a scope's first publish drew no label")
		}
		if state := testDnsEdgeState(ctx, root, networkId); state == nil || state.EdgeWildcard || state.EdgeApex || state.Mail {
			t.Fatalf("the drawn zone reads %+v", state)
		}
		apex := label + "." + root
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web.eng."+apex, true)
		requireDnsEdgeAddresses(t, view, healthy, "web."+apex, false)
		// mail is off in the zone, so in its scope
		requireDnsViewAnswer(t, view, healthy, "eng."+apex, dns.TypeMX, dns.RcodeSuccess, "0 .")
	})
}

// A scope label the zone already holds as a host is refused by §12.5's rule, counted, and written at no sync; every
// other scope is written; and the host's own record stands.
func TestSyncDnsEdgeZonesCountsAScopeTheZoneHoldsAsAHost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		// written before eng was declared, so no rule kept it out
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("eng."+apex, "own")}}); err != nil {
			t.Fatal(err)
		}
		scopes := []string{"eng", "ops"}
		for range 2 {
			synced, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes, "web.eng", "web.ops"))
			if err != nil || synced.RefusedScopeCount != 1 {
				t.Fatalf("a held host did %+v, %v", synced, err)
			}
		}
		// the host's own TXT, with the copy of *'s records it takes (QD-1190), and nothing of a scope's
		want := testDnsWithCopyLines([]string{"eng." + apex + ` TXT user "own"`}, "eng."+apex)
		if lines := testDnsZoneLinesBelow(t, ctx, root, networkId, "eng."+apex); !slices.Equal(lines, want) {
			t.Errorf("under the refused scope:\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
		}
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web.ops."+apex, true)
	})
}

// Base 1 of world's network as its first device posts it, its rows declaring scopes, sorted, as Team scopes bound to
// the owner and rules as the network's acl, each rule's subject the owner's first device, signed again by the owner.
func testDnsEdgeBaseOne(t testing.TB, world *sidecartest.ConfigLogSuiteWorld, scopes []string, rules ...*protocol.AclRule) *ConfigLogBase {
	t.Helper()
	signedHeader, rowsBytes, genesis, configPushes := world.BaseOne()
	rows := &bridge.ProjectionRows{}
	signed := &bridge.Signed{}
	header := &bridge.ProjectionHeader{}
	for _, item := range []struct {
		data    []byte
		message proto.Message
	}{{rowsBytes, rows}, {signedHeader, signed}} {
		if err := proto.Unmarshal(item.data, item.message); err != nil {
			t.Fatal(err)
		}
	}
	if err := proto.Unmarshal(signed.GetBody(), header); err != nil {
		t.Fatal(err)
	}
	marshal := func(message proto.Message) []byte {
		t.Helper()
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	for _, scope := range scopes {
		rows.TeamScopes = append(rows.TeamScopes, &bridge.TeamScopeRow{Label: scope, MemberId: world.OwnerMemberId().Bytes()})
	}
	atom := sidecar.NewId().Bytes()
	for _, rule := range rules {
		rule.SrcAtoms = [][]byte{atom}
	}
	rows.Acl = marshal(&protocol.AclRows{
		HasDocument: true,
		Atoms:       []*protocol.AclAtom{{Atom: atom, SidecarIds: [][]byte{world.OwnerSidecarId().Bytes()}}},
		Rules:       rules,
	})
	body := marshal(rows)
	digest := sha256.Sum256(body)
	header.RowsSha256, header.RowsByteCount = digest[:], uint32(len(body))
	return &ConfigLogBase{
		SignedHeader: world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, header),
		Rows:         body,
		Genesis:      genesis,
		ConfigPushes: configPushes,
	}
}

// A rule publishing host on port, with its listen, to the public, ending at expires unless zero.
func testDnsEdgePublishRule(host string, port string, expires time.Time) *protocol.AclRule {
	rule := &protocol.AclRule{
		RuleId:   sidecar.NewId().Bytes(),
		Targets:  []*protocol.AclTarget{{PublicName: host}, {PublicName: "tls/" + port + "/" + host}},
		Actions:  []uint32{uint32(protocol.AclAction_ACL_ACTION_PUBLISH), uint32(protocol.AclAction_ACL_ACTION_LISTEN)},
		Audience: protocol.AclAudience_ACL_AUDIENCE_PUBLIC,
	}
	if !expires.IsZero() {
		rule.Expires = expires.UnixMilli()
	}
	return rule
}

// A network of the config log suite's over a fake cluster, with its network row, and a sequencer on its clock.
func testDnsEdgeLogWorld(t testing.TB, ctx context.Context) (*sidecartest.ConfigLogSuiteWorld, server.Id, *ConfigLogSequencer) {
	t.Helper()
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
	return world, networkId, NewConfigLogSequencer(cluster, settings, nil)
}

// api's sync reads the state its sequencer holds, at the sequencer's clock: a base declaring a Team scope and
// publishing a host of it, until an hour on, draws the network's zone and serves the scope's addresses and none of
// the zone's own; past the grant's end the next sync takes them away; and the same state again writes nothing.
func TestSyncDnsEdgeRecordsAtTipWritesTheSequencersScopes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		defer Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		world, networkId, sequencer := testDnsEdgeLogWorld(t, ctx)
		ends := world.Clock().Now().Add(time.Hour)
		base := testDnsEdgeBaseOne(t, world, []string{"eng"}, testDnsEdgePublishRule("web.eng", "443", ends))
		if _, err := sequencer.AppendBase(ctx, networkId, base); err != nil {
			t.Fatal(err)
		}
		SyncDnsEdgeRecordsAtTip(ctx, sequencer, networkId)
		label := GetNetworkLabel(ctx, networkId, root)
		if label == "" {
			t.Fatal("the scope's first publish drew no label")
		}
		apex := label + "." + root
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web.eng."+apex, true)
		requireDnsEdgeAddresses(t, view, healthy, "web."+apex, false)

		world.Clock().Advance(2 * time.Hour)
		SyncDnsEdgeRecordsAtTip(ctx, sequencer, networkId)
		view, healthy = testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web.eng."+apex, false)
		requireDnsViewAnswer(t, view, healthy, "eng."+apex, dns.TypeMX, dns.RcodeSuccess, "0 .")
		serial := testDnsRootSerial(t, ctx, root)
		SyncDnsEdgeRecordsAtTip(ctx, sequencer, networkId)
		requireDnsSerial(t, ctx, root, serial)
	})
}

// The hourly pass over Team scopes, from its source: a scope host's first publish draws the zone and writes the
// scope's addresses, a scope whose grant ended loses them, a scope the zone holds as a host is counted at each pass,
// and a second pass writes nothing more.
func TestReconcileDnsEdgeRecordsWritesTeamScopes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		defer Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		now := server.NowUtc()
		scopes := []string{"eng"}
		publishing := testDnsEdgeNetworkWithLog(ctx)
		ending := testDnsEdgeNetworkWithLog(ctx)
		if _, err := SyncDnsEdgeZones(ctx, ending, root, testDnsEdgeZoneGrants(scopes, "web.eng")); err != nil {
			t.Fatal(err)
		}
		refused := testDnsEdgeNetworkWithLog(ctx)
		refusedLabel, err := EnsureNetworkLabel(ctx, refused, root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: refused, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("eng."+refusedLabel+"."+root, "own")}}); err != nil {
			t.Fatal(err)
		}
		networkIdNames := map[server.Id][]*sidecar.AclPublishedName{
			publishing: {{Host: "web.eng", Port: 443}},
			refused:    {{Host: "web.eng", Port: 443}},
		}
		defer SetDnsEdgePublishedSource(func(ctx context.Context, networkId server.Id, sourceNow time.Time) (*DnsEdgePublished, error) {
			return &DnsEdgePublished{Names: networkIdNames[networkId], ScopeLabels: scopes}, nil
		})()

		result, err := ReconcileDnsEdgeRecords(ctx, now, nil, DefaultDnsEdgeRecordsReconcileSettings())
		if err != nil || result.NetworkCount != 3 || result.WriteCount != 3 || result.RefusedScopeCount != 1 || result.FailedCount != 0 || result.WaitingCount != 0 {
			t.Fatalf("the pass did %+v, %v", result, err)
		}
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, "web.eng."+GetNetworkLabel(ctx, publishing, root)+"."+root, true)
		requireDnsEdgeAddresses(t, view, healthy, "web.eng."+GetNetworkLabel(ctx, ending, root)+"."+root, false)

		serial := testDnsRootSerial(t, ctx, root)
		result, err = ReconcileDnsEdgeRecords(ctx, now, nil, DefaultDnsEdgeRecordsReconcileSettings())
		if err != nil || result.WriteCount != 0 || result.RefusedScopeCount != 1 {
			t.Fatalf("a second pass did %+v, %v", result, err)
		}
		requireDnsSerial(t, ctx, root, serial)
	})
}

// A certified alias answers the edge through its label's rows: claimed alone it answers no address; certified, its
// hosts under * and its @, a scope's hosts and the system publish's host answer the healthy edge, and the scope's
// mail host is the alias's own; once the grants end every one of them loses its addresses at the next sync.
func TestSyncDnsEdgeZonesReachACertifiedAlias(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world, name := newTestDnsAliasWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
			t.Fatal(err)
		}
		if _, err := world.claimAlias(ctx, name); err != nil {
			t.Fatal(err)
		}
		alias := name + "." + world.root
		scopes := []string{"eng"}
		if _, err := SyncDnsEdgeZones(ctx, world.networkId, world.root, testDnsEdgeZoneGrants(scopes, "web", "@", "web.eng")); err != nil {
			t.Fatal(err)
		}
		view, healthy := testDnsEdgeView(t, ctx, world.root)
		requireDnsEdgeAddresses(t, view, healthy, "web."+world.apex, true)
		requireDnsViewAnswer(t, view, healthy, "web."+alias, dns.TypeA, dns.RcodeNameError)

		if _, err := DnsAliasCertified(ctx, world.root, world.networkId, name, server.NowUtc()); err != nil {
			t.Fatal(err)
		}
		view, healthy = testDnsEdgeView(t, ctx, world.root)
		for _, host := range []string{"web." + alias, alias, "any.deeper." + alias, "x.eng." + alias, dnsEdgeSystemPublishHost + "." + alias} {
			requireDnsEdgeAddresses(t, view, healthy, host, true)
		}
		requireDnsViewAnswer(t, view, healthy, "eng."+alias, dns.TypeMX, dns.RcodeSuccess, "10 mx.eng."+alias)
		requireDnsViewAnswer(t, view, healthy, "mx.eng."+alias, dns.TypeA, dns.RcodeSuccess, "192.0.2.2")

		if _, err := SyncDnsEdgeZones(ctx, world.networkId, world.root, testDnsEdgeZoneGrants(scopes)); err != nil {
			t.Fatal(err)
		}
		view, healthy = testDnsEdgeView(t, ctx, world.root)
		for _, host := range []string{"web." + alias, alias, "x.eng." + alias} {
			requireDnsEdgeAddresses(t, view, healthy, host, false)
		}
		requireDnsViewAnswer(t, view, healthy, "eng."+alias, dns.TypeMX, dns.RcodeSuccess, "10 mx.eng."+alias)
	})
}

// The mail toggle turns every written scope with its zone, both ways, in its own transaction: mail on gives each
// scope its mail host, b and _mta-sts, keeping its addresses; mail off takes them back to the null MX.
func TestDnsSetMailRecordsTurnsEveryWrittenScope(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		if _, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants([]string{"eng", "ops"}, "web.eng")); err != nil {
			t.Fatal(err)
		}
		engApex := "eng." + apex
		opsApex := "ops." + apex
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess, "0 .")

		before := testDnsRootSerial(t, ctx, root)
		on, err := DnsSetMailRecords(ctx, networkId, root, true)
		if err != nil || on == nil || on.Serial != before+3 {
			t.Fatalf("mail on wrote %+v, %v", on, err)
		}
		view, healthy = testDnsEdgeView(t, ctx, root)
		for _, scopeApex := range []string{engApex, opsApex} {
			requireDnsViewAnswer(t, view, healthy, scopeApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+scopeApex)
			requireDnsViewAnswer(t, view, healthy, "mx."+scopeApex, dns.TypeA, dns.RcodeSuccess, "192.0.2.2")
			requireDnsViewAnswer(t, view, healthy, "b."+scopeApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+scopeApex)
			requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+scopeApex, dns.TypeTXT, dns.RcodeSuccess, testDnsMtaStsValue(scopeApex))
		}
		requireDnsEdgeAddresses(t, view, healthy, "web."+engApex, true)
		// the sync finds the scopes' mail at the zone's, and writes nothing
		serial := testDnsRootSerial(t, ctx, root)
		if synced, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants([]string{"eng", "ops"}, "web.eng")); err != nil || synced.WriteCount != 0 {
			t.Fatalf("a sync after mail on did %+v, %v", synced, err)
		}
		requireDnsSerial(t, ctx, root, serial)

		if _, err := DnsSetMailRecords(ctx, networkId, root, false); err != nil {
			t.Fatal(err)
		}
		view, healthy = testDnsEdgeView(t, ctx, root)
		for _, scopeApex := range []string{engApex, opsApex} {
			requireDnsViewAnswer(t, view, healthy, scopeApex, dns.TypeMX, dns.RcodeSuccess, "0 .")
			requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+scopeApex, dns.TypeTXT, dns.RcodeSuccess)
			// no mail host of its own: mx.<scope> is any name under the scope's *
			for _, line := range testDnsZoneLinesBelow(t, ctx, root, networkId, scopeApex) {
				if strings.HasPrefix(line, "mx."+scopeApex+" ") || strings.HasPrefix(line, "b."+scopeApex+" ") {
					t.Errorf("mail off left %s", line)
				}
			}
		}
		requireDnsEdgeAddresses(t, view, healthy, "web."+engApex, true)
	})
}

// A scope a host suspension withholds takes no write from the mail toggle, which turns every other scope and does not
// fail for it; once the suspension lifts, the next sync brings the scope's mail to the zone's.
func TestDnsSetMailRecordsLeavesAWithheldScopeToTheSync(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		scopes := []string{"eng", "ops"}
		if _, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes)); err != nil {
			t.Fatal(err)
		}
		if err := SuspendHost(ctx, networkId, "eng"); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatalf("mail on with a withheld scope: %v", err)
		}
		engApex := "eng." + apex
		opsApex := "ops." + apex
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsViewAnswer(t, view, healthy, opsApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+opsApex)
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess)
		// the sync waits on the suspension as well
		if synced, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes)); err != nil || synced.WriteCount != 0 || synced.WaitingScopeCount != 1 {
			t.Fatalf("a sync under the suspension did %+v, %v", synced, err)
		}

		if err := Restore(ctx, networkId, "eng"); err != nil {
			t.Fatal(err)
		}
		view, healthy = testDnsEdgeView(t, ctx, root)
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess, "0 .")
		if synced, err := SyncDnsEdgeZones(ctx, networkId, root, testDnsEdgeZoneGrants(scopes)); err != nil || synced.WriteCount != 1 || synced.WaitingScopeCount != 0 {
			t.Fatalf("the sync after the lift did %+v, %v", synced, err)
		}
		view, healthy = testDnsEdgeView(t, ctx, root)
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+engApex)
	})
}

// POST /mail/enable's write syncs the edge records once it commits: a state whose only grant is the system publish,
// a receiver's smtp/25/mx listen, gets no zone before mail; the enable draws the zone and, at once, mta-sts.<net>
// answers the edge through *, _mta-sts stands, and the state's Team scope has its set with mail on.
func TestEnableMailWritesTheSystemPublishAndScopeMailAtOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		defer Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		world, networkId, sequencer := testDnsEdgeLogWorld(t, ctx)
		receiverListen := &protocol.AclRule{
			RuleId:  sidecar.NewId().Bytes(),
			Targets: []*protocol.AclTarget{{PublicName: "smtp/25/mx"}},
			Actions: []uint32{uint32(protocol.AclAction_ACL_ACTION_LISTEN)},
		}
		if _, err := sequencer.AppendBase(ctx, networkId, testDnsEdgeBaseOne(t, world, []string{"eng"}, receiverListen)); err != nil {
			t.Fatal(err)
		}
		SyncDnsEdgeRecordsAtTip(ctx, sequencer, networkId)
		if label := GetNetworkLabel(ctx, networkId, root); label != "" {
			t.Fatalf("the system publish with mail off drew label %q", label)
		}

		SetConfigLogSequencer(sequencer)
		defer SetConfigLogSequencer(nil)
		if _, err := EnableMail(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		apex := GetNetworkLabel(ctx, networkId, root) + "." + root
		view, healthy := testDnsEdgeView(t, ctx, root)
		requireDnsEdgeAddresses(t, view, healthy, dnsEdgeSystemPublishHost+"."+apex, true)
		requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+apex, dns.TypeTXT, dns.RcodeSuccess, testDnsMtaStsValue(apex))
		engApex := "eng." + apex
		requireDnsViewAnswer(t, view, healthy, engApex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+engApex)
		requireDnsViewAnswer(t, view, healthy, dnsMtaStsLabel+"."+engApex, dns.TypeTXT, dns.RcodeSuccess, testDnsMtaStsValue(engApex))
		requireDnsEdgeAddresses(t, view, healthy, "web."+engApex, false)
	})
}

// A source that fails for a network stops the pass before any scope of it is written, and the error is the source's.
func TestReconcileDnsEdgeRecordsStopsAtAScopeStateItCannotRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		defer Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		networkIds := []server.Id{testDnsEdgeNetworkWithLog(ctx), testDnsEdgeNetworkWithLog(ctx)}
		slices.SortFunc(networkIds, func(a server.Id, b server.Id) int {
			return bytes.Compare(a[:], b[:])
		})
		unread := errors.New("the gate cache holds no verified state of the network")
		defer SetDnsEdgePublishedSource(func(ctx context.Context, networkId server.Id, sourceNow time.Time) (*DnsEdgePublished, error) {
			if networkId == networkIds[1] {
				return nil, unread
			}
			return &DnsEdgePublished{Names: []*sidecar.AclPublishedName{{Host: "web.eng", Port: 443}}, ScopeLabels: []string{"eng"}}, nil
		})()
		if result, err := ReconcileDnsEdgeRecords(ctx, server.NowUtc(), nil, DefaultDnsEdgeRecordsReconcileSettings()); !errors.Is(err, unread) {
			t.Fatalf("a failing source answered %+v, %v", result, err)
		}
		if label := GetNetworkLabel(ctx, networkIds[1], root); label != "" {
			t.Errorf("the unread network drew label %q", label)
		}
	})
}
