// Rolling-index controls retain real chronological financial ownership and posts.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// Positive and negative observations expire from admission, and one entry cannot
// leak readiness into another database identity or retain an unbounded history.
func TestLegacySettlementPayerIndexCacheExpiryAndIdentity(t *testing.T) {
	cache := &legacySettlementPayerIndexCache{}
	instant := time.Unix(100, 0)
	now := func() time.Time { return instant }
	first, second := sha256.Sum256([]byte("first.example")), sha256.Sum256([]byte("second.example"))
	calls, answer := 0, true
	check := func(context.Context) (bool, error) { calls++; return answer, nil }
	if !cache.load(t.Context(), first, now, check) || calls != 1 {
		t.Fatal("initial readiness was not checked")
	}
	instant = instant.Add(legacySettlementPayerIndexCacheLifetime - time.Nanosecond)
	answer = false
	if !cache.load(t.Context(), first, now, check) || calls != 1 {
		t.Fatal("unexpired readiness was not cached")
	}
	instant = instant.Add(time.Nanosecond)
	if cache.load(t.Context(), first, now, check) || calls != 2 {
		t.Fatal("positive readiness survived its exact expiry")
	}
	answer = true
	if cache.load(t.Context(), first, now, check) || calls != 2 {
		t.Fatal("negative readiness caused repeated catalog work")
	}
	instant = instant.Add(legacySettlementPayerIndexCacheLifetime)
	if !cache.load(t.Context(), first, now, check) || calls != 3 ||
		!cache.load(t.Context(), second, now, check) || calls != 4 ||
		!cache.load(t.Context(), first, now, check) || calls != 5 {
		t.Fatal("expiry or resource replacement did not check the catalog")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if cache.load(ctx, first, now, check) || calls != 5 {
		t.Fatal("a canceled page consumed cached positive readiness")
	}
}

// The refresh barrier proves that concurrent pages fall back without waiting
// for the catalog owner and without starting a second catalog call.
func TestLegacySettlementPayerIndexCacheConcurrentRefresh(t *testing.T) {
	cache := &legacySettlementPayerIndexCache{}
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	identity := sha256.Sum256([]byte("concurrent.example"))
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan bool, 1)
	var releaseOnce sync.Once
	releaseRefresh := func() { releaseOnce.Do(func() { close(release) }) }
	defer func() { releaseRefresh(); <-finished }()
	go func() {
		finished <- cache.load(ctx, identity, time.Now, func(context.Context) (bool, error) {
			close(started)
			select {
			case <-release:
				return true, nil
			case <-ctx.Done():
				return false, ctx.Err()
			}
		})
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("catalog owner never reached its explicit barrier")
	}
	type followerResult struct{ ready, called bool }
	follower := make(chan followerResult, 1)
	followerJoined := make(chan struct{})
	defer func() { releaseRefresh(); <-followerJoined }()
	go func() {
		defer close(followerJoined)
		called := false
		ready := cache.load(ctx, identity, time.Now, func(context.Context) (bool, error) {
			called = true
			return true, nil
		})
		follower <- followerResult{ready: ready, called: called}
	}()
	select {
	case result := <-follower:
		if result.ready || result.called {
			t.Fatal("another page duplicated the catalog refresh")
		}
	case <-ctx.Done():
		t.Fatal("another page waited for the catalog refresh")
	}
}

// Optional catalog failures and late replies cannot authorize payer traversal.
func TestLegacySettlementPayerIndexCacheUnknownAndLateReply(t *testing.T) {
	identity := sha256.Sum256([]byte("unknown.example"))
	for _, kind := range []string{"error", "panic", "cancel", "late"} {
		cache := &legacySettlementPayerIndexCache{}
		instant := time.Unix(100, 0)
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		got := cache.load(ctx, identity, func() time.Time { return instant }, func(context.Context) (bool, error) {
			calls++
			switch kind {
			case "error":
				return true, errors.New("synthetic unavailable catalog")
			case "panic":
				panic(errors.New("synthetic unavailable catalog"))
			case "cancel":
				cancel()
			case "late":
				instant = instant.Add(legacySettlementPayerIndexCacheLifetime)
			}
			return true, nil
		})
		cancel()
		if got || calls != 1 || cache.refreshing || cache.ready {
			t.Fatal("unknown observation authorized traversal or stranded refresh", kind)
		}
	}
}

