// todo(fp2): a local stand-in for S1's file of the same path, which owns the
// marker and replaces this one when the evidence migration merges. Only the
// two names S2 calls are here, with S1's signatures.
package model

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server"
)

// Set once the URL probe has measured post-quantum outcomes for a whole
// evidence window (FP2-PROTOCOL.md §10).
const ClientScorePostQuantumReadyKey = "client_score_post_quantum_v1_ready"

// Whether the marker is set after this export; the stand-in only reads it.
func MarkClientScorePostQuantumReady(ctx context.Context, ttl time.Duration) (ready bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.Get(ctx, ClientScorePostQuantumReadyKey).Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		ready = true
	})
	return
}
