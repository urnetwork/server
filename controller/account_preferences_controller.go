package controller

import (
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/onboarding"
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

// AccountPreferencesSetBySettings is the settings screen's change: the user
// chose, so the preference's source becomes settings (the campaigns' consent
// evidence). Sign-up and verification sync through AccountPreferencesSet and
// keep the source the sign-up recorded.
func AccountPreferencesSetBySettings(
	preferencesSet *model.AccountPreferencesSetArgs,
	session *session.ClientSession,
) (*model.AccountPreferencesSetResult, error) {
	r, err := AccountPreferencesSet(preferencesSet, session)
	if err == nil {
		model.SetNetworkProductUpdatesSource(session.Ctx, session.ByJwt.NetworkId, onboarding.ConsentSourceSettings, server.NowUtc())
	}
	return r, err
}
