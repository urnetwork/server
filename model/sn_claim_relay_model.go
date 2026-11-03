package model

// sn_claim_relay_model.go — what the claim relay (controller/sn_claim_relay.go)
// keeps or reads beyond the durable transaction intents of its account.
//
// Its status and Claim now live in Redis: no table is needed, a new one would
// hold every later Main deploy until it is applied, and losing them costs little.
// The status is rewritten by the next pass. A lost Claim now request can be tapped
// again, and a lost limit lets one coldkey tap again early, under the same daily
// cap, which is read from Postgres.

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/holiman/uint256"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Outlives any pass; the reader goes by the status's own time.
const snClaimRelayStatusTtl = 24 * time.Hour

// Past any request's life, so that an abandoned queue goes away.
const snClaimRelayRequestsTtl = 60 * 24 * time.Hour

func snClaimRelayStatusKey(deploymentKey StDeploymentKey) string {
	return fmt.Sprintf("sn_claim_relay_status:%s", deploymentKey)
}

func snClaimRelayRequestsKey(deploymentKey StDeploymentKey) string {
	return fmt.Sprintf("sn_claim_relay_requests:%s", deploymentKey)
}

func snClaimRelayLimitKey(deploymentKey StDeploymentKey, coldkey [32]byte) string {
	return fmt.Sprintf("sn_claim_relay_limit:%s:%x", deploymentKey, coldkey)
}

// SetSnClaimRelayStatus stores the relay's last pass as the api reads it.
func SetSnClaimRelayStatus(ctx context.Context, deploymentKey StDeploymentKey, statusJson []byte) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, snClaimRelayStatusKey(deploymentKey), string(statusJson), snClaimRelayStatusTtl).Err())
	})
}

// GetSnClaimRelayStatus is nil before a pass has run or after its ttl.
func GetSnClaimRelayStatus(ctx context.Context, deploymentKey StDeploymentKey) (statusJson []byte) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, snClaimRelayStatusKey(deploymentKey)).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		statusJson = []byte(value)
	})
	return
}

// SnClaimRelayRequest is one unfinished Claim now.
type SnClaimRelayRequest struct {
	Coldkey [32]byte
	Time    time.Time
}

// AddSnClaimRelayRequest takes the coldkey's one Claim now of the period and
// queues it. A coldkey already within its period queues nothing, and limitUntil
// says when the period ends. The limit's expiry is the period.
func AddSnClaimRelayRequest(ctx context.Context, deploymentKey StDeploymentKey, coldkey [32]byte, now time.Time, period time.Duration) (added bool, limitUntil time.Time) {
	server.Redis(ctx, func(r server.RedisClient) {
		limitKey := snClaimRelayLimitKey(deploymentKey, coldkey)
		var err error
		added, err = r.SetNX(ctx, limitKey, strconv.FormatInt(now.UnixMilli(), 10), period).Result()
		server.Raise(err)
		if !added {
			remaining, err := r.PTTL(ctx, limitKey).Result()
			server.Raise(err)
			limitUntil = now.Add(max(remaining, 0))
			return
		}
		requestsKey := snClaimRelayRequestsKey(deploymentKey)
		member := hex.EncodeToString(coldkey[:])
		server.Raise(r.ZAdd(ctx, requestsKey, redis.Z{Score: float64(now.UnixMilli()), Member: member}).Err())
		server.Raise(r.Expire(ctx, requestsKey, snClaimRelayRequestsTtl).Err())
		limitUntil = now.Add(period)
	})
	return
}

// GetSnClaimRelayRequest reports a coldkey's unfinished request and when its limit
// ends, the zero time when none holds.
func GetSnClaimRelayRequest(ctx context.Context, deploymentKey StDeploymentKey, coldkey [32]byte, now time.Time) (requested bool, limitUntil time.Time) {
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.ZScore(ctx, snClaimRelayRequestsKey(deploymentKey), hex.EncodeToString(coldkey[:])).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		requested = err == nil
		remaining, err := r.PTTL(ctx, snClaimRelayLimitKey(deploymentKey, coldkey)).Result()
		server.Raise(err)
		if 0 < remaining {
			limitUntil = now.Add(remaining)
		}
	})
	return
}

