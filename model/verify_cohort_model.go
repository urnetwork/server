package model

// verify_cohort_model.go — the bounded next-hop cohort used by
// SampleVerifyNextHop when VerifySettings.CohortSize is positive.
//
// Why: an SN25 validator keeps a census of every provider it was assigned in
// the current settlement epoch, every provider it has ever given a quality
// score, and every provider whose egress hash it recorded in the current
// native epoch. Past its signed bound (max_providers) every measurement and
// settlement transition fails hard. Quality scores never expire: at each fold
// an idle provider's prior EMA carries forward unchanged, so the carried part
// of the census is every provider the validator lineage has ever scored.
// Uniform sampling over all eligible providers overflows that census within
// minutes and keeps it overflowed.
//
// Two hard caps are enforced by one atomic Redis script, so any number of API
// processes and hosts sharing Redis admit at most:
//   - CohortSize distinct providers per settlement epoch. Only an admitted
//     provider is ever returned as a next hop.
//   - CohortLifetimeLimit distinct providers over the deployment's life. Every
//     provider a validator can carry was once admitted, so its census cannot
//     exceed this limit however epochs, restarts or boundary timing interleave.
//     (For the same reason the verify controller never names a synthetic next
//     hop under a cohort: a validator would count it as a provider too.)
//     Nothing expires the lifetime set. Delete it only when the validator's
//     census starts over (a new lineage) or the validator bounds it itself.
//
// The epoch is the coordinator's settlement epoch exactly as the st sync task
// mirrors it (StSyncChainState, refreshed every few seconds from
// currentEpoch() at the finalized head). There is no time-based estimate. A
// boundary seen slightly earlier or later than a validator sees it moves a few
// assignments between adjacent cohorts, which are inside the lifetime set.
//
// Within the caps, providers already in the lifetime set (carried scores, last
// epoch's cohort among them) are re-admitted first; they cost no lifetime
// budget. A new provider is admitted only after the cohort has averaged
// ReliabilityAMin draws this epoch, so early members reach a_min quickly
// rather than every member waiting for the whole cohort to fill, and after a
// rollover carried members that reconnect late still find room before new
// providers take it. A trail that cannot find an unused member may admit past
// that gate, never past a cap.
//
// Nothing here falls back to unbounded sampling: an unknown epoch, a refused
// admission or no usable member yields no next hop, and a Redis error panics
// like every other verify state read.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// Epoch keys are refreshed by every draw, so this only bounds the lifetime of
// a cohort nobody draws from any more. It must exceed one settlement epoch.
const verifyCohortEpochTtl = 30 * 24 * time.Hour

// Carried members are drawn from the lifetime set, which also holds providers
// that went offline or were already re-admitted. A wider draw than the
// eligible-set sample keeps "carried first" effective as that set fills.
const verifyCohortCarryDrawCount = 64

// All keys share one hash tag, so the admission script stays in one cluster
// slot. The deployment key keeps a redeployed coordinator, whose epochs
// restart at zero, from inheriting another deployment's cohorts.
type verifyCohortKeys struct {
	epoch    string
	draws    string
	lifetime string
}

func newVerifyCohortKeys(deploymentKey StDeploymentKey, epoch uint64) verifyCohortKeys {
	prefix := fmt.Sprintf("{verify_cohort:%s}", deploymentKey)
	return verifyCohortKeys{
		epoch:    fmt.Sprintf("%se%d", prefix, epoch),
		draws:    fmt.Sprintf("%se%ddraws", prefix, epoch),
		lifetime: prefix + "lifetime",
	}
}

