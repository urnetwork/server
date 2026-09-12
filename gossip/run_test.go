// Startup and the operator link (connect/EXTENDER.md C6, D3).
//
// Nothing here touches the database: what is under test is the configuration
// the service refuses to start without, and the mesh behavior a member sees
// when the operator goes away and comes back.

package gossip

import (
	"context"
	"strings"
	"testing"

	connectgossip "github.com/urnetwork/connect/gossip"
)

// A readiness gate that must never be reached, because the configuration is
// checked before anything external is touched.
func refuseReadiness(t testing.TB) func(context.Context) error {
	return func(context.Context) error {
		t.Error("readiness was checked even though the configuration is incomplete")
		return nil
	}
}

func refuseStatsPusher(t testing.TB) func(context.Context) func() {
	return func(context.Context) func() {
		t.Error("the stats pusher was started even though the configuration is incomplete")
		return func() {}
	}
}

// Incomplete configuration is a startup error naming the key, not a node that
// comes up under an identity nobody was told to dial or with a key set that
// accepts nothing. The error names the key because the whole remedy is to put
// it in the vault resource.
func TestGossipRunRefusesIncompleteConfiguration(t *testing.T) {
	cases := []struct {
		name       string
		yamlLines  []string
		expectName string
	}{
		{
			name: "no gossip identity",
			yamlLines: []string{
				"root_public_keys_hex:",
				"  - 0000000000000000000000000000000000000000000000000000000000000000",
				"network_host: " + testNetworkHost,
			},
			expectName: "gossip_identity_key_hex",
		},
		{
			name: "an unreadable gossip identity",
			yamlLines: []string{
				"root_public_keys_hex:",
				"  - 0000000000000000000000000000000000000000000000000000000000000000",
				"network_host: " + testNetworkHost,
				"gossip_identity_key_hex: not-a-seed",
			},
			expectName: "gossip_identity_key_hex",
		},
		{
			name: "no root keys",
			yamlLines: []string{
				"network_host: " + testNetworkHost,
				"gossip_identity_key_hex: 0000000000000000000000000000000000000000000000000000000000000000",
			},
			expectName: "root_public_keys_hex",
		},
		{
			name: "root keys that are not keys",
			yamlLines: []string{
				"root_public_keys_hex:",
				"  - not-a-key",
				"network_host: " + testNetworkHost,
				"gossip_identity_key_hex: 0000000000000000000000000000000000000000000000000000000000000000",
			},
			expectName: "root_public_keys_hex",
		},
		{
			name: "no network host",
			yamlLines: []string{
				"root_public_keys_hex:",
				"  - 0000000000000000000000000000000000000000000000000000000000000000",
				"gossip_identity_key_hex: 0000000000000000000000000000000000000000000000000000000000000000",
			},
			expectName: "network_host",
		},
	}
	for _, c := range cases {
		func() {
			installTestConfigYaml(t, strings.Join(c.yamlLines, "\n"))
			err := runWithDependencies(
				context.Background(),
				RunOptions{Port: 80},
				refuseReadiness(t),
				refuseStatsPusher(t),
			)
			if err == nil {
				t.Errorf("%s: the service started", c.name)
				return
			}
			if !strings.Contains(err.Error(), c.expectName) {
				t.Errorf("%s: err = %s, want it to name %s", c.name, err, c.expectName)
			}
		}()
	}
}

func TestGossipRunRefusesAPortOutsideTheRange(t *testing.T) {
	for _, port := range []int{0, -1, 65_536} {
		if err := (RunOptions{Port: port}).Validate(); err == nil {
			t.Errorf("port %d must be refused", port)
		}
	}
	if err := (RunOptions{Port: 80}).Validate(); err != nil {
		t.Fatal(err)
	}
}

// A member keeps one link to the operator and redials it as the mesh churns
// (D3). The operator is a single replica, so every redeploy is exactly this:
// the node goes away and comes back at the same address under the same
// identity, and every member must find it again on its own.
func TestGossipMemberReconnectsToARestartedOperator(t *testing.T) {
	config := newTestConfig(t)

	operator := newTestOperator(t, config, 0)
	listenPort := operator.listenPort(t)
	operatorAddrs := operator.addrs(t)

	member := newTestMember(t, config, operatorAddrs)
	defer member.close()
	waitForNodeStatus(t, member.node, "the operator connected", func(status connectgossip.NodeStatus) bool {
		return status.OperatorConnected
	})

	operator.close()
	waitForNodeStatus(t, member.node, "the operator gone", func(status connectgossip.NodeStatus) bool {
		return !status.OperatorConnected
	})

	// the same address and the same identity, which is what the member's
	// `/p2p/<id>` address demands at the far end of the dial
	restarted := newTestOperator(t, config, listenPort)
	defer restarted.close()
	if restarted.node.PeerId() != operator.node.PeerId() {
		t.Fatalf(
			"the restarted operator is %s, not %s",
			restarted.node.PeerId(),
			operator.node.PeerId(),
		)
	}
	waitForNodeStatus(t, member.node, "the operator connected again", func(status connectgossip.NodeStatus) bool {
		return status.OperatorConnected
	})
}
