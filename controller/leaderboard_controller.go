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

type GetLeaderboardResult struct{}

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

type SetLeaderboardArgs struct {
	IsPublic bool `json:"is_public"`
}

type SetLeaderboardResult struct {
	Error *SetLeaderboardError `json:"error,omitempty"`
}

type SetLeaderboardError struct {
	Message string `json:"message"`
}

func SetNetworkLeaderboardRankingPublic(
	args SetLeaderboardArgs,
	session *session.ClientSession,
) (*SetLeaderboardResult, error) {

	err := model.SetNetworkLeaderboardPublic(args.IsPublic, session)
	if err != nil {
		return &SetLeaderboardResult{
			Error: &SetLeaderboardError{
				Message: err.Error(),
			},
		}, nil
	}

	return &SetLeaderboardResult{}, nil

}

/**
 * Network ranking
 */

type GetNetworkRankingArgs struct {
}

type GetNetworkRankingResult struct {
	NetworkRanking model.NetworkRanking    `json:"network_ranking"`
	Error          *GetNetworkRankingError `json:"error,omitempty"`
}
type GetNetworkRankingError struct {
	Message string `json:"message"`
}

func GetNetworkLeaderboardRanking(
	args *GetNetworkRankingArgs,
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
