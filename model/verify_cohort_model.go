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
// Within the caps, members of claimed head fleets are drawn first
// (LEADERTOP.md design question 6): the binding sync keeps a priority set of
// each live claimed fleet's members, at most a per-fleet cap of them
// (SetVerifyCohortPriority), and a priority member skips the growth gate,
// never a cap, so claiming a UID leads to measurement. Next, providers
// already in the lifetime set (carried scores, last epoch's cohort among
// them) are re-admitted; they cost no lifetime budget. A new provider is admitted only after the cohort has averaged
// ReliabilityAMin draws this epoch, so early members reach a_min quickly
// rather than every member waiting for the whole cohort to fill, and after a
// rollover carried members that reconnect late still find room before new
// providers take it. A trail that cannot find an unused member may admit past
// that gate, never past a cap.
//
// Every source skips a provider on dead-hop probation (verify_dead_hop.go):
// one with no confirmation of at least 50 assignments in 24 h. Without that,
// a carried member that never confirms is re-admitted every epoch, since
// admission checks the member's identity, egress and provide mode but never
// its outcomes, and each of its assignments ends a trail.
//
// Nothing here falls back to unbounded sampling: an unknown epoch, or no usable
// member that the caps admit, yields no next hop (a refused candidate only
// moves the draw to members the caps admit), and a Redis error panics like
// every other verify state read.
//
// The rolling census (sn VERIFY-COHORT-V2.md, verify_cohort_rolling.go) is the
// second mode of the same admission script. From VERIFY-COHORT-V2.md on, the
// validator drops at each settlement fold the carried quality of every provider
// with no assignment in the closing window, so the lifetime limit above no
// longer has to hold every provider ever admitted: once the validator's head
// summaries show it runs that rule, the census set holds only the members still
// counted, each until the validator has folded two windows after its last
// assignment. Freed room goes to claimed fleets' members first, then to one
// representative of each unclaimed top-ranked network (verify_cohort_ranked.go),
// then carried members and the uniform sample, while an interlock keeps the
// validator's reported census below its hard bound. Until then nothing here
// changes.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
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

// The priority set holds at most a per-fleet share of each claimed fleet; one
// draw covers a few fleets' members.
const verifyCohortPriorityDrawCount = 64

var (
	// Next hops admitted from the claimed-fleet priority set.
	verifyCohortPriorityAdmissions = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "priority_admissions_total",
		Help: "Verify next hops drawn from the claimed-fleet priority set and admitted to the cohort (LEADERTOP.md design question 6). Within both cohort caps.",
	})
	verifyCohortAdmissions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "admissions_total",
		Help: "Verify next hops admitted to this settlement epoch's cohort, by the source they were drawn from (sn VERIFY-COHORT-V2.md §2.6): claimed (a claimed fleet's member, tier A), ranked (an unclaimed top-ranked network's representative, tier B, rolling census only), carried (a member still counted) and sample (the uniform draw from the eligible set).",
	}, []string{"tier"})
	verifyCohortAdmissionRefusals = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "urnetwork", Subsystem: "verify_cohort", Name: "admission_refusals_total",
		Help: "Candidates the verify cohort's admission script refused, by reason: epoch_full (this epoch's cohort is full), rolling_full (the rolling census is at its tier's bound, or its copy is being restored), interlock (the validator's reported census plus recent admissions is at 7/8 of its 4,096 bound, or its census is unknown) and census_full (the legacy lifetime or census limit). Growth-gate waits are pacing, not counted.",
	}, []string{"reason"})
)

func init() {
	for _, tier := range []string{verifyCohortTierClaimed, verifyCohortTierRanked, verifyCohortTierCarried, verifyCohortTierSample} {
		verifyCohortAdmissions.WithLabelValues(tier)
	}
	for _, refusal := range []verifyCohortRefusal{verifyCohortRefusedEpochFull, verifyCohortRefusedRollingFull, verifyCohortRefusedInterlock, verifyCohortRefusedCensusFull} {
		label, _ := refusal.label()
		verifyCohortAdmissionRefusals.WithLabelValues(label)
	}
	prometheus.MustRegister(verifyCohortPriorityAdmissions, verifyCohortAdmissions, verifyCohortAdmissionRefusals)
}

