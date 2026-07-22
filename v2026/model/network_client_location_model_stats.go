package model

// FindProviders2 stats sample export.
//
// recordFindProviders2Sample captures one anonymized sample per call: the
// loaded candidate pool (after redis shard sub-sampling, with the weights the
// selection actually used) in scaled-weight order, and the chosen client ids.
// Together with the call args this replays the sampling, sorting, and
// selection so the matchmaking can be traced end to end (SIM-LATENCY.md goal
// 9).
//
// It is best-effort and off the hot path: it runs only when the process has
// stats enabled (disabled by default), the candidate list is capped, and the
// append never blocks or fails the search (stats.Append drops on a full
// queue). Ids are anonymized so the exported stream cannot be matched to
// production ids while staying traceable across samples.

import (
	"container/heap"
	mathrand "math/rand"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/stats"
	"github.com/urnetwork/server/v2026/stats/sample"
)

// findProviders2StatsStream is the stats stream for FindProviders2 samples. Its
// default retention is defined centrally in the stats package (see
// stats.StreamFindProviders2 / defaultStreamTTLs).
const findProviders2StatsStream = stats.StreamFindProviders2

// tunables read once from settings. Editing settings.yml + restarting the
// process changes them (no code deploy). Absent keys take the defaults.
var findProviders2SampleFraction = sync.OnceValue(func() float64 {
	fallback := 0.01
	if env, _ := server.Env(); env == "local" {
		fallback = 1.0
	}
	return min(1.0, max(0.0, settingsFloat("stats_findproviders2_sample_fraction", fallback)))
})
var findProviders2MaxCandidates = sync.OnceValue(func() int {
	fallback := 256
	if env, _ := server.Env(); env == "local" {
		fallback = findProviders2HardMaxCandidates
	}
	return min(
		findProviders2HardMaxCandidates,
		max(0, int(settingsFloat("stats_findproviders2_max_candidates", float64(fallback)))),
	)
})

const (
	findProviders2HardMaxCandidates = 2000
	findProviders2JobQueueSize      = 64
	findProviders2JobQueueMaxBytes  = 16 * 1024 * 1024
)

func settingsFloat(key string, fallback float64) float64 {
	v, ok := server.GetSettings()[key]
	if !ok {
		return fallback
	}
	switch f := v.(type) {
	case float64:
		return f
	case int:
		return float64(f)
	}
	return fallback
}

type findProviders2CandidateSnapshot struct {
	clientId                     server.Id
	networkId                    server.Id
	scaledWeight                 float32
	tier                         int
	score                        int
	reliabilityWeight            float64
	independentReliabilityWeight float64
	minRelativeLatencyMillis     int
	maxBytesPerSecond            ByteCount
	hasLatencyTest               bool
	hasSpeedTest                 bool
}

type weightedCandidate struct {
	clientId server.Id
	weight   float32
}

// weightedCandidateHeap keeps the least desirable retained candidate at the
// root, allowing O(N log K) top-K selection without sorting the full pool.
type weightedCandidateHeap []weightedCandidate

func (self weightedCandidateHeap) Len() int { return len(self) }
func (self weightedCandidateHeap) Less(i int, j int) bool {
	if self[i].weight != self[j].weight {
		return self[i].weight < self[j].weight
	}
	return 0 < self[i].clientId.Cmp(self[j].clientId)
}
func (self weightedCandidateHeap) Swap(i int, j int) { self[i], self[j] = self[j], self[i] }
func (self *weightedCandidateHeap) Push(value any) {
	*self = append(*self, value.(weightedCandidate))
}
func (self *weightedCandidateHeap) Pop() any {
	old := *self
	n := len(old)
	value := old[n-1]
	*self = old[:n-1]
	return value
}

func betterWeightedCandidate(a weightedCandidate, b weightedCandidate) bool {
	if a.weight != b.weight {
		return b.weight < a.weight
	}
	return a.clientId.Cmp(b.clientId) < 0
}

