//go:build sidecar_pending

package model

// §14.6's DKIM records (dns_dkim_model.go; DNS-M9-01, S10, FS-42's M9 shape): a sending sidecar's selector free of
// dns_entries with its admin event, written once; a long key cut into TXT strings; the refusals; S10 through the
// journal, a replying burner's first selector writing <blabel>'s copy in its own write, a second none and the last
// taking it away, its label answering *'s records through the copy, health drops included; a user write at a
// word-number owner refused; and a delete in the caller's transaction leaving no selector served.

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A DKIM key record of the given length of key bytes, in the shape RSA keys take.
func testDkimValue(keyLength int) string {
	return "v=DKIM1; k=rsa; p=" + strings.Repeat("A", keyLength)
}

// Runs write in a transaction of its own and answers its result.
func testDkimWrite(ctx context.Context, write func(ctx context.Context, tx server.PgTx, selector *DnsDkimSelector) (*DnsWriteResult, error), selector *DnsDkimSelector) (*DnsWriteResult, error) {
	var result *DnsWriteResult
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			result, returnErr = write(ctx, tx, selector)
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

// The owners a journal row changes under apex, relative, by upserts and deletes.
func testDkimRowOwners(row dnsservice.DnsJournalRow, apex string) (upserted []string, deleted []string) {
	for _, set := range row.Upserts {
		upserted = append(upserted, strings.TrimSuffix(set.Owner, "."+apex)+" "+string(set.Kind))
	}
	for _, key := range row.Deletes {
		deleted = append(deleted, strings.TrimSuffix(key.Owner, "."+apex))
	}
	slices.Sort(upserted)
	slices.Sort(deleted)
	return
}

// A sending sidecar's selector: one write with its admin event, a key cut into strings of at most 255 bytes,
// nothing counted against dns_entries, the same key again no write, and its deletion one write; the refusals.
func TestDnsDkimSelectorWrites(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		sidecarId := server.NewId()
		selector := &DnsDkimSelector{Root: world.root, NetworkId: world.networkId, Selector: "s20261115a", Value: testDkimValue(392), SidecarId: &sidecarId}
		before := world.serial(t, ctx)
		result, err := testDkimWrite(ctx, DnsPutDkimSelectorInTx, selector)
		if err != nil {
			t.Fatal(err)
		}
		if result == nil || result.Serial != before+1 {
			t.Fatalf("the put wrote %+v", result)
		}
		owner := "s20261115a._domainkey." + world.apex
		served := testDnsSnapshotRows(t, ctx, world.root)[dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeTXT}]
		if served.Kind != dnsservice.DnsRecordKindKey || served.Validate(world.root) != nil {
			t.Fatalf("the selector serves %+v", served)
		}
		records, err := served.Records()
		if err != nil || len(records) != 1 {
			t.Fatalf("the selector's records %v, %v", records, err)
		}
		txt := records[0].(*dns.TXT)
		if len(txt.Txt) != 2 || len(txt.Txt[0]) != 255 || strings.Join(txt.Txt, "") != testDkimValue(392) {
			t.Fatalf("the key's strings %d: %v", len(txt.Txt), txt.Txt)
		}
		if count := DnsEntryCount(ctx, world.networkId); count != 0 {
			t.Errorf("the selector counts %d entries", count)
		}
		if again, err := testDkimWrite(ctx, DnsPutDkimSelectorInTx, selector); err != nil || again != nil {
			t.Fatalf("the same key again wrote %+v, %v", again, err)
		}
		if deleted, err := testDkimWrite(ctx, DnsDeleteDkimSelectorInTx, selector); err != nil || deleted == nil {
			t.Fatalf("the delete wrote %+v, %v", deleted, err)
		}
		if _, held := testDnsSnapshotRows(t, ctx, world.root)[dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeTXT}]; held {
			t.Fatal("the deleted selector is served")
		}
		if again, err := testDkimWrite(ctx, DnsDeleteDkimSelectorInTx, selector); err != nil || again != nil {
			t.Fatalf("deleting again wrote %+v, %v", again, err)
		}
		actions := []string{}
		for _, event := range GetNetworkAdminEvents(ctx, world.networkId, 0, 0) {
			if strings.HasPrefix(event.Action, "dns.dkim") {
				actions = append(actions, event.Action)
				if event.TargetKind != AdminEventTargetSidecar || event.TargetId == nil || *event.TargetId != sidecarId {
					t.Errorf("an event names %s %v", event.TargetKind, event.TargetId)
				}
			}
		}
		if strings.Join(actions, ",") != AdminEventActionDnsDkimKey+","+AdminEventActionDnsDkimKeyDelete {
			t.Errorf("the key writes' events %v", actions)
		}

		for _, refused := range []*DnsDkimSelector{
			{Root: world.root, NetworkId: world.networkId, Selector: "Bad_Selector", Value: testDkimValue(64)},
			{Root: world.root, NetworkId: world.networkId, Selector: "s1", Value: "v=DKIM1; k=rsa"},
			{Root: world.root, NetworkId: world.networkId, Selector: "s1", Value: `v=DKIM1; p="quoted"`},
			{Root: world.root, NetworkId: world.networkId, Selector: "s1", Value: "k=rsa; v=DKIM1; p=AAAA"},
			{Root: world.root, NetworkId: world.networkId, Selector: "s1", Value: testDkimValue(4096)},
			{Root: world.root, NetworkId: world.networkId, Selector: "s1", BurnerLabel: "maple-1234", Value: testDkimValue(64)},
		} {
			if _, err := testDkimWrite(ctx, DnsPutDkimSelectorInTx, refused); !errors.Is(err, ErrDnsDkimRefused) {
				t.Errorf("%+v: %v", refused, err)
			}
		}
		for _, label := range []string{"notaburner", "maple-12345", "", "Maple-1234"} {
			refused := &DnsDkimSelector{Root: world.root, NetworkId: world.networkId, Selector: "s1", BurnerLabel: label, Value: testDkimValue(64)}
			if _, err := testDkimWrite(ctx, DnsPutBurnerSelectorInTx, refused); !errors.Is(err, ErrDnsDkimRefused) {
				t.Errorf("burner label %q: %v", label, err)
			}
		}
		// a user may not write at a word-number owner, which only these writers take
		_, err = world.write(ctx, testDnsUpsert("maple-1234."+world.apex, "TXT", `"mine"`))
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusBadRequest {
			t.Errorf("a user write at a word-number owner: %v", apiError)
		}
	})
}

