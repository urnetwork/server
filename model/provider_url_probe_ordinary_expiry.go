// Ordinary URL admission retires expired completed runs once per shard interval.
package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Ordinary admission orders by due time alone and never reads completed-run
// counts, yet its expiry sweep merges the expired head of every owned slot.
// The xmin horizon keeps dead cycle versions in those heads, so on Main the
// sweep cost three to four times the claim it preceded, on every worker turn.
// One complete sweep per shard and interval keeps counts and receipt
// retirement converging; priority admission still maintains on every claim.
const providerUrlProbeOrdinaryExpiryInterval = 10 * time.Second

// The gate only saves work: a slow or failed Redis read sweeps as before.
const providerUrlProbeOrdinaryExpiryRedisTimeout = 250 * time.Millisecond

// One hash-tagged key per logical shard. A new slot geometry starts unswept.
func providerUrlProbeOrdinaryExpiryKey(shardIndex, shardCount int) string {
	return fmt.Sprintf("url_probe_ordinary_expiry:v1:{%d:%d}", shardCount, shardIndex)
}

// Returns the admission clock of the shard's latest complete ordinary sweep;
// present is false for a missing key. Replaced in tests to inject an error.
var readProviderUrlProbeOrdinaryExpiry = func(ctx context.Context, key string) (sweptAt time.Time, present bool, returnErr error) {
	readCtx, cancel := context.WithTimeout(ctx, providerUrlProbeOrdinaryExpiryRedisTimeout)
	defer cancel()
	returnErr = server.RedisWithDeadline(readCtx, func(client server.RedisClient) error {
		value, err := client.Get(readCtx, key).Result()
		if errors.Is(err, redis.Nil) {
			return nil
		}
		if err != nil {
			return err
		}
		micros, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return err
		}
		sweptAt, present = time.UnixMicro(micros).UTC(), true
		return nil
	})
	if returnErr != nil {
		sweptAt, present = time.Time{}, false
	}
	return
}

// Keeps the record for two intervals of wall time. Replaced in tests to
// inject an error.
var writeProviderUrlProbeOrdinaryExpiry = func(ctx context.Context, key string, sweptAt time.Time) error {
	writeCtx, cancel := context.WithTimeout(ctx, providerUrlProbeOrdinaryExpiryRedisTimeout)
	defer cancel()
	return server.RedisWithDeadline(writeCtx, func(client server.RedisClient) error {
		return client.Eval(writeCtx, `return redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2])`, []string{key},
			strconv.FormatInt(sweptAt.UnixMicro(), 10), (2 * providerUrlProbeOrdinaryExpiryInterval).Milliseconds()).Err()
	})
}

// A missing, unreadable, later or interval-old record means sweep now. Two
// concurrent admissions of one shard may both sweep; each page is bounded and
// skips the other's locked rows.
func providerUrlProbeOrdinaryExpiryDue(ctx context.Context, now time.Time, shardIndex, shardCount int) bool {
	sweptAt, present, err := readProviderUrlProbeOrdinaryExpiry(ctx, providerUrlProbeOrdinaryExpiryKey(shardIndex, shardCount))
	if err != nil || !present {
		return true
	}
	return now.Before(sweptAt) || !now.Before(sweptAt.Add(providerUrlProbeOrdinaryExpiryInterval))
}
