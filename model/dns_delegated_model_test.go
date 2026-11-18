//go:build sidecar_pending

package model

// A Team network's delegated _domainkey.<domain> zones (DnsSetDelegatedZonesInTx; DNS-M9-02): one row at the
// label apex through §12.6's one write path, the same set again no write, the edge's view answering a sending
// sidecar's selector at <sel>._domainkey.<domain> from its ordinary key row, a domain inside the root refused,
// no domain withdrawing the row, and GET /network/dns's RRsets never showing it.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// Runs DnsSetDelegatedZonesInTx in a transaction of its own and answers its result.
func testSetDelegatedZones(ctx context.Context, root string, networkId server.Id, domains ...string) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = DnsSetDelegatedZonesInTx(ctx, tx, root, networkId, domains)
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

func TestDnsSetDelegatedZonesWritesOneRowTheEdgeAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		sidecarId := server.NewId()
		if _, err := testDkimWrite(ctx, DnsPutDkimSelectorInTx, &DnsDkimSelector{
			Root: world.root, NetworkId: world.networkId, Selector: "s20261118a", Value: testDkimValue(392), SidecarId: &sidecarId,
		}); err != nil {
			t.Fatal(err)
		}

		before := world.serial(t, ctx)
		result, err := testSetDelegatedZones(ctx, world.root, world.networkId, "corp.example", "shop.example")
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || result.Serial != before+1 {
			t.Fatalf("the delegation wrote %+v", result)
		}
		// the same set again writes nothing, in either order
		if again, err := testSetDelegatedZones(ctx, world.root, world.networkId, "shop.example", "corp.example."); err != nil || again != nil {
			t.Fatalf("the same set again %+v %v", again, err)
		}

		view, _ := world.view(t, ctx)
		if got := view.Delegated().Apexes(); !slices.Equal(got, []string{"_domainkey.corp.example.", "_domainkey.shop.example."}) {
			t.Fatalf("delegated apexes %v", got)
		}
		name := "s20261118a._domainkey.corp.example."
		apex, labelApex, ok := view.Delegated().Lookup(name)
		if !ok || labelApex != world.apex {
			t.Fatalf("lookup %s %s %t", apex, labelApex, ok)
		}
		query := &dns.Msg{}
		query.SetQuestion(name, dns.TypeTXT)
		query.SetEdns0(1232, true)
		reply := dnsservice.AnswerDelegated(&dnsservice.DnsQuery{Message: query}, view, apex, labelApex)
		if reply.Rcode != dns.RcodeSuccess || len(reply.Answer) != 1 || !strings.HasPrefix(reply.Answer[0].(*dns.TXT).Txt[0], "v=DKIM1") {
			t.Fatalf("the selector under the delegated zone %v", reply)
		}

		// GET /network/dns's RRsets do not show the delegation
		zone, err := GetDnsZoneView(ctx, world.root, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		for _, set := range zone.RecordSets {
			if set.Kind == string(dnsservice.DnsRecordKindDelegated) {
				t.Errorf("the zone view shows %+v", set)
			}
		}

		// a domain inside the root, and one not a host name, are refused and write nothing
		beforeRefusals := world.serial(t, ctx)
		for _, domain := range []string{"corp." + world.root, "bad_label.example"} {
			if _, err := testSetDelegatedZones(ctx, world.root, world.networkId, domain); !errors.Is(err, dnsservice.ErrDnsDelegatedRefused) {
				t.Errorf("%s: %v", domain, err)
			}
		}
		if serial := world.serial(t, ctx); serial != beforeRefusals {
			t.Errorf("a refusal wrote: serial %d after %d", serial, beforeRefusals)
		}

		// no domain withdraws the row
		removed, err := testSetDelegatedZones(ctx, world.root, world.networkId)
		if err != nil || removed == nil {
			t.Fatalf("the withdrawal %+v %v", removed, err)
		}
		view, _ = world.view(t, ctx)
		if got := view.Delegated().Apexes(); len(got) != 0 {
			t.Errorf("after the withdrawal %v", got)
		}
	})
}
