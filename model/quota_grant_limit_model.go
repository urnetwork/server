package model

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// §20.7's tls_endpoints row at api's verifier (QUOTA-M4-04): publish grants per (host, port), counted at grant
// activation. Readers evaluate no tier, so the count sits outside the shared fold: the writer checks its
// QuotaLimits with sidecar.AclCheckGrantActivateQuota, and api checks NetworkSidecarLimits here, through the
// sequencer's AdmissionCap hook beside config_admissions (config_log_redis.go), on the one table
// sidecar.AclCheckTlsEndpoints keeps. A refusal is the QuotaRefusal itself, answered as §7.9's refused
// statement. Dark while enforce_sidecar_limits is off, and it judges only the activation, so a grant that is
// active keeps running through a downgrade until it expires.

// The sequencer's grant activation limits: a GrantActivate that passed at state, the tip api appends at,
// sequenced at timeMs, is refused past the network's tls_endpoints with a *QuotaRefusalError. Anything else
// passes.
func CheckGrantActivationLimits(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, signedBytes []byte, verdict sidecar.ConfigVerdict, timeMs int64) error {
	if !Pro().EnforceSidecarLimits || !verdict.Accepted() || verdict.SignedType != bridge.SignedType_SIGNED_GRANT_ACTIVATE {
		return nil
	}
	signed := &bridge.Signed{}
	activate := &protocol.GrantActivate{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		return fmt.Errorf("a GrantActivate that is no Signed: %w", err)
	}
	if err := proto.Unmarshal(signed.GetBody(), activate); err != nil {
		return fmt.Errorf("a GrantActivate's body: %w", err)
	}
	ask, err := sidecar.AclGrantActivateAsk(activate)
	if err != nil {
		return err
	}
	acl, err := state.Acl()
	if err != nil {
		return err
	}
	tier, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
	refusal := sidecar.AclCheckTlsEndpoints(acl, ask, timeMs, limits.Limit(sidecar.AclTlsEndpointsLimitKey), true)
	if refusal == nil {
		return nil
	}
	refusal.Upgrade = quotaRefusalUpgrade(tier, sidecar.AclTlsEndpointsLimitKey, refusal.Used, networkSidecarLimitHasRow(ctx, nil, networkId, sidecar.AclTlsEndpointsLimitKey))
	return NewQuotaRefusalError(refusal)
}

// The sequencer's AdmissionCap: config_admissions, then the grant activation limits.
func CheckConfigAppendLimits(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, signedBytes []byte, verdict sidecar.ConfigVerdict, timeMs int64) error {
	if err := CheckConfigAdmissionCap(ctx, networkId, state, signedBytes, verdict, timeMs); err != nil {
		return err
	}
	return CheckGrantActivationLimits(ctx, networkId, state, signedBytes, verdict, timeMs)
}
