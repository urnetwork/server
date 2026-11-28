// The manager owns one contract creation bound and every device it opens
// creates contracts through it. The bound comes from proxy.yml, keeps its
// default when the value is missing or bad, and counts toward the process
// capacity gauge while the manager is open.
package proxy

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
)

// The default manager bounds contract creations and hands its bound, with its
// registration bound, to the local authority of every device it opens; a
// nonpositive setting is unbounded.
func TestProxyDeviceManagerOwnsOneContractAdmission(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("{}\n"))
	defer popConfig()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedContractConcurrency != defaultHostedContractConcurrency || defaultHostedContractConcurrency <= 0 {
		t.Fatalf("default contract creation bound = %d", settings.HostedContractConcurrency)
	}
	settings := &ProxyDeviceManagerSettings{HostedMintConcurrency: 2, HostedContractConcurrency: 2}
	manager := NewProxyDeviceManager(ctx, settings)
	if manager.contractAdmission == nil {
		t.Fatal("a positive bound built no admission")
	}
	authoritySettings := manager.proxyDeviceSettings().authoritySettings()
	if authoritySettings.ContractAdmission != manager.contractAdmission {
		t.Fatal("a device's authority does not create contracts through its manager's bound")
	}
	if authoritySettings.MintAdmission != manager.mintAdmission {
		t.Fatal("a device's authority does not register through its manager's bound")
	}
	settings.HostedContractConcurrency = 0
	if NewProxyDeviceManager(ctx, settings).contractAdmission != nil {
		t.Fatal("an unbounded manager built an admission")
	}
}

// The default admits 256 contract creations in flight per process, far above
// ordinary operation and far below the pile-up after a connect block restart.
// localclient's TestAuthorityContractAdmissionHoldsSlotsUntilControlReturns
// shows a full bound refusing the next creation as a 429.
func TestProxyDeviceManagerDefaultContractBoundIs256(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("{}\n"))
	defer popConfig()
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedContractConcurrency != 256 {
		t.Fatalf("default contract creation bound = %d, want 256", settings.HostedContractConcurrency)
	}
}

// proxy.yml replaces the default bound without a build.
func TestDefaultProxyDeviceManagerSettingsLoadsHostedContractConcurrency(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("hosted_contract_concurrency: 512\n"))
	defer popConfig()
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedContractConcurrency != 512 {
		t.Fatalf("configured contract creation bound = %d, want 512", settings.HostedContractConcurrency)
	}
}

// A host reads the bound from its deployed proxy.yml, beside the key that
// already lives there.
func TestDefaultProxyDeviceManagerSettingsReadsHostedContractConcurrencyFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", home)
	resource := []byte("device_memory_budget: 32MiB\nhosted_contract_concurrency: 384\n")
	if err := os.WriteFile(filepath.Join(home, "proxy.yml"), resource, 0o644); err != nil {
		t.Fatal(err)
	}
	if settings := DefaultProxyDeviceManagerSettings(); settings.HostedContractConcurrency != 384 {
		t.Fatalf("contract creation bound from the config file = %d, want 384", settings.HostedContractConcurrency)
	}
}

// Without the key, or without proxy.yml at all, the bound keeps its default.
func TestDefaultProxyDeviceManagerSettingsKeepsContractBoundWithoutConfig(t *testing.T) {
	for _, resource := range []string{"{}\n", "device_memory_budget: 24MiB\n"} {
		popConfig := server.Config.PushSimpleResource("proxy.yml", []byte(resource))
		bound := DefaultProxyDeviceManagerSettings().HostedContractConcurrency
		popConfig()
		if bound != defaultHostedContractConcurrency {
			t.Errorf("proxy.yml %q gave a bound of %d, want the default %d", resource, bound, defaultHostedContractConcurrency)
		}
	}
	t.Setenv("WARP_CONFIG_HOME", t.TempDir())
	if bound := DefaultProxyDeviceManagerSettings().HostedContractConcurrency; bound != defaultHostedContractConcurrency {
		t.Errorf("no proxy.yml gave a bound of %d, want the default %d", bound, defaultHostedContractConcurrency)
	}
}

