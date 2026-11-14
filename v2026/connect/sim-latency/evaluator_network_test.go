// The rendered network and live inspection guard must deny the host gateway,
// not merely mark a Docker bridge internal.
package main

import (
	"encoding/json"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Check the actual Compose template rather than a parallel settings fixture.
func TestEvaluatorComposeRequiresIsolatedHostGateway(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("evaluator", "container", "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var compose struct {
		Networks map[string]map[string]any `yaml:"networks"`
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &compose); err != nil {
		t.Fatal(err)
	}
	network := compose.Networks["evaluation"]
	options, ok := network["driver_opts"].(map[string]any)
	if !ok || network["driver"] != "bridge" || network["internal"] != true || network["enable_ipv6"] != false || network["attachable"] != false ||
		options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" ||
		options["com.docker.network.bridge.gateway_mode_ipv6"] != "isolated" {
		t.Fatal("evaluation bridge still exposes a host gateway", network)
	}
	for _, service := range []string{"runner", "migrate", "postgres", "redis", "scorer"} {
		sysctls, ok := compose.Services[service]["sysctls"].(map[string]any)
		if !ok || sysctls["net.ipv6.conf.all.disable_ipv6"] != "1" || sysctls["net.ipv6.conf.default.disable_ipv6"] != "1" {
			t.Fatal("job namespace can reach a host link-local IPv6 address", service)
		}
	}
}

// The live helper is also callable on fixture JSON, with no daemon access.
func checkEvaluatorNetworkFixture(t *testing.T, mode string, value any) error {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(t.Context(), "/bin/bash", filepath.Join("evaluator", "container", "network-boundary.sh"), mode)
	command.Stdin = strings.NewReader(string(data))
	output, err := command.CombinedOutput()
	if len(output) != 0 {
		t.Fatalf("network JSON check unexpectedly emitted output: %s", output)
	}
	return err
}

// Every field is synthetic and every case starts from a complete observation.
func evaluatorIsolatedNetworkFixture() map[string]any {
	return map[string]any{
		"Id": strings.Repeat("a", 64), "Driver": "bridge", "Scope": "local",
		"Internal": true, "EnableIPv6": false, "Attachable": false,
		"Options": map[string]any{
			"com.docker.network.bridge.gateway_mode_ipv4": "isolated",
			"com.docker.network.bridge.gateway_mode_ipv6": "isolated",
		},
	}
}

func TestEvaluatorNetworkInspectionRequiresIsolatedGateway(t *testing.T) {
	if err := checkEvaluatorNetworkFixture(t, "--check-inspect", []any{evaluatorIsolatedNetworkFixture()}); err != nil {
		t.Fatal("complete isolated network rejected", err)
	}
	for _, mutate := range []func(map[string]any){
		func(value map[string]any) { value["Internal"] = false },
		func(value map[string]any) { value["EnableIPv6"] = true },
		func(value map[string]any) { value["Attachable"] = true },
		func(value map[string]any) { value["Driver"] = "host" },
		func(value map[string]any) { value["Scope"] = "swarm" },
		func(value map[string]any) { value["Id"] = "synthetic-invalid-id" },
		func(value map[string]any) { delete(value, "Options") },
		func(value map[string]any) {
			value["Options"].(map[string]any)["com.docker.network.bridge.gateway_mode_ipv4"] = "nat"
		},
		func(value map[string]any) {
			value["Options"].(map[string]any)["com.docker.network.bridge.gateway_mode_ipv6"] = "routed"
		},
		func(value map[string]any) {
			value["Options"].(map[string]any)["com.docker.network.bridge.name"] = "synthetic-host-bridge"
		},
	} {
		value := evaluatorIsolatedNetworkFixture()
		mutate(value)
		if err := checkEvaluatorNetworkFixture(t, "--check-inspect", []any{value}); err == nil {
			t.Fatal("unsafe or incomplete network accepted", value)
		}
	}
}

// Isolated bridges gain a kernel-generated link-local IPv6 address after peers
// attach. Only that protocol address is allowed, with every job's IPv6 disabled;
// all host IPv4 and global IPv6 addresses remain forbidden.
func TestEvaluatorHostBridgeMustHaveNoReachableAddress(t *testing.T) {
	name := "br-" + strings.Repeat("a", 12)
	linkLocal := netip.AddrFrom16([16]byte{0: 0xfe, 1: 0x80, 15: 1}).String()
	if err := checkEvaluatorNetworkFixture(t, "--check-link", []any{map[string]any{"ifname": name, "addr_info": []any{}}}); err != nil {
		t.Fatal("address-free host bridge rejected", err)
	}
	if err := checkEvaluatorNetworkFixture(t, "--check-link", []any{map[string]any{"ifname": name, "addr_info": []any{map[string]any{
		"family": "inet6", "local": linkLocal, "scope": "link", "prefixlen": 64,
	}}}}); err != nil {
		t.Fatal("automatic host link-local address rejected despite disabled job IPv6", err)
	}
	for _, value := range []any{
		[]any{},
		[]any{map[string]any{"ifname": name}},
		[]any{map[string]any{"ifname": "synthetic-wrong-link", "addr_info": []any{}}},
		[]any{map[string]any{"ifname": name, "addr_info": []any{map[string]any{"family": "inet", "local": "192.0.2.1", "scope": "global"}}}},
		[]any{map[string]any{"ifname": name, "addr_info": []any{map[string]any{"family": "inet6", "local": "2001:db8::1", "scope": "link"}}}},
		[]any{map[string]any{"ifname": name, "addr_info": []any{map[string]any{"family": "inet6", "local": linkLocal, "scope": "global", "prefixlen": 64}}}},
	} {
		if err := checkEvaluatorNetworkFixture(t, "--check-link", value); err == nil {
			t.Fatal("host-addressed or incomplete bridge accepted", value)
		}
	}
}
