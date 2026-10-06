// Native queue selection controls distinguish head visits from bounded grant
// wait turns. Settlement callbacks model ownership; they do not test finances
// or prove that any production grant follows the synthetic release sequence.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Sixteen surviving heads exactly fill the per-page quota. Successful forward
// deletions and new arrivals must not hide a repeated wait allocation to an
// always-busy first head while the other grant repeatedly releases ownership.
func TestLegacySettlementHeadWaitTurnReachesPeriodicallyReleasedGrant(t *testing.T) {
	if os.Getenv("URN_LEGACY_EXPECT_WAIT_RED") != "1" {
		t.Skip("explicit policy-theory RED reproducer; not evidence of Main target ownership")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newLegacyHeadWaitTurnFixture(t, ctx, 16, false)
		for range 4 {
			page := fixture.nextPage(ctx)
			if fixture.sharedGrantWaitCount > 0 {
				fixture.requireNativeState(ctx, 16-fixture.sharedGrantWaitCount)
				return
			}
			if page.HeadVisited != 16 || page.HeadBusyOrGone != 16 || page.HeadCompleted != 0 ||
				page.Completed != 48 || page.HeadGrantWaitTimedOut != 1 {
				t.Fatalf("persistent-head setup changed before the wait-turn assertion: %+v", page)
			}
		}
		fixture.requireNativeState(ctx, 16)
		for _, id := range fixture.headIds {
			if fixture.headVisits[id] != 5 {
				t.Fatal("a retained head was skipped or repeated within the four continued pages")
			}
		}
		if fixture.sharedGrantReleaseCount != 240 || fixture.forwardCompleted != 240 ||
			len(fixture.newIds) != 4 || len(fixture.waitIds) != 4 {
			t.Fatal("successful deletion, inflow, or periodic owner-release control did not run")
		}
		if fixture.sharedGrantWaitCount == 0 {
			t.Fatalf("periodically released grant received no bounded wait turn: pages=4 head_visits=64 first_waits=%d shared_waits=0 releases=%d forward_deleted=%d post_cutoff_arrivals=%d",
				fixture.firstGrantWaitCount, fixture.sharedGrantReleaseCount, fixture.forwardCompleted, len(fixture.newIds))
		}
	})
}

// A permanently busy first owner cannot prevent later nonwaiting heads from
// completing when the shared grant stays free after its next periodic release.
func TestLegacySettlementHeadWaitTurnBusyFirstAllowsAvailableSharedGrant(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newLegacyHeadWaitTurnFixture(t, ctx, 16, false)
		fixture.reacquireSharedGrant = false
		first := fixture.nextPage(ctx)
		second := fixture.nextPage(ctx)
		if first.HeadVisited != 16 || first.HeadCompleted != 15 || first.HeadBusyOrGone != 1 ||
			first.HeadGrantWaitTimedOut != 1 || first.Completed != 63 ||
			second.HeadVisited != 1 || second.HeadCompleted != 0 || second.Completed != 63 ||
			fixture.firstGrantWaitCount != 2 || fixture.sharedGrantWaitCount != 0 || fixture.forwardCompleted != 159 {
			t.Fatalf("permanent first owner prevented available shared heads from completing: first=%+v second=%+v", first, second)
		}
		fixture.requireNativeState(ctx, 1)
	})
}

// A completed first head really disappears from the indexed queue. Its removal
// changes the retained cycle and naturally transfers the next page's wait turn.
func TestLegacySettlementHeadWaitTurnRotatesAfterSuccessfulHeadDeletion(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newLegacyHeadWaitTurnFixture(t, ctx, 16, true)
		for turn := range 3 {
			page := fixture.nextPage(ctx)
			if page.HeadVisited != 16-turn || page.HeadCompleted != 1 || page.HeadBusyOrGone != 15-turn ||
				page.HeadGrantWaitCompleted != 1 || page.HeadGrantWaitTimedOut != 0 ||
				len(fixture.waitIds) != turn+1 || fixture.waitIds[turn] != fixture.headIds[turn] {
				t.Fatalf("successful first-head removal did not rotate the next wait recipient: turn=%d page=%+v", turn, page)
			}
		}
		fixture.requireNativeState(ctx, 13)
		if fixture.firstGrantWaitCount != 1 || fixture.sharedGrantWaitCount != 2 || fixture.forwardCompleted != 195 {
			t.Fatal("head deletions did not preserve independent forward progress")
		}
	})
}

// With seventeen retained heads, the second page begins at the seventeenth
// row. The first owner may remain busy while another grant gets a wait turn.
func TestLegacySettlementHeadWaitTurnChangesWithNonMultipleCohort(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := newLegacyHeadWaitTurnFixture(t, ctx, 17, false)
		first := fixture.nextPage(ctx)
		second := fixture.nextPage(ctx)
		if first.HeadVisited != 16 || first.HeadCompleted != 0 || first.HeadGrantWaitTimedOut != 1 ||
			second.HeadVisited != 16 || second.HeadCompleted != 1 || second.HeadGrantWaitCompleted != 1 ||
			len(fixture.waitIds) != 2 || fixture.waitIds[0] != fixture.headIds[0] || fixture.waitIds[1] != fixture.headIds[16] {
			t.Fatalf("seventeen-row cycle did not transfer the bounded wait: first=%+v second=%+v", first, second)
		}
		fixture.requireNativeState(ctx, 16)
		if fixture.firstGrantWaitCount != 1 || fixture.sharedGrantWaitCount != 1 || fixture.forwardCompleted != 143 {
			t.Fatal("changed head cycle lost its busy-first or successful-forward control")
		}
	})
}

