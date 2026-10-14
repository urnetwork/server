package work

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
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

// A post-step can receive a nil result, for example from a RunPost retry of a
// result that did not decode. Disabled, each post-step must schedule nothing
// rather than panic on the result.
func TestDisabledRecurringPostsTolerateANilResult(t *testing.T) {
	controller.SetStConfig(&controller.StConfig{Enabled: false})
	defer controller.SetStConfig(nil)
	posts := []struct {
		name string
		post func() error
	}{
		{name: "trail sweep", post: func() error { return SweepVerifyTrailsPost(&SweepVerifyTrailsArgs{}, nil, nil, nil) }},
		{name: "stats rollup", post: func() error {
			return RollupVerifyProviderStatsPost(&RollupVerifyProviderStatsArgs{}, nil, nil, nil)
		}},
		{name: "stats retention", post: func() error {
			return RemoveOldVerifyProviderStatsPost(&RemoveOldVerifyProviderStatsArgs{}, nil, nil, nil)
		}},
		{name: "contract degradation check", post: func() error {
			return CheckContractDegradationPost(&CheckContractDegradationArgs{}, nil, nil, nil)
		}},
	}
	for _, p := range posts {
		var err error
		failure := server.HandleError(func() {
			err = p.post()
		})
		if failure != nil || err != nil {
			t.Errorf("%s: a nil result failed the disabled post-step: panic=%v err=%v", p.name, failure, err)
		}
	}
}
