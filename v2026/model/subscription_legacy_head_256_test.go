// Larger pages preserve the head/forward ratio and grant-wait opportunity
// frequency through the actual indexed selector and serialized cursor loop.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Callback ownership is synthetic; the selector and queue mutations are native.
// Two full pages retain sixty-four busy heads, delete successful forward rows,
// and exclude later due arrivals. No callback mutates financial or outcome state.
func TestLegacySettlementHead256PreservesWaitFrequencyAndCursor(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newNetEscrowOrderingTestFixture(t, ctx)
		idPrefix := server.NewId()
		var sequence uint32
		nextId := func() server.Id {
			sequence++
			id := idPrefix
			binary.BigEndian.PutUint32(id[11:15], sequence)
			id[15] = 1
			return id
		}
		headIds := make([]server.Id, 64)
		forwardIds := make([]server.Id, 512)
		headIdSet := map[server.Id]bool{}
		forwardIdSet := map[server.Id]bool{}
		allIds := make([]server.Id, 0, len(headIds)+len(forwardIds))
		for index := range headIds {
			id := nextId()
			headIds[index] = id
			headIdSet[id] = true
			allIds = append(allIds, id)
		}
		for index := range forwardIds {
			id := nextId()
			forwardIds[index] = id
			forwardIdSet[id] = true
			allIds = append(allIds, id)
		}
		enqueue := func(ids []server.Id, times []time.Time) {
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
					(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
					SELECT id,$2,$3,$4,$5,$2,0 FROM unnest($1::uuid[]) AS entry(id)`,
					ids, fixture.sourceNetworkId, fixture.sourceId, fixture.destinationNetworkId, fixture.destinationId))
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent
					(contract_id,shard,outcome,clear_dispute,next_attempt_time)
					SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS entry(id,due)`, ids, times))
			})
		}
		oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
		times := make([]time.Time, len(allIds))
		for index := range times {
			times[index] = oldest.Add(time.Duration(index) * time.Second)
		}
		enqueue(allIds, times)

		pageHeadIds := []server.Id{}
		pageWaitIds := []server.Id{}
		pageForwardIds := []server.Id{}
		settle := func(ctx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
			if headIdSet[id] {
				pageHeadIds = append(pageHeadIds, id)
				if wait != nil {
					pageWaitIds = append(pageWaitIds, id)
					wait.attempted = true
					wait.timedOut = true
				}
				return false, true, legacySettlementBusyGrantSet, nil
			}
			if !forwardIdSet[id] {
				t.Fatal("larger page selected an arrival beyond its fixed cutoff")
			}
			if wait != nil {
				t.Fatal("a forward visit received the head-only grant wait")
			}
			server.Tx(ctx, func(tx server.PgTx) {
				if server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, id)).RowsAffected() != 1 {
					t.Fatal("larger page repeated a deleted successful forward visit")
				}
			})
			pageForwardIds = append(pageForwardIds, id)
			return true, false, legacySettlementBusyNone, nil
		}
		initial, err := flushLegacySettlementsPage(ctx, ctx, 1, nil, 64, settle)
		if err != nil || initial.Visited != 64 || initial.BusyOrGone != 64 || initial.Completed != 0 ||
			initial.HeadVisited != 0 || initial.Cursor == nil || initial.Cursor.ContractId != headIds[63] ||
			initial.Cursor.HeadAfter != nil || !initial.More || len(pageWaitIds) != 0 || !slices.Equal(pageHeadIds, headIds) {
			t.Fatalf("initial real page did not establish sixty-four retained heads: %+v err=%v", initial, err)
		}
		cursor := initial.Cursor
		cutoff := cursor.PassEndTime
		newIds := []server.Id{}
		for turn := range 2 {
			id := nextId()
			var due time.Time
			server.Db(ctx, func(conn server.PgConn) {
				server.Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&due))
			})
			if !cutoff.Before(due) {
				t.Fatal("new larger-page arrival did not follow the fixed cutoff")
			}
			enqueue([]server.Id{id}, []time.Time{due})
			newIds = append(newIds, id)
			pageHeadIds, pageWaitIds, pageForwardIds = nil, nil, nil
			before, err := json.Marshal(cursor)
			server.Raise(err)
			page, err := flushLegacySettlementsPage(ctx, ctx, 1, cursor, 256, settle)
			after, marshalErr := json.Marshal(cursor)
			server.Raise(marshalErr)
			if err != nil || page.Visited != 256 || page.Completed != 192 || page.BusyOrGone != 64 || page.Failed != 0 ||
				page.HeadVisited != 64 || page.HeadBusyOrGone != 64 || page.HeadCompleted != 0 ||
				page.HeadBusyGrantSetMismatch != 64 || !page.More || page.Cursor == nil || page.Cursor.HeadAfter == nil ||
				page.Cursor.HeadAfter.ContractId != headIds[63] || page.Cursor.ContractId != forwardIds[191+192*turn] ||
				!page.Cursor.PassEndTime.Equal(cutoff) || !bytes.Equal(before, after) ||
				!slices.Equal(pageHeadIds, headIds) || !slices.Equal(pageForwardIds, forwardIds[192*turn:192*(turn+1)]) {
				t.Fatalf("larger page lost distinct head share, forward deletions, or cursor authority: turn=%d page=%+v err=%v", turn, page, err)
			}
			wantWaitIds := []server.Id{headIds[0], headIds[16], headIds[32], headIds[48]}
			if !slices.Equal(pageWaitIds, wantWaitIds) || page.HeadGrantWaitAttempted != 4 || page.HeadGrantWaitTimedOut != 4 || page.HeadGrantWaitCompleted != 0 {
				t.Fatalf("larger page diluted bounded grant-wait opportunities: head_visits=%d offered=%d attempted=%d timed_out=%d, want four at head offsets 0,16,32,48",
					page.HeadVisited, len(pageWaitIds), page.HeadGrantWaitAttempted, page.HeadGrantWaitTimedOut)
			}
			raw, err := json.Marshal(page.Cursor)
			server.Raise(err)
			cursor = nil
			server.Raise(json.Unmarshal(raw, &cursor))
		}
		server.Db(ctx, func(conn server.PgConn) {
			var retainedHeads, retainedForward, arrivals, terminal int
			server.Raise(conn.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1) AND failure_code='none'),
				(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($2)),
				(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($3)
					AND next_attempt_time>$4 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'),
				(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($5) AND outcome IS NOT NULL)`,
				headIds, forwardIds, newIds, cutoff, allIds).Scan(&retainedHeads, &retainedForward, &arrivals, &terminal))
			if retainedHeads != 64 || retainedForward != 128 || arrivals != 2 || terminal != 0 {
				t.Fatalf("larger-page native queue state disagreed with policy-only callbacks: heads=%d forward=%d arrivals=%d terminal=%d", retainedHeads, retainedForward, arrivals, terminal)
			}
		})
	})
}
