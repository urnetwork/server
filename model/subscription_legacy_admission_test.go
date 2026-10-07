// Real Redis transitions exercise optional leases without sleep-based expiry.
package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// The new lookup uses the contract-first primary key, bounded by its sentinel.
// Its real native plan and buffers are retained outside measured work windows.
func TestLegacySettlementAdmissionLookupUsesBoundedPrimaryKey(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		defer cancel()
		fixture := legacyTargetSeedLoad(t, ctx)
		var raw []byte
		server.Db(ctx, func(conn server.PgConn) {
			server.RaisePgResult(conn.Exec(ctx, `ANALYZE transfer_escrow`))
			server.Raise(conn.QueryRow(ctx, `EXPLAIN (ANALYZE,BUFFERS,FORMAT JSON) `+legacySettlementAdmissionGrantsSql, fixture.ids[0]).Scan(&raw))
		})
		var plans []map[string]any
		server.Raise(json.Unmarshal(raw, &plans))
		indexed, limits := 0, 0
		var inspect func(any)
		inspect = func(value any) {
			switch node := value.(type) {
			case map[string]any:
				kind, _ := node["Node Type"].(string)
				switch kind {
				case "Seq Scan", "Sort", "Incremental Sort", "Bitmap Heap Scan", "Gather", "Gather Merge":
					t.Fatal("bounded membership lookup lost its direct contract key", kind)
				case "Limit":
					limits++
					if rows, ok := node["Actual Rows"].(float64); !ok || rows != 1 {
						t.Fatal("one-grant lookup did not retain exact row bound", node["Actual Rows"])
					}
				case "Index Scan", "Index Only Scan":
					condition, _ := node["Index Cond"].(string)
					if node["Index Name"] != "transfer_escrow_pkey" || !strings.Contains(condition, "contract_id =") {
						t.Fatal("lookup used a different or unbounded access path", node["Index Name"], condition)
					}
					indexed++
				}
				for _, child := range node {
					inspect(child)
				}
			case []any:
				for _, child := range node {
					inspect(child)
				}
			}
		}
		for _, plan := range plans {
			inspect(plan)
		}
		if indexed != 1 || limits != 1 {
			t.Fatal("membership lookup plan is incomplete", indexed, limits)
		}
		t.Logf("legacy_admission_lookup_plan=%s", raw)
	})
}

// Partial acquisition must release only its own prefix, in sorted grant order.
func TestLegacySettlementAdmissionSortedPartialCleanup(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		ids := []server.Id{server.NewId(), server.NewId()}
		sort.Slice(ids, func(i, j int) bool { return bytes.Compare(ids[i][:], ids[j][:]) < 0 })
		last, busy := acquireLegacySettlementAdmission(ctx, ids[1:])
		if last == nil || busy {
			t.Fatal("healthy grant could not own its finite hint")
		}
		defer last.release(ctx)
		reversed := []server.Id{ids[1], ids[0]}
		partial, busy := acquireLegacySettlementAdmission(ctx, reversed)
		if partial != nil || !busy || reversed[0] != ids[1] {
			t.Fatal("partial acquisition admitted work or mutated caller membership")
		}
		legacyAdmissionRequireToken(t, ctx, ids[0], "")
		legacyAdmissionRequireToken(t, ctx, ids[1], last.token)
		canceled, stop := context.WithCancel(ctx)
		stop()
		last.release(canceled)
		owner, busy := acquireLegacySettlementAdmission(ctx, reversed)
		if owner == nil || busy || len(owner.keys) != 2 || owner.keys[0] != legacySettlementAdmissionKey(ids[0]) || owner.keys[1] != legacySettlementAdmissionKey(ids[1]) {
			t.Fatal("complete ownership did not preserve sorted all-grant admission")
		}
		for _, id := range ids {
			legacyAdmissionRequireToken(t, ctx, id, owner.token)
		}
		owner.release(ctx)
		for _, id := range ids {
			legacyAdmissionRequireToken(t, ctx, id, "")
		}
	})
}

// Redis itself applies an already-past expiry: no scheduler interval creates
// the replacement owner, and a stale cleanup cannot remove its fresh token.
func TestLegacySettlementAdmissionExpiredOwnerCannotDeleteReplacement(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		id := server.NewId()
		old, busy := acquireLegacySettlementAdmission(ctx, []server.Id{id})
		if old == nil || busy {
			t.Fatal("initial owner missing")
		}
		legacyAdmissionRequireToken(t, ctx, id, old.token)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.PExpireAt(ctx, legacySettlementAdmissionKey(id), time.Unix(1, 0)).Err())
		})
		next, busy := acquireLegacySettlementAdmission(ctx, []server.Id{id})
		if next == nil || busy || next.token == old.token {
			t.Fatal("expired owner did not admit a distinct successor")
		}
		old.release(ctx)
		legacyAdmissionRequireToken(t, ctx, id, next.token)
		next.release(ctx)
		legacyAdmissionRequireToken(t, ctx, id, "")
	})
}