// Each negative changes the actual isolated PostgreSQL catalog in a rolled-back
// transaction. Neither an identically named wrong index nor a half-built index
// can satisfy the exact query; the ordinary completed definitions do satisfy it.
func TestLegacySettlementPayerIndexCatalogShape(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		ready, err := readLegacySettlementPayerIndexes(ctx)
		if err != nil || !ready {
			t.Fatal("completed payer indexes failed the exact catalog guard", ready, err)
		}
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		for _, change := range []string{
			`DROP INDEX legacy_settlement_intent_payer_due`,
			`DROP INDEX legacy_settlement_intent_payer_missing`,
			`UPDATE pg_index SET indisvalid=false WHERE indexrelid='legacy_settlement_intent_payer_due'::regclass`,
			`UPDATE pg_index SET indisready=false WHERE indexrelid='legacy_settlement_intent_payer_missing'::regclass`,
			`UPDATE pg_index SET indislive=false WHERE indexrelid='legacy_settlement_intent_payer_due'::regclass`,
			`DROP INDEX legacy_settlement_intent_payer_due; CREATE INDEX legacy_settlement_intent_payer_due ON legacy_settlement_intent(shard,next_attempt_time,contract_id) WHERE payer_network_id IS NOT NULL`,
			`DROP INDEX legacy_settlement_intent_payer_missing; CREATE INDEX legacy_settlement_intent_payer_missing ON legacy_settlement_intent(shard,contract_id) WHERE payer_network_id IS NOT NULL`,
			`DROP INDEX legacy_settlement_intent_payer_due; CREATE INDEX legacy_settlement_intent_payer_due ON legacy_settlement_intent(shard,payer_network_id,next_attempt_time DESC,contract_id) WHERE payer_network_id IS NOT NULL`,
		} {
			func() {
				tx, err := conn.Begin(ctx)
				server.Raise(err)
				defer tx.Rollback(context.Background())
				server.RaisePgResult(tx.Exec(ctx, change))
				var ready bool
				server.Raise(tx.QueryRow(ctx, legacySettlementPayerIndexSql).Scan(&ready))
				if ready {
					t.Fatal("incomplete or incorrect catalog enabled payer traversal", change)
				}
			}()
		}
		ready, err = readLegacySettlementPayerIndexes(ctx)
		if err != nil || !ready {
			t.Fatal("catalog control did not restore the ready indexes", ready, err)
		}
	})
}

func TestLegacySettlementPayerIndexMissingChronologicalFallback(t *testing.T) {
	requireLegacyPayerIndexFallback(t, `DROP INDEX legacy_settlement_intent_payer_due`, false)
}

func TestLegacySettlementPayerIndexInvalidChronologicalFallback(t *testing.T) {
	requireLegacyPayerIndexFallback(t, `UPDATE pg_index SET indisvalid=false WHERE indexrelid='legacy_settlement_intent_payer_missing'::regclass`, false)
}

// The existing pool remains owned by the fixture. An unreadable oversized
// resource invalidates optional readiness, without changing that financial pool.
func TestLegacySettlementPayerIndexUnknownResourceChronologicalFallback(t *testing.T) {
	requireLegacyPayerIndexFallback(t, "", true)
}

