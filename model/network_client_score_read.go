package model

import (
	"context"
	"errors"

	"github.com/redis/go-redis/v9"
)

// An absent score key preserves the existing cache-miss/fallback policy.
// Every other read error invalidates the requested batch, even when Exec
// returns only an earlier redis.Nil. Do not retry or return a partial pool.
func execClientScoreReadPipeline(ctx context.Context, pipeline redis.Pipeliner) error {
	commands, err := pipeline.Exec(ctx)
	for _, command := range commands {
		if commandErr := command.Err(); commandErr != nil && !errors.Is(commandErr, redis.Nil) {
			return commandErr
		}
	}
	if err != nil && !errors.Is(err, redis.Nil) {
		return err
	}
	return nil
}
