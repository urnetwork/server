//go:build sidecar_pending

package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// §21.10's access on MG5's abuse_access_log (abuse_access_model.go; ABUSE-M5-12): one row per read, written before
// the read; a read whose row cannot be written answers nothing; no lookup without a key; at most LookupRowLimit rows.

// One access log row as the test reads it.
type testAbuseAccessRow struct {
	reviewerId server.Id
	lookupKind string
	caseId     *server.Id
}

// The reviewer's access log rows, oldest first.
func testAbuseAccessRows(t testing.TB, ctx context.Context, reviewerId server.Id) []testAbuseAccessRow {
	t.Helper()
	rows := []testAbuseAccessRow{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`SELECT reviewer_id, lookup_kind, case_id FROM abuse_access_log WHERE reviewer_id = $1 ORDER BY access_time, log_id`,
			reviewerId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := testAbuseAccessRow{}
				server.Raise(result.Scan(&row.reviewerId, &row.lookupKind, &row.caseId))
				rows = append(rows, row)
			}
		})
	})
	return rows
}

// Each read writes one row, by case with its case id and by join key with neither the key nor a case, and the row
// is in the table before the read answers; an id no case has is logged too, since the read was made.
func TestAbuseReaderLogsEachReadBeforeItAnswers(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, actionId := testNoticedAbuseCase(t, ctx, networkId, "shop.read.example", now.Add(-time.Hour))
		reviewerId := server.NewId()
		reader, err := NewAbuseReader(reviewerId, DefaultAbuseSettings())
		if err != nil {
			t.Fatal(err)
		}

		loggedBeforeAnswer := 0
		defer Testing_SetAbuseAccessHook(func(step string) error {
			if step == AbuseAccessStepLogAfter {
				loggedBeforeAnswer = len(testAbuseAccessRows(t, ctx, reviewerId))
			}
			return nil
		})()

		caseFile, err := reader.LookupAbuseCase(ctx, caseId)
		if err != nil || caseFile.Case.CaseId != caseId || len(caseFile.Actions) != 1 || caseFile.Actions[0].ActionId != actionId || caseFile.More {
			t.Fatalf("the case read: %+v, %v", caseFile, err)
		}
		if loggedBeforeAnswer != 1 {
			t.Fatalf("%d rows were in the log before the answer", loggedBeforeAnswer)
		}
		rows := testAbuseAccessRows(t, ctx, reviewerId)
		if len(rows) != 1 || rows[0].lookupKind != string(AbuseLookupKindCase) || rows[0].caseId == nil || *rows[0].caseId != caseId {
			t.Fatalf("the case read's row: %+v", rows)
		}

		lookup, err := reader.LookupAbuseCasesByJoinKey(ctx, AbuseJoinKindHost, " SHOP.Read.Example. ")
		if err != nil || len(lookup.Cases) != 1 || lookup.Cases[0].CaseId != caseId {
			t.Fatalf("the join read: %+v, %v", lookup, err)
		}
		rows = testAbuseAccessRows(t, ctx, reviewerId)
		if len(rows) != 2 || rows[1].lookupKind != string(AbuseLookupKindJoin) || rows[1].caseId != nil {
			t.Fatalf("the join read's row: %+v", rows)
		}

		unknownCaseId := server.NewId()
		if _, err := reader.LookupAbuseCase(ctx, unknownCaseId); !errors.Is(err, ErrAbuseCaseNotFound) {
			t.Fatalf("an unknown case: %v", err)
		}
		if rows = testAbuseAccessRows(t, ctx, reviewerId); len(rows) != 3 || *rows[2].caseId != unknownCaseId {
			t.Fatalf("an unknown case's read is not logged: %+v", rows)
		}
	})
}

