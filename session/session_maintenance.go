// Cross-slot maintenance is repairable scheduling, never acceptance authority.
package session

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

const SessionIndexShards = 32

func sessionIndexKeys(networkId server.Id) []string {
	shard := int(networkId[15]) % SessionIndexShards
	prefix := fmt.Sprintf("{nsi_%02d}", shard)
	return []string{prefix + "z", prefix + "r:" + networkId.String()}
}

const sessionIndexPublishLua = `
local old=tonumber(redis.call('ZSCORE',KEYS[1],ARGV[1]));local due=tonumber(ARGV[2])
if not old or due<old then redis.call('ZADD',KEYS[1],due,ARGV[1]) end
redis.call('SET',KEYS[2],ARGV[3],'PX',10368000000)
return 1
`
const sessionIndexCasLua = `
if redis.call('GET',KEYS[2])~=ARGV[2] then return 0 end
if ARGV[3]=='' then redis.call('ZREM',KEYS[1],ARGV[1]);redis.call('DEL',KEYS[2])
else redis.call('ZADD',KEYS[1],ARGV[3],ARGV[1]);redis.call('PEXPIRE',KEYS[2],10368000000) end
return 1
`
const sessionReviewLua = sessionLuaCommon + `
prune();local generation,eid=revision()
local live=redis.call('ZRANGE',KEYS[1],0,0,'WITHSCORES');local held=redis.call('ZRANGE',KEYS[2],0,0,'WITHSCORES')
local next=math.min(tonumber(live[2]) or 9007199254740991,tonumber(held[2]) or 9007199254740991)
local maximum=redis.call('ZREVRANGE',KEYS[2],0,0,'WITHSCORES');shared(math.max(now+300000,tonumber(maximum[2]) or 0))
if next==9007199254740991 then next=0 end
return cjson.encode({next_review=next,generation=generation,event_id=eid})
`

// Every writer refreshes the outbox revision, even if the earlier deadline wins.
func QueueSessionIndexInTx(ctx context.Context, tx server.PgTx, networkId server.Id, due time.Time) error {
	_, err := tx.Exec(ctx, `INSERT INTO network_session_index_outbox(network_id,revision,next_review_time,update_time) VALUES($1,$2,$3,$4) ON CONFLICT(network_id) DO UPDATE SET revision=excluded.revision,next_review_time=LEAST(network_session_index_outbox.next_review_time,excluded.next_review_time),update_time=excluded.update_time`, networkId, server.NewId(), due, server.NowUtc())
	return err
}
func publishSessionIndex(ctx context.Context, networkId server.Id, due time.Time, revision string) error {
	return server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		return r.Eval(ctx, sessionIndexPublishLua, sessionIndexKeys(networkId), networkId.String(), due.UnixMilli(), revision).Err()
	})
}

func RepairSessionIndexes(ctx context.Context, limit int) error {
	return sessionTx(ctx, func(tx server.PgTx) error {
		rows, err := tx.Query(ctx, `SELECT network_id,revision,next_review_time FROM network_session_index_outbox ORDER BY update_time,network_id LIMIT $1 FOR UPDATE SKIP LOCKED`, limit)
		if err != nil {
			return err
		}
		type repair struct {
			networkId, revision server.Id
			due                 time.Time
		}
		repairs := []repair{}
		for rows.Next() {
			var item repair
			if err = rows.Scan(&item.networkId, &item.revision, &item.due); err != nil {
				rows.Close()
				return err
			}
			repairs = append(repairs, item)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, item := range repairs {
			// A lost index can coexist with older live sessions. The touched
			// mint's deadline alone is not the network's next review time.
			var review struct {
				NextReview int64 `json:"next_review"`
			}
			if err = sessionEval(ctx, sessionReviewLua, sessionSharedKeys(item.networkId), []any{server.NowUtc().UnixMilli(), server.NewId().String()}, &review); err != nil {
				continue
			}
			if review.NextReview > 0 && review.NextReview < item.due.UnixMilli() {
				item.due = time.UnixMilli(review.NextReview)
			}
			if err = publishSessionIndex(ctx, item.networkId, item.due, item.revision.String()); err != nil {
				continue
			}
			if _, err = tx.Exec(ctx, `DELETE FROM network_session_index_outbox WHERE network_id=$1 AND revision=$2`, item.networkId, item.revision); err != nil {
				return err
			}
		}
		return nil
	})
}

// Review each network under its own slot, then use a revision CAS on the index.
// A concurrent create either defeats this CAS or republishes after it.
func SweepSessionIndexShard(ctx context.Context, shard, limit int, now time.Time) (int, error) {
	if shard < 0 || shard >= SessionIndexShards {
		return 0, fmt.Errorf("invalid session maintenance shard")
	}
	var due []string
	indexKey := fmt.Sprintf("{nsi_%02d}z", shard)
	err := server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
		var err error
		due, err = r.ZRangeByScore(ctx, indexKey, &redis.ZRangeBy{Min: "-inf", Max: strconv.FormatInt(now.UnixMilli(), 10), Offset: 0, Count: int64(limit)}).Result()
		return err
	})
	if err != nil {
		return 0, err
	}
	reviewed := 0
	for _, value := range due {
		networkId, err := server.ParseId(value)
		if err != nil {
			return reviewed, err
		}
		keys := sessionIndexKeys(networkId)
		var revision string
		err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			var err error
			revision, err = r.Get(ctx, keys[1]).Result()
			return err
		})
		if err != nil {
			if err == server.RedisNil {
				if err = publishSessionIndex(ctx, networkId, now, server.NewId().String()); err != nil {
					return reviewed, err
				}
				continue
			}
			return reviewed, err
		}
		var review struct {
			NextReview int64 `json:"next_review"`
		}
		if err = sessionEval(ctx, sessionReviewLua, sessionSharedKeys(networkId), []any{now.UnixMilli(), server.NewId().String()}, &review); err != nil {
			return reviewed, err
		}
		next := ""
		if review.NextReview > 0 {
			next = strconv.FormatInt(review.NextReview, 10)
		}
		if err = server.RedisAuth(ctx, func(ctx context.Context, r server.RedisClient) error {
			return r.Eval(ctx, sessionIndexCasLua, keys, networkId.String(), revision, next).Err()
		}); err != nil {
			return reviewed, err
		}
		reviewed++
	}
	return reviewed, nil
}
