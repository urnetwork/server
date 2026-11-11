package model

// QUOTA-M4-04 at api's verifier: CheckGrantActivationLimits passes everything while enforce_sidecar_limits is
// off, judges only a GrantActivate that took effect at the tip, and refuses one whose bytes it cannot read,
// the closed direction. The count itself is sidecar.AclCheckTlsEndpoints, which acl_limits_test.go holds to its
// table, so the writer and the verifier judge on one table by construction.

import (
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
