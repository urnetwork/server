// Original request reads recover the first signed response after a lost reply.
// They expose retained bytes, never a signed assertion that an absent request
// did not run or that this operator covers a complete validator window.
package controller

import (
	"bytes"
	"crypto/ed25519"
	"errors"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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
	prefix := append([]byte(connect.VerifyCtx), connect.VerifyMsgTypeSeed)
	if len(args.Message) != len(prefix)+ed25519.PublicKeySize+connect.VerifyNonceSize+1 || !bytes.HasPrefix(args.Message, prefix) {
		return nil, errors.New("400 exact original seed request required")
	}
	vpk := ed25519.PublicKey(args.Message[len(prefix) : len(prefix)+ed25519.PublicKeySize])
	if !ed25519.Verify(vpk, args.Message, args.Signature) {
		return nil, errors.New("400 original seed signature differs")
	}
	original := model.GetVerifyOriginalRequest(clientSession.Ctx, *args)
	if original != nil {
		verifyDecodeOriginal(original)
	}
	return &GetVerifyOriginalRequestResult{Original: original}, nil
}
