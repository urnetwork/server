package model

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// FS-48 (§22.5's §21 row; §21.9, R-ABUSE-5, D-ABUSE-6, D-ABUSE-8; ABUSE-M5-21): a §2258A hold standing across days
// 180 to 364 while two §2703(f) holds on the same network expire at day 90 and, renewed once at day 80, at day 180,
// each placed and renewed through PlaceLegalHold and RenewLegalHold on MG5's abuse_legal_hold and a FakeBlobs first
// cluster, on an injected clock, in a build with sidecar_pending:
//
//	day      holds standing                   a reader's cover   the TTL sweep
//	0-89     2703(f), 2703(f) renewed, 2258A  until day 365      skips the network's expired rows
//	90-179   2703(f) renewed, 2258A           until day 365      skips them
//	180-364  2258A                            until day 365      skips them
//	365 on   none                             none               resumes: they go, with no notice
//
// The users' --now, the 7-day timer and per-entry erasure proceed under both kinds: they run in evs, whose readers
// of hold/ read no kind, so evsservice's shred and erase cases under a §2703(f) hold are its cases under a §2258A
// hold (TestFs48EraseAndNowReadNoHoldKind). The deadlines' pure schedule is TestLegalHoldDeadlinesFollowTheStatutes.

// A case naming networkId in category, filed by staff, for one hold of the network's. A network's open cases
// coalesce by target and category, so each hold's case takes a category of its own.
func testFs48Case(t testing.TB, ctx context.Context, networkId server.Id, category AbuseCategory) (caseId server.Id) {
	t.Helper()
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, _, err = OpenAbuseCaseInTx(ctx, tx, &AbuseCaseArgs{
			NetworkId:     &networkId,
			Category:      category,
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

// The schedule in the file header, at each day either side of a deadline.
func TestFs48HoldsAcrossTheirDeadlinesOnPostgres(t *testing.T) {
	requireLegalHoldTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		blobs, clock := newTestLegalHoldStore(t)
		settings := DefaultAbuseSettings()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "fs48-"+networkId.String()[24:], server.NewId())

		preserveCase := testFs48Case(t, ctx, networkId, AbuseCategoryOther)
		renewedCase := testFs48Case(t, ctx, networkId, AbuseCategoryCompromise)
		reportCase := testFs48Case(t, ctx, networkId, AbuseCategoryCsam)
		if preserveCase == renewedCase || renewedCase == reportCase {
			t.Fatal("the holds' cases coalesced")
		}
		reportTime := testLegalHoldDay0
		for _, args := range []*LegalHoldArgs{
			{NetworkId: networkId, CaseId: preserveCase, Kind: sidecar.HoldKindPreserve2703f},
			{NetworkId: networkId, CaseId: renewedCase, Kind: sidecar.HoldKindPreserve2703f},
			{NetworkId: networkId, CaseId: reportCase, Kind: sidecar.HoldKindReport2258a, ReportTime: &reportTime},
		} {
			if _, inserted, err := PlaceLegalHold(ctx, args, testLegalHoldDay0, nil); err != nil || !inserted {
				t.Fatalf("placing %s: %t, %v", args.Kind, inserted, err)
			}
		}
		if _, err := RenewLegalHold(ctx, networkId, renewedCase, testLegalHoldDay(80), nil); err != nil {
			t.Fatalf("the renewal at day 80: %v", err)
		}

		// rows past their 13 months on the held network and, as the twin, on a network no hold names
		closeTime := testLegalHoldDay0.AddDate(-2, 0, 0)
		heldRows := testRetentionCase(t, ctx, networkId, "closed", &closeTime)
		otherNetwork := server.NewId()
		otherRows := testRetentionCase(t, ctx, otherNetwork, "closed", &closeTime)

		lastDay := testLegalHoldDay(365)
		for _, check := range []struct {
			day      int
			standing []server.Id
		}{
			{0, []server.Id{preserveCase, renewedCase, reportCase}},
			{89, []server.Id{preserveCase, renewedCase, reportCase}},
			{90, []server.Id{renewedCase, reportCase}},
			{179, []server.Id{renewedCase, reportCase}},
			{180, []server.Id{reportCase}},
			{364, []server.Id{reportCase}},
			{365, []server.Id{}},
		} {
			now := testLegalHoldDay(check.day)
			clock.AdvanceTo(now)

			standing := []server.Id{}
			for _, hold := range GetLegalHolds(ctx, networkId) {
				if hold.Live(now) {
					standing = append(standing, hold.CaseId)
				}
			}
			slices.SortFunc(standing, func(a server.Id, b server.Id) int { return strings.Compare(a.String(), b.String()) })
			want := slices.Clone(check.standing)
			slices.SortFunc(want, func(a server.Id, b server.Id) int { return strings.Compare(a.String(), b.String()) })
			if !slices.Equal(standing, want) {
				t.Fatalf("day %d: %d holds stand, want %d", check.day, len(standing), len(want))
			}
			if held := HeldNetworkIds(ctx, now)[networkId]; held != (0 < len(check.standing)) {
				t.Fatalf("day %d: HeldNetworkIds reads the network held %t", check.day, held)
			}

			// every reader's cover is the union: the latest expiry, day 365, while any hold stands
			cover, err := sidecar.NewHoldSet(blobs, clock, nil).Covering(ctx, sidecar.Id(networkId), sidecar.Id{})
			if err != nil {
				t.Fatalf("day %d: %v", check.day, err)
			}
			expireTime, bounded := cover.ExpireTime()
			if cover.Covered() != (0 < len(check.standing)) || (cover.Covered() && (!bounded || !expireTime.Equal(lastDay))) {
				t.Fatalf("day %d: covered %t until %s, bounded %t", check.day, cover.Covered(), expireTime, bounded)
			}
			// a hold keeps what existed at its placement and collects nothing new
			if cover.Covered() && (!cover.Keeps(testLegalHoldDay0.Add(-time.Hour)) || cover.Keeps(testLegalHoldDay(1))) {
				t.Fatalf("day %d: the cover keeps what came after its placement, or not what came before", check.day)
			}

			// the TTL sweep skips the held network's rows while any hold stands, and takes the twin's at once
			RemoveExpiredAbuseRows(ctx, now, settings)
			if testRetentionExists(t, ctx, "abuse_case", "case_id", otherRows) {
				t.Fatalf("day %d: the unheld network's expired case stands", check.day)
			}
			if kept := testRetentionExists(t, ctx, "abuse_case", "case_id", heldRows); kept != (0 < len(check.standing)) {
				t.Fatalf("day %d: the held network's expired case kept %t", check.day, kept)
			}
		}
	})
}