// All keys share one hash tag, so the admission script stays in one cluster
// slot. The deployment key keeps a redeployed coordinator, whose epochs
// restart at zero, from inheriting another deployment's cohorts.
type verifyCohortKeys struct {
	epoch    string
	draws    string
	lifetime string
	priority string
	// every provider ever admitted: the validator's census (rotation). In the
	// rolling census (verify_cohort_rolling.go) it holds the members still
	// counted, and members leave it two validator folds after their last draw.
	census string
	// set once the census holds every provider ever admitted
	censusReady string
	// set once the lifetime set holds the census members it keeps: absent
	// after Redis loses the lifetime set, which has no other copy, so the
	// next rotation rebuilds it from the census (RotateVerifyCohort)
	lifetimeReady string
	// the rolling census's state (verify_cohort_rolling.go): whether it is on,
	// the validator's reported census, the headroom held for claimed members,
	// and the admissions since the last two reports
	state string
	// one representative per unclaimed top-ranked network (tier B,
	// verify_cohort_ranked.go), replaced at each board fold
	ranked string
}

func newVerifyCohortKeys(deploymentKey StDeploymentKey, epoch uint64) verifyCohortKeys {
	prefix := fmt.Sprintf("{verify_cohort:%s}", deploymentKey)
	return verifyCohortKeys{
		epoch:         fmt.Sprintf("%se%d", prefix, epoch),
		draws:         fmt.Sprintf("%se%ddraws", prefix, epoch),
		lifetime:      prefix + "lifetime",
		priority:      prefix + "priority",
		census:        prefix + "census",
		censusReady:   prefix + "censusready",
		lifetimeReady: prefix + "lifetimeready",
		state:         prefix + "state",
		ranked:        prefix + "ranked",
	}
}

// The priority set is replaced about every minute by the binding sync. A sync
// that stops lets it lapse, so a binding that ended long ago never keeps its
// priority.
const verifyCohortPriorityTtl = 1 * time.Hour

// VerifyCohortPriorityPerFleet is the most members of one claimed fleet that
// draw ahead of the rest: an even share of the cohort among the 200 head
// seats, so a full head fits the cohort and no fleet takes another's share.
func VerifyCohortPriorityPerFleet(settings *VerifySettings) int {
	return max(1, settings.CohortSize/verifyCohortHeadSeats)
}

// The protocol's maximum_head_fleets on mainnet.
const verifyCohortHeadSeats = 200

// SetVerifyCohortPriority replaces the deployment's priority set with the
// members of the live claimed fleets, at most perFleet of each: members in the
// eligible set first, then members already in the lifetime set (they cost no
// lifetime budget), then by client id. Returns how many members it set. An
// empty set removes the key.
func SetVerifyCohortPriority(ctx context.Context, deploymentKey StDeploymentKey, fleets [][]server.Id, perFleet int) int {
	if deploymentKey == "" {
		return 0
	}
	keys := newVerifyCohortKeys(deploymentKey, 0)
	candidates := []string{}
	for _, members := range fleets {
		for _, member := range members {
			candidates = append(candidates, member.String())
		}
	}
	selected := []any{}
	server.Redis(ctx, func(r server.RedisClient) {
		eligible := map[string]bool{}
		carried := map[string]bool{}
		if 0 < len(candidates) {
			args := make([]any, len(candidates))
			for i, candidate := range candidates {
				args[i] = candidate
			}
			inEligible, err := r.SMIsMember(ctx, verifyEligibleKey, args...).Result()
			server.Raise(err)
			inLifetime, err := r.SMIsMember(ctx, keys.lifetime, args...).Result()
			server.Raise(err)
			for i, candidate := range candidates {
				eligible[candidate], carried[candidate] = inEligible[i], inLifetime[i]
			}
		}
		seen := map[string]bool{}
		for _, members := range fleets {
			ordered := []string{}
			for _, member := range members {
				ordered = append(ordered, member.String())
			}
			slices.SortFunc(ordered, func(a string, b string) int {
				if eligible[a] != eligible[b] {
					if eligible[a] {
						return -1
					}
					return 1
				}
				if carried[a] != carried[b] {
					if carried[a] {
						return -1
					}
					return 1
				}
				return strings.Compare(a, b)
			})
			for _, member := range ordered[:min(len(ordered), max(0, perFleet))] {
				if !seen[member] {
					seen[member] = true
					selected = append(selected, member)
				}
			}
		}
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, keys.priority)
			if 0 < len(selected) {
				pipe.SAdd(ctx, keys.priority, selected...)
				pipe.Expire(ctx, keys.priority, verifyCohortPriorityTtl)
			}
			return nil
		})
		server.Raise(err)
	})
	return len(selected)
}

