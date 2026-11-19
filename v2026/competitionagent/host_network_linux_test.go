// Network proof tests contain only synthetic Docker/link documents and fake
// callbacks. No namespace, host interface, daemon or privileged ip is touched.
package competitionagent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHostNetworkInspectionRequiresExactOwnedIsolatedBridge(t *testing.T) {
	fixture := newDockerProxyTest(t)
	plan, networkId := fixture.plan, strings.Repeat("d", 64)
	object := map[string]any{"Id": networkId, "Driver": "bridge", "Scope": "local", "Internal": true,
		"EnableIPv6": false, "Attachable": false, "Ingress": false,
		"Labels": map[string]string{cleanupJobLabel: plan.JobId, cleanupRoundLabel: plan.RoundId, cleanupRunLabel: plan.RunId},
		"Options": map[string]string{"com.docker.network.bridge.name": "unev-" + plan.RunId[:10],
			"com.docker.network.bridge.gateway_mode_ipv4": "isolated", "com.docker.network.bridge.gateway_mode_ipv6": "isolated"}}
	content, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if bridge, err := validateHostNetworkInspection(content, plan, networkId); err != nil || bridge != "unev-"+plan.RunId[:10] {
		t.Fatal("exact private bridge was rejected", err)
	}
	for _, change := range []func(map[string]any){
		func(value map[string]any) { value["Id"] = strings.Repeat("e", 64) },
		func(value map[string]any) { value["Driver"] = "host" },
		func(value map[string]any) { value["Scope"] = "swarm" },
		func(value map[string]any) { value["Internal"] = false },
		func(value map[string]any) { value["EnableIPv6"] = true },
		func(value map[string]any) { delete(value, "EnableIPv6") },
		func(value map[string]any) { value["Attachable"] = true },
		func(value map[string]any) { value["Ingress"] = true },
		func(value map[string]any) {
			value["Labels"].(map[string]any)[cleanupRunLabel] = strings.Repeat("f", 64)
		},
		func(value map[string]any) {
			value["Options"].(map[string]any)["com.docker.network.bridge.name"] = "synthetic-other"
		},
		func(value map[string]any) {
			value["Options"].(map[string]any)["com.docker.network.bridge.gateway_mode_ipv4"] = "nat"
		},
	} {
		var changed map[string]any
		if json.Unmarshal(content, &changed) != nil {
			t.Fatal("synthetic network document did not decode")
		}
		change(changed)
		changedBytes, err := json.Marshal(changed)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateHostNetworkInspection(changedBytes, plan, networkId); err == nil {
			t.Fatal("unowned/external/incomplete network was admitted")
		}
	}
}

func TestHostNetworkLinkRequiresPositiveAbsenceOfHostAddresses(t *testing.T) {
	bridge := "unev-aaaaaaaaaa"
	for _, content := range []string{`[{"ifname":"unev-aaaaaaaaaa","addr_info":[]}]`,
		`[{"ifname":"unev-aaaaaaaaaa","addr_info":[{"family":"inet6","local":"fe80::1","scope":"link","prefixlen":64}]}]`} {
		if err := validateHostNetworkLink([]byte(content), bridge); err != nil {
			t.Fatal("synthetic address-free/link-local-only bridge was refused", err)
		}
	}
	for _, content := range []string{`[]`, `null`, `[{"ifname":"synthetic-other","addr_info":[]}]`,
		`[{"ifname":"unev-aaaaaaaaaa"}]`, `[{"ifname":"unev-aaaaaaaaaa","addr_info":null}]`,
		`[{"ifname":"unev-aaaaaaaaaa","addr_info":[{"family":"inet","local":"192.0.2.1","scope":"global","prefixlen":24}]}]`,
		`[{"ifname":"unev-aaaaaaaaaa","addr_info":[{"family":"inet6","local":"2001:db8::1","scope":"global","prefixlen":64}]}]`,
		`[{"ifname":"unev-aaaaaaaaaa","addr_info":[{"family":"inet6","local":"fe80::1","scope":"global","prefixlen":64}]}]`,
		`[{"ifname":"unev-aaaaaaaaaa","addr_info":[{"family":"inet6","local":"fe80::1","scope":"link","prefixlen":32}]}]`} {
		if validateHostNetworkLink([]byte(content), bridge) == nil {
			t.Fatal("missing facts or reachable bridge address was accepted", content)
		}
	}
}

func TestHostNetworkProofIsFiniteAndCannotAuthorizeMapHandoff(t *testing.T) {
	fixture := newHostFilesTest(t)
	networkId := strings.Repeat("d", 64)
	proof := HostNetworkBoundary{Schema: Schema, NetworkId: networkId, BridgeName: "unev-" + fixture.broker.plan.RunId[:10],
		PrivateNamespace: true, IsolatedGateway: true, LinkSha256: strings.Repeat("e", 64)}
	calls := 0
	fixture.broker.proveNetwork = func(context.Context, string) (HostNetworkBoundary, error) { calls++; return proof, nil }
	request := fixture.request("verify-network", "")
	request.NetworkId = networkId
	result := fixture.broker.Execute(t.Context(), request)
	if result.ExitCode != 0 || !result.Acknowledged || result.Network == nil || result.Namespace != nil || calls != 1 {
		t.Fatal("finite synthetic network proof was lost", result)
	}
	if result := fixture.broker.Execute(t.Context(), fixture.request("prepare-output", fixture.output)); result.ExitCode == 0 || fixture.chowns != 0 {
		t.Fatal("read-only network proof became namespace/ownership authority")
	}
	for _, change := range []func(*HostFilesRequest){
		func(value *HostFilesRequest) { value.NetworkId = "synthetic-prefix" },
		func(value *HostFilesRequest) { value.ContainerId = strings.Repeat("a", 64) },
		func(value *HostFilesRequest) { value.Path = "/synthetic/namespace" },
		func(value *HostFilesRequest) { value.Operation = "mount-evidence" },
		func(value *HostFilesRequest) { value.Operation = "verify-id-map" },
	} {
		changed := request
		change(&changed)
		if result := fixture.broker.Execute(t.Context(), changed); result.ExitCode == 0 || calls != 1 || fixture.mounts != 0 {
			t.Fatal("network identity expanded the finite native role", result)
		}
	}
	proof.NetworkId = strings.Repeat("f", 64)
	if result := fixture.broker.Execute(t.Context(), request); result.ExitCode == 0 || result.Network != nil {
		t.Fatal("another network's proof was accepted")
	}
}
