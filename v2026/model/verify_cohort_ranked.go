package model

// Tier B of the verify cohort's rolling census (sn VERIFY-COHORT-V2.md §2.6):
// one representative of each unclaimed network the board ranks in its top 400,
// drawn after claimed fleets' members and before carried members, so a network
// near the 200 line can become measurable without claiming first. The line at
// 200 sits in a near-tie of one-prefix networks, so the whole paged top 400 is
// covered and a network's measurability doesn't flip on float noise there.
//
// Anti-sybil, without refusing anyone a sign-up:
//   - one representative per /29 or /48 prefix, the higher-ranked network's
//     winning; the board already splits a shared prefix's score;
//   - a network qualifies only in the top 400 in at least 3 of the last 4 board
//     folds, about 3.6 hours, which also limits churn at the boundary;
//   - an exact address that more than one client holds attributes to none of
//     them, so none of them is eligible (verifyClientHasLiveEgress);
//   - no network age gate (owner, 2026-11-13 and 11-14: no sign-up checks).
//
// The board fold recomputes the set once a tempo, in the rolling census only;
// it lapses after two hours if the folds stop.

import (
	"context"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/server/v2026"
)

// The ranks tier B covers: the board's paged top 400 (SnTopPagedPositions).
const VerifyCohortRankedPositions = SnTopPagedPositions

// A network qualifies when it is in the top 400 in at least this many of the
// last verifyCohortRankedFolds board folds, the current one among them.
const (
	verifyCohortRankedStableFolds = 3
	verifyCohortRankedFolds       = 4
)

// Two folds: a fold that stops lets the set lapse instead of keeping a stale
// ranking's representatives ahead of carried members.
const verifyCohortRankedTtl = 2 * time.Hour

var verifyCohortRankedMembers = prometheus.NewGauge(prometheus.GaugeOpts{
	Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "ranked_members",
	Help: "Representatives in the verify cohort's ranked set after the last board fold (tier B, sn VERIFY-COHORT-V2.md §2.6): one per unclaimed network ranked in the top 400 in 3 of the last 4 folds, one per prefix. 0 outside the rolling census.",
})

func init() {
	prometheus.MustRegister(verifyCohortRankedMembers)
}

// One network on the board.
type VerifyCohortRankedNetwork struct {
	NetworkId server.Id
	// 1-based rank; 0 is unranked
	Position int
	Claimed  bool
}

// One provider on the board.
type VerifyCohortRankedProvider struct {
	ClientId   server.Id
	NetworkId  server.Id
	Eligible   bool
	PrefixHash [32]byte
	HasPrefix  bool
}

// Whether the provider has a prefix pin. The zero hash is none: it is what a
// provider that declared a proxy egress records, which shows no unit.
func (self VerifyCohortRankedProvider) hasPrefix() bool {
	return self.HasPrefix && self.PrefixHash != ([32]byte{})
}

// SelectVerifyCohortRanked picks tier B's representatives, in rank order: for
// each unclaimed network ranked 1 to 400 now and in at least 3 of the last 4
// folds (recentTop, newest first, the current fold first), one eligible
// provider not on dead-hop probation whose prefix no higher-ranked network's
// representative holds; a provider already counted first, then one with a
// prefix pin, then the lowest client id. A network with no such provider has
// no representative this fold.
func SelectVerifyCohortRanked(
	networks []VerifyCohortRankedNetwork,
	recentTop []map[server.Id]bool,
	providers []VerifyCohortRankedProvider,
	counted map[server.Id]bool,
	probation map[server.Id]bool,
) []server.Id {
	if len(recentTop) == 0 {
		return []server.Id{}
	}
	ranked := []VerifyCohortRankedNetwork{}
	for _, network := range networks {
		if network.Claimed || network.Position < 1 || VerifyCohortRankedPositions < network.Position {
			continue
		}
		folds := 0
		for _, top := range recentTop[:min(len(recentTop), verifyCohortRankedFolds)] {
			if top[network.NetworkId] {
				folds += 1
			}
		}
		if !recentTop[0][network.NetworkId] || folds < verifyCohortRankedStableFolds {
			continue
		}
		ranked = append(ranked, network)
	}
	slices.SortStableFunc(ranked, func(a VerifyCohortRankedNetwork, b VerifyCohortRankedNetwork) int {
		if a.Position != b.Position {
			return a.Position - b.Position
		}
		return a.NetworkId.Cmp(b.NetworkId)
	})

	candidates := map[server.Id][]VerifyCohortRankedProvider{}
	for _, provider := range providers {
		if provider.Eligible && !probation[provider.ClientId] {
			candidates[provider.NetworkId] = append(candidates[provider.NetworkId], provider)
		}
	}
	for networkId := range candidates {
		slices.SortFunc(candidates[networkId], func(a VerifyCohortRankedProvider, b VerifyCohortRankedProvider) int {
			if counted[a.ClientId] != counted[b.ClientId] {
				if counted[a.ClientId] {
					return -1
				}
				return 1
			}
			if a.hasPrefix() != b.hasPrefix() {
				if a.hasPrefix() {
					return -1
				}
				return 1
			}
			return a.ClientId.Cmp(b.ClientId)
		})
	}

	taken := map[[32]byte]bool{}
	chosen := map[server.Id]bool{}
	representatives := []server.Id{}
	for _, network := range ranked {
		for _, provider := range candidates[network.NetworkId] {
			if chosen[provider.ClientId] || provider.hasPrefix() && taken[provider.PrefixHash] {
				continue
			}
			if provider.hasPrefix() {
				taken[provider.PrefixHash] = true
			}
			chosen[provider.ClientId] = true
			representatives = append(representatives, provider.ClientId)
			break
		}
	}
	return representatives
}

