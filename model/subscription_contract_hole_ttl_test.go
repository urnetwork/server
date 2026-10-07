package model

// Real Redis controls preserve values across renewal and force lifecycle races
// at command admission. PostgreSQL is forbidden inside the renewal boundary.

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

// Redis DUMP omits TTL, so equality proves every value and member survived.
func contractHoleValues(t testing.TB, ctx context.Context, keys []string) []string {
	t.Helper()
	values := make([]string, len(keys))
	server.Redis(ctx, func(client server.RedisClient) {
		for index, key := range keys {
			value, err := client.Dump(ctx, key).Result()
			if err != nil && !errors.Is(err, server.RedisNil) {
				t.Fatal("cannot inspect hole value", err)
			}
			values[index] = value
		}
	})
	return values
}

// The periodic operation and its readiness observation are both read-only with
// respect to values, even if lazy packet expiry cleanup has not run yet.
func TestContractHoleTtlRefreshOnlyExtendsExistingExpirations(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := server.WithoutPostgres(t.Context())
		source, destination, live, expired := server.NewId(), server.NewId(), server.NewId(), server.NewId()
		keys := contractHoleKeys(source, destination)
		server.Raise(applyContractHoleEvent(ctx, live, source, destination, "create"))
		server.Raise(applyContractHoleEvent(ctx, expired, source, destination, "create"))
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.ZAdd(ctx, keys[1], redis.Z{Score: 1, Member: expired.String()}).Err())
			server.Raise(client.Set(ctx, keys[3], "independent-initialization-token", 45*time.Second).Err())
			for _, key := range keys[:2] {
				server.Raise(client.PExpire(ctx, key, ContractHoleTtl/4).Err())
			}
		})
		before := contractHoleValues(t, ctx, keys)
		refreshed, count, err := refreshContractHoleTtl(ctx, source, destination)
		if err != nil || !refreshed || count != 2 {
			t.Fatalf("renewal=%t count=%d error=%v", refreshed, count, err)
		}
		status, _, err := inspectContractHoleLease(ctx, source, destination)
		if err != nil || status != ContractHolePositive {
			t.Fatalf("read-only witness status=%d error=%v", status, err)
		}
		if after := contractHoleValues(t, ctx, keys); !reflect.DeepEqual(before, after) {
			t.Fatal("TTL renewal or witness changed a projection value")
		}
		server.Redis(ctx, func(client server.RedisClient) {
			for _, key := range keys[:2] {
				ttl, err := client.PTTL(ctx, key).Result()
				if err != nil || ttl <= ContractHoleRefreshInterval || ttl > ContractHoleTtl {
					t.Fatalf("TTL was not renewed inside its bound: ttl=%s error=%v", ttl, err)
				}
			}
		})
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("TTL renewal attempted PostgreSQL")
		}
	})
}

// Missing, malformed or wholly expired state is left untouched. In particular,
// a surviving companion key cannot recreate a count that a close deleted.
func TestContractHoleTtlRefreshNeverRepairsOrResurrects(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := server.WithoutPostgres(t.Context())
		for _, scenario := range []string{"missing", "deleted", "missing_count", "missing_members", "persistent", "overlong", "malformed", "drift", "expired", "oversized", "wrongtype"} {
			source, destination, contract := server.NewId(), server.NewId(), server.NewId()
			keys := contractHoleKeys(source, destination)
			if scenario != "missing" {
				server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "create"))
			}
			if scenario == "deleted" {
				server.Raise(applyContractHoleEvent(ctx, contract, source, destination, "remove"))
			}
			server.Redis(ctx, func(client server.RedisClient) {
				switch scenario {
				case "missing_count":
					server.Raise(client.Del(ctx, keys[0]).Err())
				case "missing_members":
					server.Raise(client.Del(ctx, keys[1]).Err())
				case "persistent":
					server.Raise(client.Persist(ctx, keys[0]).Err())
				case "overlong":
					server.Raise(client.PExpire(ctx, keys[0], time.Hour).Err())
				case "malformed":
					server.Raise(client.Set(ctx, keys[0], "01", ContractHoleTtl/4).Err())
				case "drift":
					server.Raise(client.Set(ctx, keys[0], "2", ContractHoleTtl/4).Err())
				case "expired":
					server.Raise(client.ZAdd(ctx, keys[1], redis.Z{Score: 1, Member: contract.String()}).Err())
				case "oversized":
					server.Raise(client.Set(ctx, keys[4], 1, ContractHoleTtl).Err())
				case "wrongtype":
					server.Raise(client.Set(ctx, keys[1], "wrong", ContractHoleTtl).Err())
				}
			})
			before := contractHoleValues(t, ctx, keys)
			var expirations []int64
			server.Redis(ctx, func(client server.RedisClient) {
				for _, key := range keys {
					expires, err := client.Eval(ctx, `return redis.call('PEXPIRETIME',KEYS[1])`, []string{key}).Int64()
					server.Raise(err)
					expirations = append(expirations, expires)
				}
			})
			if refreshed, _, _ := refreshContractHoleTtl(ctx, source, destination); refreshed {
				t.Fatal("invalid projection was renewed", scenario)
			}
			if after := contractHoleValues(t, ctx, keys); !reflect.DeepEqual(before, after) {
				t.Fatal("invalid projection was mutated", scenario)
			}
			server.Redis(ctx, func(client server.RedisClient) {
				for index, key := range keys {
					expires, err := client.Eval(ctx, `return redis.call('PEXPIRETIME',KEYS[1])`, []string{key}).Int64()
					if err != nil || expires != expirations[index] {
						t.Fatalf("%s changed invalid expiry: got=%d want=%d error=%v", scenario, expires, expirations[index], err)
					}
				}
			})
		}
		if server.PacketPostgresAttempts(ctx) != 0 {
			t.Fatal("invalid renewal attempted PostgreSQL")
		}
	})
}

