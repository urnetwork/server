package monitor

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestMonitorSSHKeyPathsResolveRelativeToWarpHome(t *testing.T) {
	warpHome := t.TempDir()
	t.Setenv("WARP_HOME", warpHome)
	paths := monitorSSHKeyPaths([]string{"root/ssh/vpn", "/keys/absolute", "  "})
	want := []string{filepath.Join(warpHome, "root/ssh/vpn"), "/keys/absolute"}
	if !slices.Equal(paths, want) {
		t.Fatalf("resolved monitor SSH paths=%v, want %v", paths, want)
	}
}

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

func TestHostSSHIdentityOverridesEnvironmentIdentity(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.SSHUser = "monitor"
	settings.SSHDevUser = "by"
	settings.SSHKeyPaths = []string{"/keys/service"}
	settings.AddressMode = AddressModeOverlay
	settings.Hosts = []HostSettings{{
		Name: "vpn", OverlayAddress: "192.0.2.1",
		SSHUser: "ubuntu", SSHKeyPaths: []string{"/keys/vpn"},
	}}
	runner := newRunner(configFromSignalSettings(settings))
	runner.runSSH = func(_ context.Context, args []string, _ string) (string, string, error) {
		if !slices.Contains(args, "/keys/vpn") || slices.Contains(args, "/keys/service") {
			t.Fatalf("host-specific SSH keys not isolated: %v", args)
		}
		if !slices.Contains(args, "ubuntu@192.0.2.1") {
			t.Fatalf("host-specific SSH user missing: %v", args)
		}
		return "ok", "", nil
	}
	if _, err := runner.shell(context.Background(), runner.cfg.hosts[0], "true"); err != nil {
		t.Fatal(err)
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
