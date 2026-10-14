// Native wiring of the published backlog depth through the read script.
package model

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server"
)

// The published depth is the first call's occupancy: everything retained less
// at most one page of the committed prefix. Publishing the last call instead
// reported 1000, the trough after this drain's own cleanup of 1000 committed
// records, while producers had met 2000.
func TestProviderEgressTallyDrainPublishesFirstCallOccupancy(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		run := ProviderEgressRunTally{Place: ProviderEgressPlace{CountryCode: "zz", Region: "Synthetic backlog"}}
		shard := ProviderEgressTallyShard(run.Place)
		generation := "synthetic-owner"
		testingOpenTallyShard(t, ctx, shard, generation)
		writer := NewProviderEgressTallyWriter()
		record := func(n int) {
			for range n {
				if err := writer.Record(ctx, time.Now(), run, []ProviderEgressSiteLoad{{Name: "synthetic.example", Ok: true}}); err != nil {
					t.Fatal(err)
				}
			}
		}
		published := func() float64 {
			metrics := make(chan prometheus.Metric, 256)
			providerEgressTallyBacklogs.Collect(metrics)
			close(metrics)
			value := -1.0
			for metric := range metrics {
				if !strings.Contains(metric.Desc().String(), `"urnetwork_provider_egress_tally_queue_records"`) {
					continue
				}
				var written dto.Metric
				server.Raise(metric.Write(&written))
				for _, label := range written.GetLabel() {
					if label.GetName() == "shard" && label.GetValue() == strconv.Itoa(shard) {
						value = written.GetGauge().GetValue()
					}
				}
			}
			return value
		}
		record(1000)
		first, err := DrainProviderEgressTallies(ctx, shard, generation, "0-0", false)
		if err != nil || first.Records != 1000 {
			t.Fatalf("first drain=%+v err=%v", first, err)
		}
		record(1000)
		// The first call deletes 256 of the 1000 committed records, then the
		// drain's remaining calls delete the rest.
		second, err := DrainProviderEgressTallies(ctx, shard, generation, first.NextCursor, false)
		if err != nil || second.Records != 1000 {
			t.Fatalf("second drain=%+v err=%v", second, err)
		}
		const want = 2000 - providerEgressTallyPageLength
		if second.Backlog.Records != want || published() != want {
			t.Fatalf("drain occupancy=%d published=%.0f; want the claim's %d", second.Backlog.Records, published(), want)
		}
	})
}