// verifyCohortAdmitScript admits ARGV[1] into the epoch cohort (or confirms it
// is already a member) and counts one draw. Every cap is checked and applied
// in one script, so concurrent admissions can never overshoot one. Every
// read, including the draw counter's integer check, comes before the first
// write: Redis does not roll back a script that fails part way, so corrupt
// state must fail it before it changes anything.
//
// The lifetime set is the active set: the providers re-admitted ahead of new
// ones, at most the lifetime limit. Stale members rotate out of it
// (RotateVerifyCohort) but never out of the census set, every provider ever
// admitted, which the validator keeps scoring forever. Once the census is
// ready (it holds every provider ever admitted), a new provider counts
// against it: an unclaimed one within the lifetime limit, exactly as before
// rotation, and a claimed fleet's member within the census limit. A census
// member that rotated out returns at no census cost. Before the census is
// ready, admission is as it was before rotation; while a durable census
// exists but Redis has lost its copy, no new provider is admitted.
//
//	KEYS[1] epoch cohort set, KEYS[2] epoch draw counter, KEYS[3] lifetime
//	(active) set, KEYS[4] census set, KEYS[5] census ready marker
//	ARGV[1] candidate, ARGV[2] cohort size, ARGV[3] lifetime limit,
//	ARGV[4] a_min (growth gate), ARGV[5] '1' to skip the growth gate (never a
//	cap), ARGV[6] epoch key ttl seconds, ARGV[7] census limit, ARGV[8] '1'
//	for a claimed fleet's member, ARGV[9] '1' when a durable census exists,
//	ARGV[10..] excluded ids (for n)
//
// Returns {1, n, new, 0} when the candidate may be assigned, n being the
// cohort size net of excluded members and new 1 when the candidate entered a
// ready census (the caller writes it through to Postgres), or {0, size, 0,
// reason} when it is refused (verifyCohortRefusal). Before the census is
// ready, rotation's first run mirrors it.
//
// The rolling census (sn VERIFY-COHORT-V2.md §2.4-2.6, verify_cohort_rolling.go)
// is the second mode of the same script, so the mode can't change between the
// read and the write. It is on while KEYS[6]'s rolling field is '1', which the
// taskworker sets only once the validator's head summaries show it has folded a
// settlement window with the retirement rule. Then the census set holds the
// members still counted, and a new provider is admitted only:
//   - while the census is under the lifetime limit (L) for a claimed fleet's
//     member, or under L less the headroom held for claimed members for any
//     other tier;
//   - while the validator's reported census plus the admissions since the last
//     two reports stays under the interlock (verifyCohortInterlockLimit), and
//     never while the validator's census is unknown;
//   - past the growth gate, as before, unless it skips it.
//
// A member already counted needs only room in the epoch cohort. Without the
// rolling field the script is exactly the legacy rule above.
//
//	KEYS[6] rolling state hash: rolling, census, since, previous, headroom
//	ARGV[10] the interlock limit, ARGV[11..] excluded ids (for n)
const verifyCohortAdmitScript = `
local member = redis.call('SISMEMBER', KEYS[1], ARGV[1]) == 1
local size = redis.call('SCARD', KEYS[1])
local carried = redis.call('SISMEMBER', KEYS[3], ARGV[1]) == 1
local lifetime = redis.call('SCARD', KEYS[3])
local ready = redis.call('EXISTS', KEYS[5]) == 1
local counted = redis.call('SISMEMBER', KEYS[4], ARGV[1]) == 1
local census = redis.call('SCARD', KEYS[4])
local state = redis.call('HMGET', KEYS[6], 'rolling', 'census', 'since', 'previous', 'headroom')
local raw = redis.call('GET', KEYS[2])
if raw and not string.match(raw, '^%d+$') then
  return redis.error_reply('verify cohort draw counter is not an integer')
end
for i = 2, 5 do
  if state[i] and not string.match(state[i], '^%d+$') then
    return redis.error_reply('verify cohort rolling state is not an integer')
  end
end
local draws = tonumber(raw or '0')
local new = 0
if not member then
  if size >= tonumber(ARGV[2]) then return {0, size, 0, 1} end
  if state[1] == '1' then
    if not counted then
      if not ready then return {0, size, 0, 2} end
      local bound = tonumber(ARGV[3])
      if ARGV[8] ~= '1' then bound = bound - tonumber(state[5] or '0') end
      if census >= bound then return {0, size, 0, 2} end
      if not state[2] then return {0, size, 0, 3} end
      if tonumber(state[2]) + tonumber(state[3] or '0') + tonumber(state[4] or '0') >= tonumber(ARGV[10]) then return {0, size, 0, 3} end
      if ARGV[5] ~= '1' and draws < tonumber(ARGV[4]) * size then return {0, size, 0, 4} end
      redis.call('SADD', KEYS[4], ARGV[1])
      redis.call('SADD', KEYS[3], ARGV[1])
      redis.call('HINCRBY', KEYS[6], 'since', 1)
      new = 1
    end
  elseif not carried then
    if lifetime >= tonumber(ARGV[3]) then return {0, size, 0, 5} end
    if not (ready and counted) then
      if ARGV[9] == '1' and not ready then return {0, size, 0, 5} end
      local ever = lifetime
      local bound = tonumber(ARGV[3])
      if ready then
        ever = census
        if ARGV[8] == '1' then bound = tonumber(ARGV[7]) end
      end
      if ever >= bound then return {0, size, 0, 5} end
      if ARGV[5] ~= '1' and draws < tonumber(ARGV[4]) * size then return {0, size, 0, 4} end
      if ready then new = 1 end
    end
    redis.call('SADD', KEYS[3], ARGV[1])
    redis.call('SADD', KEYS[4], ARGV[1])
  end
  redis.call('SADD', KEYS[1], ARGV[1])
end
redis.call('INCR', KEYS[2])
redis.call('EXPIRE', KEYS[1], ARGV[6])
redis.call('EXPIRE', KEYS[2], ARGV[6])
local n = redis.call('SCARD', KEYS[1])
for i = 11, #ARGV do
  if redis.call('SISMEMBER', KEYS[1], ARGV[i]) == 1 then n = n - 1 end
end
return {1, n, new, 0}
`