// The fixture owns only synthetic queue state and a callback grant model.
// Native selection, input/output cursor serialization, committed successful
// deletions, and post-cutoff arrivals use the unmodified production page loop.
type legacyHeadWaitTurnFixture struct {
	t                       testing.TB
	network                 netEscrowOrderingTestFixture
	headIds                 []server.Id
	headVisits              map[server.Id]int
	forwardIds              []server.Id
	newIds                  []server.Id
	deletedIds              []server.Id
	waitIds                 []server.Id
	idPrefix                server.Id
	nextSequence            uint32
	cursor                  *LegacySettlementCursor
	cutoff                  time.Time
	completeFirst           bool
	sharedGrantOwned        bool
	reacquireSharedGrant    bool
	sharedGrantReleaseCount int
	firstGrantWaitCount     int
	sharedGrantWaitCount    int
	forwardCompleted        int
}

// Establish the incoming cursor through a real initial page: all old heads
// are busy, and its remaining forward rows complete and disappear. The fixed
// original cohort is large enough for every asserted continued page.
func newLegacyHeadWaitTurnFixture(t testing.TB, ctx context.Context, headCount int, completeFirst bool) *legacyHeadWaitTurnFixture {
	t.Helper()
	fixture := &legacyHeadWaitTurnFixture{
		t:                    t,
		network:              newNetEscrowOrderingTestFixture(t, ctx),
		headIds:              make([]server.Id, headCount),
		headVisits:           map[server.Id]int{},
		forwardIds:           make([]server.Id, 256),
		idPrefix:             server.NewId(),
		completeFirst:        completeFirst,
		sharedGrantOwned:     true,
		reacquireSharedGrant: true,
	}
	ids := make([]server.Id, 0, headCount+len(fixture.forwardIds))
	for index := range fixture.headIds {
		id := fixture.nextId()
		fixture.headIds[index] = id
		fixture.headVisits[id] = 0
		ids = append(ids, id)
	}
	for index := range fixture.forwardIds {
		id := fixture.nextId()
		fixture.forwardIds[index] = id
		ids = append(ids, id)
	}
	oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
	times := make([]time.Time, len(ids))
	for index := range times {
		times[index] = oldest.Add(time.Duration(index) * time.Second)
	}
	fixture.enqueue(ctx, ids, times)
	page, err := flushLegacySettlementsPage(ctx, ctx, 1, nil, 64, fixture.settle)
	if err != nil || page.Visited != 64 || page.HeadVisited != 0 || page.BusyOrGone != headCount ||
		page.Completed != 64-headCount || page.Cursor == nil || !page.More || page.Cursor.HeadAfter != nil ||
		page.Cursor.ContractId != fixture.forwardIds[63-headCount] {
		t.Fatalf("initial page did not establish a real retained head cycle: %+v err=%v", page, err)
	}
	fixture.cursor = page.Cursor
	fixture.cutoff = page.Cursor.PassEndTime
	return fixture
}

// Keep every identity unique after applying the synthetic shard byte; no random
// low-byte truncation can turn fixture setup into a duplicate-primary-key failure.
func (self *legacyHeadWaitTurnFixture) nextId() server.Id {
	self.nextSequence++
	id := self.idPrefix
	binary.BigEndian.PutUint32(id[11:15], self.nextSequence)
	id[15] = 1
	return id
}

// Zero-byte contracts are enough for this queue-policy control. They remain
// open and unproved; callback success models only deletion of a selected intent.
func (self *legacyHeadWaitTurnFixture) enqueue(ctx context.Context, ids []server.Id, times []time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
			(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count)
			SELECT id,$2,$3,$4,$5,$2,0 FROM unnest($1::uuid[]) AS entry(id)`,
			ids, self.network.sourceNetworkId, self.network.sourceId, self.network.destinationNetworkId, self.network.destinationId))
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent
			(contract_id,shard,outcome,clear_dispute,next_attempt_time)
			SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS entry(id,due)`, ids, times))
	})
}