// S10 through the journal: mail on, a replying burner's first selector writes itself and <blabel>'s copy, and
// _domainkey.<blabel>'s and its own, in one write (QD-1190); a second selector writes itself and its copy alone; the
// first one's removal writes its delete and its copy's alone; the last one's removal takes every copy in its own
// write, back to the wildcard alone. Through the edge's answer path, the label answers *'s MX and the healthy
// addresses, a health drop reaching it with no write, and the selector answers its key. A receive-only burner has
// no row at all.
func TestDnsBurnerSelectorsKeepTheLabelsMail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		if _, err := DnsSyncEdgeRecords(ctx, world.networkId, world.root, true, false); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetMailRecords(ctx, world.networkId, world.root, true); err != nil {
			t.Fatal(err)
		}
		selector := func(name string) *DnsDkimSelector {
			return &DnsDkimSelector{Root: world.root, NetworkId: world.networkId, Selector: name, BurnerLabel: "maple-1234", Value: testDkimValue(64)}
		}
		lastRow := func() dnsservice.DnsJournalRow {
			rows := testDnsJournal(t, ctx, world.root, world.serial(t, ctx)-1)
			if len(rows) != 1 {
				t.Fatalf("%d rows", len(rows))
			}
			return rows[0]
		}
		requireRow := func(wantUpserted []string, wantDeleted []string) {
			t.Helper()
			upserted, deleted := testDkimRowOwners(lastRow(), world.apex)
			if !slices.Equal(upserted, wantUpserted) || !slices.Equal(deleted, wantDeleted) {
				t.Fatalf("the write upserted %v and deleted %v, want %v and %v", upserted, deleted, wantUpserted, wantDeleted)
			}
		}
		if _, err := testDkimWrite(ctx, DnsPutBurnerSelectorInTx, selector("s1")); err != nil {
			t.Fatal(err)
		}
		requireRow([]string{"_domainkey.maple-1234 copy", "maple-1234 copy", "s1._domainkey.maple-1234 copy", "s1._domainkey.maple-1234 key"}, nil)

		view, _ := world.view(t, ctx)
		both := dnsservice.NewHealthySet([]dnsservice.DnsLbInterface{
			{Name: "lb-1", Ipv4: netip.MustParseAddr("192.0.2.1")},
			{Name: "lb-2", Ipv4: netip.MustParseAddr("192.0.2.2"), Router: true},
		}, []dnsservice.DnsLbInterface{{Name: "lb-2", Ipv4: netip.MustParseAddr("192.0.2.2"), Router: true}})
		dropped := dnsservice.NewHealthySet([]dnsservice.DnsLbInterface{
			{Name: "lb-2", Ipv4: netip.MustParseAddr("192.0.2.2"), Router: true},
		}, []dnsservice.DnsLbInterface{{Name: "lb-2", Ipv4: netip.MustParseAddr("192.0.2.2"), Router: true}})
		blabel := "maple-1234." + world.apex
		requireDnsViewAnswer(t, view, both, blabel, dns.TypeMX, dns.RcodeSuccess, "10 mx."+world.apex)
		requireDnsViewAnswer(t, view, both, blabel, dns.TypeA, dns.RcodeSuccess, "192.0.2.1", "192.0.2.2")
		requireDnsViewAnswer(t, view, dropped, blabel, dns.TypeA, dns.RcodeSuccess, "192.0.2.2")
		requireDnsViewAnswer(t, view, both, "s1._domainkey."+blabel, dns.TypeTXT, dns.RcodeSuccess, `"`+testDkimValue(64)+`"`)
		// a receive-only burner's label, with no selector, answers the same through * itself
		requireDnsViewAnswer(t, view, both, "cedar-0042."+world.apex, dns.TypeMX, dns.RcodeSuccess, "10 mx."+world.apex)

		if _, err := testDkimWrite(ctx, DnsPutBurnerSelectorInTx, selector("s2")); err != nil {
			t.Fatal(err)
		}
		requireRow([]string{"s2._domainkey.maple-1234 copy", "s2._domainkey.maple-1234 key"}, nil)
		if _, err := testDkimWrite(ctx, DnsDeleteBurnerSelectorInTx, selector("s1")); err != nil {
			t.Fatal(err)
		}
		requireRow(nil, []string{"s1._domainkey.maple-1234", "s1._domainkey.maple-1234"})
		if _, err := testDkimWrite(ctx, DnsDeleteBurnerSelectorInTx, selector("s2")); err != nil {
			t.Fatal(err)
		}
		requireRow(nil, []string{"_domainkey.maple-1234", "maple-1234", "s2._domainkey.maple-1234", "s2._domainkey.maple-1234"})
		for key := range testDnsSnapshotRows(t, ctx, world.root) {
			if strings.Contains(key.Owner, "maple-1234") || strings.Contains(key.Owner, "cedar-0042") {
				t.Errorf("a burner left %s %s", key.Owner, dns.TypeToString[key.Type])
			}
		}
		view, _ = world.view(t, ctx)
		requireDnsViewAnswer(t, view, both, blabel, dns.TypeMX, dns.RcodeSuccess, "10 mx."+world.apex)
	})
}

