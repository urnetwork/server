package model

import (
	"context"
	"errors"
	"maps"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// One-slot PostgreSQL contention exercises the real selector, native/legacy
// Redis pages, current subscriber SQL and concurrent positive rechecks. The
// refused rows are verified subscribers too, so a negative cache cannot mask
// unnecessary database work on the baseline. Timing is evidence, not a gate.
func TestFindProviders2PrefilterBoundsSubscriberReadsUnderContention(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		enableSubscriberQualityPolicy(t)
		nativeTestEnableReader(t)
		ctx := t.Context()
		previousCache := providerSubscriberNegativeCache
		defer func() { providerSubscriberNegativeCache = previousCache }()
		for _, source := range []string{"primary", "alternate", "online"} {
			location, callerNetwork := server.NewId(), server.NewId()
			args := &FindProviders2Args{Specs: []*ProviderSpec{{LocationId: &location}}}
			mode := RankModeQuality
			if source == "alternate" {
				mode = RankModeSpeed
			}
			scores := map[ipFamilyFacet][]*ClientScore{}
			rejected := 2800
			if source == "online" {
				rejected = 2400
			}
			for range rejected {
				score := nativeTestScore(mode, ipFamilyFacetDualstack)
				if source == "online" {
					score.PassesMinimums = nil
					args.ExcludeDestinations = append(args.ExcludeDestinations, []server.Id{score.ClientId})
				} else {
					score.NetworkOnly = true
				}
				scores[ipFamilyFacetDualstack] = append(scores[ipFamilyFacetDualstack], score)
			}
			allowed := map[server.Id]bool{}
			for range 20 {
				score := nativeTestScore(mode, ipFamilyFacetV4Only)
				if source == "online" {
					score.PassesMinimums = nil
				}
				// Private candidates in the caller's own network remain eligible.
				score.NetworkOnly, score.NetworkId = true, callerNetwork
				allowed[score.ClientId] = true
				scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], score)
			}
			hard, unknown := nativeTestScore(mode, ipFamilyFacetV4Only), nativeTestScore(mode, ipFamilyFacetV4Only)
			if source == "online" {
				hard.PassesMinimums, unknown.PassesMinimums = nil, nil
			}
			scores[ipFamilyFacetV4Only] = append(scores[ipFamilyFacetV4Only], hard, unknown)
			for _, publishedMode := range []RankMode{RankModeQuality, RankModeSpeed} {
				nativeTestPublishLocation(t, location, publishedMode, scores)
			}
			// Make a new current security refusal and revoke subscriber evidence
			// after publication. Neither may escape through the cheaper filters.
			server.Redis(ctx, func(r server.RedisClient) {
				server.Raise(r.SAdd(ctx, providerHardExclusionsKey, hard.ClientId.String()).Err())
			})
			server.Tx(ctx, func(tx server.PgTx) {
				server.RaisePgResult(tx.Exec(ctx, `DELETE FROM network_client_location WHERE connection_id=$1`, unknown.ClientId))
			})
			if source != "primary" {
				// Subscriber evidence is required by native Quality only. The
				// same unknown provider remains eligible as Speed or Online.
				allowed[unknown.ClientId] = true
				args.Count, args.ForceCount = len(allowed), true
			}
			providerSubscriberNegativeCache = newSubscriberNegativeCache(subscriberNegativeCapacity, time.Now)
			var candidateReads atomic.Int64
			providerSubscriberNegativeCache.observe = func(event string, count int) {
				if event == "negative_miss" {
					candidateReads.Add(int64(count))
				}
			}
			beforeBatches := testutil.ToFloat64(subscriberEligibilityEventCounters["sql_batch"])
			const requests = 8
			started := time.Now()
			withNetworkUserSingleConnection(t, func() {
				requestCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				defer cancel()
				start := make(chan struct{})
				finished := make(chan error, requests)
				for range requests {
					go func() {
						<-start
						var resultErr error
						if panicErr := server.HandleError(func() {
							clientSession := testingCreateProviderSearchSession(requestCtx, session.NewByJwt(callerNetwork, server.NewId(), "prefilter-contention-test", false, false))
							result, err := FindProviders2(args, clientSession)
							if err != nil || result == nil || len(result.Providers) != len(allowed) {
								resultErr = errors.New("request lost the complete allowed population")
								return
							}
							wantTier := 0
							if source == "alternate" {
								wantTier = egressTestBackfillOffset()
							} else if source == "online" {
								wantTier = 2 * egressTestBackfillOffset()
							}
							seen := map[server.Id]bool{}
							for _, provider := range result.Providers {
								if !allowed[provider.ClientId] || seen[provider.ClientId] || provider.Tier != wantTier {
									resultErr = errors.New("request widened eligibility, repeated a provider or changed native priority")
									return
								}
								seen[provider.ClientId] = true
							}
						}); panicErr != nil {
							resultErr = errors.New("selector panicked under single-slot contention")
						}
						finished <- resultErr
					}()
				}
				close(start)
				var requestErrors error
				for range requests {
					if err := <-finished; err != nil {
						requestErrors = errors.Join(requestErrors, err)
					}
				}
				if requestErrors != nil {
					t.Fatalf("%s: %v", source, requestErrors)
				}
			})
			batches := testutil.ToFloat64(subscriberEligibilityEventCounters["sql_batch"]) - beforeBatches
			t.Logf("prefilter_contention source=%s concurrent_requests=%d pg_slots=1 cached_refusals=%d sql_batches=%g subscriber_candidate_reads=%d elapsed_ms=%.3f", source, requests, rejected, batches, candidateReads.Load(), float64(time.Since(started))/float64(time.Millisecond))
			minBatches, maxBatches, maxCandidateReads := float64(0), float64(0), int64(0)
			if source == "primary" {
				// The initial weighted draw validates only the twenty needed
				// candidates. Its single subscriber refusal can require one
				// replacement draw. The negative-only single-flight cache can
				// split each draw into owned and fresh positive-follower reads;
				// the 2,800 request refusals still cost no candidate reads.
				minBatches, maxBatches, maxCandidateReads = requests, 4*requests, requests*21
			}
			if batches < minBatches || maxBatches < batches || candidateReads.Load() > maxCandidateReads {
				t.Fatalf("%s request-only refusals consumed subscriber SQL: batches=%g candidates=%d; want %g..%g batches and at most %d candidates", source, batches, candidateReads.Load(), minBatches, maxBatches, maxCandidateReads)
			}
		}
	})
}

