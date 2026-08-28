package competition

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/urnetwork/server/v2026"
)

// PostgreSQL remains the authority for job state, FIFO order, leases, and
// finalization. This Redis list is a rebuildable dispatch index: losing it can
// add at most one worker poll interval because Claim always falls back to the
// authoritative ordered SQL query.
func competitionFifoKeys(settings *Settings) (string, string) {
	digest := sha256.Sum256([]byte(settings.CompetitionId))
	prefix := "competition:{" + hex.EncodeToString(digest[:]) + "}:fifo:v1"
	return prefix + ":list", prefix + ":members"
}

// Adds a queued job at most once. The set is only an O(1) deduplication sidecar;
// the list is the FIFO. Both keys share one Redis Cluster hash slot and mutate
// in one script. A stale or missing index remains recoverable from PostgreSQL.
func enqueueCompetitionJob(ctx context.Context, settings *Settings, jobId server.Id) error {
	const enqueueScript = `
if redis.call('SADD', KEYS[2], ARGV[1]) == 1 then
  redis.call('RPUSH', KEYS[1], ARGV[1])
  return 1
end
return 0
`
	listKey, memberKey := competitionFifoKeys(settings)
	return captureRedisError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.Eval(
				ctx,
				enqueueScript,
				[]string{listKey, memberKey},
				jobId.String(),
			).Err())
		})
	})
}

func dequeueCompetitionJob(ctx context.Context, settings *Settings) (*server.Id, error) {
	const dequeueScript = `
local value = redis.call('LPOP', KEYS[1])
if value then
  redis.call('SREM', KEYS[2], value)
end
return value
`
	listKey, memberKey := competitionFifoKeys(settings)
	var encoded string
	err := captureRedisError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			value, popErr := client.Eval(ctx, dequeueScript, []string{listKey, memberKey}).Text()
			if errors.Is(popErr, server.RedisNil) {
				return
			}
			server.Raise(popErr)
			encoded = value
		})
	})
	if err != nil || encoded == "" {
		return nil, err
	}
	jobId, err := server.ParseId(encoded)
	if err != nil {
		// Redis is not authoritative. Discard a malformed wake-up and let the
		// ordered PostgreSQL query recover the durable job, if one exists.
		return nil, nil
	}
	return &jobId, nil
}

func checkCompetitionFifo(ctx context.Context, settings *Settings) error {
	listKey, _ := competitionFifoKeys(settings)
	return captureRedisError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			server.Raise(client.LLen(ctx, listKey).Err())
		})
	})
}

func captureRedisError(run func()) error {
	if recovered := server.HandleError(run); recovered != nil {
		if err, ok := recovered.(error); ok {
			return err
		}
		return fmt.Errorf("%v", recovered)
	}
	return nil
}
