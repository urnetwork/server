package controller

import (
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func AccountPreferencesGet(session *session.ClientSession) (*model.AccountPreferencesGetResult, error) {
	return model.AccountPreferencesGet(session), nil
}

func AccountPreferencesSet(
	preferencesSet *model.AccountPreferencesSetArgs,
	session *session.ClientSession,
) (*model.AccountPreferencesSetResult, error) {
	r, err := model.AccountPreferencesSet(preferencesSet, session)
	if err == nil {
		server.Tx(session.Ctx, func(tx server.PgTx) {
			ScheduleSyncProductUpdatesForUser(
				session,
				session.ByJwt.UserId,
				tx,
			)
		})
	}
	return r, err
}
