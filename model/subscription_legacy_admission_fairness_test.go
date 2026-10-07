// Real ownership and financial commits distinguish optional admission hints
// from the PG opportunity retained by fresh pages and continued head revisits.
package model

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/task"
)

// The scope is local to this control; unrelated Redis work cannot satisfy an
// admission assertion. No command or reply is replaced by the observing hook.
type legacyAdmissionFairnessVisitKey struct{}

type legacyAdmissionFairnessVisit struct {
	observer *legacyAdmissionFairnessObserver
	head     bool
}

// Count the actual admission command separately for the two page lanes. The
// zero-head assertion remains causal even if a fixture lease happens to expire.
type legacyAdmissionFairnessObserver struct {
	headAcquires    atomic.Int64
	forwardAcquires atomic.Int64
}

func (self *legacyAdmissionFairnessObserver) DialHook(next redis.DialHook) redis.DialHook {
	return next
}

func (self *legacyAdmissionFairnessObserver) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		visit, _ := ctx.Value(legacyAdmissionFairnessVisitKey{}).(legacyAdmissionFairnessVisit)
		args := command.Args()
		if visit.observer == self && command.Name() == "eval" && len(args) > 1 && args[1] == legacySettlementAdmissionAcquireLua {
			if visit.head {
				self.headAcquires.Add(1)
			} else {
				self.forwardAcquires.Add(1)
			}
		}
		return next(ctx, command)
	}
}