func topFindProviders2Candidates(
	clientScores map[server.Id]*ClientScore,
	rankMode RankMode,
	limit int,
) []weightedCandidate {
	if limit <= 0 {
		return nil
	}
	retained := &weightedCandidateHeap{}
	heap.Init(retained)
	for clientId, clientScore := range clientScores {
		candidate := weightedCandidate{
			clientId: clientId,
			weight:   clientScore.ScaledWeights[rankMode],
		}
		if retained.Len() < limit {
			heap.Push(retained, candidate)
		} else if betterWeightedCandidate(candidate, (*retained)[0]) {
			(*retained)[0] = candidate
			heap.Fix(retained, 0)
		}
	}
	result := slices.Clone(*retained)
	slices.SortFunc(result, func(a weightedCandidate, b weightedCandidate) int {
		if a.weight < b.weight {
			return 1
		}
		if b.weight < a.weight {
			return -1
		}
		return a.clientId.Cmp(b.clientId)
	})
	return result
}

type findProviders2SampleJob struct {
	stats                   *stats.Stats
	timeUnixMilli           uint64
	callerCountryCode       string
	rankMode                RankMode
	forceMinimum            bool
	forceCount              bool
	requestedCount          int
	effectiveCount          int
	specLocationCount       int
	specLocationGroupCount  int
	specClientCount         int
	specBestAvailable       bool
	excludeClientCount      int
	excludeDestinationCount int
	loadMillis              float64
	poolCount               int
	candidates              []findProviders2CandidateSnapshot
	chosenClientIds         []server.Id
	estimatedBytes          int64
}

var findProviders2SampleJobs = make(chan *findProviders2SampleJob, findProviders2JobQueueSize)
var findProviders2SampleWorkerOnce sync.Once
var findProviders2SampleJobBytes atomic.Int64
var findProviders2SampleJobWg sync.WaitGroup

// sample jobs dropped because the queue was full (count or byte budget).
// Drops are by design under pressure — the counter makes them visible.
var findProviders2SampleDrops = prometheus.NewCounter(prometheus.CounterOpts{
	Name: "urnetwork_findproviders2_sample_drops_total",
	Help: "FindProviders2 sample jobs dropped at the queue (count or byte budget)",
})

func init() {
	prometheus.MustRegister(findProviders2SampleDrops)
}

func enqueueFindProviders2Sample(job *findProviders2SampleJob) {
	for {
		current := findProviders2SampleJobBytes.Load()
		if findProviders2JobQueueMaxBytes-current < job.estimatedBytes {
			findProviders2SampleDrops.Inc()
			return
		}
		if findProviders2SampleJobBytes.CompareAndSwap(current, current+job.estimatedBytes) {
			break
		}
	}

	findProviders2SampleWorkerOnce.Do(func() {
		go server.HandleError(func() {
			for job := range findProviders2SampleJobs {
				// contain a panic to the job: the worker loop must survive
				// (an exited worker would strand queued jobs and deadlock
				// the test drain), and a sample must never crash the api
				server.HandleError(func() {
					appendFindProviders2Sample(job)
				})
				findProviders2SampleJobBytes.Add(-job.estimatedBytes)
				findProviders2SampleJobWg.Done()
			}
		})
	})

	findProviders2SampleJobWg.Add(1)
	select {
	case findProviders2SampleJobs <- job:
	default:
		findProviders2SampleJobWg.Done()
		findProviders2SampleJobBytes.Add(-job.estimatedBytes)
		findProviders2SampleDrops.Inc()
	}
}

func appendFindProviders2Sample(job *findProviders2SampleJob) {
	candidates := make([]*sample.Candidate, 0, len(job.candidates))
	for _, candidate := range job.candidates {
		candidates = append(candidates, &sample.Candidate{
			ClientId:                     job.stats.Anonymize(candidate.clientId),
			NetworkId:                    job.stats.Anonymize(candidate.networkId),
			ScaledWeight:                 candidate.scaledWeight,
			Tier:                         int32(candidate.tier),
			Score:                        int32(candidate.score),
			ReliabilityWeight:            candidate.reliabilityWeight,
			IndependentReliabilityWeight: candidate.independentReliabilityWeight,
			MinRelativeLatencyMillis:     int32(candidate.minRelativeLatencyMillis),
			MaxBytesPerSecond:            int64(candidate.maxBytesPerSecond),
			HasLatencyTest:               candidate.hasLatencyTest,
			HasSpeedTest:                 candidate.hasSpeedTest,
		})
	}
	chosen := make([][]byte, 0, len(job.chosenClientIds))
	for _, clientId := range job.chosenClientIds {
		chosen = append(chosen, job.stats.Anonymize(clientId))
	}
	job.stats.Append(findProviders2StatsStream, &sample.FindProviders2Sample{
		TimeUnixMilli:           job.timeUnixMilli,
		CallerCountryCode:       job.callerCountryCode,
		RankMode:                job.rankMode,
		ForceMinimum:            job.forceMinimum,
		ForceCount:              job.forceCount,
		RequestedCount:          int32(job.requestedCount),
		EffectiveCount:          int32(job.effectiveCount),
		SpecLocationCount:       int32(job.specLocationCount),
		SpecLocationGroupCount:  int32(job.specLocationGroupCount),
		SpecClientCount:         int32(job.specClientCount),
		SpecBestAvailable:       job.specBestAvailable,
		ExcludeClientCount:      int32(job.excludeClientCount),
		ExcludeDestinationCount: int32(job.excludeDestinationCount),
		LoadMillis:              float32(job.loadMillis),
		PoolCount:               int32(job.poolCount),
		Candidates:              candidates,
		ChosenClientIds:         chosen,
	})
}

