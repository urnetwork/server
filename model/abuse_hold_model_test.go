package model

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// §21.9's holds (abuse_hold_model.go; ABUSE-M5-14): the statutes' deadlines on a fake clock in either build, as
// T2 (plan-ABUSE-small-readings #15), and placement, renewal and the rows on MG5's abuse_legal_hold over a
// FakeBlobs first cluster, in a build with sidecar_pending.

// Day 0 of the schedules.
var testLegalHoldDay0 = time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)

func testLegalHoldDay(day int) time.Time {
	return testLegalHoldDay0.AddDate(0, 0, day)
}

// FS-48's schedule as the deadlines give it, pure: a §2258A hold reported and placed at day 0 stands across days
// 180 to 364, while a §2703(f) hold placed at day 0 ends at day 90, and one renewed at day 80 ends at day 180.
func TestLegalHoldDeadlinesFollowTheStatutes(t *testing.T) {
	preserve, err := LegalHoldDeadline(sidecar.HoldKindPreserve2703f, testLegalHoldDay0, nil)
	if err != nil || !preserve.Equal(testLegalHoldDay(90)) {
		t.Fatalf("§2703(f): %s, %v", preserve, err)
	}
	if renewed := LegalHoldRenewedDeadline(testLegalHoldDay0); !renewed.Equal(testLegalHoldDay(180)) {
		t.Fatalf("§2703(f) renewed: %s", renewed)
	}
	reportTime := testLegalHoldDay0
	report, err := LegalHoldDeadline(sidecar.HoldKindReport2258a, testLegalHoldDay(10), &reportTime)
	if err != nil || !report.Equal(testLegalHoldDay0.AddDate(1, 0, 0)) {
		t.Fatalf("§2258A runs from the report, not the placement: %s, %v", report, err)
	}

	preserveHold := &LegalHold{Kind: sidecar.HoldKindPreserve2703f, PlaceTime: testLegalHoldDay0, ExpireTime: preserve}
	renewedHold := &LegalHold{Kind: sidecar.HoldKindPreserve2703f, PlaceTime: testLegalHoldDay0, ExpireTime: LegalHoldRenewedDeadline(testLegalHoldDay0)}
	reportHold := &LegalHold{Kind: sidecar.HoldKindReport2258a, PlaceTime: testLegalHoldDay0, ReportTime: &reportTime, ExpireTime: report}
	for day := 0; day <= 400; day += 1 {
		now := testLegalHoldDay(day)
		if got, want := preserveHold.Live(now), day < 90; got != want {
			t.Fatalf("day %d: the §2703(f) hold reads live %t", day, got)
		}
		if got, want := renewedHold.Live(now), day < 180; got != want {
			t.Fatalf("day %d: the renewed hold reads live %t", day, got)
		}
		if 180 <= day && day <= 364 && !reportHold.Live(now) {
			t.Fatalf("day %d: the §2258A hold has ended", day)
		}
		if 366 <= day && reportHold.Live(now) {
			t.Fatalf("day %d: the §2258A hold stands past its year", day)
		}
	}

	// a renewal is §2703(f)'s alone, once, and only while the hold stands
	if err := legalHoldRenewable(preserveHold, testLegalHoldDay(80)); err != nil {
		t.Fatalf("a renewal at day 80: %v", err)
	}
	renewTime := testLegalHoldDay(80)
	renewedHold.RenewTime = &renewTime
	if err := legalHoldRenewable(renewedHold, testLegalHoldDay(81)); !errors.Is(err, ErrLegalHoldNotRenewable) {
		t.Fatalf("a second renewal: %v", err)
	}
	if err := legalHoldRenewable(reportHold, testLegalHoldDay(10)); !errors.Is(err, ErrLegalHoldNotRenewable) {
		t.Fatalf("a §2258A renewal: %v", err)
	}
	if err := legalHoldRenewable(preserveHold, testLegalHoldDay(90)); !errors.Is(err, ErrLegalHoldExpired) {
		t.Fatalf("a renewal at the expiry: %v", err)
	}
}

