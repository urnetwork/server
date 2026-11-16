package model

// The root cause of the 2026-11-16 key history retention incident: what a
// delete batch read was bounded by the records it deleted, not by a page of
// records. The batch selected its due records in one ordered query whose
// filter was the check that the record's client has no row, so a region of
// records it could not delete made one batch read past all of them to fill
// its batch, and on Main, where the planner expects every record's client to
// still have a row, one batch read both tables whole for 30 minutes.
//
// DB-backed: under the owner rule these run only after the branch, with its
// migrations, is merged to main.

import (
	"testing"

	"github.com/urnetwork/server"
)

// A delete batch reads one page of records in (gone_time, client_id) order and
// deletes only that page's due records. With twelve records of clients that
// still have a row ahead of three gone clients, and a page of five, the first
// two batches delete nothing and their cursor stops at the last record of
// their page; the third deletes the three gone clients, and the fourth reads
// an empty page and ends the pass. Before the fix the first batch read all
// fifteen records and deleted the three gone clients.
func TestGoneStClientKeyHistoryBatchReadsOnlyItsPage(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		existingIds := []server.Id{}
		for i := range 12 {
			existingId := server.NewId()
			Testing_CreateDevice(ctx, networkId, server.NewId(), existingId, "test", "test")
			insertGoneKeyHistoryTestRows(t, ctx, existingId, networkId)
			// a record a client that exists must never have, ahead of the gone
			// clients
			setGoneKeyHistoryTestDays(t, ctx, existingId, 100-i)
			existingIds = append(existingIds, existingId)
		}
		goneIds := []server.Id{}
		for i := range 3 {
			goneId := server.NewId()
			insertGoneKeyHistoryTestRows(t, ctx, goneId, server.NewId())
			setGoneKeyHistoryTestDays(t, ctx, goneId, 50-i)
			goneIds = append(goneIds, goneId)
		}
		settings := DefaultStClientKeyHistoryRetentionSettings()
		settings.DeleteBatchCount = 5
		// one batch a run
		settings.RunBudget = 0

		now := goneKeyHistoryTestDbNow(t, ctx)
		cursor := goneKeyHistoryTestDeleteOnly(now)
		for i, want := range []struct {
			removedCount int64
			// the cursor after the batch, nil for an ended pass
			afterClientId *server.Id
		}{
			{removedCount: 0, afterClientId: &existingIds[4]},
			{removedCount: 0, afterClientId: &existingIds[9]},
			{removedCount: 3, afterClientId: &goneIds[2]},
			{removedCount: 0, afterClientId: nil},
		} {
			var report *StClientKeyHistoryRetentionReport
			report, cursor = RemoveGoneStClientKeyHistory(ctx, now, cursor, settings)
			if report.RemovedClientCount != want.removedCount {
				t.Fatalf("batch %d removed %d clients, want %d: a batch read past its page of %d records (%+v)", i+1, report.RemovedClientCount, want.removedCount, settings.DeleteBatchCount, report)
			}
			if want.afterClientId == nil {
				if cursor.DeleteAfterClientId != nil || cursor.DeleteAfterGoneTime != nil || report.More || report.DeleteStop != StClientKeyHistoryRetentionStopDue {
					t.Fatalf("batch %d left cursor %+v and report %+v, want the pass ended", i+1, cursor, report)
				}
			} else if cursor.DeleteAfterClientId == nil || *cursor.DeleteAfterClientId != *want.afterClientId || !report.More {
				t.Fatalf("batch %d left cursor %+v and report %+v, want the cursor at the last record of its page", i+1, cursor, report)
			}
		}

		for _, existingId := range existingIds {
			if rows := readGoneKeyHistoryTestRows(t, ctx, existingId); rows != (goneKeyHistoryTestRows{history: 1, heads: 1, gone: true}) {
				t.Fatalf("a client that still has a row lost key history or its record: %+v", rows)
			}
		}
		for _, goneId := range goneIds {
			if rows := readGoneKeyHistoryTestRows(t, ctx, goneId); rows != (goneKeyHistoryTestRows{}) {
				t.Fatalf("a gone client kept rows %+v", rows)
			}
		}
	})
}
