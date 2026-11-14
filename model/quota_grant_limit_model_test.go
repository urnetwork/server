package model

// QUOTA-M4-04 at api's verifier: CheckGrantActivationLimits passes everything while enforce_sidecar_limits is
// off, judges only a GrantActivate that took effect at the tip, and refuses one whose bytes it cannot read,
// the closed direction. The count itself is sidecar.AclCheckTlsEndpoints, which acl_limits_test.go holds to its
// table, so the writer and the verifier judge on one table by construction. The limit it counts against is
// the lower of the tier's and §21.7's standing's (C-20; QUOTA-M5-01), probation's one while no standing read is
// installed.

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

func TestGrantActivationLimitsPassWhileDark(t *testing.T) {
	defer Testing_SetEnforceSidecarLimits(false)()
	verdict := sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_GRANT_ACTIVATE}
	if err := CheckGrantActivationLimits(t.Context(), server.NewId(), nil, []byte("not a statement"), verdict, 0); err != nil {
		t.Fatalf("dark limits refused: %v", err)
	}
}

func TestGrantActivationLimitsJudgeOnlyAnAcceptedActivation(t *testing.T) {
	defer Testing_SetEnforceSidecarLimits(true)()
	ctx := t.Context()
	networkId := server.NewId()
	// another statement type, and an activation the tip already refused, pass with nothing read
	for _, verdict := range []sidecar.ConfigVerdict{
		{SignedType: bridge.SignedType_SIGNED_SIDECAR_ADMIT},
		{SignedType: bridge.SignedType_SIGNED_GRANT_ACTIVATE, Refusal: errors.New("refused at the tip")},
	} {
		if err := CheckGrantActivationLimits(ctx, networkId, nil, []byte("not a statement"), verdict, 0); err != nil {
			t.Errorf("%v: %v", verdict.SignedType, err)
		}
	}
	// an accepted activation whose bytes do not read is refused, never passed unseen
	verdict := sidecar.ConfigVerdict{SignedType: bridge.SignedType_SIGNED_GRANT_ACTIVATE}
	if err := CheckGrantActivationLimits(ctx, networkId, nil, []byte{0xff, 0xff, 0xff}, verdict, 0); err == nil {
		t.Error("an activation that does not parse passed")
	}
}

// Until ABUSE installs the standing read, every network is held to probation's one endpoint, the closed direction,
// under a tier that allows more or has no limit; a tier that allows none keeps its zero, and its refusal then says
// a purchase lifts it.
func TestTlsEndpointsLimitIsProbationsUntilTheStandingIsRead(t *testing.T) {
	defer Testing_SetTlsEndpointStandingLimit(nil)()
	for _, c := range []struct {
		tierLimit     int64
		limit         int64
		standingBinds bool
	}{
		{tierLimit: 20, limit: 1, standingBinds: true},
		{tierLimit: -1, limit: 1, standingBinds: true},
		{tierLimit: 1, limit: 1, standingBinds: true},
		{tierLimit: 0, limit: 0, standingBinds: false},
	} {
		limit, standingBinds, err := networkTlsEndpointsLimit(t.Context(), server.NewId(), c.tierLimit)
		if err != nil || limit != c.limit || standingBinds != c.standingBinds {
			t.Errorf("tier %d: %d, binds %t, %v; want %d, binds %t", c.tierLimit, limit, standingBinds, err, c.limit, c.standingBinds)
		}
	}
}

// With the standing read installed, a standard network has its tier's endpoints, and one on probation or
// restricted at most one whatever its tier, so a Pro network's first week holds one (C-20); a read that fails
// refuses.
func TestTlsEndpointsLimitTakesTheLowerOfTierAndStanding(t *testing.T) {
	probationNetworkId := server.NewId()
	failingNetworkId := server.NewId()
	standardNetworkId := server.NewId()
	defer Testing_SetTlsEndpointStandingLimit(func(ctx context.Context, networkId server.Id) (int64, error) {
		switch networkId {
		case probationNetworkId:
			return 1, nil
		case failingNetworkId:
			return 0, errors.New("the standing could not be read")
		}
		return -1, nil
	})()
	for _, c := range []struct {
		networkId     server.Id
		tierLimit     int64
		limit         int64
		standingBinds bool
	}{
		{networkId: standardNetworkId, tierLimit: 20, limit: 20, standingBinds: false},
		{networkId: standardNetworkId, tierLimit: -1, limit: -1, standingBinds: false},
		{networkId: probationNetworkId, tierLimit: 20, limit: 1, standingBinds: true},
		{networkId: probationNetworkId, tierLimit: -1, limit: 1, standingBinds: true},
		{networkId: probationNetworkId, tierLimit: 0, limit: 0, standingBinds: false},
	} {
		limit, standingBinds, err := networkTlsEndpointsLimit(t.Context(), c.networkId, c.tierLimit)
		if err != nil || limit != c.limit || standingBinds != c.standingBinds {
			t.Errorf("tier %d: %d, binds %t, %v; want %d, binds %t", c.tierLimit, limit, standingBinds, err, c.limit, c.standingBinds)
		}
	}
	if _, _, err := networkTlsEndpointsLimit(t.Context(), failingNetworkId, 20); err == nil {
		t.Error("a standing that could not be read gave a limit")
	}
}