// GetVerifyCohortRankedLookups reads, for tier B's candidates, which are
// already counted in the rolling census and which are on dead-hop probation.
func GetVerifyCohortRankedLookups(ctx context.Context, settings *VerifySettings, clientIds []server.Id) (counted map[server.Id]bool, probation map[server.Id]bool) {
	counted, probation = map[server.Id]bool{}, map[server.Id]bool{}
	if settings.CohortSize <= 0 || settings.CohortDeploymentKey == "" || len(clientIds) == 0 {
		return
	}
	keys := newVerifyCohortKeys(settings.CohortDeploymentKey, 0)
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(clientIds); start += 1024 {
			batch := clientIds[start:min(start+1024, len(clientIds))]
			args := make([]any, len(batch))
			for i, clientId := range batch {
				args[i] = clientId.String()
			}
			inCensus, err := r.SMIsMember(ctx, keys.census, args...).Result()
			server.Raise(err)
			benched, err := r.SMIsMember(ctx, verifyDeadHopKey, args...).Result()
			server.Raise(err)
			for i, clientId := range batch {
				if inCensus[i] {
					counted[clientId] = true
				}
				if benched[i] {
					probation[clientId] = true
				}
			}
		}
	})
	return
}

// SetVerifyCohortRanked replaces the deployment's ranked set; an empty set
// removes the key. Returns how many representatives it set.
func SetVerifyCohortRanked(ctx context.Context, deploymentKey StDeploymentKey, representatives []server.Id) int {
	if deploymentKey == "" {
		return 0
	}
	keys := newVerifyCohortKeys(deploymentKey, 0)
	members := make([]any, 0, len(representatives))
	for _, clientId := range representatives {
		members = append(members, clientId.String())
	}
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, keys.ranked)
			if 0 < len(members) {
				pipe.SAdd(ctx, keys.ranked, members...)
				pipe.Expire(ctx, keys.ranked, verifyCohortRankedTtl)
			}
			return nil
		})
		server.Raise(err)
	})
	verifyCohortRankedMembers.Set(float64(len(members)))
	return len(members)
}

// GetSnTopRecentTopNetworks reads, for the newest folds newest first, the
// networks each ranked 1 to positions, through (snapshot_id, position).
func GetSnTopRecentTopNetworks(ctx context.Context, folds int, positions int) []map[server.Id]bool {
	recent := []map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		recent = []map[server.Id]bool{}
		snapshotIds := []server.Id{}
		rows, err := conn.Query(ctx, `SELECT snapshot_id FROM sn_top_snapshot ORDER BY computed_at DESC LIMIT $1`, folds)
		server.WithPgResult(rows, err, func() {
			for rows.Next() {
				var snapshotId server.Id
				server.Raise(rows.Scan(&snapshotId))
				snapshotIds = append(snapshotIds, snapshotId)
			}
		})
		for _, snapshotId := range snapshotIds {
			top := map[server.Id]bool{}
			rows, err := conn.Query(
				ctx,
				`SELECT network_id FROM sn_top_network WHERE snapshot_id = $1 AND position BETWEEN 1 AND $2`,
				snapshotId, positions,
			)
			server.WithPgResult(rows, err, func() {
				for rows.Next() {
					var networkId server.Id
					server.Raise(rows.Scan(&networkId))
					top[networkId] = true
				}
			})
			recent = append(recent, top)
		}
	})
	return recent
}