// Unreadable or malformed Redis is optional, including a wrong Redis value type.
func TestLegacySettlementAdmissionUnknownRedisKeepsFinancialAuthority(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
		defer cancel()
		for _, metadata := range []string{"malformed", "wrong_type", "missing_expiry", "oversized_expiry"} {
			f, id := legacySettlementTestIntent(t, ctx)
			server.Redis(ctx, func(r server.RedisClient) {
				key := legacySettlementAdmissionKey(f.balanceId)
				switch metadata {
				case "wrong_type":
					server.Raise(r.HSet(ctx, key, "unknown", "value").Err())
				case "missing_expiry":
					server.Raise(r.Set(ctx, key, "v1:0123456789abcdef0123456789abcdef", 0).Err())
				case "oversized_expiry":
					server.Raise(r.Set(ctx, key, "v1:0123456789abcdef0123456789abcdef", time.Hour).Err())
				default:
					server.Raise(r.Set(ctx, key, "unknown", 0).Err())
				}
			})
			owner, busy := acquireLegacySettlementAdmission(ctx, []server.Id{f.balanceId})
			if owner != nil || busy {
				t.Fatal("unknown metadata was classified as authoritative admission", metadata, owner, busy)
			}
			page, err := FlushLegacySettlements(ctx, int(id[15])%LegacySettlementShardCount, nil, 1)
			if err != nil || page.Completed != 1 || page.BusyOrGone != 0 || page.Failed != 0 {
				t.Fatal("optional unknown hint replaced financial authority", metadata, page, err)
			}
			requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
			requireLegacyProviderDurability(t, ctx, f, id, 11)
		}
	})
}

