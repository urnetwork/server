// A URL pass without a pool measures nothing, so every pool fetch of a pass
// is counted by result (SIGNALS.md §2.19h alerts on the unavailable ones).
// The label values are spelled out: the dashboard and the alert read them.
package work

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
	"github.com/urnetwork/server/v2026/qualityprobe/ingest"
)

func TestUrlProbePassCountsEveryPoolFetch(t *testing.T) {
	count := func(result string) float64 {
		return testutil.ToFloat64(egressProbePoolFetchesTotal.WithLabelValues(result))
	}
	served, unavailable := count("server"), count("unavailable")

	pass, args, _ := testUrlProbePass()
	pass.fullDue = func(context.Context, int) ([]ingest.DueProvider, error) { return nil, nil }
	if _, err := pass.run(context.Background(), args); err != nil {
		t.Fatalf("a pass with a pool and nothing due failed: %v", err)
	}
	for name, load := range map[string]func(context.Context) (*egresshealth.Pool, error){
		"a refused fetch": func(context.Context) (*egresshealth.Pool, error) { return nil, egresshealth.ErrPoolUnavailable },
		"an empty pool":   func(context.Context) (*egresshealth.Pool, error) { return &egresshealth.Pool{}, nil },
	} {
		pass.loadPool = load
		if _, err := pass.run(context.Background(), args); err == nil {
			t.Fatalf("%s: the pass ran without a pool", name)
		}
	}
	// a drain mid-fetch says nothing of the pool
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	pass.loadPool = func(ctx context.Context) (*egresshealth.Pool, error) { return nil, ctx.Err() }
	if _, err := pass.run(canceled, args); err == nil {
		t.Fatal("a canceled pass ran")
	}

	if got := count("server") - served; got != 1 {
		t.Errorf("served fetches counted %v, want 1", got)
	}
	if got := count("unavailable") - unavailable; got != 2 {
		t.Errorf("unavailable fetches counted %v, want 2", got)
	}
}
