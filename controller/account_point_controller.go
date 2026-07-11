package controller

import (
	"encoding/json"

	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type AccountPointsResult struct {
	AccountPoints []model.AccountPoint `json:"network_points"`
}

// MarshalJSON dual-emits the deprecated `account_points` alias alongside the
// spec field `network_points` during the rename migration.
func (r AccountPointsResult) MarshalJSON() ([]byte, error) {
	type alias AccountPointsResult
	b, err := json.Marshal(alias(r))
	if err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if v, ok := m["network_points"]; ok {
		m["account_points"] = v
	}
	return json.Marshal(m)
}

func GetAccountPoints(
	session *session.ClientSession,
) (*AccountPointsResult, error) {
	result := model.FetchAccountPoints(session.Ctx, session.ByJwt.NetworkId)

	return &AccountPointsResult{
		AccountPoints: result,
	}, nil
}
