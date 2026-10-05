package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

const (
	statsProviderEgressCacheTTL  = 300 * time.Second
	statsProviderEgressFillTime  = 120 * time.Second
	statsProviderEgressLeaseTTL  = 180 * time.Second
	statsProviderEgressRedisTime = 2 * time.Second
	statsProviderEgressClockSkew = 5 * time.Second
	statsProviderEgressWireBytes = 4 << 20
	statsProviderEgressMaxIndex  = 32767
)

var (
	errStatsProviderEgressUnavailable = errors.New("provider egress stats unavailable")
	errStatsProviderEgressBusy        = errors.New("provider egress stats refresh in progress")
	errStatsProviderEgressInvalid     = errors.New("provider egress stats snapshot invalid")
)

type statsProviderEgressSnapshot struct {
	Counts      *model.ProviderEgressCounts
	StartedAt   time.Time
	CompletedAt time.Time
}

type statsProviderEgressCacheKeys struct{ value, fill string }

// Both keys share a slot; policy text and ownership tokens never become labels.
func statsProviderEgressKeys(policyKey string) statsProviderEgressCacheKeys {
	sum := sha256.Sum256([]byte(policyKey))
	prefix := "stats_provider_egress:v1:{" + hex.EncodeToString(sum[:]) + "}"
	return statsProviderEgressCacheKeys{prefix + ":value", prefix + ":fill"}
}

type statsProviderEgressCacheStore interface {
	get(context.Context, string) (string, bool, error)
	acquire(context.Context, string, string, time.Duration) (bool, error)
	publish(context.Context, statsProviderEgressCacheKeys, string, string, time.Duration) (bool, error)
	release(context.Context, string, string) error
}

type redisStatsProviderEgressCacheStore struct{}

func statsProviderEgressRedisOperation(ctx context.Context, operation func(context.Context, server.RedisClient) error) error {
	operationCtx, cancel := context.WithTimeout(ctx, statsProviderEgressRedisTime)
	defer cancel()
	if err := server.RedisWithDeadline(operationCtx, func(client server.RedisClient) error {
		return operation(operationCtx, client)
	}); err != nil {
		return errStatsProviderEgressUnavailable
	}
	return nil
}

func (redisStatsProviderEgressCacheStore) get(ctx context.Context, key string) (value string, exists bool, err error) {
	err = statsProviderEgressRedisOperation(ctx, func(ctx context.Context, client server.RedisClient) error {
		var readErr error
		value, readErr = client.Get(ctx, key).Result()
		if errors.Is(readErr, redis.Nil) {
			return nil
		}
		exists = readErr == nil
		return readErr
	})
	return
}

func (redisStatsProviderEgressCacheStore) acquire(ctx context.Context, key, token string, ttl time.Duration) (acquired bool, err error) {
	const script = `if redis.call('SET',KEYS[1],ARGV[1],'NX','PX',ARGV[2]) then return 1 end; return 0`
	err = statsProviderEgressRedisOperation(ctx, func(ctx context.Context, client server.RedisClient) error {
		result, err := client.Eval(ctx, script, []string{key}, token, ttl.Milliseconds()).Int64()
		acquired = result == 1
		if err != nil || result < 0 || result > 1 {
			return errStatsProviderEgressUnavailable
		}
		return nil
	})
	return
}

func (redisStatsProviderEgressCacheStore) publish(ctx context.Context, keys statsProviderEgressCacheKeys, token, value string, ttl time.Duration) (published bool, err error) {
	const script = `if redis.call('GET',KEYS[2]) ~= ARGV[1] then return 0 end
redis.call('SET',KEYS[1],ARGV[2],'PX',ARGV[3]); redis.call('DEL',KEYS[2]); return 1`
	err = statsProviderEgressRedisOperation(ctx, func(ctx context.Context, client server.RedisClient) error {
		result, err := client.Eval(ctx, script, []string{keys.value, keys.fill}, token, value, ttl.Milliseconds()).Int64()
		published = result == 1
		if err != nil || result < 0 || result > 1 {
			return errStatsProviderEgressUnavailable
		}
		return nil
	})
	return
}

func (redisStatsProviderEgressCacheStore) release(ctx context.Context, key, token string) error {
	const script = `if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end; return 0`
	return statsProviderEgressRedisOperation(ctx, func(ctx context.Context, client server.RedisClient) error {
		return client.Eval(ctx, script, []string{key}, token).Err()
	})
}

// Fixed map keys retain every legitimate zero and historical smallint index.
// The wire cap admits the entire three-bucket smallint domain, without truncation.
type statsProviderEgressWireCounts struct {
	Buckets  map[string]map[string]int64 `json:"b"`
	Reasons  map[string]int64            `json:"r"`
	MaxIndex *int                        `json:"m"`
}

type statsProviderEgressWire struct {
	Version   int                            `json:"v"`
	Started   int64                          `json:"s"`
	Completed int64                          `json:"c"`
	Counts    *statsProviderEgressWireCounts `json:"n"`
}

