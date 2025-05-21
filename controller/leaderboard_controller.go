package controller

import (
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

func GetNetworkLeaderboardRanking(
	session *session.ClientSession,
) (*GetNetworkRankingResult, error) {

	networkRanking, err := model.GetNetworkLeaderboardRanking(session)
	if err != nil {
		return &GetNetworkRankingResult{
			Error: &GetNetworkRankingError{
				Message: err.Error(),
			},
		}, nil
	}

	return &GetNetworkRankingResult{
		NetworkRanking: networkRanking,
	}, nil

}
