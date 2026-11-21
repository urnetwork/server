package controller

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// A missing host before its first verified load cannot turn on legacy reach
// for an enabled gate. Disabling enforcement restores the legacy contract.
func TestContractGateRolloutWaitsForItsVerifiedHost(t *testing.T) {
	defer SetAclContractGate(nil)()
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	t.Setenv(sidecarhost.ApiUrlEnvName, "https://api.example")
	t.Cleanup(server.Vault.PushSimpleResource(server.InternalGateCredentialFileName(server.InternalGateCallerConnect), []byte("bearer: missing")))

	disable := server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: false}\n")
	stop := InstallConnectAuthorization(t.Context(), host)
	if gate := ActiveAclContractGate(); gate != nil {
		t.Fatal("the disabled gate changed the legacy path")
	}
	stop()
	disable()
	t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  acl_contract_gate: {enabled: true}\n"))
	defer InstallConnectAuthorization(t.Context(), host)()
	gate := ActiveAclContractGate()
	if gate == nil {
		t.Fatal("the enabled gate selects legacy reach before the host is ready")
	}
	if verdict := gate.Decide(&sidecar.AclContractRequest{}, server.NowUtc()); verdict.Allowed() || verdict.Reason != sidecar.AclContractReasonHeadsUnknown {
		t.Fatalf("contract before the first state: %v", verdict)
	}
	if view := gate.PeerView(sidecar.NewId()); view.Listing != sidecar.AclPeerListingUnavailable {
		t.Fatalf("peer list before the first state: %v", view.Listing)
	}
}
