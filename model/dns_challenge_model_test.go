//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"fmt"
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

// The commit time of the journal's row at serial in root.
func testDnsCommitTime(t testing.TB, ctx context.Context, root string, serial uint64) time.Time {
	t.Helper()
	var commitTime time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT commit_time FROM dns_zone_journal WHERE root = $1 AND serial = $2`,
			root,
			dnsSerialColumn(serial),
		).Scan(&commitTime))
	})
	return commitTime.UTC()
}

// The cap's refusal says how long until the name takes a write again (QD-1204): until its oldest write in
// the window leaves it, rounded up to the whole second past that, the window counting a write at its edge;
// with the cap lowered below the count, until the write whose leaving brings the count below the cap has
// left; and no time with a cap of zero, which takes no write. A removal is still never refused.
func TestDnsChallengeRateSaysWhenTheNameTakesAWrite(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		defer Testing_SetDnsChallengesPerNameHour(10)()
		root := testDnsRoot(t, ctx)
		_, apex := testDnsNetwork(t, ctx, root)
		owner := DnsChallengeLabel + "." + apex
		window := DefaultDnsChallengeSettings().RateWindow
		commitTimes := []time.Time{}
		for index := range 10 {
			result, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{fmt.Sprintf("digest-%d", index)}})
			if err != nil {
				t.Fatalf("write %d: %v", index+1, err)
			}
			commitTimes = append(commitTimes, testDnsCommitTime(t, ctx, root, result.Serial))
		}
		refuse := func() *DnsChallengeRateError {
			t.Helper()
			_, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"refused"}})
			var rateErr *DnsChallengeRateError
			if !errors.As(err, &rateErr) || !errors.Is(err, ErrDnsChallengeRate) || rateErr.Owner != owner {
				t.Fatalf("a write past the cap answered %v", err)
			}
			return rateErr
		}
		// whole seconds past freeTime, as the model's clock read it between before and after
		check := func(freeTime time.Time) {
			t.Helper()
			before := server.NowUtc()
			rateErr := refuse()
			after := server.NowUtc()
			low := (freeTime.Sub(after)/time.Second + 1) * time.Second
			high := (freeTime.Sub(before)/time.Second + 1) * time.Second
			if rateErr.RetryAfter%time.Second != 0 || rateErr.RetryAfter < low || high < rateErr.RetryAfter {
				t.Fatalf("Retry-After %v, not whole seconds in [%v, %v]", rateErr.RetryAfter, low, high)
			}
		}
		check(commitTimes[0].Add(window))

		restoreEight := Testing_SetDnsChallengesPerNameHour(8)
		defer restoreEight()
		// ten in the window under a cap of eight: the third oldest's leaving brings them to seven
		check(commitTimes[2].Add(window))

		restoreZero := Testing_SetDnsChallengesPerNameHour(0)
		defer restoreZero()
		if rateErr := refuse(); rateErr.RetryAfter != 0 {
			t.Fatalf("a cap of zero said %v", rateErr.RetryAfter)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex}); err != nil {
			t.Fatalf("a removal under a cap of zero answered %v", err)
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
		// the label's zone, written with the draw
		base := testDnsRootSerial(t, ctx, root)
		if err := SetDnsRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		if _, err := DnsPutChallenge(ctx, &DnsChallengeArgs{Root: root, Name: apex, Values: []string{"digest"}}); !errors.Is(err, dnsservice.ErrDnsRestoring) {
			t.Fatalf("a challenge while restoring answered %v", err)
		}
		requireDnsSerial(t, ctx, root, base)
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