// §21.9's always row: --now, the 7-day timer and per-entry erasure proceed under every kind, destroying keys, since
// a hold keeps only the ciphertext they leave. They run in evs, and gkm keeps its own; both read hold/ through
// sidecar.HoldSet, whose cover holds each covering record's times and gives no verdict by kind. So no non-test file
// of evsservice or gkmservice names a kind, and the cases that show erasure and --now proceeding under a §2703(f)
// hold are those cases under a §2258A hold.
func TestFs48EraseAndNowReadNoHoldKind(t *testing.T) {
	root := filepath.Join("..", "..", "sidecar")
	if _, err := os.Stat(filepath.Join(root, "host_hold.go")); err != nil {
		t.Fatalf("no sidecar checkout beside server: %v", err)
	}
	for _, dir := range []string{"evsservice", "gkmservice"} {
		readCount := 0
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			readCount += 1
			for _, word := range []string{"HoldKind", `"preserve_2703f"`, `"report_2258a"`} {
				if strings.Contains(string(data), word) {
					t.Errorf("%s names %s: a hold's kind reaches a reader that erases", path, word)
				}
			}
			return nil
		})
		if err != nil || readCount == 0 {
			t.Fatalf("%s: read %d files, %v", dir, readCount, err)
		}
	}
	// the cases this reading leans on are there to lean on
	for path, name := range map[string]string{
		filepath.Join(root, "evsservice", "repo_test.go"):                "func TestRepoShredUnderAHoldKeepsCiphertextAndDeletesKeys(",
		filepath.Join(root, "evsservice", "closure_versionless_test.go"): "func TestEraseUnderAHoldDestroysAVersionlessPathsKeyAndKeepsItsCiphertext(",
		filepath.Join(root, "gkmservice", "retention_test.go"):           "func TestGkmHoldPausesEverySweepButNotShredGroup(",
	} {
		data, err := os.ReadFile(path)
		if err != nil || !strings.Contains(string(data), name) {
			t.Errorf("%s lacks %s: %v", path, strings.TrimSuffix(strings.TrimPrefix(name, "func "), "("), err)
		}
	}
	// and a cover reads the same for either kind
	place := testLegalHoldDay0
	reportTime := testLegalHoldDay0
	for _, record := range []*sidecar.HoldRecord{
		{Kind: sidecar.HoldKindPreserve2703f, Scope: sidecar.HoldScopeNetwork(), PlaceTime: place, ExpireTime: testLegalHoldDay(90)},
		{Kind: sidecar.HoldKindReport2258a, Scope: sidecar.HoldScopeNetwork(), PlaceTime: place, ExpireTime: reportTime.AddDate(1, 0, 0)},
	} {
		cover := &sidecar.HoldCover{Holds: []*sidecar.HoldRecord{record}}
		if !cover.Covered() || !cover.Keeps(place) || cover.Keeps(place.Add(time.Second)) {
			t.Errorf("a %s cover reads covered %t, keeps the placement %t, keeps what came after %t",
				record.Kind, cover.Covered(), cover.Keeps(place), cover.Keeps(place.Add(time.Second)))
		}
	}
}