// Why the admission script refused a candidate.
type verifyCohortRefusal int

const (
	verifyCohortAdmitted verifyCohortRefusal = 0
	// this epoch's cohort is full
	verifyCohortRefusedEpochFull verifyCohortRefusal = 1
	// the rolling census is at its tier's bound, or its copy is being restored
	verifyCohortRefusedRollingFull verifyCohortRefusal = 2
	// the validator's reported census plus recent admissions is at the
	// interlock, or the validator's census is unknown
	verifyCohortRefusedInterlock verifyCohortRefusal = 3
	// the growth gate: the cohort has not averaged a_min draws this epoch
	verifyCohortRefusedGrowthGate verifyCohortRefusal = 4
	// the legacy lifetime or census limit, or a census being restored
	verifyCohortRefusedCensusFull verifyCohortRefusal = 5
)

// The refusal's metric label; the growth gate is a pacing rule, not a limit,
// and is not counted.
func (self verifyCohortRefusal) label() (string, bool) {
	switch self {
	case verifyCohortRefusedEpochFull:
		return "epoch_full", true
	case verifyCohortRefusedRollingFull:
		return "rolling_full", true
	case verifyCohortRefusedInterlock:
		return "interlock", true
	case verifyCohortRefusedCensusFull:
		return "census_full", true
	}
	return "", false
}

