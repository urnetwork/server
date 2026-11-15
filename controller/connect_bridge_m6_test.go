//go:build sidecar_pending

package controller

// BRIDGE-M6-08's rows on T3 (§22.4; FS-38's share rows, CC-05's minted reach): sidecartest's BridgeShareNet, the same
// schedules sidecartest runs on T2, the sharing network's clients rows of server's and every control frame they send
// answered by server's controller (bridgeM4Platform, connect_bridge_m4_test.go), the host's FriendsAndFamily provide key
// among them. The share's contracts are the operator's, minted through the host's provide secret, which the test stack
// holds; server's CreateContract of a share between two networks' sidecars (ACL-M6-03) is not run here. Run under
// -tags sidecar_pending through .stack/server-test.sh.

import (
	"context"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest/fault"
)

// FS-38's share rows on server's platform: the twin's minted reach, and each refusal, the host's restart from its
// persisted state included.
func TestFs38ShareOnT3(t *testing.T) {
	testingAclRun(t, func(t testing.TB, ctx context.Context) {
		bridgeM4RunSchedule(t, ctx, "FS-38's share twin", fault.Fs38ShareSigner)
		bridgeM4RunSchedule(t, ctx, "FS-38's share", fault.Fs38ShareCase.Build)
	})
}
