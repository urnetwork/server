package stats

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026/stats/sample"
)

// segments -> flat xz -> read back round-trips, and metrics/diff compute over
// the samples.
func TestFlatRoundTripAndMetrics(t *testing.T) {
	siteHome := t.TempDir()
	t.Setenv("WARP_SITE_HOME", siteHome)
	t.Setenv("WARP_ENV", "local")
	t.Setenv("WARP_SERVICE", "testflat")
	t.Setenv("WARP_HOST", "h")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newStats(ctx, &Config{MaxSegmentAge: time.Hour})
	connect.AssertEqual(t, s.Enabled(), true)

	const count = 50
	for i := 0; i < count; i += 1 {
		clientId := []byte(strings.Repeat("c", 15) + string(rune('0'+i%10)))
		s.Append("findproviders2", &sample.FindProviders2Sample{
			CallerCountryCode: "zz",
			RankMode:          "quality",
			PoolCount:         int32(10 + i),
			LoadMillis:        float32(i),
			EffectiveCount:    20,
			Candidates: []*sample.Candidate{
				{ClientId: clientId, ScaledWeight: float32(i%5) + 1, Tier: int32(i % 3), Score: int32(i), ReliabilityWeight: 0.9, MaxBytesPerSecond: int64(i) * 1_000_000, HasSpeedTest: true, HasLatencyTest: true},
				{ClientId: []byte("other-candidate!"), ScaledWeight: 0.5, Tier: 5, Score: 40},
			},
			ChosenClientIds: [][]byte{clientId},
		})
	}
	s.Close()

	// bulk load via the stream loader
	loaded := 0
	err := s.LoadStream("findproviders2", func(frame []byte) error { loaded += 1; return nil })
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, loaded, count)

	// flatten local segments -> xz
	streamDir := s.StreamDir("findproviders2")
	paths, err := SegmentPaths(streamDir)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(paths) >= 1, true)

	flatPath := filepath.Join(t.TempDir(), "samples.pb.xz")
	written, err := WriteFlatFromSegmentFiles(paths, flatPath)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, written, count)

	// read the flat file back and compute metrics
	metrics := NewMetrics()
	read := 0
	err = ReadFlat(flatPath, func(fp *sample.FindProviders2Sample) error {
		read += 1
		metrics.Add(fp)
		return nil
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, read, count)

	report := metrics.Report()
	connect.AssertEqual(t, report.Samples, int64(count))
	// each call chose one candidate that IS in the pool
	connect.AssertEqual(t, report.ChosenCount, float64(1))
	// selection lift: chosen weight vs pool average (chosen are the i%5+1
	// weighted; pool includes the 0.5 other), so lift should be > 1
	connect.AssertEqual(t, report.SelectionLift > 1.0, true)
	connect.AssertEqual(t, report.ChosenHasSpeedFrac, float64(1))

	// diff renders and JSON serializes
	diff := FormatDiff("official", report, "eval", report)
	connect.AssertEqual(t, strings.Contains(diff, "selection_lift"), true)
	jsonBytes, err := DiffJSON(report, report)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, strings.Contains(string(jsonBytes), "selection_lift"), true)
}