// verifyCohortAdmitScript admits ARGV[1] into the epoch cohort (or confirms it
// is already a member) and counts one draw. Both caps are checked and applied
// in one script, so concurrent admissions can never overshoot either. Every
// read, including the draw counter's integer check, comes before the first
// write: Redis does not roll back a script that fails part way, so corrupt
// state must fail it before it changes anything.
//
//	KEYS[1] epoch cohort set, KEYS[2] epoch draw counter, KEYS[3] lifetime set
//	ARGV[1] candidate, ARGV[2] cohort size, ARGV[3] lifetime limit,
//	ARGV[4] a_min (growth gate), ARGV[5] '1' to skip the growth gate (never a
//	cap), ARGV[6] epoch key ttl seconds, ARGV[7..] excluded ids (for n)
//
// Returns {1, n} when the candidate may be assigned, n being the cohort size
// net of excluded members, or {0, size} when it is refused.
const verifyCohortAdmitScript = `
local member = redis.call('SISMEMBER', KEYS[1], ARGV[1]) == 1
local size = redis.call('SCARD', KEYS[1])
local carried = redis.call('SISMEMBER', KEYS[3], ARGV[1]) == 1
local lifetime = redis.call('SCARD', KEYS[3])
local raw = redis.call('GET', KEYS[2])
if raw and not string.match(raw, '^%d+$') then
  return redis.error_reply('verify cohort draw counter is not an integer')
end
local draws = tonumber(raw or '0')
if not member then
  if size >= tonumber(ARGV[2]) then return {0, size} end
  if not carried then
    if lifetime >= tonumber(ARGV[3]) then return {0, size} end
    if ARGV[5] ~= '1' and draws < tonumber(ARGV[4]) * size then return {0, size} end
    redis.call('SADD', KEYS[3], ARGV[1])
  end
  redis.call('SADD', KEYS[1], ARGV[1])
end
redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[1], ARGV[6])
redis.call('EXPIRE', KEYS[2], ARGV[6])
local n = redis.call('SCARD', KEYS[1])
for i = 7, #ARGV do
  if redis.call('SISMEMBER', KEYS[1], ARGV[i]) == 1 then n = n - 1 end
end
return {1, n}
`

// admitVerifyCohort runs the admission script for one candidate. Redis
// errors panic; a refusal is an ordinary false.
func admitVerifyCohort(
	ctx context.Context,
	r server.RedisClient,
	keys verifyCohortKeys,
	candidate string,
	settings *VerifySettings,
	skipGrowthGate bool,
	excluded []string,
) (admitted bool, n int) {
	skip := "0"
	if skipGrowthGate {
		skip = "1"
	}
	args := []any{
		candidate,
		settings.CohortSize,
		settings.CohortLifetimeLimit,
		settings.ReliabilityAMin,
		skip,
		int64(verifyCohortEpochTtl / time.Second),
	}
	for _, id := range excluded {
		args = append(args, id)
	}
	values, err := r.Eval(ctx, verifyCohortAdmitScript, []string{keys.epoch, keys.draws, keys.lifetime}, args...).Int64Slice()
	server.Raise(err)
	if len(values) != 2 {
		panic(fmt.Errorf("verify cohort admission returned %d values", len(values)))
	}
	return values[0] == 1, int(values[1])
}

// verifyCohortEpoch is the coordinator settlement epoch from the st sync
// task's Redis mirror. A missing deployment or an expired mirror (the sync
// task has stalled for its ttl) is unknown, and an unknown epoch admits nobody.
func verifyCohortEpoch(ctx context.Context, deploymentKey StDeploymentKey) (uint64, bool) {
	if deploymentKey == "" {
		return 0, false
	}
	summary := GetStEpochSummaryCache(ctx, deploymentKey)
	if summary == nil {
		return 0, false
	}
	return summary.Epoch, true
}

// drawVerifyCohortCandidate draws up to count members of source and returns
// one at random that is not excluded, is currently in the eligible set, and,
// when notIn is set, is not a member of notIn. Cohort sets keep members that
// disconnect, so the eligible-set check keeps such members from using up the
// sampler's attempts. Returns "" when no member qualifies.
func drawVerifyCohortCandidate(
	ctx context.Context,
	r server.RedisClient,
	source string,
	count int,
	notIn string,
	excluded map[string]bool,
) string {
	members, err := r.SRandMemberN(ctx, source, int64(count)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		server.Raise(err)
	}
	candidates := []string{}
	for _, member := range members {
		if !excluded[member] {
			candidates = append(candidates, member)
		}
	}
	keep := func(key string, want bool) {
		if len(candidates) == 0 {
			return
		}
		args := make([]any, len(candidates))
		for i, candidate := range candidates {
			args[i] = candidate
		}
		in, err := r.SMIsMember(ctx, key, args...).Result()
		server.Raise(err)
		kept := []string{}
		for i, candidate := range candidates {
			if in[i] == want {
				kept = append(kept, candidate)
			}
		}
		candidates = kept
	}
	if source != verifyEligibleKey {
		keep(verifyEligibleKey, true)
	}
	if notIn != "" {
		keep(notIn, false)
	}
	if len(candidates) == 0 {
		return ""
	}
	pickIndex, err := rand.Int(rand.Reader, big.NewInt(int64(len(candidates))))
	server.Raise(err)
	return candidates[pickIndex.Int64()]
}

