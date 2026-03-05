package controller

import (
	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type GetApiKeysError struct {
	Message string `json:"message"`
}

type GetApiKeysResult struct {
	ApiKeys []*model.PublicAccountApiKey `json:"api_keys"`
	Error   *GetApiKeysError             `json:"error,omitempty"`
}

func GetApiKeys(session *session.ClientSession) (result *GetApiKeysResult, err error) {

	keys, err := model.GetAccountApiKeys(session)
	if err != nil {
		glog.Info("Error getting account api keys", "error", err)
		return &GetApiKeysResult{
			Error: &GetApiKeysError{
				Message: "Error getting account api keys",
			},
		}, nil
	}

	return &GetApiKeysResult{
		ApiKeys: keys,
	}, nil

}

type DeleteApiKeyError struct {
	Message string `json:"message"`
}

type DeleteApiKeyResult struct {
	Error *DeleteApiKeyError `json:"error,omitempty"`
}

type DeleteApiKeyArgs struct {
	Id *server.Id `json:"id"`
}

func DeleteApiKey(deleteApiKey *DeleteApiKeyArgs, session *session.ClientSession) (*DeleteApiKeyResult, error) {
	err := model.DeleteApiKey(deleteApiKey.Id, session)
	if err != nil {
		glog.Info("Error deleting api key: %s", err.Error())
		return &DeleteApiKeyResult{
			Error: &DeleteApiKeyError{
				Message: "Error deleting api key",
			},
		}, nil
	}
	return &DeleteApiKeyResult{}, nil
}