// What no hold may hold is refused before anything is written.
func TestLegalHoldArgsRefuseWhatNoHoldHolds(t *testing.T) {
	now := testLegalHoldDay(30)
	future := now.Add(time.Minute)
	stale := now.AddDate(-1, 0, -1)
	recent := now.AddDate(0, 0, -3)
	repoId := sidecar.NewId()
	tooMany := []sidecar.Id{}
	for range sidecar.HoldScopeMaxRepoCount + 1 {
		tooMany = append(tooMany, sidecar.NewId())
	}
	good := func() *LegalHoldArgs {
		return &LegalHoldArgs{NetworkId: server.NewId(), CaseId: server.NewId(), Kind: sidecar.HoldKindPreserve2703f}
	}
	for name, change := range map[string]func(args *LegalHoldArgs){
		"no network":               func(args *LegalHoldArgs) { args.NetworkId = server.Id{} },
		"no case":                  func(args *LegalHoldArgs) { args.CaseId = server.Id{} },
		"no kind":                  func(args *LegalHoldArgs) { args.Kind = "" },
		"another kind":             func(args *LegalHoldArgs) { args.Kind = "preserve" },
		"a §2703(f) report time":   func(args *LegalHoldArgs) { args.ReportTime = &recent },
		"a §2258A with no report":  func(args *LegalHoldArgs) { args.Kind = sidecar.HoldKindReport2258a },
		"a report in the future":   func(args *LegalHoldArgs) { args.Kind, args.ReportTime = sidecar.HoldKindReport2258a, &future },
		"a report past its year":   func(args *LegalHoldArgs) { args.Kind, args.ReportTime = sidecar.HoldKindReport2258a, &stale },
		"a zero repo":              func(args *LegalHoldArgs) { args.Scope = sidecar.HoldScopeRepos(sidecar.Id{}) },
		"a repo twice":             func(args *LegalHoldArgs) { args.Scope = sidecar.HoldScopeRepos(repoId, repoId) },
		"more repos than a scope ": func(args *LegalHoldArgs) { args.Scope = sidecar.HoldScopeRepos(tooMany...) },
	} {
		args := good()
		change(args)
		if err := args.validate(now); !errors.Is(err, ErrLegalHoldInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	report := good()
	report.Kind, report.ReportTime = sidecar.HoldKindReport2258a, &recent
	report.Scope = sidecar.HoldScopeRepos(repoId)
	for _, args := range []*LegalHoldArgs{good(), report} {
		if err := args.validate(now); err != nil {
			t.Errorf("%+v: %v", args, err)
		}
	}
}

// A scope column reads back as written, and anything else reads as the network, the wider scope.
func TestLegalHoldScopeColumnKeepsMoreOnDoubt(t *testing.T) {
	repoIds := []sidecar.Id{sidecar.NewId(), sidecar.NewId()}
	for _, scope := range []sidecar.HoldScope{sidecar.HoldScopeNetwork(), sidecar.HoldScopeRepos(repoIds...)} {
		if got := parseLegalHoldScope([]byte(legalHoldScopeJson(scope))); !slices.Equal(got.RepoIds, scope.RepoIds) {
			t.Fatalf("%v read back as %v", scope, got)
		}
	}
	for _, raw := range []string{``, `null`, `"repos"`, `[]`, `["not an id"]`, `{"repos":[]}`, `[1]`} {
		if got := parseLegalHoldScope([]byte(raw)); !got.Network() {
			t.Errorf("%q read as %v", raw, got)
		}
	}
}

// Only hold/<network_id>/<case_id>/_ names a hold.
func TestAbuseHoldKeysParseOnlyTheirOwnShape(t *testing.T) {
	networkId := sidecar.NewId()
	caseId := sidecar.NewId()
	key := sidecar.HoldObjectKey(networkId, caseId)
	if gotNetwork, gotCase, ok := ParseAbuseHoldKey(key); !ok || gotNetwork != server.Id(networkId) || gotCase != server.Id(caseId) {
		t.Fatalf("%s: %s %s %t", key, gotNetwork, gotCase, ok)
	}
	for _, bad := range []string{
		"hold/" + networkId.String() + "/" + caseId.String(),
		"hold/" + networkId.String() + "/" + caseId.String() + "/x",
		"hold/" + networkId.String() + "/_",
		"hold/x/" + caseId.String() + "/_",
		"hold/" + networkId.String() + "/" + caseId.String() + "/_/_",
		"proj/" + networkId.String() + "/" + caseId.String() + "/_",
	} {
		if _, _, ok := ParseAbuseHoldKey(bad); ok {
			t.Errorf("%s parsed", bad)
		}
	}
}

// Skips a store case in a build that does not link MG5.
func requireLegalHoldTables(t testing.TB) {
	t.Helper()
	if !server.SidecarMg5Linked() {
		t.Skip("MG5 is pending: these cases run with -tags sidecar_pending")
	}
}

// A first cluster in memory, installed as this process's hold/ writer for the test, with holds turned on.
func newTestLegalHoldStore(t testing.TB) (*sidecartest.FakeBlobs, *sidecartest.FakeClock) {
	t.Helper()
	clock := sidecartest.NewFakeClock(testLegalHoldDay0)
	blobs := sidecartest.NewFakeBlobs(clock, nil, nil, nil)
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	SetAbuseHoldStore(NewAbuseHoldStore(first))
	popConfig := server.Testing_PushAbuseConfig("holds_enabled: true\n")
	t.Cleanup(func() {
		SetAbuseHoldStore(nil)
		popConfig()
	})
	return blobs, clock
}

// A network and a staff-filed case naming it.
func newTestLegalHoldCase(t testing.TB, ctx context.Context) (networkId server.Id, caseId server.Id) {
	t.Helper()
	networkId = server.NewId()
	Testing_CreateNetwork(ctx, networkId, "hold-"+networkId.String()[24:], server.NewId())
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      AbuseCategoryOther,
			Target:        AbuseTarget{Kind: AbuseTargetKindNetwork, Name: networkId.String()},
			ReporterClass: AbuseReporterClassLawEnforcement,
			JoinKind:      AbuseJoinKindNetwork,
			JoinKey:       networkId.String(),
		}, server.NowUtc(), DefaultAbuseSettings())
	})
	if err != nil {
		t.Fatal(err)
	}
	return
}