func validStatsProviderEgressCounts(counts *model.ProviderEgressCounts) bool {
	if counts == nil || counts.MaxIndex < 0 || counts.MaxIndex > statsProviderEgressMaxIndex || len(counts.BucketIndexCounts) != 3 || len(counts.ReasonCounts) != 8 {
		return false
	}
	for _, bucket := range []string{model.RankModeQuality, model.RankModeSpeed, model.ProviderEgressBucketOnline} {
		indexes, ok := counts.BucketIndexCounts[bucket]
		if !ok || indexes == nil || len(indexes) > statsProviderEgressMaxIndex+2 {
			return false
		}
		for label, count := range indexes {
			if count < 0 {
				return false
			}
			if label != model.ProviderEgressIndexNone {
				index, err := strconv.Atoi(label)
				if err != nil || index < 0 || index > statsProviderEgressMaxIndex || strconv.Itoa(index) != label {
					return false
				}
			}
		}
	}
	for _, reason := range []string{model.ProviderExcludedBlackhole, model.ProviderExcludedTls, model.ProviderExcludedCountry, model.ProviderExcludedHealth, model.ProviderExcludedUnprobed, model.ProviderExcludedArinRisk, model.ProviderExcludedArinNonQuality, model.ProviderExcludedReliability} {
		if count, ok := counts.ReasonCounts[reason]; !ok || count < 0 {
			return false
		}
	}
	return true
}

func statsProviderEgressSnapshotFresh(snapshot *statsProviderEgressSnapshot, now time.Time) (bool, error) {
	if snapshot == nil || !validStatsProviderEgressCounts(snapshot.Counts) || snapshot.StartedAt.UnixMilli() <= 0 || snapshot.CompletedAt.Before(snapshot.StartedAt) || snapshot.CompletedAt.Sub(snapshot.StartedAt) > statsProviderEgressFillTime+statsProviderEgressClockSkew || snapshot.CompletedAt.After(now.Add(statsProviderEgressClockSkew)) {
		return false, errStatsProviderEgressInvalid
	}
	// Preserve the completion-plus-five-minute cadence; hits never renew this time.
	return now.Sub(snapshot.CompletedAt) < statsProviderEgressCacheTTL, nil
}

func encodeStatsProviderEgressSnapshot(snapshot *statsProviderEgressSnapshot, now time.Time) (string, error) {
	if fresh, err := statsProviderEgressSnapshotFresh(snapshot, now); err != nil || !fresh {
		return "", errStatsProviderEgressInvalid
	}
	encoded, err := json.Marshal(statsProviderEgressWire{1, snapshot.StartedAt.UnixMilli(), snapshot.CompletedAt.UnixMilli(), &statsProviderEgressWireCounts{snapshot.Counts.BucketIndexCounts, snapshot.Counts.ReasonCounts, &snapshot.Counts.MaxIndex}})
	if err != nil || len(encoded) > statsProviderEgressWireBytes {
		return "", errStatsProviderEgressInvalid
	}
	return string(encoded), nil
}

func decodeStatsProviderEgressSnapshot(value string, now time.Time) (*statsProviderEgressSnapshot, bool, error) {
	if len(value) == 0 || len(value) > statsProviderEgressWireBytes {
		return nil, false, errStatsProviderEgressInvalid
	}
	var wire statsProviderEgressWire
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil || wire.Version != 1 || wire.Counts == nil || wire.Counts.MaxIndex == nil {
		return nil, false, errStatsProviderEgressInvalid
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, false, errStatsProviderEgressInvalid
	}
	snapshot := &statsProviderEgressSnapshot{&model.ProviderEgressCounts{BucketIndexCounts: wire.Counts.Buckets, ReasonCounts: wire.Counts.Reasons, MaxIndex: *wire.Counts.MaxIndex}, time.UnixMilli(wire.Started), time.UnixMilli(wire.Completed)}
	fresh, err := statsProviderEgressSnapshotFresh(snapshot, now)
	return snapshot, fresh, err
}

type statsProviderEgressCache struct {
	store    statsProviderEgressCacheStore
	now      func() time.Time
	newToken func() string
	fill     func(context.Context) (*model.ProviderEgressCounts, error)
}

func (cache statsProviderEgressCache) read(ctx context.Context, key string) (*statsProviderEgressSnapshot, bool, error) {
	value, exists, err := cache.store.get(ctx, key)
	if err != nil || ctx.Err() != nil {
		return nil, false, errStatsProviderEgressUnavailable
	}
	if !exists {
		return nil, false, nil
	}
	snapshot, fresh, decodeErr := decodeStatsProviderEgressSnapshot(value, cache.now())
	if ctx.Err() != nil {
		return nil, false, errStatsProviderEgressUnavailable
	}
	return snapshot, fresh, decodeErr
}

