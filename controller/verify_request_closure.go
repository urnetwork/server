// Per-request owner consent resolves lost replies without observing SQL absence
// as complete verifier authority. The consumer still validates its signed cut.
package controller

import (
	"crypto/ed25519"
	"errors"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Request closure pins the same configured deployment as assignment originals.
func verifyRequestClosureScope() (protocol.ProviderAttemptReceiptScope, error) {
	scope := verifyCurrentOriginalScope()
	if scope == nil {
		return protocol.ProviderAttemptReceiptScope{}, errors.New("503 verification original scope unavailable")
	}
	return protocol.ProviderAttemptReceiptScope{Profile: scope.Profile, GenesisHash: scope.GenesisHash, DeploymentId: scope.DeploymentId, DeploymentKey: string(scope.DeploymentKey), PolicyHash: scope.PolicyHash, Netuid: scope.Netuid, NoId: scope.NoId}, nil
}

// Only the original request key may fence that request. Current registration
// retirement cannot revoke its exact closure consent or committed receipt.
func CloseVerifyOriginalRequest(args *protocol.ProviderAttemptRequestClosure, clientSession *session.ClientSession) (*model.VerifyRequestClosureResult, error) {
	if args == nil || clientSession == nil || clientSession.Ctx == nil {
		return nil, errors.New("400 missing verification closure owner")
	}
	ctx := clientSession.Ctx
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scope, err := verifyRequestClosureScope()
	if err != nil {
		return nil, err
	}
	if err := protocol.VerifyProviderAttemptRequestClosure(ctx, *args, scope); err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		return nil, errors.Join(errors.New("400 invalid verification closure"), err)
	}
	vpk, err := protocol.VerifyProviderAttemptRequestWire(args.Message, args.RequestSignature)
	if err != nil {
		return nil, err
	}
	settings := verifySettings()
	ip, ok := model.ParseVerifyEgressIp(clientSession.ClientAddress)
	if !ok {
		return nil, errors.New("400 verification closure source unavailable")
	}
	ipCount, keyCount := model.IncrVerifySeedRates(ctx, ip.String(), vpk[:], settings)
	if settings.SeedRateHardLimit < ipCount || settings.SeedRateHardLimit < keyCount {
		return nil, errors.New("429 verification closure rate exceeded")
	}
	key := verifySigningKey()
	proposed, err := protocol.SealProviderAttemptClosedUnreceived(ctx, *args, key.ServerKeyId, key.PrivateKey)
	if err != nil {
		return nil, err
	}
	keys := make(map[byte]ed25519.PublicKey)
	for _, key := range verifyServerKeys() {
		keys[key.ServerKeyId] = key.PrivateKey.Public().(ed25519.PublicKey)
	}
	var result *model.VerifyRequestClosureResult
	server.HandleError(func() {
		result = model.CloseVerifyOriginalRequest(ctx, *args, proposed, keys)
		if result.Original != nil {
			verifyDecodeOriginal(result.Original)
		}
	}, func(cause error) { err = cause })
	return result, err
}
