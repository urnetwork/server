//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The challenge row of name as the snapshot serves it, or nil.
func testDnsChallengeRow(t testing.TB, ctx context.Context, root string, owner string) *dnsservice.DnsRecordSet {
	t.Helper()
	snapshot, err := OpenDnsSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	for {
		set, err := snapshot.Next()
		if err != nil {
			return nil
		}
		if set.Owner == owner && set.Type == dns.TypeTXT {
			return set
		}
	}
}

// Two values stand together, the wildcard's and the apex's; a third is refused; a rewrite of the same
// values takes a new serial; an empty write removes them.
func TestDnsChallengeValues(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetDnsChallengesPerNameHour(10)()
		root := testDnsRoot(t, ctx)
		_, apex := testDnsNetwork(t, ctx, root)
		owner := DnsChallengeLabel + "." + apex
		first, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: "*." + apex, Values: []string{"wildcard-digest", "apex-digest"}})
		if err != nil {
			t.Fatal(err)
		}
		row := testDnsChallengeRow(t, ctx, root, owner)
		if row == nil || len(row.Rdata) != 2 || row.Ttl != 60 || row.Kind != dnsservice.DnsRecordKindChallenge {
			t.Fatalf("row %+v", row)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"a", "b", "c"}}); !errors.Is(err, ErrDnsChallengeValues) {
			t.Fatalf("three values answered %v", err)
		}
		again, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"wildcard-digest", "apex-digest"}})
		if err != nil {
			t.Fatal(err)
		}
		if again.Serial <= first.Serial {
			t.Fatalf("a rewrite at serial %d after %d", again.Serial, first.Serial)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex}); err != nil {
			t.Fatal(err)
		}
		if row := testDnsChallengeRow(t, ctx, root, owner); row != nil {
			t.Fatalf("a removed challenge still serves: %+v", row)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex}); !errors.Is(err, dnsservice.ErrDnsWriteEmpty) {
			t.Fatalf("removing nothing answered %v", err)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: "nolabel123." + root, Values: []string{"a"}}); !errors.Is(err, ErrDnsChallengeName) {
			t.Fatalf("a name no label holds answered %v", err)
		}
	})
}

// The 11th write of a name in an hour is refused, and a removal still goes.
func TestDnsChallengeRateLimit(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetDnsChallengesPerNameHour(10)()
		root := testDnsRoot(t, ctx)
		_, apex := testDnsNetwork(t, ctx, root)
		for index := range 10 {
			if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"digest"}}); err != nil {
				t.Fatalf("write %d: %v", index+1, err)
			}
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"digest"}}); !errors.Is(err, ErrDnsChallengeRate) {
			t.Fatalf("the 11th answered %v", err)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex}); err != nil {
			t.Fatalf("a removal at the cap answered %v", err)
		}
	})
}

// A challenge works under a live alias, before any other record of the alias answers (reading 11).
func TestDnsChallengeUnderALiveAlias(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetDnsChallengesPerNameHour(10)()
		root := testDnsRoot(t, ctx)
		networkId, _ := testDnsNetwork(t, ctx, root)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, 'claimed-alias', $2, $3, $4, now())
			`, root, networkId, DnsLabelKindAlias, DnsLabelStateLive))
		})
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: "claimed-alias." + root, Values: []string{"digest"}}); err != nil {
			t.Fatal(err)
		}
		if row := testDnsChallengeRow(t, ctx, root, DnsChallengeLabel+".claimed-alias."+root); row == nil {
			t.Fatal("the alias's challenge does not serve")
		}
	})
}

// While restoring a challenge answers ErrDnsRestoring, which the route answers 503, and certs retries.
func TestDnsChallengeWhileRestoring(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetDnsChallengesPerNameHour(10)()
		root := testDnsRoot(t, ctx)
		_, apex := testDnsNetwork(t, ctx, root)
		if err := SetDnsRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"digest"}}); !errors.Is(err, dnsservice.ErrDnsRestoring) {
			t.Fatalf("a challenge while restoring answered %v", err)
		}
		requireDnsSerial(t, ctx, root, 0)
	})
}

// A challenge an hour old goes through the journal at the next pass, and a younger one stays.
func TestDnsChallengeExpiresAfterAnHour(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetDnsChallengesPerNameHour(10)()
		root := testDnsRoot(t, ctx)
		_, apex := testDnsNetwork(t, ctx, root)
		written, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"digest"}})
		if err != nil {
			t.Fatal(err)
		}
		owner := DnsChallengeLabel + "." + apex
		if _, _, err := RemoveExpiredDnsChallenges(ctx, server.NowUtc().Add(59*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if row := testDnsChallengeRow(t, ctx, root, owner); row == nil {
			t.Fatal("a challenge went before its hour")
		}
		if _, _, err := RemoveExpiredDnsChallenges(ctx, server.NowUtc().Add(61*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if row := testDnsChallengeRow(t, ctx, root, owner); row != nil {
			t.Fatal("a challenge outlived its hour")
		}
		rows := testDnsJournal(t, ctx, root, written.Serial)
		if len(rows) != 1 || !slices.Contains(rows[0].Deletes, dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeTXT}) {
			t.Fatalf("the expiry's journal %+v", rows)
		}
	})
}
