package work

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/qualityprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/fleetprobe"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
	"github.com/urnetwork/server/v2026/qualityprobe/prober"
)

type testingUrlCompletionIngest struct {
	*recordingEgressProbeIngest
	completions []qualityprobe.UrlProbeCompletion
}

func (self *testingUrlCompletionIngest) ReportUrlProbeCompletion(_ context.Context, completion qualityprobe.UrlProbeCompletion) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.completions = append(self.completions, completion)
	return nil
}

// Count the completed turn during platform failure, but preserve the existing
// no-provider-verdict rule: no negative quality is authorized. The model owns
// a separate bounded retry once this exact claim is durably complete.
func TestUrlCompletedReadinessCountsWithoutProviderVerdict(t *testing.T) {
	for _, funded := range []bool{false, true} {
		inner := &testingUrlCompletionIngest{recordingEgressProbeIngest: newRecordingEgressProbeIngest()}
		reporter := &providerEgressProbeReadinessReporter{
			egressProbeIngest: inner,
			readiness: &providerEgressProbeReadiness{minimum: 1,
				available: func(context.Context) (model.ByteCount, error) {
					if funded {
						return 1, nil
					}
					return 0, nil
				},
			},
		}
		at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		err := reporter.ReportUrlProbeCompletion(context.Background(), qualityprobe.UrlProbeCompletion{
			ClientId: "synthetic-provider", ClaimOrdinal: 23, CompletedAt: at,
			ProbeFailure: prober.FailureTunnel, AllowPacing: true,
		})
		if (err == nil) != funded || !funded && !errors.Is(err, errProviderEgressProbeUnfunded) || len(inner.completions) != 1 || len(inner.health) != 0 || len(inner.calls) != 0 {
			t.Fatalf("readiness lost terminal count or published a provider verdict: funded=%t reports=%+v error=%v", funded, inner.completions, err)
		}
		completion := inner.completions[0]
		if completion.AllowPacing != funded || completion.ClaimOrdinal != 23 || !completion.CompletedAt.Equal(at) {
			t.Fatalf("readiness rewrote owning identity or authorized false pacing: %+v", completion)
		}
	}
}

// The due identity crosses the real fleet adapter, guarded buffer, and metrics
// adapter. Release retries preserve the stored receipt rather than re-clock it.
func TestUrlCompletedBufferedReleasePreservesIdentity(t *testing.T) {
	for _, tripped := range []bool{false, true} {
		at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		providers := fleetprobe.ProvidersFromDue([]ingest.DueProvider{{ClientId: "synthetic-provider", ClaimOrdinal: 31, ClaimedAt: at}})
		if len(providers) != 1 || providers[0].ClaimOrdinal != 31 {
			t.Fatalf("fleet adapter dropped claim identity: %+v", providers)
		}
		inner := &testingUrlCompletionIngest{recordingEgressProbeIngest: newRecordingEgressProbeIngest()}
		batch := newProviderEgressFullBatch(testFullBatchSink(inner))
		completion := qualityprobe.UrlProbeCompletion{ClientId: providers[0].ClientId, ClaimOrdinal: providers[0].ClaimOrdinal,
			CompletedAt: at.Add(time.Second), ProbeFailure: prober.FailureTunnel, AllowPacing: true}
		if err := batch.ReportUrlProbeCompletion(context.Background(), completion); err != nil {
			t.Fatal(err)
		}
		completion.ClaimOrdinal = 99
		completion.CompletedAt = at.Add(time.Hour)
		for range 2 {
			batch.release(context.Background(), tripped, nil, nil, 0, nil)
		}
		if len(inner.completions) != 2 || len(inner.calls) != 0 || len(inner.health) != 0 || batch.releaseAttemptFailures != 0 {
			t.Fatalf("buffered release silently downgraded to legacy: reports=%+v calls=%v", inner.completions, inner.calls)
		}
		wantFailure := prober.FailureTunnel
		if tripped {
			wantFailure = model.ProbeRunBatchGuardClass
		}
		for _, reported := range inner.completions {
			if reported.ClaimOrdinal != 31 || !reported.CompletedAt.Equal(at.Add(time.Second)) || reported.ProbeFailure != wantFailure {
				t.Fatalf("release retry changed completed-turn identity: %+v", reported)
			}
		}
	}
}
