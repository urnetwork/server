package ratelimiter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type RateLimiter struct {
	rc *redis.Client
}

func New(rc *redis.Client) *RateLimiter {
	return &RateLimiter{
		rc: rc,
	}
}

var ErrRateLimitExceeded = errors.New("rate limit exceeded")

// Allow checks if a request is allowed under the rate limit for a given key.
// Each key is prefixed with "ratelimit:".
// It increments the request count for the key and sets an expiration time if it's the first request.
// If the request count exceeds the specified limit within the given duration, it returns an error.
//
// Parameters:
//
//	ctx - The context for controlling cancellation and deadlines.
//	key - The unique identifier for the rate limit key.
//	limit - The maximum number of allowed requests within the duration.
//	duration - The time window for the rate limit.
//
// Returns:
//
//	error - Returns an error if incrementing the key or setting the expiration fails,
//	        or if the rate limit is exceeded.
func (r *RateLimiter) Allow(ctx context.Context, key string, limit int, duration time.Duration) error {

	rateLimitKey := fmt.Sprintf("ratelimit:%s", key)

	// Increment key
	val, err := r.rc.Incr(ctx, rateLimitKey).Result()
	if err != nil {
		return fmt.Errorf("increment key failed: %w", err)
	}

	// Expire key
	if val == 1 {
		err = r.rc.Expire(ctx, rateLimitKey, duration).Err()
		if err != nil {
			return fmt.Errorf("expire key failed: %w", err)
		}
	}

	// Check limit
	if val > int64(limit) {
		return ErrRateLimitExceeded
	}

	return nil
}

// AllowPerMinute checks if a request is allowed based on a rate limit defined per minute.
// It uses the provided context, key, and maximum rate to determine if the request should be allowed.
// The key is combined with the current time formatted to the minute to create a unique identifier for rate limiting.
// Returns an error if the request exceeds the allowed rate.
func (r *RateLimiter) AllowPerMinute(ctx context.Context, key string, maxRate int) error {
	return r.Allow(ctx, fmt.Sprintf("%s-%s", key, time.Now().Format("2006-01-02-15-04")), maxRate, time.Minute)
}

func (r *RateLimiter) Close() error {
	return r.rc.Close()
}
