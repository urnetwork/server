// Original request reads recover the first signed response after a lost reply.
// They expose retained bytes, never a signed assertion that an absent request
// did not run or that this operator covers a complete validator window.
package controller

import (
	"errors"

	"github.com/urfoundation/sn/v2026/protocol"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// New requests use exactly the same deployment scope as original receipts.
func verifyCurrentOriginalScope() *model.VerifyOriginalScope {
	if cfg := stConfig(); cfg != nil && cfg.DeploymentKey() != "" {
		return &model.VerifyOriginalScope{Profile: cfg.Profile, GenesisHash: cfg.GenesisHash, DeploymentId: cfg.DeploymentId, DeploymentKey: cfg.DeploymentKey(), PolicyHash: cfg.PolicyHash, Netuid: cfg.Netuid, NoId: cfg.NoId}
	}
	return nil
}

// Missing is explicit and does not erase the caller's outstanding request.
type GetVerifyOriginalRequestResult struct {
	Original *model.VerifyOriginalTransition `json:"original"`
}

// Historical key rotation needs no current client-directory lookup: the
// returned original already retains its authenticated client and original key.
func GetVerifyOriginalRequest(args *model.VerifyOriginalRequest, clientSession *session.ClientSession) (*GetVerifyOriginalRequestResult, error) {
	if args == nil || clientSession == nil || clientSession.Ctx == nil {
		return nil, errors.New("400 missing original request owner")
	}
	if _, err := args.Hash(); err != nil {
		return nil, err
	}
	if _, err := protocol.VerifyProviderAttemptRequestWire(args.Message, args.Signature); err != nil {
		return nil, errors.Join(errors.New("400 original request signature differs"), err)
	}
	original := model.GetVerifyOriginalRequest(clientSession.Ctx, *args)
	if original != nil {
		verifyDecodeOriginal(original)
	}
	return &GetVerifyOriginalRequestResult{Original: original}, nil
}