// Family rejection can still be needed for legacy pages. The same predicate
// drives both read admission and result filtering, including overlap priority.
func TestFindProviders2PrefilterPreservesRequestFilterBoundaries(t *testing.T) {
	caller := server.NewId()
	filter := clientScoreRequestFilter{callerNetworkId: caller, facets: []ipFamilyFacet{ipFamilyFacetDualstack, ipFamilyFacetV4Only}, excludedClientIds: map[server.Id]uint8{}}
	scores := map[server.Id]*ClientScore{}
	add := func(change func(*ClientScore)) *ClientScore {
		score := onlineBackfillScore(true, 1)
		change(score)
		scores[score.ClientId] = score
		return score
	}
	public := add(func(*ClientScore) {})
	private := add(func(score *ClientScore) { score.NetworkOnly, score.NetworkId = true, caller })
	add(func(score *ClientScore) { score.NetworkOnly = true })
	add(func(score *ClientScore) { score.IpFamilies = ClientScoreIpFamilyV6 })
	explicit := add(func(*ClientScore) {})
	filter.excludedClientIds[explicit.ClientId] = 3
	overlap := add(func(score *ClientScore) { score.NetworkOnly, score.IpFamilies = true, ClientScoreIpFamilyV6 })
	filter.excludedClientIds[overlap.ClientId] = 1
	unchecked := filter.unchecked(scores, map[server.Id]bool{public.ClientId: true})
	if len(unchecked) != 1 || unchecked[0] != private.ClientId || len(scores) != 6 {
		t.Fatal("read prefilter discarded an own-network candidate, reread checked facts or mutated source samples")
	}
	observation := &findProviders2SelectionObservation{}
	filtered := maps.Clone(scores)
	filter.apply(filtered, observation)
	if len(filtered) != 2 || filtered[public.ClientId] == nil || filtered[private.ClientId] == nil || observation.dropped != [4]int{0, 2, 1, 1} || observation.explicitSources != 3 {
		t.Fatal("request prefilter changed eligibility or existing request-rejection precedence")
	}
}