// A one-shot command barrier leaves lifecycle scripts free to commit first.
type contractHoleTtlBarrier struct {
	key     string
	armed   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

// Connection ownership stays with the test environment.
func (self *contractHoleTtlBarrier) DialHook(next redis.DialHook) redis.DialHook { return next }

// Only the individual renewal command participates in the barrier.
func (self *contractHoleTtlBarrier) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// Cancellation releases the one admitted command and lets its owner join it.
func (self *contractHoleTtlBarrier) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, command redis.Cmder) error {
		args := command.Args()
		if command.Name() == "eval" && len(args) > 3 && args[1] == contractHoleRefreshTtlScript && args[3] == self.key && self.armed.CompareAndSwap(true, false) {
			close(self.entered)
			select {
			case <-self.release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return next(ctx, command)
	}
}

// Create and final close commit while renewal is held before its atomic EVAL.
// The later renewal observes their exact values and never replays a source count.
func TestContractHoleTtlRefreshSerializesWithCreateAndClose(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := server.WithoutPostgres(t.Context())
		for _, operation := range []string{"create", "close"} {
			source, destination, first := server.NewId(), server.NewId(), server.NewId()
			server.Raise(applyContractHoleEvent(ctx, first, source, destination, "create"))
			barrier := &contractHoleTtlBarrier{key: contractHoleKeys(source, destination)[0], entered: make(chan struct{}), release: make(chan struct{})}
			barrier.armed.Store(true)
			server.Redis(ctx, func(client server.RedisClient) { client.AddHook(barrier) })
			type result struct {
				refreshed bool
				err       error
			}
			done := make(chan result, 1)
			finished := make(chan struct{})
			operationCtx, cancel := context.WithCancel(ctx)
			go func() {
				defer close(finished)
				refreshed, _, err := refreshContractHoleTtl(operationCtx, source, destination)
				done <- result{refreshed, err}
			}()
			defer func() {
				cancel()
				<-finished
			}()
			select {
			case <-barrier.entered:
			case got := <-done:
				t.Fatal("renewal escaped command barrier", got)
			}
			want := int64(2)
			if operation == "create" {
				server.Raise(applyContractHoleEvent(ctx, server.NewId(), source, destination, "create"))
			} else {
				server.Raise(applyContractHoleEvent(ctx, first, source, destination, "remove"))
				want = 0
			}
			close(barrier.release)
			got := <-done
			if got.err != nil || got.refreshed != (want > 0) {
				t.Fatalf("%s renewal=%t error=%v", operation, got.refreshed, got.err)
			}
			requireContractHoleCount(t, ctx, source, destination, want)
			if operation == "create" {
				server.Raise(applyContractHoleEvent(ctx, first, source, destination, "remove"))
				requireContractHoleCount(t, ctx, source, destination, 1)
			}
		}
	})
}