// The bytes of a hold's object, nil when there is none.
func testLegalHoldObject(t testing.TB, ctx context.Context, blobs *sidecartest.FakeBlobs, networkId server.Id, caseId server.Id) []byte {
	t.Helper()
	first, _ := blobs.FirstCluster()
	object, err := first.Get(ctx, sidecar.HoldObjectKey(sidecar.Id(networkId), sidecar.Id(caseId)))
	if errors.Is(err, sidecar.ErrBlobNotFound) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer object.Body.Close()
	body := make([]byte, sidecar.HoldRecordMaxByteCount+1)
	n, _ := object.Body.Read(body)
	return body[:n]
}

// holds_enabled false refuses placement and renewal and writes nothing, as an absent abuse.yml does; a process
// with no hold/ writer refuses too, since the object comes first.
func TestLegalHoldRefusesUntilEnabled(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blobs, _ := newTestLegalHoldStore(t)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		args := &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}

		for _, config := range []string{"holds_enabled: false\n", ""} {
			pop := server.Testing_PushAbuseConfig(config)
			if _, _, err := PlaceLegalHold(ctx, args, testLegalHoldDay0, nil); !errors.Is(err, ErrLegalHoldsDisabled) {
				t.Fatalf("%q: %v", config, err)
			}
			if _, err := RenewLegalHold(ctx, networkId, caseId, testLegalHoldDay0, nil); !errors.Is(err, ErrLegalHoldsDisabled) {
				t.Fatalf("%q renews: %v", config, err)
			}
			pop()
		}
		if GetLegalHold(ctx, networkId, caseId) != nil || testLegalHoldObject(t, ctx, blobs, networkId, caseId) != nil {
			t.Fatal("a refused placement wrote something")
		}

		SetAbuseHoldStore(nil)
		if _, _, err := PlaceLegalHold(ctx, args, testLegalHoldDay0, nil); !errors.Is(err, ErrLegalHoldStoreUnavailable) {
			t.Fatalf("no writer: %v", err)
		}
		if GetLegalHold(ctx, networkId, caseId) != nil {
			t.Fatal("a placement with no writer committed a row")
		}

		first, _ := blobs.FirstCluster()
		SetAbuseHoldStore(NewAbuseHoldStore(first))
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: server.NewId(), Kind: sidecar.HoldKindPreserve2703f}, testLegalHoldDay0, nil); !errors.Is(err, ErrAbuseCaseNotFound) {
			t.Fatalf("no case: %v", err)
		}
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: server.NewId(), CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, testLegalHoldDay0, nil); !errors.Is(err, ErrNetworkNotFound) {
			t.Fatalf("no network: %v", err)
		}
	})
}

