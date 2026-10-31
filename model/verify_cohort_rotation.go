package model

// Stale rotation of the verify cohort (LEADERTOP.md design question 6, part
// two; COHORT-ROTATION.md). Without it the lifetime set fills once and no new
// provider is ever measured again, so a claimed fleet whose members were never
// admitted can never earn a head score.
//
// The validator keeps scoring every provider it ever scored, so a provider
// rotated out of the server's lifetime set stays in the validator's census.
// Rotation therefore keeps a second, monotone set: the census, every provider
// ever admitted, mirrored durably in verify_cohort_census. The lifetime set
// becomes the active set that rotation shrinks:
//   - Unclaimed providers are admitted exactly as before rotation: the census
//     never grows past the lifetime limit for them.
//   - Claimed fleets' members may grow the census up to the census limit
//     (CohortCensusLimit), three quarters of the validator's 4,096 bound.
//   - Rotation frees active room only for claimed members waiting for it:
//     it removes at most that many stale members, never a claimed member,
//     never one drawn in this or the previous settlement epoch, never one
//     that is eligible now. A rotated member that returns costs no census.
//   - Every new census member is written through to Postgres. A Redis that
//     loses its census refuses every new provider until this task restores
//     the census from Postgres, so a data loss can never re-admit a census.

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The most stale members one rotation removes.
const verifyCohortMaxRotations = 64

var (
	verifyCohortRotations = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "rotations_total",
		Help: "Stale members rotated out of the verify cohort's lifetime (active) set to make room for claimed fleets' members. They stay in the census.",
	})
	verifyCohortCensusSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "census_size",
		Help: "Providers the deployment has ever admitted to the verify cohort, at the last rotation: the validator's census, bounded by the census limit.",
	})
	verifyCohortCensusRestores = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "census_restores_total",
		Help: "Rotations that found Redis without its census copy and restored it from Postgres. Until then no new provider is admitted.",
	})
)

func init() {
	prometheus.MustRegister(verifyCohortRotations, verifyCohortCensusSize, verifyCohortCensusRestores)
}

// The durable census exists for a deployment: cached per process, since every
// next-hop draw asks.
type verifyCohortCensusCache struct {
	stateLock sync.Mutex
	exists    map[StDeploymentKey]bool
	readAt    map[StDeploymentKey]time.Time
}

var verifyCohortCensusExists = &verifyCohortCensusCache{exists: map[StDeploymentKey]bool{}, readAt: map[StDeploymentKey]time.Time{}}

const verifyCohortCensusCacheTtl = time.Minute

// Whether a durable census exists for the deployment. Once one does it never
// goes away, so a true answer is kept and a false one is read again after the
// cache ttl, which only delays fail-closed by that long.
//
// Every next-hop draw asks, so a failed read must not fail the draw. Without
// the census table (an api deployed ahead of its migration) no durable census
// can exist, and admission is as it was before rotation. Any other failure
// fails closed and is not cached: a ready Redis census admits as usual, and
// a lost one refuses new providers until a read succeeds.
func verifyCohortCensusRequired(ctx context.Context, deploymentKey StDeploymentKey) bool {
	cache := verifyCohortCensusExists
	cache.stateLock.Lock()
	exists, readAt := cache.exists[deploymentKey], cache.readAt[deploymentKey]
	cache.stateLock.Unlock()
	if exists || !readAt.IsZero() && time.Since(readAt) < verifyCohortCensusCacheTtl {
		return exists
	}
	var readErr error
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			rows, err := conn.Query(ctx, `SELECT EXISTS (SELECT 1 FROM verify_cohort_census WHERE deployment_key = $1)`, string(deploymentKey))
			server.WithPgResult(rows, err, func() {
				if rows.Next() {
					server.Raise(rows.Scan(&exists))
				}
			})
		})
	}, func(err error) {
		readErr = err
	})
	if readErr != nil {
		if !verifyCohortCensusTableMissing(readErr) {
			glog.Infof("[verify]cohort census read failed; new providers wait for a ready census: %s\n", readErr)
			return true
		}
		exists = false
	}
	cache.stateLock.Lock()
	cache.exists[deploymentKey], cache.readAt[deploymentKey] = exists, time.Now()
	cache.stateLock.Unlock()
	return exists
}

// The census table does not exist: the api runs ahead of its migration.
func verifyCohortCensusTableMissing(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UndefinedTable
}

