package controller

import (
	"github.com/urnetwork/server/bringyour/model"
	"github.com/urnetwork/server/bringyour/session"
)

func AccountPreferencesGet(session *session.ClientSession) (*model.AccountPreferencesGetResult, error) {
	return model.AccountPreferencesGet(session), nil
}
