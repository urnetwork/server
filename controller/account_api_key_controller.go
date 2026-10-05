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
		// a server-side failure (a key row the listing cannot read), not client
		// input, so it is logged at the default level
		glog.Infof("[api key][%s]error getting account api keys: %s\n", session.ByJwt.NetworkId, err)
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
		// a server-side failure (of the delete statement), not client input: an
		// unknown or missing id deletes nothing and is no error
		glog.Infof("[api key][%s]error deleting api key: %s\n", session.ByJwt.NetworkId, err)
		return &DeleteApiKeyResult{
			Error: &DeleteApiKeyError{
				Message: "Error deleting api key",
			},
		}, nil
	}
	return &DeleteApiKeyResult{}, nil
}
