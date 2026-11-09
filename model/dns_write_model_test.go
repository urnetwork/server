//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A fresh root for one test, under .example.
func testDnsRoot(t testing.TB, ctx context.Context) string {
	t.Helper()
	id := server.NewId().String()
	root := fmt.Sprintf("t%s.example.", strings.ReplaceAll(id[len(id)-12:], "-", ""))
	if err := EnsureDnsRoot(ctx, root); err != nil {
		t.Fatal(err)
	}
	return root
}

// A network with its label in root, and the label's zone apex.
func testDnsNetwork(t testing.TB, ctx context.Context, root string) (server.Id, string) {
	t.Helper()
	networkId := server.NewId()
	Testing_CreateNetwork(ctx, networkId, "dns-"+strings.ReplaceAll(networkId.String()[24:], "-", ""), server.NewId())
	label, err := EnsureNetworkLabel(ctx, networkId, root)
	if err != nil {
		t.Fatal(err)
	}
	return networkId, label + "." + root
}

// One TXT at owner.
func testDnsTxt(owner string, text string) dnsservice.DnsRecordSet {
	return dnsservice.DnsRecordSet{Owner: owner, Type: dns.TypeTXT, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{`"` + text + `"`}}
}

func requireDnsSerial(t testing.TB, ctx context.Context, root string, want uint64) {
	t.Helper()
	state, err := GetDnsRootState(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if state.Serial != want {
		t.Fatalf("serial %d, want %d", state.Serial, want)
	}
}

// Every journal row of root above after.
func testDnsJournal(t testing.TB, ctx context.Context, root string, after uint64) []dnsservice.DnsJournalRow {
	t.Helper()
	read, err := GetDnsJournal(ctx, root, after, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return read.Rows
}

// Two writers in one root serialize on dns_root's row lock: the second waits while the first holds its
// serial uncommitted, and takes the next one.
func TestDnsWritersInOneRootTakeConsecutiveSerials(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		firstNetworkId, firstApex := testDnsNetwork(t, ctx, root)
		secondNetworkId, secondApex := testDnsNetwork(t, ctx, root)

		held := make(chan struct{})
		release := make(chan struct{})
		secondBefore := make(chan struct{})
		var holdOnce sync.Once
		var stateLock sync.Mutex
		beforeCount := 0
		Testing_DnsWriteBarrier = func(name string, barrierRoot string) {
			if barrierRoot != root {
				return
			}
			switch name {
			case DnsWriteBarrier:
				holdOnce.Do(func() {
					close(held)
					<-release
				})
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

		results := make(chan *DnsWriteResult, 2)
		errs := make(chan error, 2)
		go func() {
			result, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: firstNetworkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+firstApex, "first")}})
			results <- result
			errs <- err
		}()
		<-held
		secondDone := make(chan struct{})
		go func() {
			defer close(secondDone)
			result, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: secondNetworkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+secondApex, "second")}})
			results <- result
			errs <- err
		}()
		// the second reaches the row lock while the first holds serial 1 uncommitted: had it not waited
		// there, it would read serial 0 too and its journal row would collide
		<-secondBefore
		close(release)
		<-secondDone
		serials := map[uint64]bool{}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			serials[(<-results).Serial] = true
		}
		if !serials[1] || !serials[2] {
			t.Fatalf("serials %v", serials)
		}
		if rows := testDnsJournal(t, ctx, root, 0); len(rows) != 2 || rows[0].Serial != 1 || rows[1].Serial != 2 {
			t.Fatalf("journal %+v", rows)
		}
	})
}

// A crash between the commit and the raise leaves the journal readable and the key behind it, which
// the feed's reread at its hold's end catches.
func TestDnsWriteCommitsBeforeTheRaise(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)

		atRaise := make(chan struct{})
		release := make(chan struct{})
		Testing_DnsWriteBarrier = func(name string, barrierRoot string) {
			if name == DnsWriteBarrierAfter && barrierRoot == root {
				close(atRaise)
				<-release
			}
		}
		defer func() { Testing_DnsWriteBarrier = nil }()

		done := make(chan error, 1)
		go func() {
			_, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+apex, "one")}})
			done <- err
		}()
		<-atRaise
		if rows := testDnsJournal(t, ctx, root, 0); len(rows) != 1 {
			t.Fatalf("the committed row reads as %+v", rows)
		}
		raised, err := GetDnsRaisedSerial(ctx, nil, root)
		if err != nil {
			t.Fatal(err)
		}
		if raised != 0 {
			t.Fatalf("the key holds %d before the raise", raised)
		}
		close(release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if raised, _ := GetDnsRaisedSerial(ctx, nil, root); raised != 1 {
			t.Fatalf("the key holds %d after the raise", raised)
		}
	})
}

