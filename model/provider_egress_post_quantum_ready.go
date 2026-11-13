// What the URL ingest does with a stored outcome's end-to-end evidence beyond
// its pq_outcome column (FP2-PROTOCOL.md §12.3, §10 step 2), and the ready
// marker strict post-quantum selection waits for.
//
// A conclusive observation is recorded in provider_modern_evidence, as the
// capability probe recorded its own, so the window reading and the modern
// census keep their table whichever probe measured the provider. It is
// post-quantum only when the turn's outcome is positive (FP2-PROTOCOL.md
// §12.8).
//
// The ready marker. Strict post-quantum selection matches only providers whose
// newest post-quantum outcome in the eight-hour window is positive, so it may
// read the measured signal only once a whole window of outcomes from a
// post-quantum-measuring prober exists; before that, every provider would read
// not positive. Each stored outcome that carries end-to-end evidence notes the
// stream: the time it began (since) and the time of its latest outcome (seen),
// in Redis. The score export calls MarkClientScorePostQuantumReady after each
// complete export, which sets ClientScorePostQuantumReadyKey, with the
// export's ttl, while the stream has run for a whole window and is still
// running, and deletes it otherwise. A stream silent for longer than
// SeenMaxAge has ended, a prober rolled back for instance: the marker is
// cleared at the next export, so strict post-quantum selection returns to the
// probe's modern evidence of the last seven days (provider_modern_evidence,
// readProviderPostQuantumEvidenceClientIds and readProviderModernClientIds)
// rather than to an emptying window, and the next stream starts a window of
// its own. Neither side of the marker counts a provider's own declaration.
//
// Functions are safe for concurrent use; the stream's keys tolerate racing
// writers, which can only move since by the time between two outcomes.
package model

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
)

// Set while strict post-quantum selection may read the measured signal: the
// export's value is the stream's start, in unix milliseconds. The selection
// reads it (GetClientScorePostQuantumReady).
const ClientScorePostQuantumReadyKey = "client_score_post_quantum_v1_ready"

// When the current stream of post-quantum-measured outcomes began, in unix
// milliseconds.
const clientScorePostQuantumSinceKey = "client_score_post_quantum_v1_since"

// When the stream's latest outcome was stored, in unix milliseconds.
const clientScorePostQuantumSeenKey = "client_score_post_quantum_v1_seen"

// The marker's timings.
type ClientScorePostQuantumReadySettings struct {
	// How long a stream must have run before the marker is set: one whole
	// evidence window, ProviderEgressHealthMaxAge, so every online provider
	// has had its post-quantum turns inside the window strict selection
	// reads.
	ReadyAfter time.Duration
	// The longest a running stream goes between two stored outcomes. About
	// one in ten URL turns measures post-quantum, several a second across the
	// fleet, so thirty minutes of silence means no post-quantum prober is
	// running. A longer gap ends the stream: the marker clears and the next
	// outcome starts a new one.
	SeenMaxAge time.Duration
}

// The marker's settings.
func DefaultClientScorePostQuantumReadySettings() *ClientScorePostQuantumReadySettings {
	return &ClientScorePostQuantumReadySettings{
		ReadyAfter: ProviderEgressHealthMaxAge,
		SeenMaxAge: 30 * time.Minute,
	}
}

// After a stored outcome: records its conclusive end-to-end observation as
// modern evidence and notes the post-quantum stream. A replay stored nothing
// and does neither. Neither may fail the ingest, whose outcome is already
// committed: a failure is logged, and the provider's next post-quantum turn
// records again.
func recordProviderEgressHealthE2e(ctx context.Context, health *ProviderEgressHealth, stored ProviderEgressHealthStored) {
	if !stored.Stored || health.UrlProbeEvidence == nil || health.UrlProbeEvidence.E2e == nil {
		return
	}
	e2e := health.UrlProbeEvidence.E2e
	// only an outcome the window keeps is a sample of the provider: a recheck
	// or one on probation stores no post-quantum outcome either
	if !stored.Recheck && !stored.Probation {
		evidence, conclusive := providerModernEvidenceOfE2e(e2e, health.UrlProbeEvidence.MeasuredAt)
		if conclusive {
			server.HandleError(func() {
				if err := RecordProviderModernEvidence(ctx, health.ClientId, evidence); err != nil {
					glog.Warningf("[egress]modern evidence of %s from a URL turn was refused: %s\n", health.ClientId, err)
				}
			})
		}
	}
	server.HandleError(func() {
		noteClientScorePostQuantumStream(ctx, server.NowUtc(), DefaultClientScorePostQuantumReadySettings())
	})
}

