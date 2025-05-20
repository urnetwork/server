package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type GetLeaderboardArgs struct {
}

type GetLeaderboardResult struct{}

func GetLeaderboardStats(
	args *GetLeaderboardArgs,
	session *session.ClientSession,
) (*model.LeaderboardResult, error) {

	networkRanking, err := model.GetNetworkLeaderboardRanking(session)
	if err != nil {
		return &model.LeaderboardResult{
			Error: &model.TopEarnersError{
				Message: err.Error(),
			},
		}, nil
	}

	earners, err := model.GetTopEarners(session.Ctx)
	if err != nil {
		return &model.LeaderboardResult{
			Error: &model.TopEarnersError{
				Message: err.Error(),
			},
		}, nil
	}

	return &model.LeaderboardResult{
		Earners:        earners,
		NetworkRanking: networkRanking,
	}, nil

}

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
