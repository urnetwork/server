// Accounting controls distinguish authoritative reservations from optional legacy caches.
package work

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Native admission has no obligation to create a legacy snapshot or revision row.
const privateLoadAccountingSql = `SELECT
 (SELECT COALESCE(sum(e.balance_byte_count),0) FROM transfer_escrow e JOIN transfer_contract c USING(contract_id)
  WHERE e.balance_id=$1 AND NOT e.settled AND c.outcome IS NULL),
 (SELECT COALESCE(sum(e.balance_byte_count),0) FROM transfer_escrow e JOIN transfer_contract c USING(contract_id)
  WHERE e.balance_id=$1 AND NOT e.settled AND c.outcome IS NULL AND NOT e.redis_reserved),
 (SELECT COALESCE(sum(e.balance_byte_count),0) FROM transfer_escrow e JOIN transfer_contract c USING(contract_id)
  WHERE e.balance_id=$1 AND NOT e.settled AND c.outcome IS NULL AND e.redis_reserved),
 (SELECT COALESCE(sum(transfer_byte_count),0) FROM transfer_contract WHERE payer_network_id=$2 AND outcome IS NULL),
 b.balance_byte_count,b.start_balance_byte_count,
 COALESCE(s.reserved_byte_count,0),s.balance_id IS NOT NULL,
 COALESCE(s.revision=COALESCE(r.revision,0),false),
 (SELECT count(*) FROM transfer_contract WHERE payer_network_id=$2 AND outcome IS NULL AND transfer_byte_count>1),
 (SELECT count(*) FROM transfer_contract WHERE payer_network_id=$2 AND outcome IS NOT NULL)
 FROM transfer_balance b LEFT JOIN transfer_balance_net_escrow_snapshot s USING(balance_id)
 LEFT JOIN transfer_balance_net_escrow_revision r USING(balance_id) WHERE balance_id=$1`

// Cache fields describe only legacy escrow; native reservations have their own Redis ledger.
type privateLoadAccounting struct {
	exact        model.ByteCount
	legacy       model.ByteCount
	native       model.ByteCount
	promised     model.ByteCount
	remaining    model.ByteCount
	start        model.ByteCount
	cached       model.ByteCount
	cachePresent bool
	cacheCurrent bool
	open         int64
	terminal     int64
	legacyMirror model.ByteCount
	nativeMirror model.ByteCount
	nativeTokens model.ByteCount
}

// These diagnostics close contracts with zero consumption and observe only joined workloads.
// A stale cache cannot override durable financial state; positive pending debits are outside this oracle.
func (self privateLoadAccounting) validate() error {
	if self.exact < 0 || self.legacy < 0 || self.native < 0 || self.exact != self.legacy+self.native ||
		self.exact != self.promised || self.exact > self.remaining || self.remaining != self.start {
		return errors.New("authoritative accounting invariant failed")
	}
	if self.cachePresent && self.cacheCurrent && self.cached != self.legacy {
		return errors.New("current legacy cache differs from legacy authority")
	}
	if self.legacyMirror != self.legacy || self.nativeMirror != self.native || self.nativeTokens != self.native {
		return errors.New("reservation mirrors differ from their durable partitions")
	}
	return nil
}

// Redis absence is a valid zero, but malformed counters and read failures cannot look healthy.
func privateLoadRedisCounter(ctx context.Context, r server.RedisClient, key string) (model.ByteCount, error) {
	text, err := r.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return privateLoadParseReservation(text)
}

// Match the admission script's canonical unsigned decimal rather than accepting alternate encodings.
func privateLoadParseReservation(text string) (model.ByteCount, error) {
	count, err := strconv.ParseInt(text, 10, 64)
	if err != nil || count < 0 || strconv.FormatInt(count, 10) != text {
		return 0, errors.New("invalid reservation decimal")
	}
	return count, nil
}

// Read only this fixture's exact balance and its two independently owned mirrors.
func privateLoadReadAccounting(ctx context.Context, f privateLoadFixture) (privateLoadAccounting, error) {
	state := privateLoadAccounting{}
	var readErr error
	server.Db(ctx, func(conn server.PgConn) {
		readErr = conn.QueryRow(ctx, privateLoadAccountingSql, f.owner.BalanceId, f.owner.NetworkId).Scan(
			&state.exact, &state.legacy, &state.native, &state.promised, &state.remaining, &state.start,
			&state.cached, &state.cachePresent, &state.cacheCurrent, &state.open, &state.terminal)
	})
	if readErr != nil {
		return state, readErr
	}
	server.Redis(ctx, func(r server.RedisClient) {
		base := fmt.Sprintf("{escrow_%s}", f.owner.BalanceId)
		state.legacyMirror, readErr = privateLoadRedisCounter(ctx, r, base+"net")
		if readErr != nil {
			return
		}
		state.nativeMirror, readErr = privateLoadRedisCounter(ctx, r, base+"approx_reserved")
		if readErr != nil {
			return
		}
		count, err := r.HLen(ctx, base+"approx_contracts").Result()
		if err != nil {
			readErr = err
			return
		}
		if count > 16384 {
			readErr = errors.New("fixture reservation ledger exceeds diagnostic bound")
			return
		}
		tokens, err := r.HGetAll(ctx, base+"approx_contracts").Result()
		if err != nil {
			readErr = err
			return
		}
		if len(tokens) > 16384 {
			readErr = errors.New("fixture reservation ledger exceeds diagnostic bound")
			return
		}
		for _, value := range tokens {
			amount, err := privateLoadParseReservation(value)
			if err != nil || amount < 0 || amount > state.remaining-state.nativeTokens {
				readErr = errors.New("invalid native reservation token amount")
				return
			}
			state.nativeTokens += amount
		}
	})
	return state, readErr
}

// Attempts are path evidence, while committed native rows remain the completion oracle.
func privateLoadAssertNativeAdmission(t testing.TB, ctx context.Context, f privateLoadFixture, stats map[string]float64, want int) {
	t.Helper()
	var committed int
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT count(*) FROM transfer_escrow
   WHERE balance_id=$1 AND redis_reserved`, f.owner.BalanceId).Scan(&committed))
	})
	if committed != want || stats["native_reserved"] < float64(want) {
		t.Fatalf("native admission evidence differs: committed=%d expected=%d accepted_attempts=%v", committed, want, stats["native_reserved"])
	}
}
