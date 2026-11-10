//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"
	"sync"
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

// The rows root serves now at or below apex, as dnsSystemRecordLines renders them.
func testDnsZoneLines(t testing.TB, ctx context.Context, root string, apex string) []string {
	t.Helper()
	sets := []dnsservice.DnsRecordSet{}
	for _, set := range testDnsSnapshotRows(t, ctx, root) {
		if dns.IsSubDomain(apex, set.Owner) {
			sets = append(sets, set)
		}
	}
	return dnsSystemRecordLines(sets)
}

// dnsSystemRecordLines' goldens for abcdefghij.zone.example., moved to apex in root.
func testDnsGoldenLines(lines []string, root string, apex string) []string {
	moved := []string{}
	for _, line := range lines {
		line = strings.ReplaceAll(line, "abcdefghij.zone.example.", apex)
		line = strings.ReplaceAll(line, "_spf.zone.example", "_spf."+strings.TrimSuffix(root, "."))
		moved = append(moved, line)
	}
	slices.Sort(moved)
	return moved
}

// Whether network_dns_zone says mail is on.
func testDnsZoneMail(t testing.TB, ctx context.Context, root string, networkId server.Id) bool {
	t.Helper()
	mail := false
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT mail FROM network_dns_zone WHERE root = $1 AND network_id = $2`, root, networkId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&mail))
			}
		})
	})
	return mail
}

// Mail on as a zone's first need allocates its label and writes M7's set in one journal row; on again
// writes nothing; off writes one row that takes mx and b away and puts the null MX back, M5's set
// exactly. No row of either set is a user's, so none counts as an entry.
func TestDnsSetMailRecordsTransitions(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "mail-"+server.NewId().String()[:8], server.NewId())
		on, err := DnsSetMailRecords(ctx, networkId, root, true)
		if err != nil {
			t.Fatal(err)
		}
		if on == nil || on.Serial != 1 {
			t.Fatalf("mail on %+v", on)
		}
		label, err := EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		apex := label + "." + root
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsGoldenLines(dnsTestMailOnLines, root, apex); !slices.Equal(got, want) {
			t.Fatalf("mail on serves\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if !testDnsZoneMail(t, ctx, root, networkId) {
			t.Fatal("network_dns_zone.mail is off")
		}
		for _, set := range testDnsSnapshotRows(t, ctx, root) {
			if set.Kind == dnsservice.DnsRecordKindUser {
				t.Errorf("%s %s is a user row, an entry", set.Owner, dns.TypeToString[set.Type])
			}
		}

		again, err := DnsSetMailRecords(ctx, networkId, root, true)
		if err != nil || again != nil {
			t.Fatalf("mail on again wrote %+v, %v", again, err)
		}
		requireDnsSerial(t, ctx, root, 1)

		off, err := DnsSetMailRecords(ctx, networkId, root, false)
		if err != nil {
			t.Fatal(err)
		}
		if off == nil || off.Serial != 2 {
			t.Fatalf("mail off %+v", off)
		}
		rows := testDnsJournal(t, ctx, root, 1)
		if len(rows) != 1 {
			t.Fatalf("mail off's rows %+v", rows)
		}
		deleted := []string{}
		for _, key := range rows[0].Deletes {
			deleted = append(deleted, key.Owner+" "+dns.TypeToString[key.Type])
		}
		slices.Sort(deleted)
		wantDeleted := []string{"b." + apex + " MX", "b." + apex + " TXT", "mx." + apex + " A", "mx." + apex + " AAAA"}
		if !slices.Equal(deleted, wantDeleted) {
			t.Errorf("mail off deleted %v, want %v", deleted, wantDeleted)
		}
		for _, set := range rows[0].Upserts {
			if set.Type != dns.TypeMX || !slices.Equal(set.Rdata, []string{"0 ."}) {
				t.Errorf("mail off wrote %+v", set)
			}
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsGoldenLines(dnsTestMailOffLines, root, apex); !slices.Equal(got, want) {
			t.Fatalf("mail off serves\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if testDnsZoneMail(t, ctx, root, networkId) {
			t.Fatal("network_dns_zone.mail is on")
		}
	})
}

// What one query of view answers at this healthy set: the rcode and each answer record's RDATA, sorted.
func testDnsViewAnswer(t testing.TB, view *dnsservice.ZoneView, healthy *dnsservice.HealthySet, name string, rrType uint16) (int, []string) {
	t.Helper()
	query := &dns.Msg{}
	query.SetQuestion(name, rrType)
	reply := dnsservice.Answer(&dnsservice.DnsQuery{Message: query, Tcp: true}, view, healthy)
	rdatas := []string{}
	for _, record := range reply.Answer {
		if record.Header().Rrtype == rrType {
			rdatas = append(rdatas, strings.TrimPrefix(record.String(), record.Header().String()))
		}
	}
	slices.Sort(rdatas)
	return reply.Rcode, rdatas
}

// S7's records (§22.7's receive-mail scenario, its dns half) as a sender's resolver meets them, through
// the edge's own answer path over the rows Postgres serves: the zone's MX and any burner label's, at
// any depth under *, name mx.<net>.<root>, which answers the router interfaces alone while * answers
// every healthy one; b answers its MX and an SPF whose include resolves; and abuse@<root> reaches the
// report host.
func TestDnsMailRecordsAnswerWhatReceivingMailNeeds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		settings := testDnsRootSettings(t, "")
		settings.Roots = []string{root}
		settings.RootRecords = map[string]*dnsservice.DnsRootRecordSettings{root: {ReportMx: "rpt.report.example"}}
		if err := DnsReconcileRootRecords(ctx, settings); err != nil {
			t.Fatal(err)
		}
		networkId, apex := testDnsNetwork(t, ctx, root)
		if _, err := DnsSyncEdgeRecords(ctx, networkId, root, true, true); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
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
		mailHost := "mx." + apex
		trimmedRoot := strings.TrimSuffix(root, ".")
		cases := []struct {
			name   string
			rrType uint16
			want   []string
		}{
			{name: apex, rrType: dns.TypeMX, want: []string{"10 " + mailHost}},
			{name: "swift-1234." + apex, rrType: dns.TypeMX, want: []string{"10 " + mailHost}},
			{name: "a.deeper.burner." + apex, rrType: dns.TypeMX, want: []string{"10 " + mailHost}},
			{name: mailHost, rrType: dns.TypeA, want: []string{"192.0.2.2"}},
			{name: mailHost, rrType: dns.TypeAAAA, want: []string{"2001:db8::2"}},
			{name: "swift-1234." + apex, rrType: dns.TypeA, want: []string{"192.0.2.1", "192.0.2.2"}},
			{name: apex, rrType: dns.TypeA, want: []string{"192.0.2.1", "192.0.2.2"}},
			{name: "b." + apex, rrType: dns.TypeMX, want: []string{"10 " + mailHost}},
			{name: "b." + apex, rrType: dns.TypeTXT, want: []string{`"v=spf1 include:_spf.` + trimmedRoot + ` -all"`}},
			{name: apex, rrType: dns.TypeTXT, want: []string{`"v=spf1 -all"`}},
			{name: "_dmarc." + apex, rrType: dns.TypeTXT, want: []string{`"` + dnsDmarcPolicy + `"`}},
			{name: "_spf." + root, rrType: dns.TypeTXT, want: []string{`"v=spf1 -all"`}},
			{name: root, rrType: dns.TypeMX, want: []string{"10 rpt.report.example."}},
		}
		for _, c := range cases {
			rcode, got := testDnsViewAnswer(t, view, healthy, c.name, c.rrType)
			if rcode != dns.RcodeSuccess || !slices.Equal(got, c.want) {
				t.Errorf("%s %s: %s %v, want %v", c.name, dns.TypeToString[c.rrType], dns.RcodeToString[rcode], got, c.want)
			}
		}
	})
}

// A suspended zone takes no mail records: the write is refused, no serial taken, and the flag rolls back
// with it, so Restore brings back the zone as it was and mail goes on only when asked again.
func TestDnsSetMailRecordsWaitsOutASuspension(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		if _, err := EnsureNetworkZone(ctx, networkId, root); err != nil {
			t.Fatal(err)
		}
		if err := SuspendZone(ctx, networkId, "abuse", nil); err != nil {
			t.Fatal(err)
		}
		state, err := GetDnsRootState(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); !errors.Is(err, ErrDnsZoneSuspended) {
			t.Fatalf("mail on while suspended: %v", err)
		}
		requireDnsSerial(t, ctx, root, state.Serial)
		if testDnsZoneMail(t, ctx, root, networkId) {
			t.Fatal("the refused write left mail on")
		}
		if err := Restore(ctx, networkId, ""); err != nil {
			t.Fatal(err)
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsGoldenLines(dnsTestMailOffLines, root, apex); !slices.Equal(got, want) {
			t.Fatalf("restored\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
		if _, err := DnsSetMailRecords(ctx, networkId, root, true); err != nil {
			t.Fatal(err)
		}
		if got, want := testDnsZoneLines(t, ctx, root, apex), testDnsGoldenLines(dnsTestMailOnLines, root, apex); !slices.Equal(got, want) {
			t.Fatalf("mail on after the lift\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
		}
	})
}

// Whether another transaction could take the zone's lock now: false while a writer holds it.
func testDnsZoneLockFree(t testing.TB, ctx context.Context, root string, networkId server.Id) bool {
	t.Helper()
	free := false
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`SELECT pg_try_advisory_xact_lock(hashtextextended('dns_zone:' || $1 || ':' || $2::uuid::text, 0))`,
			root,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&free))
			}
		})
	})
	return free
}

// Every writer of a zone's state takes the network's label lock before the zone's, as
// EnsureNetworkZoneInTx does, so no two of them wait on each other: held at its first label lock, each
// leaves the zone's lock free to another transaction.
func TestDnsZoneStateWritersTakeTheLabelLockFirst(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, _ := testDnsNetwork(t, ctx, root)
		t.Cleanup(func() { Testing_DnsLabelLockBarrier = nil })
		writers := []struct {
			name  string
			write func() error
		}{
			{name: "the zone", write: func() error { _, err := EnsureNetworkZone(ctx, networkId, root); return err }},
			{name: "edge records", write: func() error { _, err := DnsSyncEdgeRecords(ctx, networkId, root, true, false); return err }},
			{name: "mail records", write: func() error { _, err := DnsSetMailRecords(ctx, networkId, root, true); return err }},
		}
		for _, writer := range writers {
			atLabel := make(chan struct{})
			checked := make(chan struct{})
			var holdOnce sync.Once
			Testing_DnsLabelLockBarrier = func(barrierNetworkId server.Id) {
				if barrierNetworkId != networkId {
					return
				}
				holdOnce.Do(func() {
					close(atLabel)
					<-checked
				})
			}
			written := make(chan error, 1)
			go func() {
				written <- writer.write()
			}()
			select {
			case <-atLabel:
			case err := <-written:
				t.Fatalf("%s took no label lock: %v", writer.name, err)
			}
			free := testDnsZoneLockFree(t, ctx, root, networkId)
			close(checked)
			if err := <-written; err != nil {
				t.Fatalf("%s: %v", writer.name, err)
			}
			Testing_DnsLabelLockBarrier = nil
			if !free {
				t.Errorf("%s held the zone's lock as it took the label's", writer.name)
			}
		}
	})
}