// sampleVerifyCohortNextHop is SampleVerifyNextHop restricted to the epoch
// cohort. Each attempt draws, in order of preference:
//  1. while the cohort has room, a provider from the lifetime set that is not
//     yet in this epoch's cohort (carried scores, including last epoch's
//     cohort);
//  2. while the cohort and the lifetime set have room and the growth gate is
//     open (or the previous attempt found no usable member), a provider from
//     the eligible set;
//  3. otherwise an existing member of this epoch's cohort.
//
// The admission script then decides; a refused candidate is excluded and the
// next attempt re-reads the cohort.
func sampleVerifyCohortNextHop(
	ctx context.Context,
	excludeClientIds []server.Id,
	settings *VerifySettings,
) (nextHop *server.Id, n int) {
	epoch, ok := verifyCohortEpoch(ctx, settings.CohortDeploymentKey)
	if !ok {
		glog.Infof("[verify]cohort settlement epoch unknown; no next hop assigned\n")
		return
	}
	keys := newVerifyCohortKeys(settings.CohortDeploymentKey, epoch)

	excluded := map[string]bool{}
	excludedList := []string{}
	for _, excludeClientId := range excludeClientIds {
		member := excludeClientId.String()
		if !excluded[member] {
			excluded[member] = true
			excludedList = append(excludedList, member)
		}
	}

	server.Redis(ctx, func(r server.RedisClient) {
		starved := false
		for attempt := 0; attempt < settings.SampleMaxAttempts; attempt += 1 {
			var sizeCmd, lifetimeCmd *redis.IntCmd
			var drawsCmd *redis.StringCmd
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				sizeCmd = pipe.SCard(ctx, keys.epoch)
				lifetimeCmd = pipe.SCard(ctx, keys.lifetime)
				drawsCmd = pipe.Get(ctx, keys.draws)
				return nil
			})
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			size := int(sizeCmd.Val())
			lifetime := int(lifetimeCmd.Val())
			draws, _ := drawsCmd.Int64()

			candidate := ""
			skipGrowthGate := false
			if size < settings.CohortSize {
				candidate = drawVerifyCohortCandidate(ctx, r, keys.lifetime, verifyCohortCarryDrawCount, keys.epoch, excluded)
				if candidate == "" && lifetime < settings.CohortLifetimeLimit &&
					(starved || settings.ReliabilityAMin*int64(size) <= draws) {
					candidate = drawVerifyCohortCandidate(ctx, r, verifyEligibleKey, settings.SampleCandidateCount, keys.epoch, excluded)
					skipGrowthGate = starved
				}
			}
			if candidate == "" {
				candidate = drawVerifyCohortCandidate(ctx, r, keys.epoch, settings.SampleCandidateCount, "", excluded)
			}
			if candidate == "" {
				// no usable member: a trail may need a new provider past the
				// growth gate on the next attempt, within both caps
				starved = true
				continue
			}

			candidateId, ok := verifyCandidateAssignable(ctx, r, candidate)
			if !ok {
				excluded[candidate] = true
				continue
			}
			admitted, cohortN := admitVerifyCohort(ctx, r, keys, candidate, settings, skipGrowthGate, excludedList)
			if !admitted {
				excluded[candidate] = true
				continue
			}
			if !SpendVerifyEligibilityToken(ctx, candidateId, settings) {
				// token exhausted: temporarily unassignable, try another
				excluded[candidate] = true
				continue
			}
			nextHop = &candidateId
			n = cohortN
			return
		}
	})
	return
}