// Writes a new census member through to Postgres. A failed write is logged:
// the next rotation mirrors the Redis census again.
func recordVerifyCohortCensus(ctx context.Context, deploymentKey StDeploymentKey, clientId server.Id) {
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO verify_cohort_census (deployment_key, client_id, admitted_at) VALUES ($1, $2, $3) ON CONFLICT (deployment_key, client_id) DO NOTHING`,
				string(deploymentKey), clientId, server.NowUtc(),
			))
		})
	}, func(err error) {
		glog.Infof("[verify]cohort census write-through failed (the next rotation mirrors it): %s\n", err)
	})
}

// The outcome of one rotation.
type VerifyCohortRotation struct {
	// providers ever admitted, as this run found them
	Census int
	// the active (lifetime) set after this run's rotation
	Active int
	// claimed members waiting for active room
	Demand int
	// stale members rotated out: the demand less the free active slots (the
	// lifetime limit less the active set before rotation), at most 64
	Rotated  int
	Restored bool
}

// RotateVerifyCohort makes the census ready and durable, then rotates stale
// members out of the lifetime set while claimed fleets' members wait for
// room. The taskworker runs it after each binding sync, once the priority set
// is fresh.
func RotateVerifyCohort(ctx context.Context, settings *VerifySettings) *VerifyCohortRotation {
	if settings.CohortSize <= 0 || settings.CohortDeploymentKey == "" {
		return nil
	}
	deploymentKey := settings.CohortDeploymentKey
	epoch, ok := verifyCohortEpoch(ctx, deploymentKey)
	if !ok {
		return nil
	}
	keys := newVerifyCohortKeys(deploymentKey, epoch)
	result := &VerifyCohortRotation{}

	// the durable census, then Redis's census with every active member
	durable := []string{}
	server.Db(ctx, func(conn server.PgConn) {
		rows, err := conn.Query(ctx, `SELECT client_id FROM verify_cohort_census WHERE deployment_key = $1`, string(deploymentKey))
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var clientId server.Id
				server.Raise(rows.Scan(&clientId))
				durable = append(durable, clientId.String())
			}
		})
	})
	var census []string
	server.Redis(ctx, func(r server.RedisClient) {
		ready, err := r.Exists(ctx, keys.censusReady).Result()
		server.Raise(err)
		count, err := r.SCard(ctx, keys.census).Result()
		server.Raise(err)
		// restore what Redis lost; a ready census holds every durable member
		if ready == 0 || count < int64(len(durable)) {
			result.Restored = 0 < len(durable)
			for start := 0; start < len(durable); start += 512 {
				batch := make([]any, 0, 512)
				for _, member := range durable[start:min(start+512, len(durable))] {
					batch = append(batch, member)
				}
				server.Raise(r.SAdd(ctx, keys.census, batch...).Err())
			}
		}
		// every active member, including any an api still on the old script
		// admitted, is in the census
		server.Raise(r.SUnionStore(ctx, keys.census, keys.census, keys.lifetime).Err())
		count, err = r.SCard(ctx, keys.census).Result()
		server.Raise(err)
		if int64(len(durable)) < count {
			census, err = r.SMembers(ctx, keys.census).Result()
			server.Raise(err)
		}
	})
	// mirror what Postgres lacks before the census counts
	missing := []server.Id{}
	known := map[string]bool{}
	for _, member := range durable {
		known[member] = true
	}
	for _, member := range census {
		if !known[member] {
			if clientId, err := server.ParseId(member); err == nil {
				missing = append(missing, clientId)
			}
		}
	}
	if 0 < len(missing) {
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO verify_cohort_census (deployment_key, client_id, admitted_at)
					SELECT $1, client_id, $3 FROM unnest($2::uuid[]) AS client_id
					ON CONFLICT (deployment_key, client_id) DO NOTHING
				`,
				string(deploymentKey), missing, server.NowUtc(),
			))
		})
	}
	if result.Restored {
		verifyCohortCensusRestores.Inc()
	}
	if 0 < len(durable)+len(missing) {
		cache := verifyCohortCensusExists
		cache.stateLock.Lock()
		cache.exists[deploymentKey], cache.readAt[deploymentKey] = true, time.Now()
		cache.stateLock.Unlock()
	}
	previous := newVerifyCohortKeys(deploymentKey, max(epoch, 1)-1)
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.Set(ctx, keys.censusReady, "1", 0).Err())

		var active, censusCount *redis.IntCmd
		var priority *redis.StringSliceCmd
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			active = pipe.SCard(ctx, keys.lifetime)
			censusCount = pipe.SCard(ctx, keys.census)
			priority = pipe.SMembers(ctx, keys.priority)
			return nil
		})
		server.Raise(err)
		result.Active, result.Census = int(active.Val()), int(censusCount.Val())

		// claimed members that are eligible and not active
		waiting := priority.Val()
		if 0 < len(waiting) {
			args := make([]any, len(waiting))
			for i, member := range waiting {
				args[i] = member
			}
			inActive, err := r.SMIsMember(ctx, keys.lifetime, args...).Result()
			server.Raise(err)
			inEligible, err := r.SMIsMember(ctx, verifyEligibleKey, args...).Result()
			server.Raise(err)
			for i := range waiting {
				if !inActive[i] && inEligible[i] {
					result.Demand++
				}
			}
		}
		need := min(result.Demand-(settings.CohortLifetimeLimit-result.Active), verifyCohortMaxRotations)
		if need <= 0 {
			return
		}

		// stale: not drawn this or last epoch, not claimed, not eligible now
		members, err := r.SMembers(ctx, keys.lifetime).Result()
		server.Raise(err)
		slices.Sort(members)
		protected := map[string]bool{}
		for _, member := range waiting {
			protected[member] = true
		}
		for start := 0; start < len(members) && result.Rotated < need; start += 512 {
			batch := members[start:min(start+512, len(members))]
			args := make([]any, len(batch))
			for i, member := range batch {
				args[i] = member
			}
			inEpoch, err := r.SMIsMember(ctx, keys.epoch, args...).Result()
			server.Raise(err)
			inPrevious, err := r.SMIsMember(ctx, previous.epoch, args...).Result()
			server.Raise(err)
			inEligible, err := r.SMIsMember(ctx, verifyEligibleKey, args...).Result()
			server.Raise(err)
			stale := []any{}
			for i, member := range batch {
				if !inEpoch[i] && !inPrevious[i] && !inEligible[i] && !protected[member] && len(stale)+result.Rotated < need {
					stale = append(stale, member)
				}
			}
			if 0 < len(stale) {
				removed, err := r.SRem(ctx, keys.lifetime, stale...).Result()
				server.Raise(err)
				result.Rotated += int(removed)
			}
		}
		result.Active -= result.Rotated
	})
	verifyCohortRotations.Add(float64(result.Rotated))
	verifyCohortCensusSize.Set(float64(result.Census))
	return result
}