// Anything but a positive integer keeps the default, without failing startup:
// a nonpositive value would remove the bound, and the rest are typos.
func TestDefaultProxyDeviceManagerSettingsRejectsInvalidHostedContractConcurrency(t *testing.T) {
	for _, value := range []string{
		"0", "-1", "-256", `"512"`, "abc", "1.5", "256.0", "1e3", "true",
		"null", "", "[]", "[512]", "{bound: 512}", "99999999999999999999",
	} {
		popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("hosted_contract_concurrency: "+value+"\n"))
		bound := DefaultProxyDeviceManagerSettings().HostedContractConcurrency
		popConfig()
		if bound != defaultHostedContractConcurrency {
			t.Errorf("hosted_contract_concurrency: %s gave a bound of %d, want the default %d", value, bound, defaultHostedContractConcurrency)
		}
	}
}

// A proxy.yml that does not parse, or cannot be read, keeps the default bound
// instead of failing startup. (device_memory_budget, read from the same
// resource, fails startup on its own when the resource does not parse.)
func TestProxyHostedContractConcurrencySurvivesUnreadableConfig(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("hosted_contract_concurrency: [512\n"))
	bound := proxyHostedContractConcurrencyFromConfig()
	popConfig()
	if bound != defaultHostedContractConcurrency {
		t.Errorf("an unparsable proxy.yml gave a bound of %d, want the default %d", bound, defaultHostedContractConcurrency)
	}

	home := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", home)
	// a directory where the file belongs is unavailable, not absent
	if err := os.Mkdir(filepath.Join(home, "proxy.yml"), 0o755); err != nil {
		t.Fatal(err)
	}
	if bound := proxyHostedContractConcurrencyFromConfig(); bound != defaultHostedContractConcurrency {
		t.Errorf("an unreadable proxy.yml gave a bound of %d, want the default %d", bound, defaultHostedContractConcurrency)
	}
}

// The process capacity gauge, read from the default registry by the name the
// proxy dashboard queries.
func proxyContractAdmissionCapacity(t testing.TB) float64 {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	for _, family := range families {
		if family.GetName() == "urnetwork_hosted_contract_admission_capacity" && len(family.GetMetric()) == 1 {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("urnetwork_hosted_contract_admission_capacity is not exported (gather: %v)", err)
	return 0
}

// The configured bound is the one the manager builds and hands every device's
// authority. Its slots count toward the process capacity while the manager is
// open, and closing the manager withdraws them.
func TestProxyDeviceManagerBuildsTheConfiguredContractBound(t *testing.T) {
	popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("hosted_contract_concurrency: 3\n"))
	defer popConfig()
	settings := DefaultProxyDeviceManagerSettings()
	if settings.HostedContractConcurrency != 3 {
		t.Fatalf("configured contract creation bound = %d, want 3", settings.HostedContractConcurrency)
	}
	capacity := proxyContractAdmissionCapacity(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	manager := NewProxyDeviceManager(ctx, settings)
	contractAdmission := manager.proxyDeviceSettings().authoritySettings().ContractAdmission
	if contractAdmission == nil || contractAdmission != manager.contractAdmission {
		t.Fatal("a device's authority does not create contracts through its manager's bound")
	}
	if contractAdmission.Testing_Capacity() != 3 {
		t.Fatalf("the manager built a bound of %d, want the configured 3", contractAdmission.Testing_Capacity())
	}
	if opened := proxyContractAdmissionCapacity(t) - capacity; opened != 3 {
		t.Fatalf("an open manager added %v slots to the process capacity, want 3", opened)
	}
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer closeCancel()
	if err := manager.CloseAndWait(closeCtx); err != nil {
		t.Fatal(err)
	}
	if left := proxyContractAdmissionCapacity(t) - capacity; left != 0 {
		t.Fatalf("a closed manager left %v slots in the process capacity", left)
	}
}

// A device opened under the configured bound creates contracts through it:
// the bound from proxy.yml reaches the device's local authority.
func TestProxyDeviceAuthorityUsesTheConfiguredContractBound(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		popConfig := server.Config.PushSimpleResource("proxy.yml", []byte("hosted_contract_concurrency: 3\n"))
		defer popConfig()
		h := setupProxyTestWithOptions(t, defaultProxyTestOptions())
		defer h.close(t)
		pd, err := h.proxyDeviceManager.OpenProxyDevice(h.proxyId)
		if err != nil {
			t.Fatal(err)
		}
		if pd.localControl == nil || pd.localControl.Testing_ContractAdmission() != h.proxyDeviceManager.contractAdmission {
			t.Fatal("the device authority does not create contracts through its manager's bound")
		}
		if capacity := pd.localControl.Testing_ContractAdmission().Testing_Capacity(); capacity != 3 {
			t.Fatalf("the device authority creates contracts through a bound of %d, want the configured 3", capacity)
		}
	})
}