// verifyCohortInterlockLimit stops new admissions in the rolling census before
// the validator could reach its hard bound: 7/8 of its signed max_providers,
// 4,096 (server controller/verify_controller.go verifyValidatorMaxProviders).
// The validator's reported census plus everything admitted since its last two
// reports must stay under it (sn VERIFY-COHORT-V2.md §2.5). With the rolling
// limit at 2,000 it binds only if the validator stops retiring idle members.
const verifyCohortInterlockLimit = 3584

// How one candidate is admitted.
type verifyCohortAdmission struct {
	// skip the growth gate, never a cap
	skipGrowthGate bool
	// a claimed fleet's member, bounded by the census limit
	priority bool
	// a durable census exists, so a missing Redis census refuses new providers
	censusRequired bool
}

// admitVerifyCohort runs the admission script for one unclaimed candidate.
// Redis errors panic; a refusal is an ordinary false.
func admitVerifyCohort(
	ctx context.Context,
	r server.RedisClient,
	keys verifyCohortKeys,
	candidate string,
	settings *VerifySettings,
	skipGrowthGate bool,
	excluded []string,
) (admitted bool, n int) {
	admitted, n, _ = admitVerifyCohortAs(ctx, r, keys, candidate, settings, verifyCohortAdmission{skipGrowthGate: skipGrowthGate}, excluded)
	return
}

// admitVerifyCohortAs runs the admission script; entered is true when the
// candidate entered the census.
func admitVerifyCohortAs(
	ctx context.Context,
	r server.RedisClient,
	keys verifyCohortKeys,
	candidate string,
	settings *VerifySettings,
	admission verifyCohortAdmission,
	excluded []string,
) (admitted bool, n int, entered bool) {
	admitted, n, entered, _ = admitVerifyCohortWithReason(ctx, r, keys, candidate, settings, admission, excluded)
	return
}