// Two real financial pages and an empty replay retain funds, provider custody,
// byte-identical reports/proofs and exact once-only clock publication. A trigger
// refuses registration so even an attempted optional write is visible. When the
// required native pgss profile is enabled, successful payer reads are also zero.
func requireLegacyPayerIndexFallback(t *testing.T, change string, unknownResource bool) {
	t.Helper()
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		first, firstId, second, secondIds := legacyHeadRevisitFixture(t, ctx, 1)
		shard := int(firstId[15]) % LegacySettlementShardCount
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE legacy_settlement_intent SET payer_network_id=NULL WHERE contract_id=ANY($1)`, []server.Id{firstId, secondIds[0]}))
			server.RaisePgResult(tx.Exec(ctx, `CREATE FUNCTION synthetic_index_registration_tripwire() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN RAISE EXCEPTION 'synthetic forbidden registration' USING ERRCODE='23514'; END $$;
			 CREATE TRIGGER synthetic_index_registration_tripwire BEFORE UPDATE OF payer_network_id ON legacy_settlement_intent
			 FOR EACH ROW EXECUTE FUNCTION synthetic_index_registration_tripwire()`))
			if change != "" {
				server.RaisePgResult(tx.Exec(ctx, change))
			}
			if os.Getenv("URN_LEGACY_REQUIRE_PGSS") == "1" {
				server.RaisePgResult(tx.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`))
			}
		})
		if unknownResource {
			pop := server.Vault.PushSimpleResource(server.DefaultPgVaultResourceName, bytes.Repeat([]byte("x"), 16*1024+1))
			defer pop()
		}
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		payer := &LegacySettlementPayerCursor{After: &first.sourceNetworkId, End: second.sourceNetworkId, PassEndTime: server.NowUtc()}
		payerBytes, _ := json.Marshal(payer)
		beforeSql := legacyTargetSqlSnapshot(t, ctx)
		page, err := FlushLegacySettlementShard(ctx, shard, nil, payer, 1)
		if err != nil || page.Visited != 1 || page.Completed != 1 || page.Cursor == nil || page.Cursor.ContractId != firstId || !page.More {
			t.Fatal("fallback lost its chronological committed prefix", page, err)
		}
		requireLegacyPayerIndexDormantCursor(t, payer, payerBytes, page)
		requireLegacySettlementTestState(t, ctx, first, firstId, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], true, false, 1000000, 100)
		requireLegacyProviderDurability(t, ctx, first, firstId, 11)
		requireRedisExpiryClock(t, ctx, "11")
		firstProof := legacyPayerIndexFinancialProof(t, ctx, firstId)
		continued, err := FlushLegacySettlementShard(ctx, shard, page.Cursor, page.PayerCursor, 64)
		if err != nil || continued.Completed != 1 || continued.Visited != 1 || continued.Cursor != nil || continued.More {
			t.Fatal("dormant payer cursor replaced chronological eof", continued, err)
		}
		requireLegacyPayerIndexDormantCursor(t, payer, payerBytes, continued)
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], false, true, 999989, 0)
		requireLegacyProviderDurability(t, ctx, second, secondIds[0], 11)
		requireRedisExpiryClock(t, ctx, "22")
		secondProof := legacyPayerIndexFinancialProof(t, ctx, secondIds[0])
		replay, err := FlushLegacySettlementShard(ctx, shard, nil, continued.PayerCursor, 64)
		if err != nil || replay.Visited != 0 || replay.More {
			t.Fatal("fallback replay repeated financial work or invented more work", replay, err)
		}
		requireLegacyPayerIndexDormantCursor(t, payer, payerBytes, replay)
		requireLegacySettlementTestState(t, ctx, first, firstId, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], false, true, 999989, 0)
		requireLegacyProviderDurability(t, ctx, first, firstId, 11)
		requireLegacyProviderDurability(t, ctx, second, secondIds[0], 11)
		requireRedisExpiryClock(t, ctx, "22")
		if firstProof != legacyPayerIndexFinancialProof(t, ctx, firstId) || secondProof != legacyPayerIndexFinancialProof(t, ctx, secondIds[0]) {
			t.Fatal("chronological replay changed custody or reports")
		}
		delta := legacyTargetSqlDelta(t, beforeSql, legacyTargetSqlSnapshot(t, ctx))
		if delta != nil {
			for _, statement := range delta.Statements {
				query := strings.Join(strings.Fields(strings.ToLower(statement.Query)), " ")
				if strings.Contains(query, "select picked.contract_id,contract.payer") ||
					strings.Contains(query, "select payer_network_id,statement_timestamp()") ||
					strings.Contains(query, "select payer_network_id from legacy_settlement_intent") ||
					strings.Contains(query, "as payer_head") {
					t.Fatal("disabled payer traversal executed optional sql", statement.Query)
				}
			}
			t.Logf("fallback_no_payer_sql_qualified=true calls=%v", delta.TopLevelTotals["calls"])
		}
	})
}

// Cursor and counters distinguish absence of an optional attempt from a failed
// registration; the input remains byte-identical and the result owns its copy.
func requireLegacyPayerIndexDormantCursor(t testing.TB, input *LegacySettlementPayerCursor, original []byte, page LegacySettlementShardResult) {
	t.Helper()
	got, _ := json.Marshal(page.PayerCursor)
	inputAfter, _ := json.Marshal(input)
	if page.PayerCursor == nil || page.PayerCursor == input || !bytes.Equal(original, got) || !bytes.Equal(original, inputAfter) ||
		page.PayerProbes != 0 || page.PayerVisited != 0 || page.PayerRegistered != 0 || page.PayerRegistrationFailed || page.PayerRegistrationMs != 0 {
		t.Fatal("fallback performed payer work or mutated its saved cursor", page)
	}
}

