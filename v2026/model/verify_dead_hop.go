package model

// verify_dead_hop.go — dead-hop probation for the verify next-hop draw (owner
// decision 2026-11-04, option (a) of the 11-03 validator dead-hop root cause).
//
// Why: an assignment the provider never confirms ends the validator's trail at
// that hop, and some providers confirm nothing at all. In the public
// /verify/stats index for 2026-10-28 to 11-04, 68 of 296 providers confirmed
// none of their assignments in 7 days, and they took 20.8% of all
// assignments. Per-provider outcomes are close to all or nothing: of the
// providers with at least 20 assignments, 66 confirmed under 10% and 173 at
// least 90%. Admission checks a candidate's identity, live egress and provide
// mode, never its outcomes, so the cohort re-admits the same carried members
// every epoch (verify_cohort_model.go), and every 8-hop trail that draws one
// of them fails.
//
// The rule, over the trailing DeadHopWindow (24 h) of verify_provider_stats:
//   - A provider with no confirmation and at least DeadHopAssignments (50)
//     assignments goes on probation.
//   - Every draw skips a provider on probation, except that it may be drawn
//     once per DeadHopProbeInterval (4 h), so that one that comes back can
//     confirm again.
//   - It stays on probation while its window holds assignments (its probes)
//     and no confirmation. Its first confirmation ends it, and so does a window
//     with no assignment at all (it went away; it has to earn probation again).
//
// Why these values: in that week a 24 h window would have put on probation
// 64 of the 68 providers that never confirmed, and 2 that had confirmed
// earlier: one had confirmed nothing for its last 3 days, and the other
// confirms 1.4% of its hops. A 6 h window would also have caught 13
// providers that confirm 60-67% of their hops, a fleet that went down
// together for about 7 hours, and a 12 h window 3 providers with outages of
// 12 to 24 hours. The never-confirming providers drew 137 to 281 assignments
// a day (p10 to max), so 50 is reached within a day, and a provider that
// confirms even half its hops fails 50 in a row with probability 2^-50. One
// probe every 4 h costs about 1% of all assignments, against the 21-23% that
// go to these providers today.
//
// Payout and Wilson inputs: the counts are the verify_provider_stats rollup,
// the rows that feed the Wilson reliability of the audit rollups and the
// payout artifact (GetStEpochClientReliability, protocol.ReliabilityPPM with
// ReliabilityAMin). Probation only reads them. A skipped draw is not an
// assignment and is not recorded as a miss: that would be a payout-policy
// change, which the owner did not take. With no confirmation a provider's
// reliability is 0 at any exposure, so a provider with no confirmation in
// the payout epoch earns nothing with or without the skipped draws. One that
// confirmed earlier in the epoch keeps the reliability of the assignments it
// actually got.
//
// One api process at a time recomputes the probation set from Postgres when
// the shared refresh marker lapses, every DeadHopRefreshInterval, and every
// draw reads the set from Redis. A refresh that fails keeps the last set and
// never fails the draw. A set nobody refreshes lapses, and an empty set is
// the draw as it was before probation.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// The providers on probation: client ids, replaced by each refresh.
const verifyDeadHopKey = "verify_dead_hop"

// Held for one refresh interval by the process that refreshes.
const verifyDeadHopRefreshKey = "verify_dead_hop_refresh"

// A provider's probe slot, held for one probe interval after its probe.
func verifyDeadHopProbeKey(clientId string) string {
	return fmt.Sprintf("{vdhp_%s}", clientId)
}

// Bounds the refresh's read inside the draw that runs it. The read is one
// indexed aggregate over a day of 15-minute rollup rows, about 20,000.
const verifyDeadHopRefreshTimeout = 5 * time.Second

var (
	verifyDeadHopProbes = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_dead_hop", Name: "probes_total",
		Help: "Verify next hops assigned to a provider on dead-hop probation (no confirmation of at least 50 assignments in 24 h): at most one per provider every probe interval, so a provider that comes back can confirm again.",
	})
	verifyDeadHopRefreshes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_dead_hop", Name: "refreshes_total",
		Help: "Dead-hop probation refreshes from verify_provider_stats, by result (ok, failed). A failed refresh keeps the last probation set.",
	}, []string{"result"})
)

func init() {
	prometheus.MustRegister(verifyDeadHopProbes, verifyDeadHopRefreshes)
}

func verifyDeadHopEnabled(settings *VerifySettings) bool {
	return 0 < settings.DeadHopAssignments && 0 < settings.DeadHopWindow && 0 < settings.DeadHopRefreshInterval
}

// A set that no refresh replaces lapses after this. By then a member has had
// no probe for a whole window, which would end its probation anyway.
func verifyDeadHopTtl(settings *VerifySettings) time.Duration {
	return 2 * settings.DeadHopWindow
}

