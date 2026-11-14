package model

import (
	"context"
	"fmt"
	"slices"
	"sync"

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
//
// §21.7's standing caps the count whatever the tier (CONTRACTS.md C-20; QUOTA-M5-01): a network on probation,
// under 7 days old, or restricted holds at most one endpoint, so a Pro network's first week holds one. The
// standing is ABUSE's to read (model.AbuseStandingLimits, ABUSE-M5-07), which installs its read here with
// SetTlsEndpointStandingLimit. Until it does, every network is judged as on probation, the closed direction, and
// a read that fails refuses the activation. A refusal the standing makes says no purchase lifts it: time lifts
// probation, and review a restriction. The writer does not know the standing, so on probation its second
// endpoint is refused here rather than before it signs.

// The most endpoints the network's §21.7 standing allows, -1 for no cap beyond its tier's. Safe for concurrent use.
type TlsEndpointStandingLimit func(ctx context.Context, networkId server.Id) (limit int64, err error)

type GrantActivationLimitSettings struct {
	// The endpoints a network may hold while no standing read is installed: §21.7 probation's one, the most a
	// network whose standing is unknown may be allowed.
	UnreadStandingTlsEndpoints int64
}

func DefaultGrantActivationLimitSettings() *GrantActivationLimitSettings {
	return &GrantActivationLimitSettings{
		UnreadStandingTlsEndpoints: 1,
	}
}

var grantActivationLimitSettings = DefaultGrantActivationLimitSettings()

var tlsEndpointStandingLimitLock sync.Mutex
var tlsEndpointStandingLimit TlsEndpointStandingLimit

// Installs the standing read the tls_endpoints count takes, which ABUSE supplies from model.AbuseStandingLimits.
// nil removes it, and every network is then judged as on probation.
func SetTlsEndpointStandingLimit(standingLimit TlsEndpointStandingLimit) {
	tlsEndpointStandingLimitLock.Lock()
	defer tlsEndpointStandingLimitLock.Unlock()
	tlsEndpointStandingLimit = standingLimit
}

// Installs a standing read for a test and returns what restores the one before.
func Testing_SetTlsEndpointStandingLimit(standingLimit TlsEndpointStandingLimit) func() {
	tlsEndpointStandingLimitLock.Lock()
	defer tlsEndpointStandingLimitLock.Unlock()
	previous := tlsEndpointStandingLimit
	tlsEndpointStandingLimit = standingLimit
	return func() {
		SetTlsEndpointStandingLimit(previous)
	}
}

// The network's tls_endpoints: the lower of tierLimit, its tier's and rows' (-1 for none), and what its standing
// allows, and whether the standing's cap is the one that holds, which it does where the two are equal.
func networkTlsEndpointsLimit(ctx context.Context, networkId server.Id, tierLimit int64) (limit int64, standingBinds bool, returnErr error) {
	tlsEndpointStandingLimitLock.Lock()
	standingLimit := tlsEndpointStandingLimit
	tlsEndpointStandingLimitLock.Unlock()
	standing := grantActivationLimitSettings.UnreadStandingTlsEndpoints
	if standingLimit != nil {
		var err error
		standing, err = standingLimit(ctx, networkId)
		if err != nil {
			return 0, false, err
		}
	}
	if 0 <= standing && (tierLimit < 0 || standing <= tierLimit) {
		return standing, true, nil
	}
	return tierLimit, false, nil
}

// The sequencer's grant activation limits: a GrantActivate that passed at state, the tip api appends at,
// sequenced at timeMs, is refused past the network's tls_endpoints, after its standing, with a
// *QuotaRefusalError. Anything else passes.
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
	if !slices.Contains(ask.Allow, protocol.AclAction_ACL_ACTION_PUBLISH) {
		// publishes nothing, so it takes no endpoint, and its standing is not read
		return nil
	}
	acl, err := state.Acl()
	if err != nil {
		return err
	}
	tier, limits := networkSidecarLimitsWithTier(ctx, nil, networkId)
	limit, standingBinds, err := networkTlsEndpointsLimit(ctx, networkId, limits.Limit(sidecar.AclTlsEndpointsLimitKey))
	if err != nil {
		return fmt.Errorf("the network's standing: %w", err)
	}
	refusal := sidecar.AclCheckTlsEndpoints(acl, ask, timeMs, limit, !standingBinds)
	if refusal == nil {
		return nil
	}
	if !standingBinds {
		refusal.Upgrade = quotaRefusalUpgrade(tier, sidecar.AclTlsEndpointsLimitKey, refusal.Used, networkSidecarLimitHasRow(ctx, nil, networkId, sidecar.AclTlsEndpointsLimitKey))
	}
	return NewQuotaRefusalError(refusal)
}

// The sequencer's AdmissionCap: config_admissions, then the grant activation limits.
func CheckConfigAppendLimits(ctx context.Context, networkId server.Id, state *sidecar.ConfigState, signedBytes []byte, verdict sidecar.ConfigVerdict, timeMs int64) error {
	if err := CheckConfigAdmissionCap(ctx, networkId, state, signedBytes, verdict, timeMs); err != nil {
		return err
	}
	return CheckGrantActivationLimits(ctx, networkId, state, signedBytes, verdict, timeMs)
}