// The raise script never lowers the key, compares values past 2^53 by their digits, and refuses what is
// no serial.
func TestDnsRaiseScriptNeverLowers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := "raise-" + server.NewId().String() + ".example."
		steps := []struct {
			raise uint64
			want  uint64
		}{
			{raise: 5, want: 5},
			{raise: 4, want: 5},
			{raise: 1<<53 + 1, want: 1<<53 + 1},
			// past 2^53 a double cannot tell these apart, and the script's runs of 10 digits can
			{raise: 1<<53 + 2, want: 1<<53 + 2},
			{raise: 1<<53 + 1, want: 1<<53 + 2},
			{raise: 18446744073709551615, want: 18446744073709551615},
			{raise: 18446744073709551614, want: 18446744073709551615},
		}
		for _, step := range steps {
			got, err := RaiseDnsSerial(ctx, nil, root, step.raise)
			if err != nil {
				t.Fatal(err)
			}
			if got != step.want {
				t.Fatalf("raise %d holds %d, want %d", step.raise, got, step.want)
			}
		}
		if _, err := dnsDefaultRedis().RunScript(ctx, DnsRaiseSerialScript, []string{DnsSerialKey(root)}, []string{"012"}); err == nil {
			t.Fatal("a serial with a leading zero was raised")
		}
	})
}

// Every key of the family shares one hash tag, so no call spans two slots (§22.4's CROSSSLOT lint).
func TestDnsSerialKeysShareOneTag(t *testing.T) {
	keys := []string{DnsSerialKey("a.example."), DnsSerialKey("b.example.")}
	if !sidecar.RedisKeysShareTag(keys) {
		t.Fatalf("%v share no tag", keys)
	}
	if root, ok := ParseDnsSerialKey(keys[0]); !ok || root != "a.example." {
		t.Fatalf("parsed %q %v", root, ok)
	}
}

