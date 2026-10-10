// Only URL worker geometry may affect production deadlines or credit.
package work

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

func TestUrlProbeSettingsUseOnlyUrlGeometry(t *testing.T) {
	t.Setenv("WARP_DOMAIN", "example.test")
	pop := server.Config.PushSimpleResource("provider_egress_probe.yml", []byte(`
max_time_seconds: 900
url_probe:
  limit: 12
  concurrency: 6
  probe_timeout_seconds: 45
url_success_interval_seconds: 1200
url_failure_interval_seconds: 60
full:
  limit: 0
  concurrency: 0
blackhole:
  limit: 0
  concurrency: 0
load_attempts: 0
load_retry_mean_interval_seconds: 0
`))
	t.Cleanup(pop)
	settings, err := loadProviderEgressProbeSettings()
	if err != nil {
		t.Fatal(err)
	}
	args := providerEgressProbeArgs(settings, 0)
	if args.UrlProbe == nil || args.UrlProbe.Limit != 12 || args.UrlProbe.Concurrency != 6 || args.UrlProbe.ProbeTimeoutSeconds != 45 {
		t.Fatalf("URL settings were not snapshotted: %+v", args.UrlProbe)
	}
	if !providerEgressProbeArgsMatchSettings(args, settings) {
		t.Fatal("deep-copied URL settings compare by pointer identity")
	}
	if budget := providerUrlProbeRunBudget(args); budget > 3*time.Minute {
		t.Fatalf("single URL retained the sampled retry budget: %s", budget)
	}
	credit, err := providerUrlProbeCreditMinimum(args)
	if err != nil {
		t.Fatal(err)
	}
	args.Blackhole.Limit, args.Blackhole.Concurrency = math.MaxInt, math.MaxInt
	args.Full.Limit, args.Full.Concurrency = math.MaxInt, math.MaxInt
	if got, err := providerUrlProbeCreditMinimum(args); err != nil || got != credit {
		t.Fatalf("retired probe geometry changed credit: before=%d after=%d error=%v", credit, got, err)
	}
	args.UrlProbe.Concurrency++
	if providerEgressProbeArgsMatchSettings(args, settings) || settings.UrlProbe.Concurrency != 6 {
		t.Fatal("URL settings failed drift detection or mutated the configured snapshot")
	}
}

func TestUrlProbeCreditRejectsOverflow(t *testing.T) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("example.test"), 0)
	args.UrlProbe.Concurrency = math.MaxInt
	if _, err := providerUrlProbeCreditMinimum(args); err == nil {
		t.Fatal("overflowing URL worker reservation accepted")
	}
}

// Existing tasks must finalize into the current URL snapshot before new policy
// validation; a legacy version cannot become a permanently retried old owner.
func TestUrlProbeLegacyAndPriorPolicyTasksRetireBeforeValidation(t *testing.T) {
	settings := defaultProviderEgressProbeSettings("example.test")
	withProviderEgressProbeSettings(t, settings)
	driftTaskRunner(t, func(context.Context, *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
		t.Fatal("obsolete task reached network execution")
		return nil, nil
	})
	for _, legacyGeometry := range []bool{true, false} {
		args := providerEgressProbeArgs(settings, 0)
		args.UrlProbeResultVersion = 0
		if legacyGeometry {
			args.UrlProbe = nil
		}
		result, err := ProviderEgressProbe(args, driftTaskSession(t))
		if err != nil || result == nil || !result.Stale || args.UrlProbeResultVersion != 0 {
			t.Fatalf("obsolete task did not retire immutably: legacy%t result%+v error%v", legacyGeometry, result, err)
		}
	}
	args := providerEgressProbeArgs(settings, 0)
	args.UrlProbe.Concurrency = 0
	if _, err := ProviderEgressProbe(args, driftTaskSession(t)); err == nil {
		t.Fatal("invalid current geometry was concealed as obsolete")
	}
}
