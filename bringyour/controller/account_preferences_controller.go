package controller

import (
	"bringyour.com/bringyour/model"
	"bringyour.com/bringyour/session"
)

func AccountPreferencesGet(session *session.ClientSession) (*model.AccountPreferencesGetResult, error) {
	return model.AccountPreferencesGet(session), nil
}
