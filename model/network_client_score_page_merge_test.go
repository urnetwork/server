package model

import (
	"fmt"
	mathrand "math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"
	"unsafe"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A decoded page as the export writes one: maps for every mode, one to four
// lookbacks, location ids on part of the rows. The seed is fixed so a failure
// reproduces.
func clientScorePageMergeTestPage(rng *mathrand.Rand, clientIds []server.Id, countryLocationId server.Id) []*ClientScore {
	page := make([]*ClientScore, 0, len(clientIds))
	for i, clientId := range clientIds {
		validUntil := time.Unix(1_800_000_000+int64(i), 0).UTC()
		score := &ClientScore{
			EgressValidUntil:      &validUntil,
			UrlProbeSuccessWeight: rng.Float64(),
			ClientId:              clientId,
			NetworkId:             server.NewId(),
			Scores:                map[string]int{RankModeQuality: rng.IntN(100), RankModeSpeed: rng.IntN(100), RankModeOnline: rng.IntN(100)},
			ReliabilityWeight:     rng.Float64(),
			Tiers:                 map[string]int{RankModeQuality: rng.IntN(3), RankModeSpeed: rng.IntN(3)},
			MaxBytesPerSecond:     ByteCount(rng.IntN(100)) * Mib,
			HasSpeedTest:          rng.IntN(2) == 0,
			IpFamilies:            uint8(1 + rng.IntN(3)),
			CapabilityDowngrades:  uint8(rng.IntN(4)) & uint8(rng.IntN(4)),
			ScaledWeights:         map[string]float32{RankModeQuality: rng.Float32()},
			PassesMinimums:        map[string]bool{RankModeQuality: rng.IntN(2) == 0},
		}
		if rng.IntN(3) != 0 {
			countryId := countryLocationId
			score.CountryLocationId = &countryId
		}
		score.LookbackClientScores = map[int]*ClientScore{}
		for lookback := range 1 + rng.IntN(4) {
			score.LookbackClientScores[lookback] = &ClientScore{
				ClientId:      clientId,
				Scores:        map[string]int{RankModeQuality: rng.IntN(100)},
				Tiers:         map[string]int{RankModeQuality: rng.IntN(3)},
				LookbackIndex: lookback,
			}
		}
		page = append(page, score)
	}
	return page
}

// The read as it was: copy the whole page, filter the copies, then merge.
func clientScorePageMergeTestCopyThenFilter(scores map[server.Id]*ClientScore, page []*ClientScore, filter func(*ClientScore) bool) {
	sample := copyClientScorePage(page)
	if filter != nil {
		sample = slices.DeleteFunc(sample, func(score *ClientScore) bool {
			return !filter(score)
		})
	}
	mergeClientScoreSample(scores, sample)
}

func clientScorePageMergeTestFilters() map[string]func(*ClientScore) bool {
	return map[string]func(*ClientScore) bool{
		"none": nil,
		"not_downgraded": func(score *ClientScore) bool {
			return score.CapabilityDowngrades == 0
		},
		"downgraded_dualstack": func(score *ClientScore) bool {
			return score.CapabilityDowngrades != 0 && score.IpFamily() == connect.IpFamilyDualstack
		},
		"drops_all": func(score *ClientScore) bool {
			return false
		},
	}
}

// Merging only the kept records answers exactly what copying the page,
// filtering the copies and merging them answered: the same providers, the
// same location-bearing choice when pages overlap, and records equal to the
// page's. Every merged record is the reader's own, and the shared pages are
// unchanged.
func TestClientScorePageMergeMatchesCopyThenFilter(t *testing.T) {
	// deterministic so a failing case reproduces (CODESTYLE: tests may seed)
	rng := mathrand.New(mathrand.NewPCG(11, 22))
	for round := range 40 {
		pool := make([]server.Id, 300)
		for i := range pool {
			pool[i] = server.NewId()
		}
		// overlapping pages: the same provider in several pages, with and
		// without a location id
		pages := [][]*ClientScore{}
		for range 1 + rng.IntN(6) {
			clientIds := []server.Id{}
			seen := map[server.Id]bool{}
			for range rng.IntN(ClientScoreSampleCount + 1) {
				clientId := pool[rng.IntN(len(pool))]
				if !seen[clientId] {
					seen[clientId] = true
					clientIds = append(clientIds, clientId)
				}
			}
			pages = append(pages, clientScorePageMergeTestPage(rng, clientIds, server.NewId()))
		}
		snapshots := make([][]ClientScore, len(pages))
		for i, page := range pages {
			for _, score := range page {
				snapshots[i] = append(snapshots[i], *score)
			}
		}
		for name, filter := range clientScorePageMergeTestFilters() {
			want := map[server.Id]*ClientScore{}
			got := map[server.Id]*ClientScore{}
			for _, page := range pages {
				clientScorePageMergeTestCopyThenFilter(want, page, filter)
				mergeClientScorePage(got, page, filter)
			}
			if len(got) != len(want) {
				t.Fatalf("round %d filter %s: merged %d providers, want %d", round, name, len(got), len(want))
			}
			for clientId, wantScore := range want {
				gotScore := got[clientId]
				if gotScore == nil || !reflect.DeepEqual(*gotScore, *wantScore) {
					t.Fatalf("round %d filter %s: provider %s merged %v, want %v", round, name, clientId, gotScore, wantScore)
				}
				for _, page := range pages {
					for _, shared := range page {
						if shared == gotScore {
							t.Fatalf("round %d filter %s: provider %s merged the shared record, not a copy", round, name, clientId)
						}
					}
				}
			}
		}
		for i, page := range pages {
			for j, score := range page {
				if !reflect.DeepEqual(*score, snapshots[i][j]) {
					t.Fatalf("round %d: page %d record %d changed", round, i, j)
				}
			}
		}
	}
}

// A page whose records the filter all drops costs the read no allocation; a
// page of which it keeps a tenth costs about a tenth of a whole-page copy.
// Copying the page first allocated a copy of every record either way.
func TestClientScorePageMergeCopiesOnlyKeptRecords(t *testing.T) {
	rng := mathrand.New(mathrand.NewPCG(33, 44))
	clientIds := make([]server.Id, ClientScoreSampleCount)
	for i := range clientIds {
		clientIds[i] = server.NewId()
	}
	page := clientScorePageMergeTestPage(rng, clientIds, server.NewId())
	scores := make(map[server.Id]*ClientScore, len(page))

	if allocs := testing.AllocsPerRun(100, func() {
		mergeClientScorePage(scores, page, func(*ClientScore) bool { return false })
	}); allocs != 0 {
		t.Fatalf("a page the filter drops entirely allocated %.1f times per read, want 0", allocs)
	}

	keepTenth := func(score *ClientScore) bool {
		return score.ClientId[15]%10 == 0
	}
	keepCount := 0
	for _, score := range page {
		if keepTenth(score) {
			keepCount += 1
		}
	}
	if keepCount == 0 || len(page)/4 < keepCount {
		t.Fatalf("fixture keeps %d of %d", keepCount, len(page))
	}
	// warm the map so it does not grow inside the measurement
	mergeClientScorePage(scores, page, keepTenth)
	const reads = 200
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range reads {
		mergeClientScorePage(scores, page, keepTenth)
	}
	runtime.ReadMemStats(&after)
	perRead := float64(after.TotalAlloc-before.TotalAlloc) / reads
	recordBytes := float64(unsafe.Sizeof(ClientScore{}))
	if limit := 2 * float64(keepCount) * recordBytes; limit < perRead {
		t.Fatalf("a read keeping %d of %d records allocated %.0f bytes, want under %.0f (a whole-page copy is %.0f)", keepCount, len(page), perRead, limit, float64(len(page))*recordBytes)
	}
}

// B/op of one page read before and after copying only kept records, at the
// share of records a page's filter keeps.
func BenchmarkClientScorePageMerge(b *testing.B) {
	rng := mathrand.New(mathrand.NewPCG(55, 66))
	clientIds := make([]server.Id, ClientScoreSampleCount)
	for i := range clientIds {
		clientIds[i] = server.NewId()
	}
	page := clientScorePageMergeTestPage(rng, clientIds, server.NewId())
	for _, keepPercent := range []int{100, 50, 10} {
		filter := func(score *ClientScore) bool {
			return int(score.ClientId[15])%100 < keepPercent
		}
		for _, path := range []struct {
			name  string
			merge func(map[server.Id]*ClientScore, []*ClientScore, func(*ClientScore) bool)
		}{
			{name: "copy_then_filter", merge: clientScorePageMergeTestCopyThenFilter},
			{name: "merge_kept", merge: mergeClientScorePage},
		} {
			b.Run(fmt.Sprintf("keep%d/%s", keepPercent, path.name), func(b *testing.B) {
				scores := make(map[server.Id]*ClientScore, len(page))
				b.ReportAllocs()
				for b.Loop() {
					path.merge(scores, page, filter)
				}
			})
		}
	}
}