// admitVerifyCohortWithReason runs the admission script and also says why it
// refused a candidate.
func admitVerifyCohortWithReason(
	ctx context.Context,
	r server.RedisClient,
	keys verifyCohortKeys,
	candidate string,
	settings *VerifySettings,
	admission verifyCohortAdmission,
	excluded []string,
) (admitted bool, n int, entered bool, refusal verifyCohortRefusal) {
	flag := func(value bool) string {
		if value {
			return "1"
		}
		return "0"
	}
	args := []any{
		candidate,
		settings.CohortSize,
		settings.CohortLifetimeLimit,
		settings.ReliabilityAMin,
		flag(admission.skipGrowthGate),
		int64(verifyCohortEpochTtl / time.Second),
		max(settings.CohortCensusLimit, settings.CohortLifetimeLimit),
		flag(admission.priority),
		flag(admission.censusRequired),
		verifyCohortInterlockLimit,
	}
	for _, id := range excluded {
		args = append(args, id)
	}
	values, err := r.Eval(ctx, verifyCohortAdmitScript, []string{keys.epoch, keys.draws, keys.lifetime, keys.census, keys.censusReady, keys.state}, args...).Int64Slice()
	server.Raise(err)
	if len(values) != 4 {
		panic(fmt.Errorf("verify cohort admission returned %d values", len(values)))
	}
	return values[0] == 1, int(values[1]), values[2] == 1, verifyCohortRefusal(values[3])
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
// one at random that is not excluded, is currently in the eligible set, is
// not on dead-hop probation unless its probe is due (verify_dead_hop.go),
// and, when notIn is set, is not a member of notIn. Cohort sets keep members
// that disconnect, or that stop confirming, so these checks keep such members
// from using up the sampler's attempts. probe is true for a due probe, which
// the caller claims before assigning it. Returns "" when no member qualifies.
func drawVerifyCohortCandidate(
	ctx context.Context,
	r server.RedisClient,
	source string,
	count int,
	notIn string,
	excluded map[string]bool,
	settings *VerifySettings,
) (candidate string, probe bool) {
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
	candidates, probes := filterVerifyDeadHop(ctx, r, candidates, settings)
	if len(candidates) == 0 {
		return "", false
	}
	pickIndex, err := rand.Int(rand.Reader, big.NewInt(int64(len(candidates))))
	server.Raise(err)
	candidate = candidates[pickIndex.Int64()]
	return candidate, probes[candidate]
}

// The source a next hop was drawn from, as a metric label.
const (
	verifyCohortTierClaimed = "claimed"
	verifyCohortTierRanked  = "ranked"
	verifyCohortTierCarried = "carried"
	verifyCohortTierSample  = "sample"
)

// sampleVerifyCohortNextHop is SampleVerifyNextHop restricted to the epoch
// cohort. Each attempt draws, in order of preference:
//  1. while the cohort has room, a claimed fleet's member from the priority set
//     (tier A: at most a per-fleet share, skipping the growth gate);
//  2. in the rolling census only, while the cohort has room, a representative
//     of an unclaimed top-ranked network from the ranked set (tier B,
//     verify_cohort_ranked.go, skipping the growth gate);
//  3. while the cohort has room, a carried member that is not yet in this
//     epoch's cohort: the lifetime set, or in the rolling census the members
//     still counted;
//  4. while the cohort and its census have room and the growth gate is open
//     (or the previous attempt found no usable member), a provider from the
//     eligible set (the uniform sample). The census has room as the script
//     counts a new unclaimed provider: in the legacy census against the
//     census once it is ready, not at all while a durable census waits for
//     its restore, and against the lifetime set before rotation's first run;
//  5. otherwise an existing member of this epoch's cohort.
//
// Each source leaves out providers on dead-hop probation whose probe is not
// due. The admission script then decides, in the mode it reads itself; a
// refused candidate is excluded and the next attempt re-reads the cohort. A
// cap that refuses a new provider refuses the rest of its tier, and a claimed
// member's caps are never tighter than another tier's, so once a cap refuses
// one the remaining attempts skip the tiers it refuses: a SEED then gets a
// carried member or one of this epoch's cohort, rather than no next hop after
// every attempt went to a provider the script refuses.
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
	censusRequired := verifyCohortCensusRequired(ctx, settings.CohortDeploymentKey)
	var entered *server.Id
	enteredRolling := false
	defer func() {
		if entered != nil {
			if enteredRolling {
				recordVerifyCohortRollingCensus(ctx, settings.CohortDeploymentKey, *entered, epoch)
			} else {
				recordVerifyCohortCensus(ctx, settings.CohortDeploymentKey, *entered)
			}
		}
	}()

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
		// a cap refused a claimed member, which refuses every tier, or an
		// unclaimed new provider, which refuses the ranked and sampled tiers
		claimedCapped, unclaimedCapped := false, false
		for attempt := 0; attempt < settings.SampleMaxAttempts; attempt += 1 {
			var sizeCmd, lifetimeCmd, censusCmd, readyCmd *redis.IntCmd
			var drawsCmd *redis.StringCmd
			var stateCmd *redis.SliceCmd
			_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
				sizeCmd = pipe.SCard(ctx, keys.epoch)
				lifetimeCmd = pipe.SCard(ctx, keys.lifetime)
				censusCmd = pipe.SCard(ctx, keys.census)
				readyCmd = pipe.Exists(ctx, keys.censusReady)
				drawsCmd = pipe.Get(ctx, keys.draws)
				stateCmd = pipe.HMGet(ctx, keys.state, "rolling", "headroom")
				return nil
			})
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			size := int(sizeCmd.Val())
			lifetime := int(lifetimeCmd.Val())
			census := int(censusCmd.Val())
			ready := readyCmd.Val() == 1
			draws, _ := drawsCmd.Int64()
			// the script reads the mode itself; this only picks the sources
			rolling, headroom := verifyCohortRollingFields(stateCmd.Val())

			candidate := ""
			probe := false
			skipGrowthGate := false
			priority := false
			tier := ""
			if size < settings.CohortSize && !claimedCapped {
				// a claimed fleet's member skips the growth gate, never a cap
				candidate, probe = drawVerifyCohortCandidate(ctx, r, keys.priority, verifyCohortPriorityDrawCount, keys.epoch, excluded, settings)
				skipGrowthGate, priority = candidate != "", candidate != ""
				if candidate != "" {
					tier = verifyCohortTierClaimed
				}
			}
			if candidate == "" && size < settings.CohortSize && rolling && !unclaimedCapped {
				// an unclaimed top-ranked network's representative skips the
				// growth gate, never a cap
				candidate, probe = drawVerifyCohortCandidate(ctx, r, keys.ranked, verifyCohortRankedDrawCount, keys.epoch, excluded, settings)
				if candidate != "" {
					skipGrowthGate, tier = true, verifyCohortTierRanked
				}
			}
			if candidate == "" && size < settings.CohortSize {
				carried := keys.lifetime
				// room for a new unclaimed provider, as the script counts it
				room := lifetime < settings.CohortLifetimeLimit
				switch {
				case rolling:
					carried = keys.census
					room = census < settings.CohortLifetimeLimit-headroom
				case ready:
					// the census holds every provider ever admitted, and a new
					// unclaimed provider counts against it
					room = room && census < settings.CohortLifetimeLimit
				case censusRequired:
					// Redis has not restored the durable census: the script
					// admits no new provider
					room = false
				}
				candidate, probe = drawVerifyCohortCandidate(ctx, r, carried, verifyCohortCarryDrawCount, keys.epoch, excluded, settings)
				if candidate != "" {
					tier = verifyCohortTierCarried
				}
				if candidate == "" && room && !unclaimedCapped &&
					(starved || settings.ReliabilityAMin*int64(size) <= draws) {
					candidate, probe = drawVerifyCohortCandidate(ctx, r, verifyEligibleKey, settings.SampleCandidateCount, keys.epoch, excluded, settings)
					skipGrowthGate = starved
					if candidate != "" {
						tier = verifyCohortTierSample
					}
				}
			}
			if candidate == "" {
				candidate, probe = drawVerifyCohortCandidate(ctx, r, keys.epoch, settings.SampleCandidateCount, "", excluded, settings)
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
			admitted, cohortN, newToCensus, refusal := admitVerifyCohortWithReason(ctx, r, keys, candidate, settings,
				verifyCohortAdmission{skipGrowthGate: skipGrowthGate, priority: priority, censusRequired: censusRequired}, excludedList)
			if !admitted {
				if label, counted := refusal.label(); counted {
					verifyCohortAdmissionRefusals.WithLabelValues(label).Inc()
					switch tier {
					case verifyCohortTierClaimed:
						claimedCapped, unclaimedCapped = true, true
					case verifyCohortTierRanked, verifyCohortTierSample:
						unclaimedCapped = true
					}
				}
				excluded[candidate] = true
				continue
			}
			if newToCensus {
				entered, enteredRolling = &candidateId, rolling
			}
			if !SpendVerifyEligibilityToken(ctx, candidateId, settings) {
				// token exhausted: temporarily unassignable, try another
				excluded[candidate] = true
				continue
			}
			if probe && !claimVerifyDeadHopProbe(ctx, r, candidate, settings) {
				// another draw took this probe first
				excluded[candidate] = true
				continue
			}
			if priority {
				verifyCohortPriorityAdmissions.Inc()
			}
			if tier != "" {
				verifyCohortAdmissions.WithLabelValues(tier).Inc()
			}
			nextHop = &candidateId
			n = cohortN
			return
		}
	})
	return
}

