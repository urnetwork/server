package work

import (
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

type RemoveExpiredWalletAuthChallengesArgs struct{}

type RemoveExpiredWalletAuthChallengesResult struct{}

func ScheduleRemoveExpiredWalletAuthChallenges(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredWalletAuthChallenges,
		&RemoveExpiredWalletAuthChallengesArgs{},
		clientSession,
		task.RunOnce("remove_expired_wallet_auth_challenges"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
	)
}

func RemoveExpiredWalletAuthChallenges(
	_ *RemoveExpiredWalletAuthChallengesArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredWalletAuthChallengesResult, error) {
	minTime := server.NowUtc().Add(-24 * time.Hour)
	model.RemoveExpiredWalletAuthChallenges(clientSession.Ctx, minTime)
	return &RemoveExpiredWalletAuthChallengesResult{}, nil
}

func RemoveExpiredWalletAuthChallengesPost(
	_ *RemoveExpiredWalletAuthChallengesArgs,
	_ *RemoveExpiredWalletAuthChallengesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredWalletAuthChallenges(clientSession, tx)
	return nil
}