// GetSnClaimRelayRequests lists the unfinished requests, oldest first.
func GetSnClaimRelayRequests(ctx context.Context, deploymentKey StDeploymentKey, limit int) []*SnClaimRelayRequest {
	requests := []*SnClaimRelayRequest{}
	server.Redis(ctx, func(r server.RedisClient) {
		members, err := r.ZRangeWithScores(ctx, snClaimRelayRequestsKey(deploymentKey), 0, int64(limit)-1).Result()
		server.Raise(err)
		for _, member := range members {
			text, ok := member.Member.(string)
			if !ok {
				continue
			}
			raw, err := hex.DecodeString(text)
			if err != nil || len(raw) != 32 {
				continue
			}
			requests = append(requests, &SnClaimRelayRequest{Coldkey: [32]byte(raw), Time: time.UnixMilli(int64(member.Score)).UTC()})
		}
	})
	return requests
}

// FinishSnClaimRelayRequest removes a coldkey's request. A refund also ends its
// limit: nothing was sent for it, so the tap is not used up.
func FinishSnClaimRelayRequest(ctx context.Context, deploymentKey StDeploymentKey, coldkey [32]byte, refund bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.ZRem(ctx, snClaimRelayRequestsKey(deploymentKey), hex.EncodeToString(coldkey[:])).Err())
		if refund {
			server.Raise(r.Del(ctx, snClaimRelayLimitKey(deploymentKey, coldkey)).Err())
		}
	})
}

// GetStLedgerV2Accounts returns a pool's account index in position order.
func GetStLedgerV2Accounts(ctx context.Context, deploymentKey StDeploymentKey, noId uint64) []*StLedgerV2Account {
	key := requireStDeploymentKey(deploymentKey)
	accounts := []*StLedgerV2Account{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT account_index, coldkey, root_index
				FROM st_ledger_v2_account
				WHERE deployment_key = $1 AND no_id = $2
				ORDER BY account_index
			`,
			key, int64(noId),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var index, rootIndex int64
				var coldkey []byte
				server.Raise(result.Scan(&index, &coldkey, &rootIndex))
				account := &StLedgerV2Account{NoId: noId, Index: uint64(index), RootIndex: uint64(rootIndex)}
				copy(account.Coldkey[:], coldkey)
				accounts = append(accounts, account)
			}
		})
	})
	return accounts
}

// GetStLedgerV2ClaimedCumulatives reads each account's highest claimed cumulative
// from the mirror of vault2's Claimed events. The mirror can only be behind the
// chain. The kind and block index covers the scan, and an account has few claims.
func GetStLedgerV2ClaimedCumulatives(ctx context.Context, deploymentKey StDeploymentKey, noId uint64) map[uint64]uint256.Int {
	key := requireStDeploymentKey(deploymentKey)
	claimed := map[uint64]uint256.Int{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT data_json FROM st_event WHERE deployment_key = $1 AND kind = 'V2Claimed'`, key)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var dataJson string
				server.Raise(result.Scan(&dataJson))
				var data struct {
					NoId       string `json:"no_id"`
					Index      string `json:"index"`
					Cumulative string `json:"cumulative"`
				}
				if json.Unmarshal([]byte(dataJson), &data) != nil || data.NoId != strconv.FormatUint(noId, 10) {
					continue
				}
				index, err := strconv.ParseUint(data.Index, 10, 64)
				if err != nil {
					continue
				}
				var cumulative uint256.Int
				if cumulative.SetFromDecimal(data.Cumulative) != nil {
					continue
				}
				if current, ok := claimed[index]; !ok || current.Lt(&cumulative) {
					claimed[index] = cumulative
				}
			}
		})
	})
	return claimed
}

// GetStTransactionIntentsSince returns the intents an account created since a
// time, newest nonce first. The account index covers the scan from the newest
// nonce back, and a relay account makes few in a day.
func GetStTransactionIntentsSince(ctx context.Context, chainId uint64, genesisHash string, fromAddress string, since time.Time) []*StTransactionIntent {
	intents := []*StTransactionIntent{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+stTransactionIntentColumns+`
				FROM st_transaction_intent
				WHERE chain_id = $1 AND genesis_hash = $2 AND from_address = $3 AND $4 <= create_time
				ORDER BY nonce DESC
			`,
			int64(chainId), genesisHash, fromAddress, since,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				intents = append(intents, scanStTransactionIntent(result))
			}
		})
	})
	return intents
}