// refreshVerifyDeadHopIfDue refreshes the probation set when the shared marker
// has lapsed. One process takes the marker per interval and refreshes inline:
// each /verify step already writes to Postgres, and this read is smaller.
// A failed refresh is counted and logged, and the draw goes on with the last
// set.
func refreshVerifyDeadHopIfDue(ctx context.Context, settings *VerifySettings) {
	if !verifyDeadHopEnabled(settings) {
		return
	}
	due := false
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		due, err = r.SetNX(ctx, verifyDeadHopRefreshKey, server.NowUtc().UnixMilli(), settings.DeadHopRefreshInterval).Result()
		server.Raise(err)
	})
	if !due {
		return
	}
	// a caller that goes away does not leave the set stale for an interval
	refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), verifyDeadHopRefreshTimeout)
	defer cancel()
	server.HandleError(func() {
		benched, entered, left := refreshVerifyDeadHop(refreshCtx, server.NowUtc(), settings)
		verifyDeadHopRefreshes.WithLabelValues("ok").Inc()
		glog.Infof("[verify]dead-hop probation: %d providers (%d in, %d out)\n", benched, entered, left)
	}, func(err error) {
		verifyDeadHopRefreshes.WithLabelValues("failed").Inc()
		glog.Infof("[verify]dead-hop probation refresh failed; draws keep the last probation set: %s\n", err)
	})
}

// refreshVerifyDeadHop replaces the probation set from the verify_provider_stats
// periods that start in the trailing window at now. A provider enters with no
// confirmation and at least DeadHopAssignments assignments there; a member
// stays while the window holds an assignment of it and no confirmation.
// Returns the new set's size and how many providers entered and left it.
func refreshVerifyDeadHop(ctx context.Context, now time.Time, settings *VerifySettings) (benched int, entered int, left int) {
	type tally struct {
		assignments   int64
		confirmations int64
	}
	tallies := map[string]tally{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				client_id,
				SUM(assignments)::bigint,
				SUM(confirmations)::bigint
			FROM verify_provider_stats
			WHERE $1 <= period_start AND period_start <= $2
			GROUP BY client_id
			`,
			now.Add(-settings.DeadHopWindow).UTC(),
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var counts tally
				server.Raise(result.Scan(&clientId, &counts.assignments, &counts.confirmations))
				tallies[clientId.String()] = counts
			}
		})
	})

	server.Redis(ctx, func(r server.RedisClient) {
		current, err := r.SMembers(ctx, verifyDeadHopKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		members := map[string]bool{}
		for _, member := range current {
			members[member] = true
		}
		next := []any{}
		stayed := 0
		for clientId, counts := range tallies {
			if 0 < counts.confirmations {
				continue
			}
			if members[clientId] && 0 < counts.assignments {
				next = append(next, clientId)
				stayed += 1
			} else if settings.DeadHopAssignments <= counts.assignments {
				next = append(next, clientId)
				entered += 1
			}
		}
		left = len(members) - stayed
		_, err = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, verifyDeadHopKey)
			if 0 < len(next) {
				pipe.SAdd(ctx, verifyDeadHopKey, next...)
				pipe.Expire(ctx, verifyDeadHopKey, verifyDeadHopTtl(settings))
			}
			return nil
		})
		server.Raise(err)
		benched = len(next)
	})
	return
}

// filterVerifyDeadHop returns the candidates a draw may pick: every candidate
// not on probation, and each one on probation whose probe slot is free. probes
// marks the latter; a draw that assigns one claims its slot first
// (claimVerifyDeadHopProbe). Redis errors panic like every other verify state
// read.
func filterVerifyDeadHop(ctx context.Context, r server.RedisClient, candidates []string, settings *VerifySettings) (kept []string, probes map[string]bool) {
	if !verifyDeadHopEnabled(settings) || len(candidates) == 0 {
		return candidates, nil
	}
	args := make([]any, len(candidates))
	for i, candidate := range candidates {
		args[i] = candidate
	}
	onProbation, err := r.SMIsMember(ctx, verifyDeadHopKey, args...).Result()
	server.Raise(err)
	benched := []string{}
	for i, candidate := range candidates {
		if onProbation[i] {
			benched = append(benched, candidate)
		}
	}
	if len(benched) == 0 {
		return candidates, nil
	}
	probes = map[string]bool{}
	if 0 < settings.DeadHopProbeInterval {
		held := make([]*redis.IntCmd, len(benched))
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			for i, candidate := range benched {
				held[i] = pipe.Exists(ctx, verifyDeadHopProbeKey(candidate))
			}
			return nil
		})
		server.Raise(err)
		for i, candidate := range benched {
			if held[i].Val() == 0 {
				probes[candidate] = true
			}
		}
	}
	kept = []string{}
	for i, candidate := range candidates {
		if !onProbation[i] || probes[candidate] {
			kept = append(kept, candidate)
		}
	}
	return kept, probes
}

// claimVerifyDeadHopProbe takes a provider's probe slot for one probe interval.
// False when another draw holds it.
func claimVerifyDeadHopProbe(ctx context.Context, r server.RedisClient, candidate string, settings *VerifySettings) bool {
	if settings.DeadHopProbeInterval <= 0 {
		return false
	}
	claimed, err := r.SetNX(ctx, verifyDeadHopProbeKey(candidate), 1, settings.DeadHopProbeInterval).Result()
	server.Raise(err)
	if claimed {
		verifyDeadHopProbes.Inc()
	}
	return claimed
}