// The modern evidence of a URL turn's end-to-end observation, and whether it
// is conclusive as the capability probe's readings were: the session
// concluded, positive or negative, and an acknowledgement read the window.
// Post-quantum is the turn's positive outcome alone: a post-quantum group
// that sealed before the IPv4 HTTPS leg's request, and that leg passed. A
// negative is never post-quantum, a post-quantum group that never sealed
// included, though its group is recorded: strict selection reads this row
// while the post-quantum marker is down, and only a positive quality probe
// handshake counts (owner, 2026-11-13; FP2-PROTOCOL.md §12.8). The
// capability probe and this ingest before it counted any negotiated
// post-quantum group. The observation carries no window size, so a resizable
// window records a largest size of 0.
func providerModernEvidenceOfE2e(e2e *egresshealth.UrlProbeE2eEvidence, observedAt time.Time) (ProviderModernEvidence, bool) {
	if e2e.Pq != egresshealth.UrlProbePqPositive && e2e.Pq != egresshealth.UrlProbePqNegative {
		return ProviderModernEvidence{}, false
	}
	if e2e.Window != egresshealth.UrlProbeWindowResizable && e2e.Window != egresshealth.UrlProbeWindowFixed {
		return ProviderModernEvidence{}, false
	}
	return ProviderModernEvidence{
		E2eNegotiated: e2e.KeyExchange != "",
		KeyExchange:   e2e.KeyExchange,
		PostQuantum: e2e.Pq == egresshealth.UrlProbePqPositive &&
			egresshealth.UrlProbeKeyExchangePostQuantum(e2e.KeyExchange),
		WindowAdvertised: e2e.Window == egresshealth.UrlProbeWindowResizable,
		ObservedAt:       observedAt.UTC(),
	}, true
}

// Notes one stored outcome of the post-quantum stream at now: a stream whose
// latest outcome is older than SeenMaxAge, or absent, starts again at now.
func noteClientScorePostQuantumStream(ctx context.Context, now time.Time, settings *ClientScorePostQuantumReadySettings) {
	nowMillis := strconv.FormatInt(now.UnixMilli(), 10)
	server.Redis(ctx, func(r server.RedisClient) {
		seenAt, seen, err := getClientScorePostQuantumTime(ctx, r, clientScorePostQuantumSeenKey)
		server.Raise(err)
		if !seen || settings.SeenMaxAge <= now.Sub(seenAt) {
			server.Raise(r.Set(ctx, clientScorePostQuantumSinceKey, nowMillis, 0).Err())
		} else {
			// a since lost on its own (an eviction) restarts the window
			server.Raise(r.SetNX(ctx, clientScorePostQuantumSinceKey, nowMillis, 0).Err())
		}
		server.Raise(r.Set(ctx, clientScorePostQuantumSeenKey, nowMillis, 0).Err())
	})
}

// Sets or clears the ready marker after a complete score export, with the
// export's ttl, and returns whether it is set. The export calls it once per
// complete export; a Redis failure is returned and leaves the marker as it
// was, to expire with its ttl.
func MarkClientScorePostQuantumReady(ctx context.Context, ttl time.Duration) (ready bool, returnErr error) {
	return markClientScorePostQuantumReady(ctx, ttl, server.NowUtc(), DefaultClientScorePostQuantumReadySettings())
}

// MarkClientScorePostQuantumReady at now under settings.
func markClientScorePostQuantumReady(ctx context.Context, ttl time.Duration, now time.Time, settings *ClientScorePostQuantumReadySettings) (ready bool, returnErr error) {
	if ttl <= 0 {
		return false, fmt.Errorf("the post-quantum ready marker needs a positive ttl, got %s", ttl)
	}
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			seenAt, seen, err := getClientScorePostQuantumTime(ctx, r, clientScorePostQuantumSeenKey)
			server.Raise(err)
			if !seen || settings.SeenMaxAge <= now.Sub(seenAt) {
				// the stream ended: the next one starts a window of its own
				server.Raise(r.Del(ctx, clientScorePostQuantumSinceKey).Err())
				server.Raise(r.Del(ctx, ClientScorePostQuantumReadyKey).Err())
				return
			}
			sinceAt, since, err := getClientScorePostQuantumTime(ctx, r, clientScorePostQuantumSinceKey)
			server.Raise(err)
			if !since || now.Sub(sinceAt) < settings.ReadyAfter {
				server.Raise(r.Del(ctx, ClientScorePostQuantumReadyKey).Err())
				return
			}
			server.Raise(r.Set(ctx, ClientScorePostQuantumReadyKey, strconv.FormatInt(sinceAt.UnixMilli(), 10), ttl).Err())
			ready = true
		})
	}, func(err error) {
		ready = false
		returnErr = err
	})
	return
}

// One of the stream's times, false when the key is absent. A value that is
// not unix milliseconds reads as absent, so a damaged key restarts the stream
// rather than wedging it.
func getClientScorePostQuantumTime(ctx context.Context, r server.RedisClient, key string) (time.Time, bool, error) {
	value, err := r.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	millis, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(millis).UTC(), true, nil
}