// A refusal anywhere in the path rolls the whole write back: the gate's own work, the rows and the
// serial.
func TestDnsLintRefusalRollsTheWriteBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("www."+apex, "one")}}); err != nil {
			t.Fatal(err)
		}
		gateRan := false
		_, err := DnsWrite(ctx, &DnsWriteArgs{
			Root:      root,
			NetworkId: networkId,
			Upserts: []dnsservice.DnsRecordSet{{
				Owner: "www." + apex, Type: dns.TypeCNAME, Ttl: 300, Kind: dnsservice.DnsRecordKindUser,
				Rdata: []string{"target.example.net."},
			}},
			Gate: func(ctx context.Context, tx server.PgTx) (bool, error) {
				gateRan = true
				server.RaisePgResult(tx.Exec(ctx, `UPDATE dns_root SET custom_host_map_version = 77 WHERE root = $1`, root))
				return true, nil
			},
		})
		if !errors.Is(err, dnsservice.ErrDnsZoneLint) {
			t.Fatalf("a CNAME beside a TXT answered %v", err)
		}
		if !gateRan {
			t.Fatal("the gate did not run first")
		}
		requireDnsSerial(t, ctx, root, 1)
		var mapVersion int64
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT custom_host_map_version FROM dns_root WHERE root = $1`, root).Scan(&mapVersion))
		})
		if mapVersion != 0 {
			t.Fatal("the gate's work outlived the refusal")
		}
	})
}

// A gate that answers from a stored result writes nothing and takes no serial.
func TestDnsGateAnswerWritesNothing(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		result, err := DnsWrite(ctx, &DnsWriteArgs{
			Root:      root,
			NetworkId: networkId,
			Upserts:   []dnsservice.DnsRecordSet{testDnsTxt("a."+apex, "one")},
			Gate: func(ctx context.Context, tx server.PgTx) (bool, error) {
				return false, nil
			},
		})
		if err != nil || !result.Answered || result.Serial != 0 {
			t.Fatalf("result %+v, err %v", result, err)
		}
		requireDnsSerial(t, ctx, root, 0)
	})
}

// While restoring every write answers ErrDnsRestoring, which the routes answer 503, and takes no
// serial.
func TestDnsWriteWhileRestoringTakesNoSerial(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		if err := SetDnsRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		_, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+apex, "one")}})
		if !errors.Is(err, dnsservice.ErrDnsRestoring) {
			t.Fatalf("a write while restoring answered %v", err)
		}
		requireDnsSerial(t, ctx, root, 0)
		if rows := testDnsJournal(t, ctx, root, 0); len(rows) != 0 {
			t.Fatalf("journal %+v", rows)
		}
	})
}

// The path keeps * answering at an empty non-terminal: a first child adds its copy row in the same
// journal row, and removing the child removes the copy (D-DNS-11).
func TestDnsWriteAddsAndDropsCopyRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		wildcard := dnsservice.DnsRecordSet{Owner: "*." + apex, Type: dns.TypeA, Ttl: 60, Kind: dnsservice.DnsRecordKindEdge}
		if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{wildcard}}); err != nil {
			t.Fatal(err)
		}
		srv := dnsservice.DnsRecordSet{Owner: "_sip._tcp.blog." + apex, Type: dns.TypeSRV, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{"0 0 5060 sip.example.net."}}
		added, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{srv}})
		if err != nil {
			t.Fatal(err)
		}
		// _tcp.blog and blog are both empty non-terminals now, each with its copy
		rows := testDnsJournal(t, ctx, root, added.Serial-1)
		copyOwners := []string{}
		for _, set := range rows[0].Upserts {
			if set.Kind == dnsservice.DnsRecordKindCopy {
				copyOwners = append(copyOwners, set.Owner)
			}
		}
		slices.Sort(copyOwners)
		if len(rows) != 1 || len(rows[0].Upserts) != 3 || !slices.Equal(copyOwners, []string{"_tcp.blog." + apex, "blog." + apex}) {
			t.Fatalf("the child's row %+v", rows)
		}
		removed, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Deletes: []dnsservice.DnsRecordKey{srv.Key()}})
		if err != nil {
			t.Fatal(err)
		}
		rows = testDnsJournal(t, ctx, root, removed.Serial-1)
		if len(rows) != 1 || len(rows[0].Deletes) != 3 {
			t.Fatalf("the removal's row %+v", rows)
		}
	})
}

// A write names owners in its own zone only, and its zone's version moves to its serial.
func TestDnsWriteStaysInItsZone(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		_, otherApex := testDnsNetwork(t, ctx, root)
		_, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+otherApex, "one")}})
		if !errors.Is(err, ErrDnsOwnerOutsideZone) {
			t.Fatalf("a write into another zone answered %v", err)
		}
		_, err = DnsWrite(ctx, &DnsWriteArgs{Root: root, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+apex, "one")}})
		if !errors.Is(err, ErrDnsOwnerOutsideZone) {
			t.Fatalf("a root write into a network's zone answered %v", err)
		}
		result, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+apex, "one")}})
		if err != nil {
			t.Fatal(err)
		}
		version, err := GetDnsZoneVersion(ctx, root, networkId)
		if err != nil || version != result.Serial {
			t.Fatalf("version %d, err %v, want %d", version, err, result.Serial)
		}
		stale := version - 1
		_, err = DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, ExpectVersion: &stale, Upserts: []dnsservice.DnsRecordSet{testDnsTxt("a."+apex, "two")}})
		if !errors.Is(err, dnsservice.ErrDnsVersionConflict) {
			t.Fatalf("a stale compare-and-swap answered %v", err)
		}
	})
}

// The journal's 7 days: rows past the retention go, and a reader below them is in a gap.
func TestDnsJournalPurgeLeavesAGap(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := testDnsRoot(t, ctx)
		networkId, apex := testDnsNetwork(t, ctx, root)
		for index := range 3 {
			if _, err := DnsWrite(ctx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{testDnsTxt(fmt.Sprintf("w%d.", index)+apex, "one")}}); err != nil {
				t.Fatal(err)
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `UPDATE dns_zone_journal SET commit_time = $2 WHERE root = $1 AND serial <= 2`, root, server.NowUtc().Add(-8*24*time.Hour)))
		})
		purgedCount, err := PurgeDnsJournals(ctx, server.NowUtc())
		if err != nil {
			t.Fatal(err)
		}
		if purgedCount < 2 {
			t.Fatalf("purged %d", purgedCount)
		}
		read, err := GetDnsJournal(ctx, root, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		if !read.Gap {
			t.Fatalf("a reader below the purge: %+v", read)
		}
		if rows := testDnsJournal(t, ctx, root, 2); len(rows) != 1 {
			t.Fatalf("a reader at the purge: %+v", rows)
		}
	})
}