// Placement writes the object a reader parses, then the row, calling owe once in the row's transaction; a retry
// is the same hold, and another hold under the network and case is refused.
func TestPlaceLegalHoldWritesTheObjectThenTheRow(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blobs, clock := newTestLegalHoldStore(t)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		repoId := sidecar.NewId()
		args := &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f, Scope: sidecar.HoldScopeRepos(repoId)}

		owed := []*LegalHold{}
		owe := func(ctx context.Context, tx server.PgTx, hold *LegalHold) {
			if getLegalHoldInTx(ctx, tx, hold.NetworkId, hold.CaseId, false) == nil {
				t.Error("owe ran outside the row's transaction")
			}
			owed = append(owed, hold)
		}
		hold, inserted, err := PlaceLegalHold(ctx, args, testLegalHoldDay0, owe)
		if err != nil || !inserted {
			t.Fatalf("%t, %v", inserted, err)
		}
		if !hold.PlaceTime.Equal(testLegalHoldDay0) || !hold.ExpireTime.Equal(testLegalHoldDay(90)) || hold.CopyState != LegalHoldCopyStatePending {
			t.Fatalf("the row: %+v", hold)
		}
		if len(owed) != 1 {
			t.Fatalf("owed %d", len(owed))
		}
		body := testLegalHoldObject(t, ctx, blobs, networkId, caseId)
		want, _ := sidecar.EncodeHoldRecord(hold.Record())
		if string(body) != string(want) {
			t.Fatalf("the object holds %s, want %s", body, want)
		}

		// what every reader sees: the repo covered, another repo not, server rows covered
		holdSet := sidecar.NewHoldSet(blobs, clock, nil)
		for _, check := range []struct {
			repoId  sidecar.Id
			covered bool
		}{{repoId, true}, {sidecar.NewId(), false}, {sidecar.Id{}, true}} {
			cover, err := holdSet.Covering(ctx, sidecar.Id(networkId), check.repoId)
			if err != nil || cover.Covered() != check.covered {
				t.Fatalf("repo %s: %+v, %v", check.repoId, cover, err)
			}
		}
		held := HeldNetworkIds(ctx, testLegalHoldDay(89))
		if !held[networkId] {
			t.Fatal("HeldNetworkIds misses a live hold")
		}
		if HeldNetworkIds(ctx, testLegalHoldDay(90))[networkId] {
			t.Fatal("HeldNetworkIds holds a hold at its expiry")
		}

		again, inserted, err := PlaceLegalHold(ctx, args, testLegalHoldDay(1), owe)
		if err != nil || inserted || !again.PlaceTime.Equal(testLegalHoldDay0) || len(owed) != 1 {
			t.Fatalf("a retry: %+v, %t, %v, owed %d", again, inserted, err, len(owed))
		}
		other := *args
		other.Scope = sidecar.HoldScopeNetwork()
		if _, _, err := PlaceLegalHold(ctx, &other, testLegalHoldDay(1), owe); !errors.Is(err, ErrLegalHoldConflict) {
			t.Fatalf("another scope: %v", err)
		}
		if _, _, err := PlaceLegalHold(ctx, args, testLegalHoldDay(90), owe); !errors.Is(err, ErrLegalHoldExpired) {
			t.Fatalf("a placement over an ended hold: %v", err)
		}
		if holds := GetLegalHolds(ctx, networkId); len(holds) != 1 || holds[0].CaseId != caseId {
			t.Fatalf("the network's holds: %+v", holds)
		}
	})
}

// A crash between the object and the row leaves the object, which covers the network and expires by its own time,
// and no row; a retry takes the object's placement, chosen once.
func TestLegalHoldCrashBetweenObjectAndRowKeepsData(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blobs, clock := newTestLegalHoldStore(t)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		args := &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}

		Testing_LegalHoldStep = func(step string) {
			if step == LegalHoldStepObjectAfter {
				runtime.Goexit()
			}
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			PlaceLegalHold(ctx, args, testLegalHoldDay0, nil)
		}()
		<-done
		Testing_LegalHoldStep = nil

		if GetLegalHold(ctx, networkId, caseId) != nil {
			t.Fatal("the crash left a row")
		}
		holdSet := sidecar.NewHoldSet(blobs, clock, nil)
		cover, err := holdSet.Covering(ctx, sidecar.Id(networkId), sidecar.NewId())
		if err != nil || !cover.Covered() {
			t.Fatalf("the object does not keep data: %+v, %v", cover, err)
		}
		if expireTime, bounded := cover.ExpireTime(); !bounded || !expireTime.Equal(testLegalHoldDay(90)) {
			t.Fatalf("the object ends at %s, %t", expireTime, bounded)
		}
		clock.AdvanceTo(testLegalHoldDay(90))
		later, err := sidecar.NewHoldSet(blobs, clock, nil).Covering(ctx, sidecar.Id(networkId), sidecar.NewId())
		if err != nil || later.Covered() {
			t.Fatalf("the object keeps data past its expiry: %+v, %v", later, err)
		}

		hold, inserted, err := PlaceLegalHold(ctx, args, testLegalHoldDay(2), nil)
		if err != nil || !inserted || !hold.PlaceTime.Equal(testLegalHoldDay0) || !hold.ExpireTime.Equal(testLegalHoldDay(90)) {
			t.Fatalf("the retry: %+v, %t, %v", hold, inserted, err)
		}
	})
}