func waitForFindProviders2SampleJobs() {
	findProviders2SampleJobWg.Wait()
}

// recordFindProviders2Sample builds and appends a sample. clientScores is the
// loaded pool with final weights; chosenClientIds is the selected+banded
// result order.
func recordFindProviders2Sample(
	s *stats.Stats,
	findProviders2 *FindProviders2Args,
	rankMode RankMode,
	count int,
	callerCountryCode string,
	loadMillis float64,
	clientScores map[server.Id]*ClientScore,
	chosenClientIds []server.Id,
) {
	if fraction := findProviders2SampleFraction(); fraction < 1.0 {
		if fraction <= 0 || fraction < mathrand.Float64() {
			return
		}
	}

	poolCount := len(clientScores)
	maxCandidates := findProviders2MaxCandidates()
	retained := topFindProviders2Candidates(clientScores, rankMode, maxCandidates)
	candidates := make([]findProviders2CandidateSnapshot, 0, len(retained))
	for _, retainedCandidate := range retained {
		clientScore := clientScores[retainedCandidate.clientId]
		candidates = append(candidates, findProviders2CandidateSnapshot{
			clientId:                     retainedCandidate.clientId,
			networkId:                    clientScore.NetworkId,
			scaledWeight:                 retainedCandidate.weight,
			tier:                         clientScore.Tiers[rankMode],
			score:                        clientScore.Scores[rankMode],
			reliabilityWeight:            clientScore.ReliabilityWeight,
			independentReliabilityWeight: clientScore.IndependentReliabilityWeight,
			minRelativeLatencyMillis:     clientScore.MinRelativeLatencyMillis,
			maxBytesPerSecond:            clientScore.MaxBytesPerSecond,
			hasLatencyTest:               clientScore.HasLatencyTest,
			hasSpeedTest:                 clientScore.HasSpeedTest,
		})
	}

	locationCount := 0
	locationGroupCount := 0
	specClientCount := 0
	bestAvailable := false
	for _, spec := range findProviders2.Specs {
		if spec.LocationId != nil {
			locationCount += 1
		}
		if spec.LocationGroupId != nil {
			locationGroupCount += 1
		}
		if spec.ClientId != nil {
			specClientCount += 1
		}
		if spec.BestAvailable {
			bestAvailable = true
		}
	}

	chosen := slices.Clone(chosenClientIds)
	enqueueFindProviders2Sample(&findProviders2SampleJob{
		stats:                   s,
		timeUnixMilli:           uint64(server.NowUtc().UnixMilli()),
		callerCountryCode:       callerCountryCode,
		rankMode:                rankMode,
		forceMinimum:            findProviders2.ForceMinimum,
		forceCount:              findProviders2.ForceCount,
		requestedCount:          findProviders2.Count,
		effectiveCount:          count,
		specLocationCount:       locationCount,
		specLocationGroupCount:  locationGroupCount,
		specClientCount:         specClientCount,
		specBestAvailable:       bestAvailable,
		excludeClientCount:      len(findProviders2.ExcludeClientIds),
		excludeDestinationCount: len(findProviders2.ExcludeDestinations),
		loadMillis:              loadMillis,
		poolCount:               poolCount,
		candidates:              candidates,
		chosenClientIds:         chosen,
		estimatedBytes:          512 + int64(len(candidates))*128 + int64(len(chosen))*16,
	})
}
