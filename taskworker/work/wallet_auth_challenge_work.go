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

type RemoveExpiredWalletNoncesArgs struct{}

type RemoveExpiredWalletNoncesResult struct{}

func ScheduleRemoveExpiredWalletNonces(clientSession *session.ClientSession, tx server.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredWalletNonces,
		&RemoveExpiredWalletNoncesArgs{},
		clientSession,
		task.RunOnce("remove_expired_wallet_nonces"),
		task.RunAt(server.NowUtc().Add(1*time.Hour)),
		task.MaxTime(1*time.Hour),
	)
}

// RemoveExpiredWalletNonces reaps the wallet-login nonce table, which is
// live-written by the no-auth AuthWalletNonceCreate route and previously had no
// reaper. Batched so the initial backlog drains without one giant delete.
func RemoveExpiredWalletNonces(
	_ *RemoveExpiredWalletNoncesArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredWalletNoncesResult, error) {
	limit := 50000
	for {
		removedCount := model.RemoveExpiredWalletNonces(clientSession.Ctx, server.NowUtc(), limit)
		if removedCount < int64(limit) {
			break
		}
	}
	return &RemoveExpiredWalletNoncesResult{}, nil
}

func RemoveExpiredWalletNoncesPost(
	_ *RemoveExpiredWalletNoncesArgs,
	_ *RemoveExpiredWalletNoncesResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	ScheduleRemoveExpiredWalletNonces(clientSession, tx)
	return nil
}
