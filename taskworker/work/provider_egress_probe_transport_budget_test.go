// Runtime task owners copy budget values, never mutable ownership from a
// previous task or the full/blackhole sibling sharing one durable task row.
package work

import (
	"encoding/json"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/operator-proxy/providertunnel"
)

// Reusing one task argument snapshot must construct independent owners.
func TestProviderEgressProbeTransportBudgetOwnersArePrivate(t *testing.T) {
	args := ProviderEgressProbeBatchArgs{
		TransportBudgetByteCount: 64 * 1024 * 1024,
		TransportBudgetCount:     100,
	}
	base := providertunnel.Config{
		DeviceDescription:       "synthetic probe owner",
		PlatformTransportBudget: connect.NewPlatformTransportBudget(1, 1),
	}
	seen := map[*connect.PlatformTransportBudget]bool{}
	for range 50 {
		for _, cfg := range []providertunnel.Config{
			providerEgressProbeTunnelConfig(base, args),
			providerEgressProbeTunnelConfig(base, args),
		} {
			budget := cfg.PlatformTransportBudget
			if budget == nil || budget == base.PlatformTransportBudget || seen[budget] {
				t.Fatal("unrelated task/probe kinds inherited mutable transport ownership")
			}
			seen[budget] = true
			if stats := budget.Stats(); stats.TotalByteCount != args.TransportBudgetByteCount ||
				stats.MaxTransportCount != args.TransportBudgetCount || stats.UsedByteCount != 0 {
				t.Fatalf("configured owner limits changed: %+v", stats)
			}
			batchCopy := cfg
			if batchCopy.PlatformTransportBudget != budget || cfg.DeviceDescription != base.DeviceDescription {
				t.Fatal("drained-batch config copy lost its owner or identity settings")
			}
		}
	}
	if base.PlatformTransportBudget.Stats().TotalByteCount != 1 {
		t.Fatal("constructing task ownership mutated the input config")
	}
}

// Unset deployment values still create a task owner. They copy the current
// default limit values, not a process root or a per-tunnel scaled allowance.
func TestProviderEgressProbeTransportBudgetDefaultsOwnTaskRoot(t *testing.T) {
	base := providertunnel.Config{PlatformTransportBudget: connect.NewPlatformTransportBudget(1, 1)}
	limits := connect.DefaultPlatformTransportSettings().PlatformTransportBudget.Stats()
	first := providerEgressProbeTunnelConfig(base, ProviderEgressProbeBatchArgs{})
	second := providerEgressProbeTunnelConfig(base, ProviderEgressProbeBatchArgs{})
	if first.PlatformTransportBudget == nil || second.PlatformTransportBudget == nil ||
		first.PlatformTransportBudget == second.PlatformTransportBudget ||
		first.PlatformTransportBudget == base.PlatformTransportBudget {
		t.Fatal("unset deployment limits did not construct separate task/probe-kind owners")
	}
	for _, cfg := range []providertunnel.Config{first, second} {
		stats := cfg.PlatformTransportBudget.Stats()
		if stats.TotalByteCount != limits.TotalByteCount || stats.MaxTransportCount != limits.MaxTransportCount {
			t.Fatalf("default task limits changed or were multiplied by concurrency: %+v", stats)
		}
	}
}

// Partial/negative limits fail closed, while old durable snapshots remain
// valid and explicit values survive argument serialization unchanged.
func TestProviderEgressProbeTransportBudgetValidationAndSnapshot(t *testing.T) {
	for _, test := range []struct {
		bytes connect.ByteCount
		count int
		valid bool
	}{
		{bytes: 0, count: 0, valid: true},
		{bytes: 64 * 1024 * 1024, count: 100, valid: true},
		{bytes: -1, count: 1},
		{bytes: 1, count: -1},
		{bytes: 1, count: 0},
		{bytes: 0, count: 1},
	} {
		args := ProviderEgressProbeBatchArgs{
			Limit: 250, Concurrency: 50, ProbeTimeoutSeconds: 15,
			TransportBudgetByteCount: test.bytes, TransportBudgetCount: test.count,
		}
		if err := validateProviderEgressProbeBatchArgs("synthetic", args); (err == nil) != test.valid {
			t.Errorf("bytes/count=%d/%d valid=%t error=%v", test.bytes, test.count, test.valid, err)
		}
		encoded, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		var restored ProviderEgressProbeBatchArgs
		if err := json.Unmarshal(encoded, &restored); err != nil || restored != args {
			t.Fatalf("durable budget values changed: error=%v", err)
		}
	}
}
