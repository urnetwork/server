//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.8's cases on MG5's abuse_case.

// A host report against one network, with no review clock.
func testAbuseCaseArgs(networkId server.Id, host string) *AbuseCaseArgs {
	return &AbuseCaseArgs{
		NetworkId:     &networkId,
		Category:      AbuseCategoryPhishing,
		Target:        AbuseTarget{Kind: AbuseTargetKindHost, Name: host},
		ReporterClass: AbuseReporterClassWebRisk,
		JoinKind:      AbuseJoinKindHost,
		JoinKey:       host,
	}
}

// Opens or joins a case in its own transaction.
func testOpenAbuseCase(t testing.TB, ctx context.Context, args *AbuseCaseArgs, now time.Time) (caseId server.Id, opened bool) {
	t.Helper()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, opened, err = OpenAbuseCaseInTx(ctx, tx, args, now, DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

func testAbuseCaseCount(t testing.TB, ctx context.Context) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_case`).Scan(&count))
	})
	return
}

// A second report on the same network, target and category inside the window
// joins the open case and adds no row; past the window it opens another.
func TestAbuseCaseCoalescesInsideTheWindow(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		window := DefaultAbuseSettings().CaseCoalesceWindow

		caseId, opened := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "shop.root.example"), now)
		if !opened {
			t.Fatal("the first report did not open a case")
		}
		// case and a trailing dot are no difference
		joinedId, opened := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "SHOP.root.example."), now.Add(window-time.Second))
		if opened || joinedId != caseId {
			t.Fatalf("a report inside the window opened %s", joinedId)
		}
		if got := testAbuseCaseCount(t, ctx); got != 1 {
			t.Fatalf("%d cases", got)
		}
		laterId, opened := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "shop.root.example"), now.Add(window+time.Second))
		if !opened || laterId == caseId {
			t.Fatal("a report past the window joined the old case")
		}

		abuseCase := GetAbuseCase(ctx, caseId)
		if abuseCase == nil || abuseCase.State != AbuseCaseStateOpen || abuseCase.Target != "shop.root.example" ||
			abuseCase.JoinKind != AbuseJoinKindHost || abuseCase.ReporterClass != AbuseReporterClassWebRisk {
			t.Fatalf("the case reads %+v", abuseCase)
		}
		if !NetworkHasOpenAbuseCase(ctx, networkId) || NetworkHasOpenAbuseCase(ctx, server.NewId()) {
			t.Fatal("the open-case check")
		}
	})
}

// Another category, another target, another network, a bar that differs, or no
// network at all each open their own case.
func TestAbuseCaseCoalescesOnlyItsOwnKind(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc()
		caseId, _ := testOpenAbuseCase(t, ctx, testAbuseCaseArgs(networkId, "shop.root.example"), now)

		for _, c := range []struct {
			name   string
			change func(args *AbuseCaseArgs)
		}{
			{name: "another category", change: func(args *AbuseCaseArgs) { args.Category = AbuseCategoryMalware }},
			{name: "another target", change: func(args *AbuseCaseArgs) { args.Target.Name = "cart.root.example" }},
			{name: "another network", change: func(args *AbuseCaseArgs) {
				otherNetworkId := server.NewId()
				args.NetworkId = &otherNetworkId
			}},
			{name: "a barred report", change: func(args *AbuseCaseArgs) { args.Barred = true }},
			{name: "no network", change: func(args *AbuseCaseArgs) { args.NetworkId = nil }},
		} {
			args := testAbuseCaseArgs(networkId, "shop.root.example")
			c.change(args)
			otherId, opened := testOpenAbuseCase(t, ctx, args, now)
			if !opened || otherId == caseId {
				t.Errorf("%s joined the open case", c.name)
			}
		}
	})
}

// A report with a shorter review clock moves the case's clock earlier, and a
// longer one leaves it.
func TestAbuseCaseCoalescingTakesTheShortClock(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		now := server.NowUtc().Truncate(time.Second)
		args := testAbuseCaseArgs(networkId, "shop.root.example")
		args.ReviewDueTime = now.Add(72 * time.Hour)
		caseId, _ := testOpenAbuseCase(t, ctx, args, now)

		args.ReviewDueTime = now.Add(4 * time.Hour)
		testOpenAbuseCase(t, ctx, args, now)
		args.ReviewDueTime = now.Add(7 * 24 * time.Hour)
		testOpenAbuseCase(t, ctx, args, now)

		abuseCase := GetAbuseCase(ctx, caseId)
		if abuseCase.ReviewDueTime == nil || !abuseCase.ReviewDueTime.Equal(now.Add(4*time.Hour)) {
			t.Fatalf("the review clock is %v", abuseCase.ReviewDueTime)
		}
	})
}

// What no row may hold is refused before anything is written.
func TestAbuseCaseRefusesWhatNoRowHolds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		for _, c := range []struct {
			name   string
			change func(args *AbuseCaseArgs)
		}{
			{name: "an unknown category", change: func(args *AbuseCaseArgs) { args.Category = "fraud" }},
			{name: "an unknown target kind", change: func(args *AbuseCaseArgs) { args.Target.Kind = "path" }},
			{name: "no target", change: func(args *AbuseCaseArgs) { args.Target.Name = " . " }},
			{name: "a long target", change: func(args *AbuseCaseArgs) { args.Target.Name = strings.Repeat("a", 257) }},
			{name: "no reporter class", change: func(args *AbuseCaseArgs) { args.ReporterClass = "" }},
			{name: "an unknown join kind", change: func(args *AbuseCaseArgs) { args.JoinKind = "url" }},
			{name: "no join key", change: func(args *AbuseCaseArgs) { args.JoinKey = "" }},
		} {
			args := testAbuseCaseArgs(networkId, "shop.root.example")
			c.change(args)
			var err error
			server.Tx(ctx, func(tx server.PgTx) {
				_, _, err = OpenAbuseCaseInTx(ctx, tx, args, server.NowUtc(), DefaultAbuseSettings())
			})
			if !errors.Is(err, ErrAbuseCaseInvalid) {
				t.Errorf("%s: %v", c.name, err)
			}
		}
		if got := testAbuseCaseCount(t, ctx); got != 0 {
			t.Fatalf("%d cases written", got)
		}
	})
}