// An oversized escrow-membership hint never truncates the actual grant set or
// changes V14's treatment of missing unused grants in the financial core.
func TestLegacySettlementAdmissionOversizedMembershipFallsThrough(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		extra := make([]server.Id, legacySettlementAdmissionGrantLimit)
		for index := range extra {
			extra[index] = server.NewId()
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_escrow(contract_id,balance_id,balance_byte_count)
			 SELECT $1,balance_id,0 FROM unnest($2::uuid[]) AS extra(balance_id)`, id, extra))
		})
		if got := legacySettlementAdmissionGrantIds(ctx, id); len(got) != 0 {
			t.Fatal("oversized membership was silently truncated", len(got))
		}
		if got := legacySettlementAdmissionGrantIds(ctx, server.NewId()); len(got) != 0 {
			t.Fatal("missing membership invented a grant")
		}
		page, err := FlushLegacySettlements(ctx, int(id[15])%LegacySettlementShardCount, nil, 1)
		if err != nil || page.Completed != 1 || page.BusyOrGone != 0 || page.Failed != 0 {
			t.Fatal("oversized hint changed the ordinary funded outcome", page, err)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
	})
}

type legacyAdmissionFaultKey struct{}

// A context-scoped hook forces exactly one real command boundary. Lost-reply
// mode executes the actual Lua before withholding only its acknowledgement.
type legacyAdmissionFaultHook struct {
	mode     string
	fired    atomic.Bool
	acquired atomic.Int64
	released atomic.Int64
}

func (self *legacyAdmissionFaultHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (self *legacyAdmissionFaultHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		marked, _ := ctx.Value(legacyAdmissionFaultKey{}).(bool)
		args := command.Args()
		if !marked || command.Name() != "eval" || len(args) < 2 {
			return next(ctx, command)
		}
		acquire := args[1] == legacySettlementAdmissionAcquireLua
		release := args[1] == legacySettlementAdmissionReleaseLua
		matches := (acquire && self.mode != "release") || (release && self.mode == "release")
		fault := matches && self.mode != "" && self.fired.CompareAndSwap(false, true)
		if fault && self.mode != "lost_reply" {
			err := errors.New("fixture refused optional Redis command")
			command.SetErr(err)
			return err
		}
		err := next(ctx, command)
		if err == nil {
			if acquire {
				self.acquired.Add(1)
			}
			if release {
				self.released.Add(1)
			}
		}
		if fault {
			err = errors.New("fixture lost optional Redis reply after application")
			command.SetErr(err)
		}
		return err
	}
}

func (self *legacyAdmissionFaultHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

func TestLegacySettlementAdmissionAcquireFailureFallsThrough(t *testing.T) {
	legacyAdmissionFailureControl(t, "before")
}

func TestLegacySettlementAdmissionLostAcquireReplyCleansAndFallsThrough(t *testing.T) {
	legacyAdmissionFailureControl(t, "lost_reply")
}

func TestLegacySettlementAdmissionLostReleaseKeepsCommittedFinance(t *testing.T) {
	legacyAdmissionFailureControl(t, "release")
}

// Every injected Redis failure preserves ordinary debit, payout and replay.
func legacyAdmissionFailureControl(t *testing.T, mode string) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		hook := &legacyAdmissionFaultHook{mode: mode}
		server.Raise(server.RedisWithDeadline(ctx, func(r server.RedisClient) error { r.AddHook(hook); return nil }))
		marked := context.WithValue(ctx, legacyAdmissionFaultKey{}, true)
		// Direct financial calls retain lease cleanup; fresh public pages now
		// bypass the optional path before any lookup or Redis command.
		completed, busy, _, err := flushLegacySettlement(marked, id)
		if !hook.fired.Load() || err != nil || !completed || busy {
			t.Fatal("optional Redis failure escaped into financial policy", mode, completed, busy, err)
		}
		if mode != "release" {
			legacyAdmissionRequireToken(t, ctx, f.balanceId, "")
		} else {
			// Simulate crash-lease expiry as a Redis state transition, not a wait.
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.PExpireAt(ctx, legacySettlementAdmissionKey(f.balanceId), time.Unix(1, 0)).Err())
			})
		}
		if mode == "lost_reply" && (hook.acquired.Load() != 1 || hook.released.Load() != 1) {
			t.Fatal("ambiguous acquisition was not token-cleaned exactly once")
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
		if replay, err := FlushLegacySettlements(ctx, int(id[15])%LegacySettlementShardCount, nil, 1); err != nil || replay.Visited != 0 {
			t.Fatal("optional failure replay repeated finance", replay, err)
		}
		requireLegacyProviderDurability(t, ctx, f, id, 11)
	})
}

// A real joined stream callback is held after commit. At that explicit barrier,
// the Redis release has completed and another connection can own the PG grant.
func TestLegacySettlementAdmissionReleasesBeforeJoinedPosts(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		f, id := legacySettlementTestIntent(t, ctx)
		hook := &legacyAdmissionFaultHook{}
		server.Raise(server.RedisWithDeadline(ctx, func(r server.RedisClient) error { r.AddHook(hook); return nil }))
		gate := &legacySettlementTimingGate{family: "stream", streamKey: contractStreamKey(id), entered: make(chan struct{}, 1), release: make(chan struct{})}
		release := sync.OnceFunc(func() { close(gate.release) })
		server.Redis(ctx, func(r server.RedisClient) { r.AddHook(gate) })
		gate.enabled.Store(true)
		marked := context.WithValue(ctx, legacyAdmissionFaultKey{}, true)
		type financialResult struct {
			completed bool
			busy      bool
			err       error
		}
		done := make(chan financialResult, 1)
		go func() {
			completed, busy, _, err := flushLegacySettlement(marked, id)
			done <- financialResult{completed: completed, busy: busy, err: err}
		}()
		joined := false
		defer func() {
			gate.enabled.Store(false)
			release()
			cancel()
			if !joined {
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("financial fixture did not join after releasing its callback")
				}
			}
		}()
		select {
		case <-gate.entered:
		case <-done:
			joined = true
			t.Fatal("ordinary settlement never entered the joined callback")
		case <-ctx.Done():
			t.Fatal("joined callback barrier was not reached")
		}
		if hook.acquired.Load() != 1 || hook.released.Load() != 1 {
			t.Fatal("hint ownership extended past the financial owner into posts")
		}
		legacyAdmissionRequireToken(t, ctx, f.balanceId, "")
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `SELECT balance_id FROM transfer_balance WHERE balance_id=$1 FOR UPDATE NOWAIT`, f.balanceId))
			var terminal bool
			server.Raise(tx.QueryRow(ctx, `SELECT outcome='settled' FROM transfer_contract WHERE contract_id=$1`, id).Scan(&terminal))
			if !terminal {
				t.Fatal("joined callback preceded the financial commit")
			}
		})
		gate.enabled.Store(false)
		release()
		var result financialResult
		select {
		case result = <-done:
			joined = true
		case <-ctx.Done():
			t.Fatal("released financial owner did not complete")
		}
		if result.err != nil || !result.completed || result.busy {
			t.Fatal("joined-post release changed financial completion", result)
		}
		requireLegacySettlementTestState(t, ctx, f, id, false, true, 989, 0)
		requireLegacyProviderDurability(t, ctx, f, id, 11)
	})
}

// Positive TTL is checked as stored state; no narrow wall-time assertion is used.
func legacyAdmissionRequireToken(t testing.TB, ctx context.Context, id server.Id, want string) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		key := legacySettlementAdmissionKey(id)
		got, err := r.Get(ctx, key).Result()
		if want == "" {
			if err != redis.Nil {
				t.Fatal("optional token unexpectedly remains", got, err)
			}
			return
		}
		server.Raise(err)
		ttl, err := r.PTTL(ctx, key).Result()
		server.Raise(err)
		if got != want || ttl <= 0 || ttl > legacySettlementAdmissionLease {
			t.Fatal("token identity or finite expiry changed", got, ttl)
		}
	})
}