// The ranked set holds at most one representative per top-400 network; one draw
// covers a few dozen of them.
const verifyCohortRankedDrawCount = 64

// Whether the rolling census is on, and the headroom it holds for claimed
// members, from the state hash's rolling and headroom fields. Anything but an
// exact "1" is off, and an absent or malformed headroom is zero; the admission
// script reads and validates both itself.
func verifyCohortRollingFields(values []any) (rolling bool, headroom int) {
	if len(values) != 2 {
		return false, 0
	}
	if value, ok := values[0].(string); ok && value == "1" {
		rolling = true
	}
	if value, ok := values[1].(string); ok {
		if parsed, err := strconv.Atoi(value); err == nil && 0 <= parsed {
			headroom = parsed
		}
	}
	return
}

// The cohort at the current settlement epoch, for the board
// (LEADERTOP.md §3.5): how many providers the validator can measure.
type VerifyCohortStatus struct {
	Epoch         uint64
	Members       int
	Capacity      int
	Lifetime      int
	LifetimeLimit int
	// claimed-fleet members drawing ahead of the rest
	Priority int
	// the rolling census (verify_cohort_rolling.go): whether it is on, the
	// members it counts, and the ranked representatives drawing after claimed
	// members
	Rolling        bool
	RollingMembers int
	Ranked         int
}

