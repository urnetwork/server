// A slow check must not withhold completed, independently safe evidence.
package work

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

func testBlackholeEarlyPass(
	check fleetprobe.BlackholeChecker,
	submit func(context.Context, []ingest.BlackholeCheck) error,
) *providerEgressProbePass {
	return &providerEgressProbePass{
		readiness: &providerEgressProbeReadiness{
			minimum:   1,
			available: func(context.Context) (model.ByteCount, error) { return 1, nil },
		},
		blackholeOptions:          fleetprobe.BlackholeOptions{CheckOne: check},
		publishSafeBlackholeEarly: true,
		runBlackhole:              fleetprobe.RunBlackhole,
		submitBlackholeChecks:     submit,
	}
}

func testBlackholeEarlyProviders(count int) []prober.Provider {
	providers := make([]prober.Provider, count)
	for index := range providers {
		providers[index] = prober.Provider{ClientId: fmt.Sprintf("synthetic-provider-%02d.example", index)}
	}
	return providers
}

func TestBlackholeSafeChecksPublishBeforeSlowSiblingFinishes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		providers := testBlackholeEarlyProviders(17)
		releaseTail := make(chan struct{})
		firstPublished := make(chan struct{})
		done := make(chan struct{})
		var submissions [][]ingest.BlackholeCheck
		pass := testBlackholeEarlyPass(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
			check := ingest.BlackholeCheck{ClientId: provider.ClientId, CheckedAt: time.Unix(1, 0).UTC()}
			if provider.ClientId == providers[16].ClientId {
				<-releaseTail
				check.Failure = egresshealth.FailureAllDestinationsFailed
				return fleetprobe.BlackholeResult{Check: check, Dark: true}
			}
			check.Ok = true
			return fleetprobe.BlackholeResult{Check: check}
		}, func(_ context.Context, checks []ingest.BlackholeCheck) error {
			submissions = append(submissions, slices.Clone(checks))
			if len(submissions) == 1 {
				close(firstPublished)
			}
			return nil
		})
		args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
		go func() {
			defer close(done)
			if _, _, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 17, providers); err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait()
		select {
		case <-firstPublished:
		default:
			t.Fatal("safe completions remained behind a slow sibling")
		}
		if len(submissions) != 1 || len(submissions[0]) != 16 {
			t.Fatalf("early submissions=%v, want one 16-check passing batch", submissions)
		}
		for _, check := range submissions[0] {
			if !check.Ok {
				t.Fatal("ordinary negative published before the guard")
			}
		}
		close(releaseTail)
		<-done
		if len(submissions) != 2 || len(submissions[1]) != 1 || submissions[1][0].ClientId != providers[16].ClientId {
			t.Fatalf("final submissions=%v, want only the guarded tail", submissions)
		}
	})
}

func TestBlackholeEarlySafePublicationKeepsDarkGuard(t *testing.T) {
	providers := testBlackholeEarlyProviders(22)
	var submissions [][]ingest.BlackholeCheck
	pass := testBlackholeEarlyPass(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
		check := ingest.BlackholeCheck{ClientId: provider.ClientId, CheckedAt: time.Unix(1, 0).UTC()}
		if provider.ClientId < providers[16].ClientId {
			check.Ok = true
			return fleetprobe.BlackholeResult{Check: check}
		}
		check.Failure = egresshealth.FailureAllDestinationsFailed
		return fleetprobe.BlackholeResult{Check: check, Dark: true}
	}, func(_ context.Context, checks []ingest.BlackholeCheck) error {
		submissions = append(submissions, slices.Clone(checks))
		return nil
	})
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	summary, tripped, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 22, providers)
	if err != nil || !tripped || summary.NotMeasured != 6 || len(submissions) != 2 {
		t.Fatalf("guard/finalization: tripped=%t summary=%+v submissions=%d err=%v", tripped, summary, len(submissions), err)
	}
	seen := map[string]bool{}
	for _, batch := range submissions {
		for _, check := range batch {
			if seen[check.ClientId] {
				t.Errorf("duplicate publication for %s", check.ClientId)
			}
			seen[check.ClientId] = true
			if !check.Ok && !check.NotMeasured {
				t.Error("ordinary dark verdict bypassed batch guard")
			}
		}
	}
	if len(seen) != len(providers) {
		t.Errorf("published %d provider results, want %d", len(seen), len(providers))
	}
}

func TestBlackholeEarlySubmitFailureFallsBackToFinalBatch(t *testing.T) {
	providers := testBlackholeEarlyProviders(17)
	var submissions [][]ingest.BlackholeCheck
	pass := testBlackholeEarlyPass(func(_ context.Context, provider prober.Provider) fleetprobe.BlackholeResult {
		return fleetprobe.BlackholeResult{Check: ingest.BlackholeCheck{
			ClientId: provider.ClientId, Ok: true, CheckedAt: time.Unix(1, 0).UTC(),
		}}
	}, func(_ context.Context, checks []ingest.BlackholeCheck) error {
		submissions = append(submissions, slices.Clone(checks))
		if len(submissions) == 1 {
			return errors.New("synthetic early transport failure")
		}
		return nil
	})
	args := providerEgressProbeArgs(testProviderEgressProbeSettings(1), 0)
	_, tripped, err := pass.runBlackholeBatch(context.Background(), args, nil, nil, 17, providers)
	if err != nil || tripped || len(submissions) != 2 || len(submissions[0]) != 16 || len(submissions[1]) != 17 {
		t.Fatalf("fallback: tripped=%t submissions=%v err=%v", tripped, submissions, err)
	}
}