// Discovery excludes a last final close or absolute expiry, so a lost close post
// cannot renew an obsolete legacy hole forever. Missing live holes stay missing.
func TestContractHoleTtlPageDoesNotRebuildAndStopsClosedPairs(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		id := insertContractHoleSourceRow(ctx, source, destination)
		page, err := RefreshContractHolesPage(ctx, nil)
		if err != nil || page.Pairs != 1 || page.FailedPairs != 1 {
			t.Fatalf("missing hole was not unknown: page=%+v error=%v", page, err)
		}
		requireContractHoleCount(t, ctx, source, destination, 0)
		_, err = refreshContractHole(ctx, source, destination)
		server.Raise(err)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO contract_close(contract_id,party,used_transfer_byte_count,checkpoint) VALUES($1,'source',0,true)`, id))
		})
		page, err = RefreshContractHolesPage(ctx, nil)
		if err != nil || page.Pairs != 1 || page.FailedPairs != 0 {
			t.Fatalf("checkpoint lost renewal: page=%+v error=%v", page, err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE contract_close SET checkpoint=false WHERE contract_id=$1`, id))
		})
		before := contractHoleValues(t, ctx, contractHoleKeys(source, destination))
		page, err = RefreshContractHolesPage(ctx, nil)
		if err != nil || page.Pairs != 0 {
			t.Fatalf("closed pair still renewed: page=%+v error=%v", page, err)
		}
		if after := contractHoleValues(t, ctx, contractHoleKeys(source, destination)); !reflect.DeepEqual(before, after) {
			t.Fatal("discovery changed a closed pair")
		}
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DELETE FROM contract_close WHERE contract_id=$1`, id))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE transfer_contract SET expiration_time=$2 WHERE contract_id=$1`, id, time.UnixMilli(1)))
		})
		page, err = RefreshContractHolesPage(ctx, nil)
		if err != nil || page.Pairs != 0 {
			t.Fatalf("expired pair still renewed: page=%+v error=%v", page, err)
		}
	})
}

// Filtering happens after the fixed source page. A whole page of expired rows
// still advances its cursor and cannot hide an unbounded scan for a live pair.
func TestContractHoleTtlPageAdvancesAcrossIneligibleRows(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ctx := t.Context()
		source, destination := server.NewId(), server.NewId()
		ancient := time.Unix(1, 0).UTC()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO transfer_contract
 (contract_id,source_network_id,source_id,destination_network_id,destination_id,transfer_byte_count,create_time,expiration_time)
 SELECT gen_random_uuid(),$1,$2,$3,$4,0,$5,$5 FROM generate_series(1,$6)`,
				server.NewId(), source, server.NewId(), destination, ancient, ContractHoleRefreshPageSize))
		})
		live := insertContractHoleSourceRow(ctx, source, destination)
		server.Raise(applyContractHoleEvent(ctx, live, source, destination, "create"))
		key := contractHoleKeys(source, destination)[0]
		var originalExpiry int64
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.PExpire(ctx, key, ContractHoleTtl/4).Err())
			var err error
			originalExpiry, err = client.Eval(ctx, `return redis.call('PEXPIRETIME',KEYS[1])`, []string{key}).Int64()
			server.Raise(err)
		})
		page, err := RefreshContractHolesPage(ctx, nil)
		if err != nil || page.Cursor == nil || !page.Cursor.CreateTime.Equal(ancient) || page.Pairs != 0 {
			t.Fatalf("ineligible page lost its boundary: page=%+v error=%v", page, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			expires, err := client.Eval(ctx, `return redis.call('PEXPIRETIME',KEYS[1])`, []string{key}).Int64()
			if err != nil || expires != originalExpiry {
				t.Fatal("ineligible page renewed a later candidate", expires, originalExpiry, err)
			}
		})
		page, err = RefreshContractHolesPage(ctx, page.Cursor)
		if err != nil || page.Cursor != nil || page.Pairs != 1 || page.FailedPairs != 0 {
			t.Fatalf("live tail was skipped: page=%+v error=%v", page, err)
		}
		server.Redis(ctx, func(client server.RedisClient) {
			ttl, err := client.PTTL(ctx, key).Result()
			if err != nil || ttl <= ContractHoleRefreshInterval {
				t.Fatal("live tail did not renew", ttl, err)
			}
		})
		requireContractHoleCount(t, ctx, source, destination, 1)
	})
}