// A renewal at day 80 rewrites the object If-Match to day 180, keeping the placement; a second is refused, and a
// §2258A hold takes none.
func TestRenewLegalHoldRewritesTheObjectOnce(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blobs, _ := newTestLegalHoldStore(t)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, testLegalHoldDay0, nil); err != nil {
			t.Fatal(err)
		}
		before := testLegalHoldObject(t, ctx, blobs, networkId, caseId)

		owed := 0
		renewed, err := RenewLegalHold(ctx, networkId, caseId, testLegalHoldDay(80), func(ctx context.Context, tx server.PgTx, hold *LegalHold) {
			owed += 1
		})
		if err != nil || owed != 1 {
			t.Fatalf("%v, owed %d", err, owed)
		}
		if !renewed.ExpireTime.Equal(testLegalHoldDay(180)) || !renewed.PlaceTime.Equal(testLegalHoldDay0) || renewed.RenewTime == nil {
			t.Fatalf("the renewed row: %+v", renewed)
		}
		after := testLegalHoldObject(t, ctx, blobs, networkId, caseId)
		record, err := sidecar.ParseHoldRecord(after)
		if err != nil || string(after) == string(before) || !record.ExpireTime.Equal(testLegalHoldDay(180)) || !record.PlaceTime.Equal(testLegalHoldDay0) {
			t.Fatalf("the object after the renewal: %s, %v", after, err)
		}
		if _, err := RenewLegalHold(ctx, networkId, caseId, testLegalHoldDay(81), nil); !errors.Is(err, ErrLegalHoldNotRenewable) {
			t.Fatalf("a second renewal: %v", err)
		}

		reportNetwork, reportCase := newTestLegalHoldCase(t, ctx)
		reportTime := testLegalHoldDay0
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: reportNetwork, CaseId: reportCase, Kind: sidecar.HoldKindReport2258a, ReportTime: &reportTime}, testLegalHoldDay(1), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := RenewLegalHold(ctx, reportNetwork, reportCase, testLegalHoldDay(80), nil); !errors.Is(err, ErrLegalHoldNotRenewable) {
			t.Fatalf("a §2258A renewal: %v", err)
		}
		if _, err := RenewLegalHold(ctx, networkId, server.NewId(), testLegalHoldDay(80), nil); !errors.Is(err, ErrLegalHoldNotFound) {
			t.Fatalf("no hold: %v", err)
		}
	})
}

// A hold ends only past its expiry: before it the row stays whatever a pass read; past it every part goes.
func TestEndLegalHoldOnlyPastItsExpiry(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestLegalHoldStore(t)
		networkId, caseId := newTestLegalHoldCase(t, ctx)
		if _, _, err := PlaceLegalHold(ctx, &LegalHoldArgs{NetworkId: networkId, CaseId: caseId, Kind: sidecar.HoldKindPreserve2703f}, testLegalHoldDay0, nil); err != nil {
			t.Fatal(err)
		}
		expired, _ := ListExpiredLegalHolds(ctx, testLegalHoldDay(89), LegalHoldCursor{}, 10)
		live, _ := ListLiveLegalHolds(ctx, testLegalHoldDay(89), LegalHoldCursor{}, 10)
		if testLegalHoldListed(expired, caseId) || !testLegalHoldListed(live, caseId) {
			t.Fatalf("at day 89: expired %d, live %d", len(expired), len(live))
		}
		var ended bool
		server.Tx(ctx, func(tx server.PgTx) {
			_, ended = EndLegalHoldInTx(ctx, tx, networkId, caseId, testLegalHoldDay(89))
		})
		if ended || GetLegalHold(ctx, networkId, caseId) == nil {
			t.Fatal("a live hold ended")
		}
		expired, _ = ListExpiredLegalHolds(ctx, testLegalHoldDay(90), LegalHoldCursor{}, 10)
		if !testLegalHoldListed(expired, caseId) {
			t.Fatal("the hold is not listed at its expiry")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			_, ended = EndLegalHoldInTx(ctx, tx, networkId, caseId, testLegalHoldDay(90))
		})
		if !ended || GetLegalHold(ctx, networkId, caseId) != nil {
			t.Fatal("the hold did not end at its expiry")
		}
		var parts int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM abuse_legal_hold WHERE network_id = $1`, networkId).Scan(&parts))
		})
		if parts != 0 {
			t.Fatalf("%d parts stayed", parts)
		}
	})
}

func testLegalHoldListed(holds []*LegalHold, caseId server.Id) bool {
	return slices.ContainsFunc(holds, func(hold *LegalHold) bool {
		return hold.CaseId == caseId
	})
}