func (self *legacyAdmissionFairnessObserver) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A real initial page retains sixteen heads under two PG locks. Once only the
// shared grant releases, all fifteen nonwaiting heads must settle despite fresh
// Redis owners. Successful deletion naturally changes the second head cycle;
// the test neither pins that cycle nor changes the original wait allocation.
func TestLegacySettlementAdmissionHeadRevisitBypassesRepeatedLease(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
		defer cancel()
		f := newLegacyAdmissionFairnessFixture(t, ctx)
		firstConn := acquireContractLifecycleTestConnection(t, ctx)
		defer firstConn.Release()
		firstHeld, err := firstConn.Begin(ctx)
		server.Raise(err)
		defer firstHeld.Rollback(context.Background())
		server.RaisePgResult(firstHeld.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.grants[0].balanceId))
		sharedConn := acquireContractLifecycleTestConnection(t, ctx)
		defer sharedConn.Release()
		sharedHeld, err := sharedConn.Begin(ctx)
		server.Raise(err)
		defer sharedHeld.Rollback(context.Background())
		server.RaisePgResult(sharedHeld.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE`, f.grants[1].balanceId))

		initial, err := flushLegacySettlementsPage(ctx, ctx, 1, nil, 64, flushLegacySettlementWithGrantWait)
		if err != nil || initial.Visited != 64 || initial.Completed != 48 || initial.BusyGrantSetMismatch != 16 ||
			initial.BusyOrGone != 16 || initial.BusyAdmissionDeferred != 0 || initial.Failed != 0 || initial.HeadVisited != 0 ||
			!initial.More || initial.Cursor == nil || initial.Cursor.ContractId != f.forwardIds[47] || initial.Cursor.HeadAfter != nil {
			t.Fatalf("real grant locks did not establish the retained sixteen-head input: %+v err=%v", initial, err)
		}
		wantCompleted := map[server.Id]bool{}
		for _, id := range f.forwardIds[:48] {
			wantCompleted[id] = true
		}
		f.requireState(ctx, wantCompleted)
		// Only ownership changes: no contract, report, grant value, intent or
		// due tuple is repaired to create the healthy head opportunity.
		server.Raise(sharedHeld.Rollback(ctx))
		observer := &legacyAdmissionFairnessObserver{}
		installCtx, stopInstall := context.WithTimeout(ctx, time.Second)
		server.Raise(server.RedisWithDeadline(installCtx, func(client server.RedisClient) error {
			client.AddHook(observer)
			return nil
		}))
		stopInstall()
		var lastLease *legacySettlementAdmission
		defer func() { lastLease.release(ctx) }()
		leaseInstalls := 0
		cursor := initial.Cursor
		cutoff := cursor.PassEndTime
		for turn := range 2 {
			encoded, err := json.Marshal(cursor)
			server.Raise(err)
			var input *LegacySettlementCursor
			server.Raise(json.Unmarshal(encoded, &input))
			headIds := map[server.Id]bool{}
			for _, id := range f.ids {
				if !wantCompleted[id] && !f.due[id].After(input.NextAttemptTime) {
					headIds[id] = true
				}
			}
			seen := map[server.Id]bool{}
			settle := func(callCtx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
				if seen[id] {
					t.Fatal("continued page revisited the same native intent twice")
				}
				seen[id] = true
				// Each callback installs a new real two-second token. Progress
				// never waits for expiry, and this seam neither reports a fake
				// PG owner nor replaces the actual financial function below.
				lastLease = f.installLease(callCtx)
				leaseInstalls++
				marked := context.WithValue(callCtx, legacyAdmissionFairnessVisitKey{}, legacyAdmissionFairnessVisit{observer: observer, head: headIds[id]})
				return flushLegacySettlementWithGrantWait(marked, id, wait)
			}
			page, err := flushLegacySettlementsPage(ctx, ctx, 1, input, 64, settle)
			unchanged, marshalErr := json.Marshal(input)
			server.Raise(marshalErr)
			if !bytes.Equal(encoded, unchanged) || err != nil || page.Visited != 64 || page.HeadVisited != 16 || page.Failed != 0 ||
				!page.More || page.Cursor == nil || !page.Cursor.PassEndTime.Equal(cutoff) || page.Cursor.HeadAfter == nil {
				t.Fatalf("continued page lost its immutable fixed cohort or bounded head share: turn=%d page=%+v err=%v", turn, page, err)
			}
			if observer.headAcquires.Load() != 0 {
				t.Fatalf("head revisit consulted a Redis admission hint instead of its existing PostgreSQL opportunity: head_acquires=%d", observer.headAcquires.Load())
			}
			if turn == 0 {
				if page.HeadCompleted != 15 || page.HeadBusyOrGone != 1 || page.HeadGrantWaitAttempted != 1 || page.HeadGrantWaitTimedOut != 1 ||
					page.HeadGrantWaitCompleted != 0 || page.Completed != 40 || page.BusyOrGone != 24 || page.BusyGrantSetMismatch != 1 ||
					page.BusyAdmissionDeferred != 23 || page.HeadBusyAdmissionDeferred != 0 ||
					page.Cursor.ContractId != f.forwardIds[95] || page.Cursor.HeadAfter.ContractId != f.headIds[15] {
					t.Fatalf("free PG heads or disjoint forward work failed under renewed hints: %+v", page)
				}
				for _, id := range f.headIds[1:] {
					wantCompleted[id] = true
				}
				wantCompleted[f.forwardIds[48]] = true
				for index := 49; index < 96; index += 2 {
					wantCompleted[f.forwardIds[index]] = true
				}
			} else {
				if page.HeadCompleted != 16 || page.HeadBusyOrGone != 0 || page.HeadGrantWaitAttempted != 1 || page.HeadGrantWaitCompleted != 1 ||
					page.HeadGrantWaitTimedOut != 0 || page.Completed != 64 || page.BusyOrGone != 0 || page.BusyAdmissionDeferred != 0 ||
					page.Cursor.ContractId != f.forwardIds[143] || page.Cursor.HeadAfter.ContractId != f.forwardIds[80] {
					t.Fatalf("deferred forward rows lost their next-page PG head opportunity: %+v", page)
				}
				for index := 50; index <= 80; index += 2 {
					wantCompleted[f.forwardIds[index]] = true
				}
				for _, id := range f.forwardIds[96:] {
					wantCompleted[id] = true
				}
			}
			if !page.Cursor.NextAttemptTime.Equal(f.due[page.Cursor.ContractId]) ||
				!page.Cursor.HeadAfter.NextAttemptTime.Equal(f.due[page.Cursor.HeadAfter.ContractId]) || observer.forwardAcquires.Load() != int64(48*(turn+1)) {
				t.Fatal("renewed-lease control changed a due tuple or did not exercise every forward admission")
			}
			f.requireState(ctx, wantCompleted)
			cursor = page.Cursor
		}
		if leaseInstalls != 128 || len(wantCompleted) != 152 {
			t.Fatal("repeated lease and successful deletion controls did not execute exactly")
		}
		// Explicit token-checked release, never TTL expiry, admits the last
		// eight rows through the ordinary first-pass financial path.
		lastLease.release(ctx)
		lastLease = nil
		server.Raise(firstHeld.Rollback(ctx))
		final, err := FlushLegacySettlements(ctx, 1, nil, LegacySettlementPageLimit)
		if err != nil || final.Visited != 8 || final.Completed != 8 || final.BusyOrGone != 0 || final.Failed != 0 || final.Cursor != nil {
			t.Fatalf("explicit PG/Redis owner release did not drain the same remaining rows: %+v err=%v", final, err)
		}
		for _, id := range f.ids {
			wantCompleted[id] = true
		}
		f.requireState(ctx, wantCompleted)
		if replay, err := FlushLegacySettlements(ctx, 1, nil, LegacySettlementPageLimit); err != nil || replay.Visited != 0 {
			t.Fatalf("empty replay repeated a committed financial transition: %+v err=%v", replay, err)
		}
		f.requireState(ctx, wantCompleted)
		t.Log("native fairness control: two real PG owners establish sixteen heads; release only G1; 128 callback-boundary Redis leases; zero head admission reads; deferred forward rows settle at head; unchanged real finance and replay. This does not change or qualify the existing first-head wait-turn policy.")
	})
}

// Fresh pages bypass optional admission before either the lookup or acquire.
// A real intent owner cannot hide the two healthy same-grant rows behind it,
// even when an independent Redis owner is renewed before every callback.
func TestLegacySettlementAdmissionNilCursorSkipsHintsBeforeBusyIntent(t *testing.T) {
	if os.Getenv("URN_LEGACY_REQUIRE_PGSS") != "1" {
		t.Skip("isolated statement statistics are required for the fresh-page lookup control")
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
		})
		f := newLegacyAdmissionFairnessFixtureWithGrants(t, ctx, []int{1, 1, 1})
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT contract_id FROM legacy_settlement_intent WHERE contract_id=$1 FOR UPDATE`, f.ids[0]))
		observer := &legacyAdmissionFairnessObserver{}
		server.Raise(server.RedisWithDeadline(ctx, func(client server.RedisClient) error {
			client.AddHook(observer)
			return nil
		}))
		var lastLease *legacySettlementAdmission
		defer func() { lastLease.release(ctx) }()
		leaseInstalls := 0
		var pageVisitIds []server.Id
		actualCompleted := map[server.Id]bool{}
		settle := func(callCtx context.Context, id server.Id, wait *legacySettlementGrantWait) (bool, bool, legacySettlementBusyGate, error) {
			if wait != nil {
				t.Fatal("short nil-cursor control unexpectedly allocated a head wait")
			}
			pageVisitIds = append(pageVisitIds, id)
			lastLease = f.installLease(callCtx)
			leaseInstalls++
			marked := context.WithValue(callCtx, legacyAdmissionFairnessVisitKey{}, legacyAdmissionFairnessVisit{observer: observer, head: false})
			completed, busy, gate, err := flushLegacySettlementWithGrantWait(marked, id, wait)
			if completed && err == nil {
				actualCompleted[id] = true
			}
			return completed, busy, gate, err
		}
		wantCompleted := map[server.Id]bool{}
		for turn, wantVisits := range [][]server.Id{f.ids, f.ids[:1]} {
			// The ceiling exceeds the cohort. The actual EOF result, rather
			// than a fabricated cursor, determines the next fresh page.
			pageVisitIds = nil
			beforeSql := legacyTargetSqlSnapshot(t, ctx)
			page, err := flushLegacySettlementsPage(ctx, ctx, 1, nil, 64, settle)
			delta := legacyTargetSqlDelta(t, beforeSql, legacyTargetSqlSnapshot(t, ctx))
			if delta == nil {
				t.Fatal("fresh-page lookup control requires qualified native statement counters")
			}
			// Qualify actual financial custody before the expected RED. The
			// snapshot is already closed, so proof reads and replay cannot
			// contaminate the measured page's statement families.
			f.requireState(ctx, actualCompleted)
			for _, id := range f.ids {
				if actualCompleted[id] {
					completed, busy, _, replayErr := flushLegacySettlement(ctx, id)
					if replayErr != nil || completed || !busy {
						t.Fatal("committed fresh-page identity lost its replay fence", id, replayErr)
					}
				}
			}
			f.requireState(ctx, actualCompleted)
			var lookupCalls, lookupRows float64
			for _, statement := range delta.Statements {
				compact := strings.ReplaceAll(strings.ToLower(strings.Join(strings.Fields(statement.Query), " ")), " ", "")
				if statement.TopLevel && strings.HasPrefix(compact, "selectbalance_idfromtransfer_escrowwherecontract_id=$1orderbybalance_idlimit") {
					lookupCalls += statement.Metrics["calls"]
					lookupRows += statement.Metrics["rows"]
				}
			}
			// Observe actual work, not its hint result. Expiry cannot make an
			// incorrect acquire path pass this test. PGSS records completed
			// statements; these exact families require a healthy native sample.
			if lookupCalls != 0 || lookupRows != 0 || observer.forwardAcquires.Load() != 0 || observer.headAcquires.Load() != 0 {
				t.Fatalf("fresh nil-cursor page performed optional admission work: turn=%d lookup_calls=%v lookup_rows=%v acquire_evals=%d",
					turn, lookupCalls, lookupRows, observer.forwardAcquires.Load()+observer.headAcquires.Load())
			}
			wantBusy := 1 - turn
			wantSettled := 2 - turn
			if err != nil || page.Visited != len(wantVisits) || page.Completed != wantSettled || page.Failed != 0 || page.HeadVisited != 0 ||
				page.HeadGrantWaitAttempted != 0 || page.BusyGrantSetMismatch != 0 || page.BusyContractUnavailable != 0 ||
				page.BusyOrGone != wantBusy || page.BusyIntentUnavailable != wantBusy || page.BusyAdmissionDeferred != 0 || page.More || page.Cursor != nil {
				t.Fatalf("fresh nil-cursor page changed ordinary PG progress: turn=%d page=%+v err=%v", turn, page, err)
			}
			if len(pageVisitIds) != len(wantVisits) {
				t.Fatal("fresh-page callback inventory changed")
			}
			for index, id := range wantVisits {
				if pageVisitIds[index] != id {
					t.Fatal("fresh-page callback skipped or reordered an exact due identity", turn, index)
				}
			}
			legacyTargetRequireSqlFamily(t, delta, "intent_ownership", float64(len(wantVisits)), float64(wantSettled))
			for _, family := range []string{"contract_ownership", "grant_membership", "grant_ownership", "outcome_write", "grant_debit"} {
				legacyTargetRequireSqlFamily(t, delta, family, float64(wantSettled), float64(wantSettled))
			}
			if turn == 0 {
				wantCompleted[f.ids[1]] = true
				wantCompleted[f.ids[2]] = true
			} else {
				wantCompleted[f.ids[0]] = true
			}
			f.requireState(ctx, wantCompleted)
			if turn == 0 {
				// Release only the real intent owner. The next callback still
				// installs a new Redis token before normal financial ownership.
				server.Raise(held.Rollback(ctx))
			}
		}
		if leaseInstalls != 4 {
			t.Fatal("short-cohort repeated-lease control did not visit every expected identity")
		}
		if replay, err := flushLegacySettlementsPage(ctx, ctx, 1, nil, 64, settle); err != nil || replay.Visited != 0 || leaseInstalls != 4 {
			t.Fatalf("short-cohort replay repeated a retired financial identity: %+v err=%v", replay, err)
		}
		f.requireState(ctx, wantCompleted)
		t.Log("native fresh-page control: real first intent owner; two eligible rows settle on the first EOF page, then explicit intent release permits the last row; four renewed Redis hints; zero admission lookup calls and acquire EVALs; exact ownership SQL, financial identities, proof and replay conservation. No head visits or lease-expiry assumption; PGSS excludes aborted statements.")
	})
}