func statsProviderEgressFill(ctx context.Context, fill func(context.Context) (*model.ProviderEgressCounts, error)) (counts *model.ProviderEgressCounts, err error) {
	defer func() {
		if recover() != nil {
			counts, err = nil, errStatsProviderEgressUnavailable
		}
	}()
	counts, err = fill(ctx)
	if err != nil {
		return nil, errStatsProviderEgressUnavailable
	}
	return
}

func (cache statsProviderEgressCache) get(ctx context.Context, policyKey string) (*statsProviderEgressSnapshot, error) {
	if ctx.Err() != nil || len(policyKey) == 0 || len(policyKey) > 4096 {
		return nil, errStatsProviderEgressUnavailable
	}
	keys := statsProviderEgressKeys(policyKey)
	if snapshot, fresh, err := cache.read(ctx, keys.value); err != nil {
		return nil, err
	} else if fresh {
		return snapshot, nil
	}
	token, leaseStarted := cache.newToken(), cache.now()
	acquired, acquireErr := cache.store.acquire(ctx, keys.fill, token, statsProviderEgressLeaseTTL)
	if acquireErr != nil {
		owner, exists, err := cache.store.get(ctx, keys.fill)
		if err != nil || !exists || owner != token || ctx.Err() != nil {
			return nil, errStatsProviderEgressUnavailable
		}
		acquired = true
	}
	// This recheck also closes the miss -> previous owner publishes -> win race.
	if snapshot, fresh, err := cache.read(ctx, keys.value); err != nil {
		return nil, err
	} else if fresh {
		if acquired && cache.store.release(ctx, keys.fill, token) != nil || ctx.Err() != nil {
			return nil, errStatsProviderEgressUnavailable
		}
		return snapshot, nil
	}
	if !acquired {
		return nil, errStatsProviderEgressBusy
	}
	// A delayed acquisition/recheck cannot start work that outlives its lease.
	if cache.now().Sub(leaseStarted) >= statsProviderEgressLeaseTTL-statsProviderEgressFillTime {
		return nil, errStatsProviderEgressUnavailable
	}
	fillCtx, cancel := context.WithTimeout(ctx, statsProviderEgressFillTime)
	defer cancel()
	started := cache.now()
	counts, err := statsProviderEgressFill(fillCtx, cache.fill)
	completed := cache.now()
	deadline, _ := fillCtx.Deadline()
	if err != nil || fillCtx.Err() != nil || !time.Now().Before(deadline) || completed.Sub(started) > statsProviderEgressFillTime || completed.Before(started) || completed.Sub(leaseStarted) >= statsProviderEgressLeaseTTL {
		return nil, errStatsProviderEgressUnavailable
	}
	snapshot := &statsProviderEgressSnapshot{counts, started, completed}
	value, err := encodeStatsProviderEgressSnapshot(snapshot, completed)
	if err != nil {
		return nil, err
	}
	publicationTime := cache.now()
	remaining := min(statsProviderEgressCacheTTL, statsProviderEgressCacheTTL-publicationTime.Sub(completed))
	if remaining <= 0 || fillCtx.Err() != nil || !time.Now().Before(deadline) || publicationTime.Sub(leaseStarted) >= statsProviderEgressLeaseTTL {
		return nil, errStatsProviderEgressUnavailable
	}
	published, publishErr := cache.store.publish(fillCtx, keys, token, value, remaining)
	if fillCtx.Err() != nil || !time.Now().Before(deadline) {
		return nil, errStatsProviderEgressUnavailable
	}
	if publishErr == nil && published {
		return snapshot, nil
	}
	// A lost reply or replaced lease can only recover through a valid publication.
	if retained, fresh, err := cache.read(fillCtx, keys.value); err == nil && fresh {
		return retained, nil
	}
	return nil, errStatsProviderEgressUnavailable
}

func getStatsProviderEgressSnapshot(ctx context.Context, policyKey string) (snapshot *statsProviderEgressSnapshot, err error) {
	defer func() {
		if recover() != nil {
			snapshot, err = nil, errStatsProviderEgressUnavailable
		}
	}()
	cache := statsProviderEgressCache{
		store: redisStatsProviderEgressCacheStore{}, now: time.Now,
		newToken: func() string { return server.NewId().String() },
		fill: func(ctx context.Context) (*model.ProviderEgressCounts, error) {
			if model.ProviderEgressCountsPolicyKey() != policyKey {
				return nil, errStatsProviderEgressUnavailable
			}
			counts := model.CountProviderEgress(ctx)
			if model.ProviderEgressCountsPolicyKey() != policyKey {
				return nil, errStatsProviderEgressUnavailable
			}
			return counts, nil
		},
	}
	if model.ProviderEgressCountsPolicyKey() != policyKey {
		return nil, errStatsProviderEgressUnavailable
	}
	snapshot, err = cache.get(ctx, policyKey)
	if err == nil && model.ProviderEgressCountsPolicyKey() != policyKey {
		return nil, errStatsProviderEgressUnavailable
	}
	return
}
