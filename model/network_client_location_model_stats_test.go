package model

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/stats"
	"github.com/urnetwork/server/stats/sample"
)

// recordFindProviders2Sample writes a faithful, anonymized sample: the pool in
// scaled-weight order (capped, with pool_count preserved) and the chosen ids.
// Runs without a database — the pool is constructed directly.
func TestFindProviders2SampleExport(t *testing.T) {
	siteHome := t.TempDir()
	t.Setenv("WARP_SITE_HOME", siteHome)
	t.Setenv("WARP_ENV", "local") // raw ids, so chosen round-trips to the input
	t.Setenv("WARP_SERVICE", "testfp")
	t.Setenv("WARP_HOST", "testhost")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := stats.Enable(ctx, nil)
	connect.AssertEqual(t, s.Enabled(), true)

	rankMode := RankModeQuality
	clientScores := map[server.Id]*ClientScore{}
	ids := []server.Id{}
	for i := 0; i < 4; i += 1 {
		clientId := server.NewId()
		ids = append(ids, clientId)
		clientScores[clientId] = &ClientScore{
			ClientId:                     clientId,
			NetworkId:                    server.NewId(),
			ScaledWeights:                map[string]float32{rankMode: float32(i + 1)},
			Tiers:                        map[string]int{rankMode: i},
			Scores:                       map[string]int{rankMode: 10 * i},
			ReliabilityWeight:            float64(i),
			IndependentReliabilityWeight: 0.9,
			MaxBytesPerSecond:            ByteCount(1000 * (i + 1)),
			HasSpeedTest:                 true,
		}
	}
	// chosen = the two highest-weight ids
	chosen := []server.Id{ids[3], ids[2]}

	recordFindProviders2Sample(
		s,
		&FindProviders2Args{
			Count:    2,
			RankMode: rankMode,
			Specs: []*ProviderSpec{
				{LocationId: &ids[0]},
			},
		},
		rankMode,
		2,
		"zz",
		12.5,
		clientScores,
		chosen,
	)

	waitForFindProviders2SampleJobs()
	s.Close()

	// find the single segment
	var segmentPath string
	filepath.WalkDir(siteHome, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(path, ".pb.zst") {
			segmentPath = path
		}
		return nil
	})
	if segmentPath == "" {
		t.Fatalf("no segment written under %s", siteHome)
	}

	samples := []*sample.FindProviders2Sample{}
	err := stats.ReadSegment(segmentPath, func(frame []byte) error {
		var fp sample.FindProviders2Sample
		if err := proto.Unmarshal(frame, &fp); err != nil {
			return err
		}
		samples = append(samples, &fp)
		return nil
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(samples), 1)

	fp := samples[0]
	connect.AssertEqual(t, fp.CallerCountryCode, "zz")
	connect.AssertEqual(t, fp.RankMode, rankMode)
	connect.AssertEqual(t, int(fp.PoolCount), 4)
	connect.AssertEqual(t, int(fp.EffectiveCount), 2)
	connect.AssertEqual(t, int(fp.SpecLocationCount), 1)
	connect.AssertEqual(t, len(fp.Candidates), 4)
	// pool is weight-descending: first candidate is the highest weight (i=3)
	connect.AssertEqual(t, fp.Candidates[0].ScaledWeight, float32(4))
	connect.AssertEqual(t, fp.Candidates[3].ScaledWeight, float32(1))
	// raw (local) anonymization: chosen ids round-trip to the input ids
	connect.AssertEqual(t, len(fp.ChosenClientIds), 2)
	connect.AssertEqual(t, fp.ChosenClientIds[0], ids[3].Bytes())
	connect.AssertEqual(t, fp.ChosenClientIds[1], ids[2].Bytes())
}

func TestTopFindProviders2CandidatesDoesNotSortWholePool(t *testing.T) {
	rankMode := RankModeQuality
	clientScores := map[server.Id]*ClientScore{}
	for i := 0; i < 100; i++ {
		clientId := server.NewId()
		clientScores[clientId] = &ClientScore{
			ScaledWeights: map[string]float32{rankMode: float32(i)},
		}
	}
	top := topFindProviders2Candidates(clientScores, rankMode, 5)
	if len(top) != 5 {
		t.Fatalf("retained %d candidates, want 5", len(top))
	}
	for i, want := range []float32{99, 98, 97, 96, 95} {
		if top[i].weight != want {
			t.Fatalf("top[%d].weight = %v, want %v", i, top[i].weight, want)
		}
	}
}