// Seed native funded rows once. Later interventions affect only the two real
// owner transactions and the test's independent Redis admission key.
type legacyAdmissionFairnessFixture struct {
	t          testing.TB
	grants     []netEscrowOrderingTestFixture
	ids        []server.Id
	headIds    []server.Id
	forwardIds []server.Id
	grantById  map[server.Id]int
	due        map[server.Id]time.Time
	proofs     map[server.Id][]byte
	reports    []byte
}

func newLegacyAdmissionFairnessFixture(t testing.TB, ctx context.Context) *legacyAdmissionFairnessFixture {
	t.Helper()
	assignments := make([]int, 160)
	for index := range assignments {
		grant := 2
		if index == 0 {
			grant = 0
		} else if index < 16 || (index >= 64 && index < 112 && index%2 == 0) {
			grant = 1
		}
		assignments[index] = grant
	}
	f := newLegacyAdmissionFairnessFixtureWithGrants(t, ctx, assignments)
	f.headIds, f.forwardIds = f.ids[:16], f.ids[16:]
	return f
}

// A small cohort uses the identical financial seed and state oracle without
// manufacturing a continuation cursor or relying on a head-lane opportunity.
func newLegacyAdmissionFairnessFixtureWithGrants(t testing.TB, ctx context.Context, assignments []int) *legacyAdmissionFairnessFixture {
	t.Helper()
	f := &legacyAdmissionFairnessFixture{t: t, ids: make([]server.Id, len(assignments)), grantById: map[server.Id]int{}, due: map[server.Id]time.Time{}, proofs: map[server.Id][]byte{}}
	for range 3 {
		f.grants = append(f.grants, newNetEscrowOrderingTestFixture(t, ctx))
	}
	prefix := server.NewId()
	oldest := time.Date(2010, time.January, 1, 0, 0, 0, 0, time.UTC)
	for index, grant := range assignments {
		id := prefix
		binary.BigEndian.PutUint32(id[11:15], uint32(index+1))
		id[15] = 1
		f.ids[index] = id
		f.grantById[id] = grant
		f.due[id] = oldest.Add(time.Duration(index) * time.Second)
	}
	server.Tx(ctx, func(tx server.PgTx) {
		for grantIndex, grant := range f.grants {
			ids, due := []server.Id{}, []time.Time{}
			for _, id := range f.ids {
				if f.grantById[id] == grantIndex {
					ids, due = append(ids, id), append(due, f.due[id])
				}
			}
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_balance SET net_revenue_nano_cents=2000 WHERE balance_id=$1`, grant.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
				(contract_id,source_network_id,source_id,destination_network_id,destination_id,payer_network_id,transfer_byte_count,usage_origin_is_source)
				SELECT id,$2,$3,$4,$5,$2,2,true FROM unnest($1::uuid[]) AS seed(id)`, ids, grant.sourceNetworkId, grant.sourceId, grant.destinationNetworkId, grant.destinationId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
				SELECT id,$2,2 FROM unnest($1::uuid[]) AS seed(id)`, ids, grant.balanceId))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,close_time,checkpoint)
				SELECT id,party,1,statement_timestamp() AT TIME ZONE 'UTC',false FROM unnest($1::uuid[]) AS seed(id)
				CROSS JOIN (VALUES ('source'),('destination')) AS parties(party)`, ids))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO legacy_settlement_intent(contract_id,shard,outcome,clear_dispute,next_attempt_time)
				SELECT id,1,'settled',false,due FROM unnest($1::uuid[],$2::timestamp[]) AS seed(id,due)`, ids, due))
		}
	})
	refreshNetEscrow(ctx, []server.Id{f.grants[0].balanceId, f.grants[1].balanceId, f.grants[2].balanceId})
	f.reports = f.readReports(ctx)
	f.requireState(ctx, map[server.Id]bool{})
	return f
}

// SET and its TTL witness share one real Redis command. The caller renews it
// at the callback boundary; neither sleeps nor expiry allow head progress.
func (self *legacyAdmissionFairnessFixture) installLease(ctx context.Context) *legacySettlementAdmission {
	id := server.NewId()
	owner := &legacySettlementAdmission{token: "v1:" + hex.EncodeToString(id[:]), keys: []string{legacySettlementAdmissionKey(self.grants[1].balanceId)}}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	err := server.RedisWithDeadline(bounded, func(client server.RedisClient) error {
		ttl, err := client.Eval(bounded, `redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2]); return redis.call('PTTL',KEYS[1])`, owner.keys, owner.token, legacySettlementAdmissionLease.Milliseconds()).Int64()
		if err == nil && (ttl <= 0 || ttl > legacySettlementAdmissionLease.Milliseconds()) {
			self.t.Fatal("native repeated-lease setup did not install a bounded current token")
		}
		return err
	})
	if err != nil {
		owner.release(ctx)
		self.t.Fatal("native repeated-lease setup failed", err)
	}
	return owner
}

// Report inputs must remain byte-identical through busy visits, financial
// settlement and replay; this observer never changes them.
func (self *legacyAdmissionFairnessFixture) readReports(ctx context.Context) []byte {
	var raw []byte
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_agg(jsonb_build_array(contract_id,party,used_transfer_byte_count,checkpoint,close_time)
			ORDER BY contract_id,party) FROM contract_close WHERE contract_id=ANY($1)`, self.ids).Scan(&raw))
	})
	return raw
}

