package controller

import (
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

func AccountPreferencesGet(session *session.ClientSession) (*model.AccountPreferencesGetResult, error) {
	return model.AccountPreferencesGet(session), nil
}
