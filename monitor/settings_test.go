package monitor

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestSignalSettingsSSHKeyPathsBecomeIdentityArguments(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.SSHKeyPaths = []string{"/keys/edge", "/keys/db"}
	runner := newRunner(configFromSignalSettings(settings))
	args := runner.sshArgs("monitor@host", "true", 10*time.Second)
	for _, key := range settings.SSHKeyPaths {
		i := slices.Index(args, key)
		if i < 1 || args[i-1] != "-i" {
			t.Fatalf("ssh args do not contain -i %s: %v", key, args)
		}
	}
}

func TestSignalSettingsRejectsUnknownBlockInventoryWithSyntheticSource(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.LogServices = []string{"api"}
	settings.LogServiceBlocks = map[string][]string{"proxy": {"g1"}}
	if err := settings.Validate(); err == nil || !strings.Contains(err.Error(), `unknown service "proxy"`) {
		t.Fatalf("invalid block inventory error = %v", err)
	}
}

func TestExcludeEdgeIPv6HostsStripsOnlyNamedHost(t *testing.T) {
	settings := SignalSettings{Hosts: []HostSettings{
		{Name: "edge-3", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "eno3", Address: "2001:db8::3"}}},
		{Name: "edge-4", EdgeIPv6: []EdgeIPv6InterfaceSettings{{Interface: "eno4", Address: "2001:db8::4"}}},
	}}
	filtered, err := ExcludeEdgeIPv6Hosts(settings, "edge-3")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered.Hosts[0].EdgeIPv6) != 0 {
		t.Fatalf("excluded host retained IPv6 paths: %+v", filtered.Hosts[0].EdgeIPv6)
	}
	if len(filtered.Hosts[1].EdgeIPv6) != 1 {
		t.Fatalf("unrelated host lost IPv6 paths: %+v", filtered.Hosts[1].EdgeIPv6)
	}
	if len(settings.Hosts[0].EdgeIPv6) != 1 {
		t.Fatal("filter mutated the caller's settings")
	}
}

func TestExcludeEdgeIPv6HostsRejectsUnknownHost(t *testing.T) {
	settings := SignalSettings{Hosts: []HostSettings{{Name: "edge-4"}}}
	if _, err := ExcludeEdgeIPv6Hosts(settings, "edge-3"); err == nil {
		t.Fatal("unknown excluded IPv6 host was accepted")
	}
}