// Expected identities come from the prescribed page order, not callback
// success counts. Verify queue tuples, immutable usage, exact debit, retained
// reservations, sweeps and the durable provider allocations for each grant.
func (self *legacyAdmissionFairnessFixture) requireState(ctx context.Context, expected map[server.Id]bool) {
	self.t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		seen := map[server.Id]bool{}
		rows, err := conn.Query(ctx, `SELECT contract.contract_id,contract.outcome,contract.close_time,contract.provider_usage,
			escrow.settled,escrow.payout_byte_count,intent.next_attempt_time,intent.failure_code
			FROM transfer_contract AS contract INNER JOIN transfer_escrow AS escrow USING(contract_id)
			LEFT JOIN legacy_settlement_intent AS intent USING(contract_id) WHERE contract.contract_id=ANY($1)`, self.ids)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var id server.Id
				var outcome, failure *string
				var closed, due *time.Time
				var proof []byte
				var settled bool
				var paid *int64
				server.Raise(rows.Scan(&id, &outcome, &closed, &proof, &settled, &paid, &due, &failure))
				if seen[id] {
					self.t.Fatal("native fixture changed its one-escrow identity")
				}
				seen[id] = true
				if !expected[id] {
					if outcome != nil || closed != nil || len(proof) != 0 || settled || paid != nil || due == nil || !due.Equal(self.due[id]) || failure == nil || *failure != "none" {
						self.t.Fatal("deferred row changed its clean due tuple, proof or finances", id)
					}
					continue
				}
				if outcome == nil || *outcome != "settled" || closed == nil || !settled || paid == nil || *paid != 1 || due != nil || failure != nil {
					self.t.Fatal("expected financial commit did not retire the exact intent and escrow", id)
				}
				usage, err := decodeContractUsageSnapshot(proof)
				grant := self.grants[self.grantById[id]]
				if err != nil || usage == nil || usage.ByteCount != 1 || len(usage.Providers) != 1 || usage.Providers[0].NetworkId != grant.destinationNetworkId ||
					usage.Providers[0].ClientId != grant.destinationId || usage.Providers[0].ByteCount != 1 {
					self.t.Fatal("normal financial settlement lost exact provider usage", id, err)
				}
				if prior, found := self.proofs[id]; found && !bytes.Equal(prior, proof) {
					self.t.Fatal("a later page or replay changed immutable financial proof", id)
				}
				self.proofs[id] = bytes.Clone(proof)
			}
		})
		if len(seen) != len(self.ids) {
			self.t.Fatal("native fixture lost contract or escrow identities")
		}
	})
	for index, grant := range self.grants {
		cohort, complete := 0, 0
		for _, id := range self.ids {
			if self.grantById[id] == index {
				cohort++
				if expected[id] {
					complete++
				}
			}
		}
		server.Db(ctx, func(conn server.PgConn) {
			var credit, sweeps, sweptBytes, sweptRevenue, providerBytes, providerRevenue int64
			server.Raise(conn.QueryRow(ctx, `WITH unapplied AS (
				SELECT allocation FROM pending_task CROSS JOIN LATERAL jsonb_array_elements(args_json::jsonb->'totals') AS allocation
				WHERE function_name=$3 AND (args_json::jsonb->>'applied')::boolean=false AND (allocation->>'network_id')::uuid=$2
			) SELECT (SELECT balance_byte_count FROM transfer_balance WHERE balance_id=$1),
				(SELECT count(*) FROM transfer_escrow_sweep WHERE balance_id=$1),
				(SELECT COALESCE(sum(payout_byte_count),0) FROM transfer_escrow_sweep WHERE balance_id=$1),
				(SELECT COALESCE(sum(payout_net_revenue_nano_cents),0) FROM transfer_escrow_sweep WHERE balance_id=$1),
				COALESCE((SELECT provided_byte_count FROM account_balance WHERE network_id=$2),0)+COALESCE((SELECT sum((allocation->>'bytes')::bigint) FROM unapplied),0),
				COALESCE((SELECT provided_net_revenue_nano_cents FROM account_balance WHERE network_id=$2),0)+COALESCE((SELECT sum((allocation->>'revenue')::bigint) FROM unapplied),0)`,
				grant.balanceId, grant.destinationNetworkId, task.NewTaskTarget(ApplyLegacyProviderTotals).TargetFunctionName()).Scan(&credit, &sweeps, &sweptBytes, &sweptRevenue, &providerBytes, &providerRevenue))
			if credit != int64(1000-complete) || sweeps != int64(complete) || sweptBytes != int64(complete) || sweptRevenue != int64(complete) || providerBytes != int64(complete) || providerRevenue != int64(complete) {
				self.t.Fatal("grant-specific debit, sweep or durable provider conservation failed", index, complete, credit, sweeps, sweptBytes, sweptRevenue, providerBytes, providerRevenue)
			}
		})
		if Testing_NetEscrowByteCount(ctx, grant.balanceId) != ByteCount(2*(cohort-complete)) {
			self.t.Fatal("grant-specific unsettled reservations changed", index)
		}
	}
	if !bytes.Equal(self.reports, self.readReports(ctx)) {
		self.t.Fatal("ownership-only intervention or replay changed a source report")
	}
}