// FS-42's M9 shape, which SMTP-M9-19's step reads: a removal's delete in the caller's own transaction, beside its
// other work, leaves no selector served once that transaction commits, and nothing when it rolls back.
func TestDnsDkimDeleteInTheCallersTransaction(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		selector := &DnsDkimSelector{Root: world.root, NetworkId: world.networkId, Selector: "removed1", Value: testDkimValue(64)}
		if _, err := testDkimWrite(ctx, DnsPutDkimSelectorInTx, selector); err != nil {
			t.Fatal(err)
		}
		key := dnsservice.DnsRecordKey{Owner: "removed1._domainkey." + world.apex, Type: dns.TypeTXT}
		rolledBack := errors.New("the caller rolled back")
		err := dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				if _, err := DnsDeleteDkimSelectorInTx(ctx, tx, selector); err != nil {
					panic(dnsWriteRefusal{err: err})
				}
				panic(dnsWriteRefusal{err: rolledBack})
			}, server.TxReadCommitted)
		})
		if !errors.Is(err, rolledBack) {
			t.Fatalf("the rolled back transaction: %v", err)
		}
		if _, held := testDnsSnapshotRows(t, ctx, world.root)[key]; !held {
			t.Fatal("a rolled back delete took the selector")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, err := DnsDeleteDkimSelectorInTx(ctx, tx, selector)
			server.Raise(err)
		}, server.TxReadCommitted)
		if _, held := testDnsSnapshotRows(t, ctx, world.root)[key]; held {
			t.Fatal("the committed delete left the selector served")
		}
	})
}