// A common grant is owned at each head arrival, released between forward
// successes, then acquired by its next synthetic owner. A queued waiter gets
// the next release; a nonwaiting visit returns busy and has no claim on it.
func (self *legacyHeadWaitTurnFixture) settle(ctx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
	if _, head := self.headVisits[id]; head {
		self.headVisits[id]++
		if wait != nil {
			self.waitIds = append(self.waitIds, id)
			wait.attempted = true
		}
		if id == self.headIds[0] {
			if wait == nil {
				return false, true, legacySettlementBusyGrantSet, nil
			}
			self.firstGrantWaitCount++
			if !self.completeFirst {
				wait.timedOut = true
				return false, true, legacySettlementBusyGrantSet, nil
			}
		} else {
			if self.sharedGrantOwned && wait == nil {
				return false, true, legacySettlementBusyGrantSet, nil
			}
			if wait != nil {
				self.sharedGrantWaitCount++
			}
			self.sharedGrantOwned = false
		}
	} else {
		self.forwardCompleted++
		self.sharedGrantOwned = false
		self.sharedGrantReleaseCount++
	}
	server.Tx(ctx, func(tx server.PgTx) {
		deleted := server.RaisePgResult(tx.Exec(ctx, `DELETE FROM legacy_settlement_intent WHERE contract_id=$1`, id)).RowsAffected()
		if deleted != 1 {
			self.t.Fatal("successful callback repeated a previously deleted intent")
		}
	})
	self.deletedIds = append(self.deletedIds, id)
	self.sharedGrantOwned = self.reacquireSharedGrant
	return true, false, legacySettlementBusyNone, nil
}

// Every continuation gets a genuinely new due arrival. The original cutoff
// excludes it, while serialization preserves exactly the returned head cursor.
func (self *legacyHeadWaitTurnFixture) nextPage(ctx context.Context) LegacySettlementFlushResult {
	self.t.Helper()
	id := self.nextId()
	var due time.Time
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT clock_timestamp() AT TIME ZONE 'UTC'`).Scan(&due))
	})
	if !self.cutoff.Before(due) {
		self.t.Fatal("new arrival did not follow the first selection's fixed cutoff")
	}
	self.enqueue(ctx, []server.Id{id}, []time.Time{due})
	self.newIds = append(self.newIds, id)
	before, err := json.Marshal(self.cursor)
	server.Raise(err)
	page, err := flushLegacySettlementsPage(ctx, ctx, 1, self.cursor, 64, self.settle)
	after, marshalErr := json.Marshal(self.cursor)
	server.Raise(marshalErr)
	if err != nil || page.Visited != 64 || page.Failed != 0 || !page.More || page.Cursor == nil ||
		!page.Cursor.PassEndTime.Equal(self.cutoff) || !bytes.Equal(before, after) {
		self.t.Fatalf("continued page lost its fixed cohort, immutable input, or forward progress: %+v err=%v", page, err)
	}
	raw, err := json.Marshal(page.Cursor)
	server.Raise(err)
	self.cursor = nil
	server.Raise(json.Unmarshal(raw, &self.cursor))
	return page
}

// Compare every deleted identity against committed native queue state. Surviving
// heads stay clean, later arrivals remain, and no callback creates financial proof
// or terminal state that would bypass the V14 usage guard.
func (self *legacyHeadWaitTurnFixture) requireNativeState(ctx context.Context, retainedHeads int) {
	self.t.Helper()
	allIds := slices.Concat(self.headIds, self.forwardIds, self.newIds)
	wantDeletedIds := slices.Clone(self.deletedIds)
	slices.SortFunc(wantDeletedIds, func(a, b server.Id) int { return a.Cmp(b) })
	server.Db(ctx, func(conn server.PgConn) {
		var headCount, forwardCount, arrivals, changedProofOrOutcome, contracts int
		server.Raise(conn.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($1) AND failure_code='none'),
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($2)),
			(SELECT count(*) FROM legacy_settlement_intent WHERE contract_id=ANY($3)
				AND next_attempt_time>$4 AND next_attempt_time<=statement_timestamp() AT TIME ZONE 'UTC'),
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($5)
				AND (outcome IS NOT NULL OR close_time IS NOT NULL OR provider_usage IS NOT NULL)),
			(SELECT count(*) FROM transfer_contract WHERE contract_id=ANY($5))`,
			self.headIds, self.forwardIds, self.newIds, self.cutoff, allIds).Scan(&headCount, &forwardCount, &arrivals, &changedProofOrOutcome, &contracts))
		if headCount != retainedHeads || forwardCount != len(self.forwardIds)-self.forwardCompleted || arrivals != len(self.newIds) ||
			changedProofOrOutcome != 0 || contracts != len(allIds) {
			self.t.Fatalf("native deletion/inflow control failed: retained_heads=%d retained_forward=%d arrivals=%d proof_or_outcome=%d contracts=%d",
				headCount, forwardCount, arrivals, changedProofOrOutcome, contracts)
		}
		deletedIds := []server.Id{}
		rows, err := conn.Query(ctx, `SELECT expected.contract_id
			FROM unnest($1::uuid[]) AS expected(contract_id)
			LEFT JOIN legacy_settlement_intent AS intent USING(contract_id)
			WHERE intent.contract_id IS NULL ORDER BY expected.contract_id`, allIds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				server.Raise(rows.Scan(&id))
				deletedIds = append(deletedIds, id)
			}
		})
		if !slices.Equal(deletedIds, wantDeletedIds) {
			self.t.Fatal("native deleted intent identities disagree with committed callback successes")
		}
	})
}
