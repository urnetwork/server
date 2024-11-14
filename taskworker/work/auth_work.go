package work

import (
	"time"

	"github.com/golang/glog"

	"github.com/urnetwork/server/bringyour"
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/task"

	// "github.com/urnetwork/server/bringyour/controller"
	"github.com/urnetwork/server/bringyour/session"
)

type RemoveExpiredAuthCodesArgs struct {
}

type RemoveExpiredAuthCodesResult struct {
}

func ScheduleRemoveExpiredAuthCodes(clientSession *session.ClientSession, tx bringyour.PgTx) {
	task.ScheduleTaskInTx(
		tx,
		RemoveExpiredAuthCodes,
		&RemoveExpiredAuthCodesArgs{},
		clientSession,
		task.RunOnce("remove_expired_auth_codes"),
		task.RunAt(time.Now().Add(1*time.Hour)),
	)
}

func RemoveExpiredAuthCodes(
	removeExpiredAuthCodes *RemoveExpiredAuthCodesArgs,
	clientSession *session.ClientSession,
) (*RemoveExpiredAuthCodesResult, error) {
	authCodeCount := model.RemoveExpiredAuthCodes(clientSession.Ctx, bringyour.NowUtc())
	glog.Infof("[authw]removed %d auth codes.\n", authCodeCount)
	return &RemoveExpiredAuthCodesResult{}, nil
}

func RemoveExpiredAuthCodesPost(
	removeExpiredAuthCodes *RemoveExpiredAuthCodesArgs,
	removeExpiredAuthCodesResult *RemoveExpiredAuthCodesResult,
	clientSession *session.ClientSession,
	tx bringyour.PgTx,
) error {
	ScheduleRemoveExpiredAuthCodes(clientSession, tx)
	return nil
}
