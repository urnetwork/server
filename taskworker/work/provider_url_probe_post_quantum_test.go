package work

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/egresshealth"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// A post-quantum turn's resolver answers what the platform's store holds for
// the peer: its published key, no key and no error for a peer that never
// published one, and an error, never a panic, for a read that fails or
// raises.
func TestProviderUrlProbePeerKeyFetcherResolvesThroughThePlatformStore(t *testing.T) {
	published, unpublished, failing, raising := connect.NewId(), connect.NewId(), connect.NewId(), connect.NewId()
	key := bytes.Repeat([]byte{7}, 32)
	readErr := errors.New("synthetic key read failure")
	previous := getProviderUrlProbePeerClientPublicKey
	getProviderUrlProbePeerClientPublicKey = func(ctx context.Context, clientId server.Id) ([]byte, error) {
		switch connect.Id(clientId) {
		case published:
			return key, nil
		case failing:
			return nil, readErr
		case raising:
			panic(errors.New("synthetic raised datastore failure"))
		default:
			return nil, nil
		}
	}
	t.Cleanup(func() { getProviderUrlProbePeerClientPublicKey = previous })

	if fetched, err := newProviderUrlProbePeerClientPublicKeyFetcher(published)(context.Background()); err != nil || !bytes.Equal(fetched, key) {
		t.Fatalf("fetched %x, %v; want the published key", fetched, err)
	}
	if fetched, err := newProviderUrlProbePeerClientPublicKeyFetcher(unpublished)(context.Background()); err != nil || len(fetched) != 0 {
		t.Fatalf("an unpublished peer fetched %x, %v", fetched, err)
	}
	if _, err := newProviderUrlProbePeerClientPublicKeyFetcher(failing)(context.Background()); !errors.Is(err, readErr) {
		t.Fatalf("a failed read returned %v", err)
	}
	if fetched, err := newProviderUrlProbePeerClientPublicKeyFetcher(raising)(context.Background()); err == nil || len(fetched) != 0 {
		t.Fatalf("a raised read returned %x, %v", fetched, err)
	}
}

// The URL pass resolves a post-quantum turn's peer keys from the platform.
func TestProviderUrlProbePassResolvesPeerKeys(t *testing.T) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("example.test"), 0)
	operator := &ingest.Client{ServerUrl: "https://operator.example", OperatorSecret: "synthetic", ShardIndex: args.ShardIndex, ShardCount: args.ShardCount}
	pass := newProviderUrlProbePass(args, operator, providertunnel.Config{ApiUrl: "https://api.example"}, server.NewId())
	if pass.fullOptions.NewPeerClientPublicKeyFetcher == nil {
		t.Fatal("the URL pass gives its post-quantum turns no key fetcher")
	}
	if !pass.fullOptions.TunnelConfig.KeepClientKeysLocal || pass.fullOptions.TunnelConfig.CapabilityObservation != nil {
		t.Fatal("the URL pass's own tunnels changed: every turn but a post-quantum one keeps its key local and opens no session")
	}
}

// An acknowledged post-quantum turn counts its session's outcome; a turn
// without e2e evidence counts nothing.
func TestUrlProbeE2eOutcomesCountsPostQuantumTurns(t *testing.T) {
	positive := testutil.ToFloat64(urlProbeE2eOutcomes.WithLabelValues(egresshealth.UrlProbePqPositive))
	unmeasured := testutil.ToFloat64(urlProbeE2eOutcomes.WithLabelValues(egresshealth.UrlProbePqUnmeasured))
	observeUrlProbeE2eOutcome(&egresshealth.Result{UrlProbeEvidence: &egresshealth.UrlProbeEvidence{
		E2e: &egresshealth.UrlProbeE2eEvidence{Pq: egresshealth.UrlProbePqPositive, Window: egresshealth.UrlProbeWindowResizable},
	}})
	observeUrlProbeE2eOutcome(&egresshealth.Result{UrlProbeEvidence: &egresshealth.UrlProbeEvidence{}})
	observeUrlProbeE2eOutcome(&egresshealth.Result{})
	observeUrlProbeE2eOutcome(nil)
	if got := testutil.ToFloat64(urlProbeE2eOutcomes.WithLabelValues(egresshealth.UrlProbePqPositive)); got != positive+1 {
		t.Fatalf("positive turns counted %v, want %v", got, positive+1)
	}
	if got := testutil.ToFloat64(urlProbeE2eOutcomes.WithLabelValues(egresshealth.UrlProbePqUnmeasured)); got != unmeasured {
		t.Fatalf("turns without a session counted as unmeasured: %v", got)
	}
}
