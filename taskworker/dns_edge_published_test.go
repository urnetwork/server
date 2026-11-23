package taskworker

// §12.7's edge-records pass over the runtime's gate cache (dns_edge_published.go): the source answers a loaded
// state's published names, a scope host's included, with the expiry PublishedNames reads; the pass over it writes a
// network's * from those names; a network the cache never loads fails the run rather than read as publishing nothing;
// and the newest runtime's source is the one installed, an older runtime's end removing none. No sleeps: a network
// that never loads fails at once with no wait.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The source reads the state the cache holds at now: every published name with its rule's end, and, past an end, no
// longer that name; and a network the cache does not load fails, once asked for.
func TestDnsEdgePublishedSourceReadsTheCachesState(t *testing.T) {
	ctx := t.Context()
	now := time.UnixMilli(server.NowUtc().UnixMilli()).UTC()
	ends := now.Add(time.Hour)
	networkId := sidecar.NewId()
	states := newTlsPublishedNamesTestStates()
	states.snapshots[networkId] = tlsPublishedNamesTestSnapshot(t, ends)
	source := newTlsPublishedNamesSource(states, &tlsPublishedNamesSourceSettings{})

	published, err := source.edgePublished(ctx, server.Id(networkId), now)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, name := range published.Names {
		got = append(got, name.Host+" "+time.UnixMilli(name.ExpiresMs).UTC().String())
	}
	want := []string{"blog " + time.UnixMilli(0).UTC().String(), "shop " + ends.String()}
	if !slices.Equal(got, want) || len(published.ScopeLabels) != 0 {
		t.Fatalf("published %v and scopes %v, want %v and none", got, published.ScopeLabels, want)
	}
	if published, err = source.edgePublished(ctx, server.Id(networkId), ends); err != nil || len(published.Names) != 1 || published.Names[0].Host != "blog" {
		t.Fatalf("at shop's end: %+v, %v", published, err)
	}

	unloaded := sidecar.NewId()
	if _, err := source.edgePublished(ctx, server.Id(unloaded), now); !errors.Is(err, errTlsPublishedNamesNotLoaded) {
		t.Fatalf("a network the cache never loads: %v", err)
	}
	if asked := states.askedNetworks(); !slices.Equal(asked, []sidecar.Id{unloaded}) {
		t.Fatalf("asked for %v", asked)
	}
}

// The pass reads the newest runtime's source, an older runtime's end removing none: a network whose loaded state
// publishes a host gets its zone and *; a network the cache never loads then fails the run; and with the newest
// runtime ended the pass answers unavailable.
func TestDnsEdgePublishedSourceFeedsTheEdgeRecordsPass(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
			return
		}
		ctx := context.Background()
		suffix := strings.ReplaceAll(server.NewId().String()[24:], "-", "")
		root := "edge" + suffix + ".example."
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		defer model.Testing_SetDnsUserRoot(strings.TrimSuffix(root, "."))()
		now := server.NowUtc()
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "edge-"+suffix, server.NewId())
		tlsPublishedNamesTestHead(ctx, networkId)
		states := newTlsPublishedNamesTestStates()
		states.snapshots[sidecar.Id(networkId)] = tlsPublishedNamesTestSnapshot(t, now.Add(time.Hour))
		// an older runtime's cache holds nothing, so its source would fail the run
		removeOlder := installDnsEdgePublishedSourceWithSettings(newTlsPublishedNamesTestStates(), &tlsPublishedNamesSourceSettings{})
		removeNewer := installDnsEdgePublishedSourceWithSettings(states, &tlsPublishedNamesSourceSettings{})
		removeOlder()

		result, err := model.ReconcileDnsEdgeRecords(ctx, now, nil, model.DefaultDnsEdgeRecordsReconcileSettings())
		if err != nil || result.NetworkCount != 1 || result.WriteCount != 1 || result.AfterNetworkId != nil {
			t.Fatalf("the pass did %+v, %v", result, err)
		}
		label := model.GetNetworkLabel(ctx, networkId, root)
		if label == "" {
			t.Fatal("a first publish drew no label")
		}
		snapshot, err := model.OpenDnsSnapshot(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		edge := map[uint16]bool{}
		for {
			set, err := snapshot.Next()
			if err != nil {
				break
			}
			if set.Owner == "*."+label+"."+root && set.Kind == dnsservice.DnsRecordKindEdge {
				edge[set.Type] = true
			}
		}
		snapshot.Close()
		if !edge[dns.TypeA] || !edge[dns.TypeAAAA] {
			t.Fatalf("the zone's * serves edge rows %v", edge)
		}

		unreadable := server.NewId()
		model.Testing_CreateNetwork(ctx, unreadable, "edge-"+strings.ReplaceAll(unreadable.String()[24:], "-", ""), server.NewId())
		tlsPublishedNamesTestHead(ctx, unreadable)
		if result, err := model.ReconcileDnsEdgeRecords(ctx, now, nil, model.DefaultDnsEdgeRecordsReconcileSettings()); !errors.Is(err, errTlsPublishedNamesNotLoaded) {
			t.Fatalf("a network the cache never loads: %+v, %v", result, err)
		}

		removeNewer()
		if _, err := model.ReconcileDnsEdgeRecords(ctx, now, nil, model.DefaultDnsEdgeRecordsReconcileSettings()); !errors.Is(err, model.ErrDnsEdgePublishedUnavailable) {
			t.Fatalf("with the newest runtime ended: %v", err)
		}
	})
}