// GetVerifyCohortStatus reads the cohort's sizes; ok is false when there is no
// cohort or its settlement epoch is unknown.
func GetVerifyCohortStatus(ctx context.Context, settings *VerifySettings) (*VerifyCohortStatus, bool) {
	if settings.CohortSize <= 0 {
		return nil, false
	}
	epoch, ok := verifyCohortEpoch(ctx, settings.CohortDeploymentKey)
	if !ok {
		return nil, false
	}
	keys := newVerifyCohortKeys(settings.CohortDeploymentKey, epoch)
	status := &VerifyCohortStatus{Epoch: epoch, Capacity: settings.CohortSize, LifetimeLimit: settings.CohortLifetimeLimit}
	server.Redis(ctx, func(r server.RedisClient) {
		var members, lifetime, priority, census, ranked *redis.IntCmd
		var state *redis.SliceCmd
		_, err := r.Pipelined(ctx, func(pipe redis.Pipeliner) error {
			members = pipe.SCard(ctx, keys.epoch)
			lifetime = pipe.SCard(ctx, keys.lifetime)
			priority = pipe.SCard(ctx, keys.priority)
			census = pipe.SCard(ctx, keys.census)
			ranked = pipe.SCard(ctx, keys.ranked)
			state = pipe.HMGet(ctx, keys.state, "rolling", "headroom")
			return nil
		})
		server.Raise(err)
		status.Members, status.Lifetime, status.Priority = int(members.Val()), int(lifetime.Val()), int(priority.Val())
		status.Rolling, _ = verifyCohortRollingFields(state.Val())
		status.RollingMembers, status.Ranked = int(census.Val()), int(ranked.Val())
	})
	return status, true
}

// GetVerifyCohortMembership reports, for each client, whether it is in the
// current epoch's cohort and whether it is in the lifetime set (re-admitted
// ahead of new providers whenever it is eligible). Both are false without a
// cohort.
func GetVerifyCohortMembership(ctx context.Context, settings *VerifySettings, clientIds []server.Id) (epochMembers map[server.Id]bool, lifetimeMembers map[server.Id]bool) {
	epochMembers, lifetimeMembers = map[server.Id]bool{}, map[server.Id]bool{}
	if settings.CohortSize <= 0 || len(clientIds) == 0 {
		return
	}
	epoch, ok := verifyCohortEpoch(ctx, settings.CohortDeploymentKey)
	if !ok {
		return
	}
	keys := newVerifyCohortKeys(settings.CohortDeploymentKey, epoch)
	server.Redis(ctx, func(r server.RedisClient) {
		for start := 0; start < len(clientIds); start += 1024 {
			batch := clientIds[start:min(start+1024, len(clientIds))]
			args := make([]any, len(batch))
			for i, clientId := range batch {
				args[i] = clientId.String()
			}
			inEpoch, err := r.SMIsMember(ctx, keys.epoch, args...).Result()
			server.Raise(err)
			inLifetime, err := r.SMIsMember(ctx, keys.lifetime, args...).Result()
			server.Raise(err)
			for i, clientId := range batch {
				if inEpoch[i] {
					epochMembers[clientId] = true
				}
				if inLifetime[i] {
					lifetimeMembers[clientId] = true
				}
			}
		}
	})
	return
}
