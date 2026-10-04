// Original receipts make a committed verification transition recoverable before
// any response is published. Wire signature domains remain unchanged.
package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Tests place an explicit crash boundary after durable commit, before Redis.
var verifyOriginalAfterCommit func()

// Sign the exact body once and reuse the original after an uncertain commit.
func verifyRetainOriginal(ctx context.Context, previousDepth int, trail *model.VerifyTrail, requestMessage, requestSignature []byte, responseJson string) *model.VerifyOriginalTransition {
	body := &model.VerifyOriginalBody{Schema: model.VerifyOriginalDomain, PreviousDepth: previousDepth, Trail: trail, RequestMessage: requestMessage, RequestSignature: requestSignature, ResponseJson: responseJson}
	body.RecoveryMs = trail.ActivityMs
	if trail.Pending != nil {
		settings := verifySettings()
		body.RecoveryMs = trail.Pending.AssignedMs + uint64((settings.StepTimeout + settings.StepTimeoutGrace).Milliseconds())
	}
	if cfg := stConfig(); cfg != nil && cfg.DeploymentKey() != "" {
		body.Scope = &model.VerifyOriginalScope{Profile: cfg.Profile, GenesisHash: cfg.GenesisHash, DeploymentId: cfg.DeploymentId, DeploymentKey: cfg.DeploymentKey(), PolicyHash: cfg.PolicyHash, Netuid: cfg.Netuid, NoId: cfg.NoId}
	}
	if previousDepth > 0 {
		if prior := model.GetLatestVerifyOriginal(ctx, trail.TrailId); prior != nil {
			previous := verifyDecodeOriginal(prior)
			if (body.Scope == nil) != (previous.Scope == nil) || (body.Scope != nil && *body.Scope != *previous.Scope) {
				panic(errors.New("verification original deployment changed during trail"))
			}
		}
	}
	encoded, err := json.Marshal(body)
	server.Raise(err)
	key := verifyServerKeyById(trail.ServerKeyId)
	original := &model.VerifyOriginalTransition{Body: encoded, Signature: ed25519.Sign(key.PrivateKey, append([]byte(model.VerifyOriginalDomain), encoded...))}
	verifyDecodeOriginal(original)
	retained := model.RetainVerifyOriginal(ctx, original)
	verifyDecodeOriginal(retained)
	if verifyOriginalAfterCommit != nil {
		verifyOriginalAfterCommit()
	}
	return retained
}

// Only a configured historical server key may authenticate retained metadata.
func verifyDecodeOriginal(original *model.VerifyOriginalTransition) *model.VerifyOriginalBody {
	body, err := model.DecodeVerifyOriginal(original)
	server.Raise(err)
	key := verifyServerKeyById(body.Trail.ServerKeyId)
	if key == nil || !model.VerifyOriginalSignature(original, key.PrivateKey.Public().(ed25519.PublicKey)) {
		panic(errors.New("verification original signature mismatch"))
	}
	body, err = model.ValidateVerifyOriginal(original, key.PrivateKey.Public().(ed25519.PublicKey))
	server.Raise(err)
	return body
}

// Restore only an unfinished publication. A later terminal Redis projection
// must never be regressed to an older active assignment receipt.
func verifyRecoverOriginal(ctx context.Context, trailId server.Id, trail *model.VerifyTrail, settings *model.VerifySettings) *model.VerifyTrail {
	if trail != nil && trail.Poison {
		return trail
	}
	if row := model.GetVerifyTrailRow(ctx, trailId); row != nil && row.Status == model.VerifyTrailRowStatusExpired {
		if len(row.OriginalState) != 0 {
			var expired model.VerifyTrail
			server.Raise(json.Unmarshal(row.OriginalState, &expired))
			if trail != nil {
				server.Raise(model.VerifyOriginalMatchesTrail(&model.VerifyOriginalBody{Trail: &expired}, trail))
			}
			expired.Status = model.VerifyTrailStatusExpired
			if trail != nil {
				model.ExpireVerifyTrail(ctx, trailId)
			}
			return &expired
		}
		if trail != nil {
			model.ExpireVerifyTrail(ctx, trailId)
			trail.Status = model.VerifyTrailStatusExpired
			return trail
		}
	}
	original := model.GetLatestVerifyOriginal(ctx, trailId)
	if original == nil {
		return trail
	}
	body := verifyDecodeOriginal(original)
	if trail != nil {
		server.Raise(model.VerifyOriginalMatchesTrail(body, trail))
	}
	if trail == nil || len(trail.Hops) < len(body.Trail.Hops) {
		model.PublishVerifyOriginal(ctx, original, settings)
		return body.Trail
	}
	return trail
}
