package work

import (
	"testing"

	"github.com/urnetwork/server/v2026/controller"
)

// A stale row can be claimed between a feature-state change and InitTasks'
// queue cleanup. Disabled work must therefore stop at the function boundary,
// before it needs a session, PostgreSQL, Redis, or verify.yml.
func TestDisabledVerifyWorkReturnsBeforeDependencyAccess(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: false})
	defer controller.SetStConfig(nil)

	if result, err := SweepVerifyTrails(&SweepVerifyTrailsArgs{}, nil); err != nil || result == nil {
		t.Fatalf("disabled trail sweep = (%v, %v), want non-nil no-op result", result, err)
	}
	if result, err := RollupVerifyProviderStats(&RollupVerifyProviderStatsArgs{}, nil); err != nil || result == nil {
		t.Fatalf("disabled stats rollup = (%v, %v), want non-nil no-op result", result, err)
	}
	if result, err := RefreshVerifyProxyEgress(&RefreshVerifyProxyEgressArgs{}, nil); err != nil || result == nil {
		t.Fatalf("disabled egress refresh = (%v, %v), want non-nil no-op result", result, err)
	}
	if result, err := RemoveOldVerifyProviderStats(&RemoveOldVerifyProviderStatsArgs{}, nil); err != nil || result == nil {
		t.Fatalf("disabled stats retention = (%v, %v), want non-nil no-op result", result, err)
	}
}
