package main

import (
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server/proxy"
)

func TestProxyMainDoesNotRunGlobalModelWarmups(t *testing.T) {
	if targets := proxyWarmupTargets(); len(targets) != 0 {
		t.Fatalf("proxy eagerly warms %v; Proxy uses no registered search, location, or IP target", targets)
	}
}

func TestProxyMainForcesIpv4ControlTraffic(t *testing.T) {
	defer connect.SetControlIpFamilyPolicy(connect.IpFamilyAuto)
	configureProxyControlFamily()
	if got := connect.ControlIpFamilyPolicy(); got != connect.IpFamilyForce4 {
		t.Fatalf("proxy control family policy = %d, want IPv4", got)
	}
}

func TestResizeProxyMessagePoolsCapsAllClassesAtEightGiB(t *testing.T) {
	resizeProxyMessagePools()

	stats := connect.GetMessagePoolAggregateStats()
	if stats.CapacityByteCount > proxyMessagePoolByteCount {
		t.Fatalf("aggregate capacity = %d, exceeds total budget %d", stats.CapacityByteCount, proxyMessagePoolByteCount)
	}
	// Integer division by four class sizes leaves only a small rounding gap.
	// The former one-argument call is roughly 24 GiB and fails this bound by a
	// wide margin, making this a deterministic regression for the defect.
	if deficit := proxyMessagePoolByteCount - stats.CapacityByteCount; deficit >= connect.ByteCount(16<<10) {
		t.Fatalf("aggregate capacity = %d, unexpected budget deficit %d", stats.CapacityByteCount, deficit)
	}

	packetBudget := proxyMessagePoolByteCount / 3
	largeBudget := proxyMessagePoolByteCount - packetBudget
	var packetCapacity, largeCapacity connect.ByteCount
	for _, class := range connect.GetMessagePoolClassStats() {
		classBytes := connect.ByteCount(class.Size * class.Capacity)
		if class.Size <= 2048 {
			packetCapacity += classBytes
		} else {
			largeCapacity += classBytes
		}
	}
	if packetCapacity > packetBudget || packetBudget-packetCapacity >= connect.ByteCount(4<<10) {
		t.Fatalf("packet capacity = %d, want budget %d within rounding", packetCapacity, packetBudget)
	}
	if largeCapacity > largeBudget || largeBudget-largeCapacity >= connect.ByteCount(12<<10) {
		t.Fatalf("large-object capacity = %d, want budget %d within rounding", largeCapacity, largeBudget)
	}
}

func TestProxyMainHoldsIdentityRestoreForDeploymentHandoff(t *testing.T) {
	settings := proxy.DefaultProxySettings()
	settings.EnableWgHandoff = true
	if !newProxyDeviceManagerSettings(settings).HoldWindowIdentityRestore {
		t.Fatal("wg handoff did not hold replacement window identity restoration")
	}

	settings.EnableWgHandoff = false
	if newProxyDeviceManagerSettings(settings).HoldWindowIdentityRestore {
		t.Fatal("disabled wg handoff left window identity restoration held forever")
	}
}

// The production entry point must use the combined handoff boundary tested by
// proxy.TestProxyDeployOverlapPrewarmGate. Reintroducing a direct Prewarm call
// here would bypass identity release ordering even if the helper stayed sound.
func TestProxyMainUsesCombinedDeploymentHandoff(t *testing.T) {
	source, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	mainStart := strings.Index(string(source), "func main()")
	if mainStart < 0 {
		t.Fatal("main function not found")
	}
	mainSource := string(source[mainStart:])
	if !strings.Contains(mainSource, "wg.CompleteDeploymentHandoff(ctx)") {
		t.Fatal("production main does not use the tested deployment handoff boundary")
	}
	if strings.Contains(mainSource, "proxy.Prewarm(") {
		t.Fatal("production main bypasses the combined deployment handoff with direct prewarm")
	}
}
