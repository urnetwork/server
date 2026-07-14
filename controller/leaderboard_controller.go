package controller

import (
	"sync"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

/**
 * Leaderboard
 */
type GetLeaderboardArgs struct {
}

func GetLeaderboard(
	args *GetLeaderboardArgs,
	session *session.ClientSession,
) (*model.LeaderboardResult, error) {

	earners, err := model.GetLeaderboard(session.Ctx)
	if err != nil {
		return &model.LeaderboardResult{
			Error: &model.TopEarnersError{
				Message: err.Error(),
			},
		}, nil
	}

	return &model.LeaderboardResult{
		Earners: earners,
	}, nil

}

/**
 * Network leaderboard public settings
 * Users can opt in or out of having their network displayed on the leaderboard
 */

type SetNetworkRankingPublicArgs struct {
	IsPublic bool `json:"is_public"`
}

type SetNetworkRankingPublicResult struct {
	Error *SetNetworkRankingPublicError `json:"error,omitempty"`
}

type SetNetworkRankingPublicError struct {
	Message string `json:"message"`
}

func SetNetworkLeaderboardRankingPublic(
	args SetNetworkRankingPublicArgs,
	session *session.ClientSession,
) (*SetNetworkRankingPublicResult, error) {

	err := model.SetNetworkLeaderboardPublic(args.IsPublic, session)
	if err != nil {
		return &SetNetworkRankingPublicResult{
			Error: &SetNetworkRankingPublicError{
				Message: err.Error(),
			},
		}, nil
	}

	return &SetNetworkRankingPublicResult{}, nil

}

/**
 * Network ranking
 */

type GetNetworkRankingResult struct {
	NetworkRanking model.NetworkRanking    `json:"network_ranking"`
	Error          *GetNetworkRankingError `json:"error,omitempty"`
}
type GetNetworkRankingError struct {
	Message string `json:"message"`
}

// the full leaderboard ranking is O(all networks) to compute
// (model.GetNetworkLeaderboardRankings). Payouts settle slowly, so the full map
// is computed once and shared across callers for leaderboardRankingCacheTtl;
// each request reads only its own network's entry. The model function stays
// uncached (always fresh) for tests and direct callers.
const leaderboardRankingCacheTtl = 5 * time.Minute

var leaderboardRankingCache = struct {
	stateLock sync.Mutex
	rankings  map[server.Id]model.NetworkRanking
	expiry    time.Time
}{}

func cachedNetworkLeaderboardRankings(session *session.ClientSession) (map[server.Id]model.NetworkRanking, error) {
	fresh := func() map[server.Id]model.NetworkRanking {
		leaderboardRankingCache.stateLock.Lock()
		defer leaderboardRankingCache.stateLock.Unlock()
		if leaderboardRankingCache.rankings != nil && server.NowUtc().Before(leaderboardRankingCache.expiry) {
			return leaderboardRankingCache.rankings
		}
		return nil
	}()
	if fresh != nil {
		return fresh, nil
	}
	rankings, err := model.GetNetworkLeaderboardRankings(session.Ctx)
	if err != nil {
		return nil, err
	}
	func() {
		leaderboardRankingCache.stateLock.Lock()
		defer leaderboardRankingCache.stateLock.Unlock()
		leaderboardRankingCache.rankings = rankings
		leaderboardRankingCache.expiry = server.NowUtc().Add(leaderboardRankingCacheTtl)
	}()
	return rankings, nil
}

func GetNetworkLeaderboardRanking(
	session *session.ClientSession,
) (*GetNetworkRankingResult, error) {

	rankings, err := cachedNetworkLeaderboardRankings(session)
	if err != nil {
		return &GetNetworkRankingResult{
			Error: &GetNetworkRankingError{
				Message: err.Error(),
			},
		}, nil
	}

	return &GetNetworkRankingResult{
		NetworkRanking: rankings[session.ByJwt.NetworkId],
	}, nil

}