// Read-only custody bytes include both original reports, terminal proof, escrow
// metadata and debit. Provider liability is asserted separately without repair.
func legacyPayerIndexFinancialProof(t testing.TB, ctx context.Context, id server.Id) (proof string) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT jsonb_build_object(
		 'outcome',c.outcome,'closed',c.close_time,'usage',c.provider_usage,
		 'reports',(SELECT jsonb_agg(to_jsonb(r) ORDER BY r.party) FROM contract_close r WHERE r.contract_id=c.contract_id),
		 'escrow',(SELECT jsonb_agg(to_jsonb(e) ORDER BY e.balance_id) FROM transfer_escrow e WHERE e.contract_id=c.contract_id),
		 'credit',(SELECT jsonb_agg(jsonb_build_object('balance',b.balance_id,'credit',b.balance_byte_count) ORDER BY b.balance_id)
		  FROM transfer_balance b JOIN transfer_escrow e ON e.balance_id=b.balance_id WHERE e.contract_id=c.contract_id))::text
		 FROM transfer_contract c WHERE c.contract_id=$1`, id).Scan(&proof))
	})
	return
}

// Exact completed indexes enable the original registered-payer path again.
func TestLegacySettlementPayerIndexReadyKeepsPayerService(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		f, id := legacySettlementTestIntent(t, ctx)
		page, err := FlushLegacySettlementShard(ctx, int(id[15])%LegacySettlementShardCount, nil, nil, 64)
		if err != nil || page.Completed != 1 || page.PayerCompleted != 1 || page.PayerVisited != 1 || page.PayerProbes != 1 {
			t.Fatal("ready indexes did not enable the owning payer turn", page, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
	})
}

// A real barrier in the second financial body cancels the fallback page after
// its first commit. The returned prefix, dormant payer cursor and batched clock
// survive; releasing the actual owner permits one ordinary resumed completion.
func TestLegacySettlementPayerIndexFallbackCanceledPrefix(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		first, firstId, second, secondIds := legacyHeadRevisitFixture(t, ctx, 1)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP INDEX legacy_settlement_intent_payer_due`))
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION synthetic_index_budget_barrier() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN IF NEW.contract_id=TG_ARGV[0]::uuid THEN PERFORM set_config('lock_timeout','0',true); PERFORM pg_advisory_xact_lock(731073); END IF; RETURN NEW; END $$;
			 CREATE TRIGGER synthetic_index_budget_barrier BEFORE UPDATE OF outcome ON transfer_contract FOR EACH ROW
			 WHEN (OLD.outcome IS NULL AND NEW.outcome IS NOT NULL) EXECUTE FUNCTION synthetic_index_budget_barrier('%s');`, secondIds[0])))
		})
		testingResetClock(ctx)
		defer testingResetClock(ctx)
		conn := acquireContractLifecycleTestConnection(t, ctx)
		defer conn.Release()
		held, err := conn.Begin(ctx)
		server.Raise(err)
		defer held.Rollback(context.Background())
		server.RaisePgResult(held.Exec(ctx, `SELECT pg_advisory_xact_lock(731073)`))
		blocker := contractLifecycleTestBackendPid(t, ctx, held)
		payer := &LegacySettlementPayerCursor{End: server.NewId(), PassEndTime: server.NowUtc()}
		payerBytes, _ := json.Marshal(payer)
		bounded, expire := context.WithCancelCause(ctx)
		type outcome struct {
			page LegacySettlementShardResult
			err  error
		}
		done, joined := make(chan outcome, 1), make(chan struct{})
		defer func() { expire(context.Canceled); held.Rollback(context.Background()); <-joined }()
		go func() {
			defer close(joined)
			page, err := flushLegacySettlementShardPage(ctx, bounded, int(firstId[15])%LegacySettlementShardCount, nil, payer, 64)
			done <- outcome{page: page, err: err}
		}()
		requireContractLifecycleBlockedBy(t, ctx, held, blocker)
		expire(errLegacySettlementPageBudget)
		var got outcome
		select {
		case got = <-done:
		case <-ctx.Done():
			t.Fatal("canceled chronological owner failed to join", ctx.Err())
		}
		if got.err != nil || got.page.Visited != 1 || got.page.Completed != 1 || got.page.Failed != 0 || !got.page.More || got.page.Cursor == nil || got.page.Cursor.ContractId != firstId {
			t.Fatal("fallback deadline lost its exact committed prefix", got.page, got.err)
		}
		requireLegacyPayerIndexDormantCursor(t, payer, payerBytes, got.page)
		requireLegacySettlementTestState(t, ctx, first, firstId, false, true, 989, 0)
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], true, false, 1000000, 100)
		requireLegacyProviderDurability(t, ctx, first, firstId, 11)
		requireRedisExpiryClock(t, ctx, "11")
		server.Raise(held.Rollback(ctx))
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TRIGGER synthetic_index_budget_barrier ON transfer_contract; DROP FUNCTION synthetic_index_budget_barrier()`))
		})
		resumed, err := FlushLegacySettlementShard(ctx, int(firstId[15])%LegacySettlementShardCount, got.page.Cursor, got.page.PayerCursor, 64)
		if err != nil || resumed.Completed != 1 || resumed.PayerVisited != 0 || resumed.More {
			t.Fatal("fallback failed ordinary resume", resumed, err)
		}
		requireLegacySettlementTestState(t, ctx, second, secondIds[0], false, true, 999989, 0)
		requireLegacyProviderDurability(t, ctx, second, secondIds[0], 11)
		requireRedisExpiryClock(t, ctx, "22")
	})
}