// A read whose row cannot be written answers nothing, and leaves no row.
func TestAbuseReaderAnswersNothingWhenItCannotLog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseId, _ := testNoticedAbuseCase(t, ctx, networkId, "shop.unlogged.example", now.Add(-time.Hour))
		reviewerId := server.NewId()
		reader, err := NewAbuseReader(reviewerId, DefaultAbuseSettings())
		if err != nil {
			t.Fatal(err)
		}
		defer Testing_SetAbuseAccessHook(func(step string) error {
			if step == AbuseAccessStepLogBefore {
				return errors.New("the log is down")
			}
			return nil
		})()

		if caseFile, err := reader.LookupAbuseCase(ctx, caseId); !errors.Is(err, ErrAbuseAccessLogFailed) || caseFile != nil {
			t.Fatalf("a case read with no log: %+v, %v", caseFile, err)
		}
		if lookup, err := reader.LookupAbuseCasesByJoinKey(ctx, AbuseJoinKindHost, "shop.unlogged.example"); !errors.Is(err, ErrAbuseAccessLogFailed) || lookup != nil {
			t.Fatalf("a join read with no log: %+v, %v", lookup, err)
		}
		if rows := testAbuseAccessRows(t, ctx, reviewerId); len(rows) != 0 {
			t.Fatalf("%d rows", len(rows))
		}
	})
}

// A lookup with no key, an unknown join kind or no reviewer is refused before anything is read or logged.
func TestAbuseReaderRefusesABulkRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		if _, err := NewAbuseReader(server.Id{}, DefaultAbuseSettings()); !errors.Is(err, ErrAbuseReaderInvalid) {
			t.Fatalf("a reader with no reviewer: %v", err)
		}
		reviewerId := server.NewId()
		reader, err := NewAbuseReader(reviewerId, DefaultAbuseSettings())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reader.LookupAbuseCase(ctx, server.Id{}); !errors.Is(err, ErrAbuseLookupInvalid) {
			t.Errorf("no case id: %v", err)
		}
		for _, lookup := range []struct {
			kind AbuseJoinKind
			key  string
		}{
			{AbuseJoinKindHost, ""},
			{AbuseJoinKindHost, "   "},
			{AbuseJoinKindHost, "."},
			{AbuseJoinKindConnection, "not-an-id"},
			{AbuseJoinKind("everything"), "shop.example"},
			{AbuseJoinKind(""), ""},
		} {
			if _, err := reader.LookupAbuseCasesByJoinKey(ctx, lookup.kind, lookup.key); !errors.Is(err, ErrAbuseLookupInvalid) {
				t.Errorf("a join lookup of %q %q: %v", lookup.kind, lookup.key, err)
			}
		}
		if rows := testAbuseAccessRows(t, ctx, reviewerId); len(rows) != 0 {
			t.Fatalf("refused reads wrote %d rows", len(rows))
		}
	})
}

// No read answers more than LookupRowLimit rows of any kind, and says when more exist.
func TestAbuseReaderBoundsItsRows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		settings := DefaultAbuseSettings()
		settings.LookupRowLimit = 2
		settings.CaseCoalesceWindow = 0
		reader, err := NewAbuseReader(server.NewId(), settings)
		if err != nil {
			t.Fatal(err)
		}
		networkId := testAbuseCustomerNetwork(t, ctx)
		caseIds := []server.Id{}
		for range 3 {
			var caseId server.Id
			server.Tx(ctx, func(tx server.PgTx) {
				caseId, _, err = OpenAbuseCaseInTx(ctx, tx, testAbuseCaseArgs(networkId, "shop.bound.example"), now, settings)
			})
			if err != nil {
				t.Fatal(err)
			}
			caseIds = append(caseIds, caseId)
		}
		lookup, err := reader.LookupAbuseCasesByJoinKey(ctx, AbuseJoinKindHost, "shop.bound.example")
		if err != nil || len(lookup.Cases) != 2 || !lookup.More {
			t.Fatalf("three cases under a limit of two: %+v, %v", lookup, err)
		}

		reviewerId := server.NewId()
		for _, kind := range []AbuseActionKind{AbuseActionKindHostKill, AbuseActionKindLabelKill, AbuseActionKindMailHalt} {
			if _, _, err := testRecordAbuseAction(t, ctx, &AbuseActionArgs{
				ActionId:   server.NewId(),
				CaseId:     caseIds[0],
				Kind:       kind,
				Target:     "bound.example",
				ReviewerId: &reviewerId,
			}, now); err != nil {
				t.Fatal(err)
			}
		}
		caseFile, err := reader.LookupAbuseCase(ctx, caseIds[0])
		if err != nil || len(caseFile.Actions) != 2 || !caseFile.More {
			t.Fatalf("three actions under a limit of two: %+v, %v", caseFile, err)
		}
	})
}
