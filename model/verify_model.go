package model

// verify_model.go — state model for the `/verify` routing-verification
// protocol (sn/VALIDATOR.md §§5-8). Redis holds all hot per-trail and
// per-provider state so the trail path puts no pressure on Postgres; Postgres
// gets only completed/expired proofs (`verify_trail`, VALIDATOR.md §6.2) and
// periodic per-provider stat rollups (`verify_provider_stats`).
//
// Redis key map (cluster rule: keys touched in one pipeline share a `{...}`
// hash tag):
//
//	{vtr_<trailId>}h        trail header hash (vpk, server_nonce, m, status, ...)
//	{vtr_<trailId>}hops     list of confirmed hop records (json), seed first
//	{vtr_<trailId>}pending  the pending hop record (json) awaiting arrival
//	{vtr_<trailId>}resp     last response (json) for idempotent retries (§4.3)
//	verify_reap             zset trailId -> step deadline unix ms (reaper index)
//	verify_egress_<ip>      egress index: ip -> clientId, or `!` if ambiguous (§8.1)
//	{vce_<clientId>}        reverse egress hash: ip -> entry expiry unix ms (§8.2)
//	verify_eligible         set of currently eligible provider clientIds (§5.1)
//	{velig_<clientId>}      eligibility token spend counter (INCR+EXPIRE, §5.3)
//	{vstat_<clientId>}p<t>  per-period stats hash: assignments, confirmations,
//	                        log-spaced latency buckets lb_<i> (§7)
//	verify_stat_clients     set of clientIds with stats pending rollup
//	verify_seed_ip_<hash>   seed rate counter per source ip (INCR+EXPIRE, §9)
//	verify_seed_vpk_<vpk>   seed rate counter per vpk (INCR+EXPIRE, §9)
//	verify_trails_<vpk>     active trail count per vpk (concurrent-trail cap, §9)
//
// The egress index is fed by exactly one bijection-gated feeder
// (`FeedVerifyEgress`) with two call sites: observed connection source ips
// (connect/transport_announce.go) and proxy-allocated egresses
// (CreateProxyClient + the periodic `RefreshVerifyProxyEgress`). The §8.2
// invariant — one provider ⇄ one egress ip — is enforced at write time (an ip
// observed backing a second client becomes ambiguous and resolves to nothing)
// and re-checked at read time (`ResolveVerifyEgress` requires the forward and
// reverse entries to agree and the client to have exactly one live ip).
//
// All functions are safe for concurrent use.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// trail status values stored in the `{vtr_<trailId>}h` header hash.
const (
	VerifyTrailStatusActive   = "active"
	VerifyTrailStatusComplete = "complete"
	VerifyTrailStatusExpired  = "expired"
)

// `verify_trail.status` codes (VALIDATOR.md §6.2: complete | expired).
const (
	VerifyTrailRowStatusComplete = 1
	VerifyTrailRowStatusExpired  = 2
)

// verifyEgressAmbiguous marks an egress ip observed backing more than one
// client. The ip resolves to no provider while the marker lives (§8.2). The
// marker keeps its ttl from the moment of conflict — feeds do not extend it —
// so an ip whose contention ends heals after at most one egress ttl.
const verifyEgressAmbiguous = "!"

func DefaultVerifySettings() *VerifySettings {
	return &VerifySettings{
		StepTimeout:      30 * time.Second,
		StepTimeoutGrace: 5 * time.Second,
		TrailTtlGrace:    60 * time.Second,
		TrailLockTtl:     5 * time.Second,
		// D26 (guardrails off): the §5.3 eligibility token bucket defaults OFF
		// — interval 0 means "always has a token" in SpendVerifyEligibilityToken
		// / CheckVerifyEligibilityToken, so each validator drives its own
		// measurement rate (UR is the largest validator). A positive interval
		// re-enables coverage spreading.
		EligibilityInterval:   0,
		EligibilityBurst:      2,
		EgressTtl:             10 * time.Minute,
		EgressRefreshInterval: 2 * time.Minute,
		SeedRateWindow:        60 * time.Second,
		// D26 (guardrails off): the §9 SOFT seed / concurrent-trail limits gate
		// the soft→poison path only when SoftLimitsEnabled; default off. The
		// HARD limits and the per-source-ip EXTEND meter stay ON as the DoS
		// backstop — poison trails cost server state, so the state-creating
		// path keeps a hard cap regardless.
		SoftLimitsEnabled:      false,
		SeedRateSoftLimit:      10,
		SeedRateHardLimit:      40,
		ExtendRateHardLimit:    240,
		ActiveTrailsSoftLimit:  8,
		ActiveTrailsHardLimit:  32,
		StatsPeriod:            15 * time.Minute,
		SampleCandidateCount:   16,
		SampleMaxAttempts:      8,
		SweepLimit:             1000,
		ResolveConstantTimePad: true,
		// D27: head routable-IP-score granularity — the egress-IP-hash network
		// prefix. Subnet-configurable; default /29 v4, /48 v6 (what UR uses
		// today; note /48 v6, NOT the /56 of server.ClientIpHash).
		EgressHashV4Prefix: 29,
		EgressHashV6Prefix: 48,
	}
}

// VerifySettings are the protocol parameters of VALIDATOR.md §5.5 and §9.
// `TrailTtl(m)` = m×StepTimeout + TrailTtlGrace. The seed rate and
// concurrent-trail limits are two-tier: over the soft limit the caller gets an
// indistinguishable poisoned trail (§9); over the hard limit the request is
// refused outright (the DoS bound — poison trails cost server state, so the
// state-creating path must have a hard cap). D26: the soft tier + the §5.3
// eligibility bucket are validator-configurable and default off (see
// DefaultVerifySettings); the hard tier is always on.
type VerifySettings struct {
	StepTimeout      time.Duration
	StepTimeoutGrace time.Duration
	TrailTtlGrace    time.Duration
	// TrailLockTtl bounds the per-trail EXTEND mutation lock (V3): long enough
	// to cover one confirm+assign (incl. the durable insert), short enough to
	// self-heal if the holder crashes mid-mutation.
	TrailLockTtl          time.Duration
	EligibilityInterval   time.Duration
	EligibilityBurst      int
	EgressTtl             time.Duration
	EgressRefreshInterval time.Duration
	SeedRateWindow        time.Duration
	// SoftLimitsEnabled gates the §9 soft→poison path (SeedRateSoftLimit +
	// ActiveTrailsSoftLimit). D26: default false so validators set their own
	// rate; the hard limits stay on regardless.
	SoftLimitsEnabled bool
	SeedRateSoftLimit int64
	SeedRateHardLimit int64
	// ExtendRateHardLimit is the per-source-ip EXTEND cap over one
	// SeedRateWindow (V4). Legitimate per-provider EXTEND volume is bounded by
	// the eligibility burst (a provider is the confirming hop only for trails it
	// was assigned to), so this is set generously — it exists to shed a
	// single-ip flood of the mutating path, not to shape normal traffic.
	ExtendRateHardLimit    int64
	ActiveTrailsSoftLimit  int64
	ActiveTrailsHardLimit  int64
	StatsPeriod            time.Duration
	SampleCandidateCount   int
	SampleMaxAttempts      int
	SweepLimit             int
	ResolveConstantTimePad bool
	// EgressHashV4Prefix / EgressHashV6Prefix are the network prefix lengths
	// the per-hop egress-IP-hash truncates to before hashing (§8.1, D27) — the
	// head routable-IP score's "distinct IP" granularity. Subnet-configurable;
	// default /29 v4, /48 v6.
	EgressHashV4Prefix int
	EgressHashV6Prefix int
}

// TrailTtl is the redis lifetime of one trail's state (VALIDATOR.md §5.5:
// `M·T` + grace).
func (self *VerifySettings) TrailTtl(m int) time.Duration {
	return time.Duration(m)*self.StepTimeout + self.TrailTtlGrace
}

// VerifyTrailHop is one hop record of a trail. `AssignedMs`/`AssignN` are zero
// for the validator-chosen seed hop, which is never a server assignment and is
// excluded from statistics (§7.6). `ConfirmedMs` is zero while pending.
// `EgressIpHash` is the hop's egress-IP-hash at the configured subnet
// granularity (§8.1, §3.3, D27) — the head routable-IP-score input, set at
// confirmation from the confirming request's source ip (zero while pending)
// and carried verbatim into the published FINAL proof.
type VerifyTrailHop struct {
	ClientId     server.Id `json:"client_id"`
	AssignedMs   uint64    `json:"assigned_ms"`
	ConfirmedMs  uint64    `json:"confirmed_ms"`
	AssignN      int       `json:"assign_n"`
	Seed         bool      `json:"seed,omitempty"`
	EgressIpHash [32]byte  `json:"egress_ip_hash"`
}

// VerifyTrail is the full redis state of one trail (§6.1). `Hops` are the
// confirmed hops in order (seed first); `Pending` is the assigned hop awaiting
// arrival (nil once complete). A `Poison` trail is stored and advanced exactly
// like a real one but is never published and never touches provider
// statistics (§9).
type VerifyTrail struct {
	TrailId     server.Id
	ClientId    server.Id
	Vpk         []byte
	ServerNonce []byte
	M           int
	ServerKeyId byte
	Status      string
	Poison      bool
	CreateMs    uint64
	ActivityMs  uint64
	Hops        []*VerifyTrailHop
	Pending     *VerifyTrailHop
}

func verifyTrailHeaderKey(trailId server.Id) string {
	return fmt.Sprintf("{vtr_%s}h", trailId)
}

func verifyTrailHopsKey(trailId server.Id) string {
	return fmt.Sprintf("{vtr_%s}hops", trailId)
}

func verifyTrailPendingKey(trailId server.Id) string {
	return fmt.Sprintf("{vtr_%s}pending", trailId)
}

func verifyTrailResponseKey(trailId server.Id) string {
	return fmt.Sprintf("{vtr_%s}resp", trailId)
}

// verifyTrailLockKey is the per-trail EXTEND mutation lock (V3). It carries the
// trail's `{vtr_<trailId>}` hash tag so the SETNX/DEL pair shares the trail's
// cluster slot.
func verifyTrailLockKey(trailId server.Id) string {
	return fmt.Sprintf("{vtr_%s}lock", trailId)
}

const verifyReapKey = "verify_reap"

func verifyEgressKey(ip netip.Addr) string {
	return fmt.Sprintf("verify_egress_%s", ip.Unmap().String())
}

func verifyClientEgressKey(clientId server.Id) string {
	return fmt.Sprintf("{vce_%s}", clientId)
}

const verifyEligibleKey = "verify_eligible"

func verifyEligibilityTokenKey(clientId server.Id) string {
	return fmt.Sprintf("{velig_%s}", clientId)
}

func verifyStatsKey(clientId server.Id, periodStart time.Time) string {
	return fmt.Sprintf("{vstat_%s}p%d", clientId, periodStart.UTC().Unix())
}

const verifyStatClientsKey = "verify_stat_clients"

func verifySeedIpRateKey(ipHash [32]byte) string {
	return fmt.Sprintf("verify_seed_ip_%s", hex.EncodeToString(ipHash[:]))
}

func verifySeedVpkRateKey(vpk []byte) string {
	return fmt.Sprintf("verify_seed_vpk_%s", hex.EncodeToString(vpk))
}

func verifyExtendIpRateKey(ipHash [32]byte) string {
	return fmt.Sprintf("verify_extend_ip_%s", hex.EncodeToString(ipHash[:]))
}

func verifyActiveTrailsKey(vpk []byte) string {
	return fmt.Sprintf("verify_trails_%s", hex.EncodeToString(vpk))
}

// ParseVerifyEgressIp extracts the canonical (unmapped) ip from a session
// `ClientAddress` (ip:port), for use as an egress index key.
func ParseVerifyEgressIp(clientAddress string) (ip netip.Addr, ok bool) {
	host, _, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		return
	}
	ip, err = netip.ParseAddr(host)
	if err != nil {
		return
	}
	ip = ip.Unmap()
	ok = true
	return
}

// VerifyEgressIpHash is the per-hop egress-IP-hash of the head routable-IP
// score (VALIDATOR.md §8.1, §3.3, §11.1; D27): a keyed hash of the provider's
// egress ip truncated to the subnet-configurable network prefix
// (`v4Prefix`/`v6Prefix`, default /29 v4 & /48 v6 — note /48, not the /56 of
// `server.ClientIpHash`). Coarsening to a prefix is what makes an IP-hash
// shareable/splittable across fleets (§11.1); the keyed (peppered) hash reuses
// the ClientIpHash pepper so raw egress ips are never stored on the trail.
func VerifyEgressIpHash(ip netip.Addr, v4Prefix int, v6Prefix int) [32]byte {
	return server.ClientIpHashForAddrPrefix(ip, v4Prefix, v6Prefix)
}

// FeedVerifyEgress is the single bijection-gated egress-index feeder (§8.1,
// §8.2). Both feeders (observed connection source ips and proxy-allocated
// egresses) call this. It:
//  1. claims/refreshes the forward `verify_egress_<ip>` entry, atomically
//     downgrading the ip to ambiguous if another client already backs it,
//  2. records the ip in the client's reverse hash with an entry expiry, and
//  3. recomputes the client's eligible-set membership: eligible iff the
//     client has exactly one live egress ip, the forward entry for that ip
//     resolves back to the client, and the client has an active provide mode.
//
// Entries live `settings.EgressTtl` and must be re-fed while the egress holds
// (connected clients refresh every `EgressRefreshInterval`; proxy egresses are
// re-fed by `RefreshVerifyProxyEgress`), so a released or reassigned ip ages
// out and is never miscredited.
func FeedVerifyEgress(
	ctx context.Context,
	clientId server.Id,
	ip netip.Addr,
	settings *VerifySettings,
) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	nowMs := uint64(server.NowUtc().UnixMilli())
	ttlSeconds := int64(settings.EgressTtl / time.Second)

	server.Redis(ctx, func(r server.RedisClient) {
		// atomically claim or refresh the forward entry; downgrade to the
		// ambiguous marker if another client currently backs this ip. the
		// ambiguous marker keeps its ttl (not extended by feeds) so a
		// no-longer-contended ip heals after one ttl.
		claimScript := `local cur = redis.call('GET', KEYS[1])
if cur == false then redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2]) return 1
elseif cur == ARGV[1] then redis.call('EXPIRE', KEYS[1], ARGV[2]) return 1
elseif cur == ARGV[3] then return 0
else redis.call('SET', KEYS[1], ARGV[3], 'EX', ARGV[2]) return 0 end`
		r.Eval(
			ctx,
			claimScript,
			[]string{verifyEgressKey(ip)},
			clientId.String(),
			ttlSeconds,
			verifyEgressAmbiguous,
		)

		// record the ip in the client's reverse hash with a per-entry expiry
		expireMs := nowMs + uint64(settings.EgressTtl/time.Millisecond)
		clientEgressKey := verifyClientEgressKey(clientId)
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HSet(ctx, clientEgressKey, ip.String(), expireMs)
			pipe.Expire(ctx, clientEgressKey, settings.EgressTtl)
			return nil
		})
		server.Raise(err)
	})

	updateVerifyEligibleMembership(ctx, clientId)
}

// ClearVerifyEgress drops one (client, ip) egress entry — the disconnect-side
// of the observed-ip feeder. The forward entry is removed only if it still
// resolves to this client (never clobber another claimant or the ambiguous
// marker), then the client's eligible-set membership is recomputed.
func ClearVerifyEgress(
	ctx context.Context,
	clientId server.Id,
	ip netip.Addr,
) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	server.Redis(ctx, func(r server.RedisClient) {
		r.HDel(ctx, verifyClientEgressKey(clientId), ip.String())
		server.RedisRemoveIfEqual(r, ctx, verifyEgressKey(ip), []byte(clientId.String()))
	})
	updateVerifyEligibleMembership(ctx, clientId)
}

// RemoveVerifyEgressForClient drops all egress entries for a client — called
// when a client_id is reaped (`RemoveDisconnectedNetworkClients`) so a
// recycled ip is not miscredited to the previous holder (§8.2).
func RemoveVerifyEgressForClient(
	ctx context.Context,
	clientId server.Id,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		entries, err := r.HGetAll(ctx, verifyClientEgressKey(clientId)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for ipStr := range entries {
			if ip, err := netip.ParseAddr(ipStr); err == nil {
				server.RedisRemoveIfEqual(r, ctx, verifyEgressKey(ip), []byte(clientId.String()))
			}
		}
		r.Del(ctx, verifyClientEgressKey(clientId))
		r.SRem(ctx, verifyEligibleKey, clientId.String())
	})
}

// verifyLiveEgressIps reads the client's reverse hash and returns the ips
// whose entries have not expired, opportunistically deleting expired fields.
func verifyLiveEgressIps(
	ctx context.Context,
	r server.RedisClient,
	clientId server.Id,
	nowMs uint64,
) []string {
	entries, err := r.HGetAll(ctx, verifyClientEgressKey(clientId)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		server.Raise(err)
	}
	liveIps := []string{}
	expiredIps := []string{}
	for ipStr, expireMsStr := range entries {
		var expireMs uint64
		fmt.Sscanf(expireMsStr, "%d", &expireMs)
		if nowMs < expireMs {
			liveIps = append(liveIps, ipStr)
		} else {
			expiredIps = append(expiredIps, ipStr)
		}
	}
	if 0 < len(expiredIps) {
		r.HDel(ctx, verifyClientEgressKey(clientId), expiredIps...)
	}
	return liveIps
}

// updateVerifyEligibleMembership recomputes one client's membership in the
// eligible-provider set (§5.1): exactly one live egress ip, forward entry
// agreement, and a non-empty provide mode set. Membership is also lazily
// re-checked at sampling time, so a small staleness window here is safe.
func updateVerifyEligibleMembership(
	ctx context.Context,
	clientId server.Id,
) {
	nowMs := uint64(server.NowUtc().UnixMilli())

	eligible := false
	server.Redis(ctx, func(r server.RedisClient) {
		liveIps := verifyLiveEgressIps(ctx, r, clientId, nowMs)
		if len(liveIps) == 1 {
			if ip, err := netip.ParseAddr(liveIps[0]); err == nil {
				forward, err := r.Get(ctx, verifyEgressKey(ip)).Result()
				if err != nil && !errors.Is(err, redis.Nil) {
					server.Raise(err)
				}
				eligible = forward == clientId.String()
			}
		}
	})

	if eligible {
		// require an active provide mode (§5.1)
		provideModes, err := GetProvideModes(ctx, clientId)
		if err != nil || len(provideModes) == 0 {
			eligible = false
		}
	}

	server.Redis(ctx, func(r server.RedisClient) {
		if eligible {
			r.SAdd(ctx, verifyEligibleKey, clientId.String())
		} else {
			r.SRem(ctx, verifyEligibleKey, clientId.String())
		}
	})
}

// ResolveVerifyEgress maps a request source ip to the provider currently
// egressing from it (§8.1), or nil. Resolution requires the bijection both
// ways (§8.2): the forward entry names the client, and the client's reverse
// hash has exactly that ip live and no other. An ambiguous ip resolves to
// nothing — never guess. The lookup does the same number of redis round trips
// on hit and miss so it is not a timing oracle for "is this ip a provider"
// (§9).
func ResolveVerifyEgress(
	ctx context.Context,
	ip netip.Addr,
	settings *VerifySettings,
) (clientId *server.Id) {
	if !ip.IsValid() {
		return
	}
	ip = ip.Unmap()
	nowMs := uint64(server.NowUtc().UnixMilli())

	server.Redis(ctx, func(r server.RedisClient) {
		forward, err := r.Get(ctx, verifyEgressKey(ip)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}

		resolvedId, parseErr := server.ParseId(forward)
		if forward == "" || forward == verifyEgressAmbiguous || parseErr != nil {
			if settings.ResolveConstantTimePad {
				// pad the miss path to the same round-trip count as a hit
				r.HGetAll(ctx, verifyClientEgressKey(server.Id{})).Result()
			}
			return
		}

		liveIps := verifyLiveEgressIps(ctx, r, resolvedId, nowMs)
		if len(liveIps) == 1 && liveIps[0] == ip.String() {
			clientId = &resolvedId
		}
	})
	return
}

// RefreshVerifyProxyEgress re-feeds the egress index from the current
// `proxy_client` allocations (the proxy-side feeder's periodic refresh; the
// create-side feed in `CreateProxyClient` covers the gap until the first
// refresh). Rows deleted by the reaper stop being re-fed and age out within
// one `EgressTtl`.
func RefreshVerifyProxyEgress(ctx context.Context, settings *VerifySettings) {
	type proxyEgress struct {
		clientId server.Id
		ip       netip.Addr
	}
	proxyEgresses := []proxyEgress{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT client_id, client_ipv4
			FROM proxy_client
			WHERE client_ipv4 IS NOT NULL
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var clientIpv4 int64
				server.Raise(result.Scan(&clientId, &clientIpv4))
				proxyEgresses = append(proxyEgresses, proxyEgress{
					clientId: clientId,
					ip:       IntToIpv4(clientIpv4),
				})
			}
		})
	})
	for _, egress := range proxyEgresses {
		FeedVerifyEgress(ctx, egress.clientId, egress.ip, settings)
	}
}

// SampleVerifyNextHop draws the next hop uniformly at random from the
// eligible set, excluding `excludeClientIds` (the hops already in the trail
// and the validator's own client id), per §5.1. It validates each candidate
// (still a provider; spends an eligibility token §5.3) and returns the drawn
// hop plus `n`, the candidate-set size at the draw (recorded with the
// assignment so the assignment probability 1/n is known, §7.4).
//
// v1 approximation: `n` counts eligible-set members minus exclusions; members
// that fail token validation at the draw are still counted in `n` (tokens
// refill passively so set membership cannot track them). Members with no
// provide modes are lazily evicted from the set.
func SampleVerifyNextHop(
	ctx context.Context,
	excludeClientIds []server.Id,
	settings *VerifySettings,
) (nextHop *server.Id, n int) {
	server.Redis(ctx, func(r server.RedisClient) {
		total, err := r.SCard(ctx, verifyEligibleKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}

		excluded := map[string]bool{}
		excludedInSet := 0
		for _, excludeClientId := range excludeClientIds {
			member := excludeClientId.String()
			if excluded[member] {
				continue
			}
			excluded[member] = true
			isMember, err := r.SIsMember(ctx, verifyEligibleKey, member).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			if isMember {
				excludedInSet += 1
			}
		}
		n = int(total) - excludedInSet
		if n <= 0 {
			n = 0
			return
		}

		for attempt := 0; attempt < settings.SampleMaxAttempts; attempt += 1 {
			candidates, err := r.SRandMemberN(ctx, verifyEligibleKey, int64(settings.SampleCandidateCount)).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			allowed := []string{}
			for _, candidate := range candidates {
				if !excluded[candidate] {
					allowed = append(allowed, candidate)
				}
			}
			if len(allowed) == 0 {
				continue
			}
			// final pick with a csprng (§5.1)
			pickIndex, err := rand.Int(rand.Reader, big.NewInt(int64(len(allowed))))
			server.Raise(err)
			candidate := allowed[pickIndex.Int64()]

			candidateId, err := server.ParseId(candidate)
			if err != nil {
				r.SRem(ctx, verifyEligibleKey, candidate)
				continue
			}
			provideModes, err := GetProvideModes(ctx, candidateId)
			if err != nil || len(provideModes) == 0 {
				// no longer a provider: lazily evict
				r.SRem(ctx, verifyEligibleKey, candidate)
				excluded[candidate] = true
				continue
			}
			if !SpendVerifyEligibilityToken(ctx, candidateId, settings) {
				// token exhausted: temporarily unassignable, try another
				excluded[candidate] = true
				continue
			}
			nextHop = &candidateId
			return
		}
	})
	return
}

// PadVerifySample performs the read/write pattern of `SampleVerifyNextHop`
// against scratch keys, without touching any real provider's tokens, stats,
// or set membership. Poison trails call this instead of sampling so the
// response latency of the two paths stays close (§9 indistinguishability).
// Returns the eligible-set size, which poison trails record as `assign_n` for
// state realism.
func PadVerifySample(
	ctx context.Context,
	settings *VerifySettings,
) (n int) {
	server.Redis(ctx, func(r server.RedisClient) {
		total, err := r.SCard(ctx, verifyEligibleKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		n = int(total)
		r.SRandMemberN(ctx, verifyEligibleKey, int64(settings.SampleCandidateCount)).Result()
		// mimic the candidate provide-mode read and token spend
		var zeroId server.Id
		r.Get(ctx, provideModesKey(zeroId)).Result()
		if 0 < settings.EligibilityInterval {
			// only when the bucket is on (D26) does the real path
			// (SpendVerifyEligibilityToken) write a token — keep the poison pad
			// shaped like the real path so the two stay timing-close (§9)
			_, err = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				pipe.Incr(ctx, verifyEligibilityTokenKey(zeroId))
				pipe.Expire(ctx, verifyEligibilityTokenKey(zeroId), settings.EligibilityInterval)
				return nil
			})
			server.Raise(err)
		}
	})
	return
}

// IncrVerifyPoisonCounter counts poison-trail steps (`assign`, `confirm`) on
// scratch counters — an ops signal for probing volume, and a write that pads
// the poison path to the same shape as the real stats writes (§9).
func IncrVerifyPoisonCounter(
	ctx context.Context,
	name string,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		key := fmt.Sprintf("verify_poison_%s", name)
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Incr(ctx, key)
			pipe.Expire(ctx, key, 24*time.Hour)
			return nil
		})
		server.Raise(err)
	})
}

// SpendVerifyEligibilityToken spends one eligibility token for the provider
// (§5.3): at most `EligibilityBurst` assignments per `EligibilityInterval`,
// implemented with the INCR+EXPIRE window idiom (connect/transport_rate_limit).
func SpendVerifyEligibilityToken(
	ctx context.Context,
	clientId server.Id,
	settings *VerifySettings,
) (spent bool) {
	// D26: EligibilityInterval <= 0 turns the token bucket OFF — every provider
	// is always assignable (no gating), so each validator drives its own
	// measurement rate. A positive interval restores the §5.3 bucket.
	if settings.EligibilityInterval <= 0 {
		return true
	}
	server.Redis(ctx, func(r server.RedisClient) {
		var countCmd *redis.IntCmd
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			countCmd = pipe.Incr(ctx, verifyEligibilityTokenKey(clientId))
			pipe.Expire(ctx, verifyEligibilityTokenKey(clientId), settings.EligibilityInterval)
			return nil
		})
		server.Raise(err)
		count, err := countCmd.Result()
		server.Raise(err)
		spent = count <= int64(settings.EligibilityBurst)
	})
	return
}

// CheckVerifyEligibilityToken reports whether the provider currently holds a
// token, without spending one. Used for the seed hop (§4.1 step 4), which is
// validator-chosen and not an assignment.
func CheckVerifyEligibilityToken(
	ctx context.Context,
	clientId server.Id,
	settings *VerifySettings,
) (held bool) {
	// D26: interval <= 0 disables the bucket — the seed hop always holds a token.
	if settings.EligibilityInterval <= 0 {
		return true
	}
	server.Redis(ctx, func(r server.RedisClient) {
		countStr, err := r.Get(ctx, verifyEligibilityTokenKey(clientId)).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				held = true
				return
			}
			server.Raise(err)
		}
		var count int64
		fmt.Sscanf(countStr, "%d", &count)
		held = count < int64(settings.EligibilityBurst)
	})
	return
}

// IncrVerifySeedRates bumps and returns the per-source-ip and per-vpk seed
// counters for the current rate window (§9). The caller compares against the
// soft (poison) and hard (refuse) limits.
func IncrVerifySeedRates(
	ctx context.Context,
	clientIp string,
	vpk []byte,
	settings *VerifySettings,
) (ipCount int64, vpkCount int64) {
	ipHash, err := server.ClientIpHash(clientIp)
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		incrWindow := func(key string) int64 {
			var countCmd *redis.IntCmd
			_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
				countCmd = pipe.Incr(ctx, key)
				pipe.Expire(ctx, key, settings.SeedRateWindow)
				return nil
			})
			server.Raise(err)
			count, err := countCmd.Result()
			server.Raise(err)
			return count
		}
		ipCount = incrWindow(verifySeedIpRateKey(ipHash))
		vpkCount = incrWindow(verifySeedVpkRateKey(vpk))
	})
	return
}

// IncrVerifyExtendRate bumps and returns the per-source-ip EXTEND counter for
// the current rate window (V4). EXTEND is otherwise unmetered and drives the
// mutating read-modify-write path, so this is its per-source-ip DoS bound,
// complementing `IncrVerifySeedRates`. There is deliberately no per-vpk
// companion: vpk is unauthenticated caller input, so a per-vpk cap is
// best-effort only and cannot be a security control (the source ip is the
// nginx-forced `$remote_addr`, §8.1/V11, so it is the real limit).
func IncrVerifyExtendRate(
	ctx context.Context,
	clientIp string,
	settings *VerifySettings,
) (ipCount int64) {
	ipHash, err := server.ClientIpHash(clientIp)
	server.Raise(err)
	server.Redis(ctx, func(r server.RedisClient) {
		var countCmd *redis.IntCmd
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			countCmd = pipe.Incr(ctx, verifyExtendIpRateKey(ipHash))
			pipe.Expire(ctx, verifyExtendIpRateKey(ipHash), settings.SeedRateWindow)
			return nil
		})
		server.Raise(err)
		ipCount, err = countCmd.Result()
		server.Raise(err)
	})
	return
}

// IncrVerifyActiveTrails counts a new active trail against the vpk's
// concurrent-trail cap (§9). The counter is decremented on every terminal
// transition and carries a ttl so any drift heals.
func IncrVerifyActiveTrails(
	ctx context.Context,
	vpk []byte,
	settings *VerifySettings,
) (count int64) {
	maxTtl := 2 * verifyMaxTrailTtl(settings)
	server.Redis(ctx, func(r server.RedisClient) {
		var countCmd *redis.IntCmd
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			countCmd = pipe.Incr(ctx, verifyActiveTrailsKey(vpk))
			pipe.Expire(ctx, verifyActiveTrailsKey(vpk), maxTtl)
			return nil
		})
		server.Raise(err)
		var err2 error
		count, err2 = countCmd.Result()
		server.Raise(err2)
	})
	return
}

// verifyMaxTrailTtl is the trail ttl at the maximum clamped depth, used to
// bound counter lifetimes.
func verifyMaxTrailTtl(settings *VerifySettings) time.Duration {
	// MMax from the wire spec is 16; avoid importing connect here
	return 16*settings.StepTimeout + settings.TrailTtlGrace
}

// DecrVerifyActiveTrails releases one active-trail slot, flooring at zero so
// double-decrements cannot open the cap.
func DecrVerifyActiveTrails(
	ctx context.Context,
	vpk []byte,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		script := `local v = redis.call('DECR', KEYS[1]) if v < 0 then redis.call('SET', KEYS[1], 0, 'KEEPTTL') end return v`
		r.Eval(ctx, script, []string{verifyActiveTrailsKey(vpk)})
	})
}

// verifyMarshalHop panics on marshal failure (hop records are always
// marshalable).
func verifyMarshalHop(hop *VerifyTrailHop) string {
	hopJson, err := json.Marshal(hop)
	server.Raise(err)
	return string(hopJson)
}

func verifyUnmarshalHop(hopJson string) (*VerifyTrailHop, error) {
	var hop VerifyTrailHop
	err := json.Unmarshal([]byte(hopJson), &hop)
	if err != nil {
		return nil, err
	}
	return &hop, nil
}

// CreateVerifyTrail persists a newly seeded trail (§4.1 step 7): header, seed
// hop, pending hop, and the cached response, all under the `{vtr_<trailId>}`
// hash tag with `EXPIRE = TrailTtl(m)`, plus the reap-deadline registry entry.
func CreateVerifyTrail(
	ctx context.Context,
	trail *VerifyTrail,
	responseJson string,
	settings *VerifySettings,
) {
	ttl := settings.TrailTtl(trail.M)
	poison := "0"
	if trail.Poison {
		poison = "1"
	}
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			headerKey := verifyTrailHeaderKey(trail.TrailId)
			pipe.HSet(
				ctx,
				headerKey,
				"client_id", trail.ClientId.String(),
				"vpk", string(trail.Vpk),
				"server_nonce", string(trail.ServerNonce),
				"m", trail.M,
				"server_key_id", int(trail.ServerKeyId),
				"status", trail.Status,
				"poison", poison,
				"create_ms", trail.CreateMs,
				"activity_ms", trail.ActivityMs,
			)
			pipe.Expire(ctx, headerKey, ttl)
			for _, hop := range trail.Hops {
				pipe.RPush(ctx, verifyTrailHopsKey(trail.TrailId), verifyMarshalHop(hop))
			}
			pipe.Expire(ctx, verifyTrailHopsKey(trail.TrailId), ttl)
			pipe.Set(ctx, verifyTrailPendingKey(trail.TrailId), verifyMarshalHop(trail.Pending), ttl)
			pipe.Set(ctx, verifyTrailResponseKey(trail.TrailId), responseJson, ttl)
			return nil
		})
		server.Raise(err)

		// the reap registry is a different slot, so it cannot join the
		// pipeline; if this write is lost the trail still ages out via ttl
		deadlineMs := float64(trail.Pending.AssignedMs) + float64((settings.StepTimeout+settings.StepTimeoutGrace)/time.Millisecond)
		err = r.ZAdd(ctx, verifyReapKey, redis.Z{
			Score:  deadlineMs,
			Member: trail.TrailId.String(),
		}).Err()
		server.Raise(err)
	})
}

// AcquireVerifyTrailLock takes the short-lived per-trail mutation lock via
// SETNX (V3), serializing the EXTEND read-modify-write
// (`GetVerifyTrail`…`ConfirmVerifyHopAndAssign`/`CompleteVerifyTrail`) so two
// concurrent identical EXTENDs cannot both confirm the same pending hop — which
// would double-count `c_Y` (breaking the Wilson `r_Y>1` invariant), corrupt the
// hop list (e.g. `[A,B,B]`), and stuff the latency histogram. Returns whether
// the lock was acquired; on contention the caller returns 409 and the client
// retries idempotently (the retry hits the §4.3 replay branch once the holder
// commits). The lock self-heals via `ttl` if the holder crashes.
func AcquireVerifyTrailLock(
	ctx context.Context,
	trailId server.Id,
	ttl time.Duration,
) (acquired bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		ok, err := r.SetNX(ctx, verifyTrailLockKey(trailId), "1", ttl).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		acquired = ok
	})
	return
}

// ReleaseVerifyTrailLock drops the per-trail mutation lock (V3). Safe to call
// even if the lock already ttl-expired (DEL of a missing key is a no-op).
func ReleaseVerifyTrailLock(
	ctx context.Context,
	trailId server.Id,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		r.Del(ctx, verifyTrailLockKey(trailId))
	})
}

// GetVerifyTrail loads a trail's full state, or nil if the trail does not
// exist (never created, or ttl-expired).
func GetVerifyTrail(
	ctx context.Context,
	trailId server.Id,
) (trail *VerifyTrail) {
	server.Redis(ctx, func(r server.RedisClient) {
		var headerCmd *redis.MapStringStringCmd
		var hopsCmd *redis.StringSliceCmd
		var pendingCmd *redis.StringCmd
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			headerCmd = pipe.HGetAll(ctx, verifyTrailHeaderKey(trailId))
			hopsCmd = pipe.LRange(ctx, verifyTrailHopsKey(trailId), 0, -1)
			pendingCmd = pipe.Get(ctx, verifyTrailPendingKey(trailId))
			return nil
		})
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}

		header, err := headerCmd.Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		if len(header) == 0 {
			return
		}

		clientId, err := server.ParseId(header["client_id"])
		if err != nil {
			return
		}
		t := &VerifyTrail{
			TrailId:     trailId,
			ClientId:    clientId,
			Vpk:         []byte(header["vpk"]),
			ServerNonce: []byte(header["server_nonce"]),
			Status:      header["status"],
			Poison:      header["poison"] == "1",
		}
		fmt.Sscanf(header["m"], "%d", &t.M)
		fmt.Sscanf(header["create_ms"], "%d", &t.CreateMs)
		fmt.Sscanf(header["activity_ms"], "%d", &t.ActivityMs)
		var serverKeyId int
		fmt.Sscanf(header["server_key_id"], "%d", &serverKeyId)
		t.ServerKeyId = byte(serverKeyId)

		hopJsons, err := hopsCmd.Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		for _, hopJson := range hopJsons {
			hop, err := verifyUnmarshalHop(hopJson)
			if err != nil {
				return
			}
			t.Hops = append(t.Hops, hop)
		}

		pendingJson, err := pendingCmd.Result()
		if err == nil && pendingJson != "" {
			pending, err := verifyUnmarshalHop(pendingJson)
			if err != nil {
				return
			}
			t.Pending = pending
		} else if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}

		trail = t
	})
	return
}

// GetVerifyTrailResponse returns the cached last response for idempotent
// retries (§4.3), or ok=false if none.
func GetVerifyTrailResponse(
	ctx context.Context,
	trailId server.Id,
) (responseJson string, ok bool) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, verifyTrailResponseKey(trailId)).Result()
		if err != nil {
			if errors.Is(err, redis.Nil) {
				return
			}
			server.Raise(err)
		}
		responseJson = value
		ok = true
	})
	return
}

// ConfirmVerifyHopAndAssign advances one step (§4.2 step 6 + the else branch
// of step 7): appends the confirmed hop, installs the new pending hop,
// refreshes activity and ttl, caches the response, and moves the reap
// deadline to the new assignment.
func ConfirmVerifyHopAndAssign(
	ctx context.Context,
	trail *VerifyTrail,
	confirmedHop *VerifyTrailHop,
	newPending *VerifyTrailHop,
	responseJson string,
	settings *VerifySettings,
) {
	ttl := settings.TrailTtl(trail.M)
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.RPush(ctx, verifyTrailHopsKey(trail.TrailId), verifyMarshalHop(confirmedHop))
			pipe.Expire(ctx, verifyTrailHopsKey(trail.TrailId), ttl)
			pipe.Set(ctx, verifyTrailPendingKey(trail.TrailId), verifyMarshalHop(newPending), ttl)
			pipe.HSet(ctx, verifyTrailHeaderKey(trail.TrailId), "activity_ms", confirmedHop.ConfirmedMs)
			pipe.Expire(ctx, verifyTrailHeaderKey(trail.TrailId), ttl)
			pipe.Set(ctx, verifyTrailResponseKey(trail.TrailId), responseJson, ttl)
			return nil
		})
		server.Raise(err)

		deadlineMs := float64(newPending.AssignedMs) + float64((settings.StepTimeout+settings.StepTimeoutGrace)/time.Millisecond)
		err = r.ZAdd(ctx, verifyReapKey, redis.Z{
			Score:  deadlineMs,
			Member: trail.TrailId.String(),
		}).Err()
		server.Raise(err)
	})
}

// CompleteVerifyTrail marks the trail complete (§4.2 step 7): appends the
// final confirmed hop, clears pending, caches the final response for
// idempotent replay, and removes the reap entry. State lingers until ttl so
// retries of the final EXTEND replay the proof.
func CompleteVerifyTrail(
	ctx context.Context,
	trail *VerifyTrail,
	finalHop *VerifyTrailHop,
	responseJson string,
	settings *VerifySettings,
) {
	ttl := settings.TrailTtl(trail.M)
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.RPush(ctx, verifyTrailHopsKey(trail.TrailId), verifyMarshalHop(finalHop))
			pipe.Expire(ctx, verifyTrailHopsKey(trail.TrailId), ttl)
			pipe.Del(ctx, verifyTrailPendingKey(trail.TrailId))
			pipe.HSet(
				ctx,
				verifyTrailHeaderKey(trail.TrailId),
				"status", VerifyTrailStatusComplete,
				"activity_ms", finalHop.ConfirmedMs,
			)
			pipe.Expire(ctx, verifyTrailHeaderKey(trail.TrailId), ttl)
			pipe.Set(ctx, verifyTrailResponseKey(trail.TrailId), responseJson, ttl)
			return nil
		})
		server.Raise(err)
		err = r.ZRem(ctx, verifyReapKey, trail.TrailId.String()).Err()
		server.Raise(err)
	})
}

// ExpireVerifyTrail marks the trail expired (§4.4) and removes the reap
// entry. The failure is already attributed: the pending hop's assignment was
// counted at ASSIGN time and never gets a confirmation (§7.2).
func ExpireVerifyTrail(
	ctx context.Context,
	trailId server.Id,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		r.HSet(ctx, verifyTrailHeaderKey(trailId), "status", VerifyTrailStatusExpired)
		err := r.ZRem(ctx, verifyReapKey, trailId.String()).Err()
		server.Raise(err)
	})
}

// VerifyStatsPeriodStart truncates `now` to the containing rollup period.
func VerifyStatsPeriodStart(now time.Time, settings *VerifySettings) time.Time {
	return now.UTC().Truncate(settings.StatsPeriod)
}

// RecordVerifyAssignment counts one server assignment for a provider (§7.2
// exposure `a_Y`), in the current period's stats hash.
func RecordVerifyAssignment(
	ctx context.Context,
	clientId server.Id,
	now time.Time,
	settings *VerifySettings,
) {
	statsKey := verifyStatsKey(clientId, VerifyStatsPeriodStart(now, settings))
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HIncrBy(ctx, statsKey, "assignments", 1)
			pipe.Expire(ctx, statsKey, 3*settings.StatsPeriod)
			return nil
		})
		server.Raise(err)
		err = r.SAdd(ctx, verifyStatClientsKey, clientId.String()).Err()
		server.Raise(err)
	})
}

// RecordVerifyConfirmation counts one confirmed step and its latency sample
// (§7.2 `c_Y`, §7.5 per-step latency), in the current period's stats hash.
func RecordVerifyConfirmation(
	ctx context.Context,
	clientId server.Id,
	latencyMs int64,
	now time.Time,
	settings *VerifySettings,
) {
	statsKey := verifyStatsKey(clientId, VerifyStatsPeriodStart(now, settings))
	bucketField := fmt.Sprintf("lb_%d", VerifyLatencyBucketIndex(latencyMs))
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.HIncrBy(ctx, statsKey, "confirmations", 1)
			pipe.HIncrBy(ctx, statsKey, bucketField, 1)
			pipe.Expire(ctx, statsKey, 3*settings.StatsPeriod)
			return nil
		})
		server.Raise(err)
		err = r.SAdd(ctx, verifyStatClientsKey, clientId.String()).Err()
		server.Raise(err)
	})
}

// latency histogram: log-spaced buckets, `verifyLatencyBucketsPerOctave`
// buckets per doubling of latency. Percentiles recovered from bucket counts
// are exact to within one bucket width (≤ ~19% relative error), which is
// well inside the noise for network latency and keeps the per-provider
// footprint to a bounded hash instead of a sample reservoir.
const verifyLatencyBucketsPerOctave = 4

// verifyLatencyMaxBucketIndex caps the histogram at ~2^20 ms (~17 min), far
// beyond any StepTimeout.
const verifyLatencyMaxBucketIndex = 80

// VerifyLatencyBucketIndex maps a latency in ms to its histogram bucket.
func VerifyLatencyBucketIndex(latencyMs int64) int {
	if latencyMs < 1 {
		latencyMs = 1
	}
	index := int(math.Floor(float64(verifyLatencyBucketsPerOctave) * math.Log2(float64(latencyMs))))
	if verifyLatencyMaxBucketIndex < index {
		index = verifyLatencyMaxBucketIndex
	}
	return index
}

// VerifyLatencyBucketMs returns the representative latency (geometric bucket
// midpoint) for a bucket index.
func VerifyLatencyBucketMs(index int) int {
	return int(math.Round(math.Pow(2, (float64(index)+0.5)/float64(verifyLatencyBucketsPerOctave))))
}

// VerifyLatencyPercentiles recovers p50/p90/p99 from histogram bucket counts.
// Returns nils when there are no samples.
func VerifyLatencyPercentiles(bucketCounts map[int]int64) (p50 *int, p90 *int, p99 *int) {
	var total int64
	maxIndex := 0
	for index, count := range bucketCounts {
		total += count
		if maxIndex < index {
			maxIndex = index
		}
	}
	if total == 0 {
		return
	}
	percentile := func(q float64) *int {
		rank := int64(math.Ceil(q * float64(total)))
		if rank < 1 {
			rank = 1
		}
		var cumulative int64
		for index := 0; index <= maxIndex; index += 1 {
			cumulative += bucketCounts[index]
			if rank <= cumulative {
				value := VerifyLatencyBucketMs(index)
				return &value
			}
		}
		value := VerifyLatencyBucketMs(maxIndex)
		return &value
	}
	p50 = percentile(0.50)
	p90 = percentile(0.90)
	p99 = percentile(0.99)
	return
}

// VerifyTrailRow is one durable `verify_trail` record (VALIDATOR.md §6.2).
type VerifyTrailRow struct {
	TrailId      server.Id
	Vpk          []byte
	ServerKeyId  byte
	ServerNonce  []byte
	Depth        int
	Status       int
	HopsJson     string
	FinalSig     []byte
	VerifierSig  []byte
	CreateTime   time.Time
	CompleteTime *time.Time
}

// InsertVerifyTrail durably persists a completed or expired trail (never a
// poison trail, §9). Idempotent on trail_id so retries and the
// reaper/inline-failure race cannot double-insert.
func InsertVerifyTrail(
	ctx context.Context,
	row *VerifyTrailRow,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			INSERT INTO verify_trail (
				trail_id,
				vpk,
				server_key_id,
				server_nonce,
				depth,
				status,
				hops_json,
				final_sig,
				verifier_sig,
				create_time,
				complete_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
			ON CONFLICT (trail_id) DO NOTHING
			`,
			row.TrailId,
			row.Vpk,
			int16(row.ServerKeyId),
			row.ServerNonce,
			row.Depth,
			row.Status,
			row.HopsJson,
			row.FinalSig,
			row.VerifierSig,
			row.CreateTime.UTC(),
			row.CompleteTime,
		))
	})
}

// GetVerifyTrailRow loads one `verify_trail` record, or nil.
func GetVerifyTrailRow(
	ctx context.Context,
	trailId server.Id,
) (row *VerifyTrailRow) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				vpk,
				server_key_id,
				server_nonce,
				depth,
				status,
				hops_json,
				final_sig,
				verifier_sig,
				create_time,
				complete_time
			FROM verify_trail
			WHERE trail_id = $1
			`,
			trailId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				r := &VerifyTrailRow{
					TrailId: trailId,
				}
				var serverKeyId int16
				server.Raise(result.Scan(
					&r.Vpk,
					&serverKeyId,
					&r.ServerNonce,
					&r.Depth,
					&r.Status,
					&r.HopsJson,
					&r.FinalSig,
					&r.VerifierSig,
					&r.CreateTime,
					&r.CompleteTime,
				))
				r.ServerKeyId = byte(serverKeyId)
				row = r
			}
		})
	})
	return
}

// SweepExpiredVerifyTrails is the trail reaper (§4.4, §6.1): it walks the
// reap registry for trails whose pending step deadline has passed, marks them
// expired, and persists the expired record for real (non-poison) trails. The
// failure is attributed to the pending hop by construction: its assignment
// was counted at ASSIGN time and no confirmation ever arrives (§7.2). Returns
// the number of trails expired.
func SweepExpiredVerifyTrails(
	ctx context.Context,
	now time.Time,
	settings *VerifySettings,
) (sweptCount int) {
	nowMs := uint64(now.UnixMilli())

	var trailIdStrs []string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		trailIdStrs, err = r.ZRangeByScore(ctx, verifyReapKey, &redis.ZRangeBy{
			Min:    "-inf",
			Max:    fmt.Sprintf("%d", nowMs),
			Offset: 0,
			Count:  int64(settings.SweepLimit),
		}).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
	})

	for _, trailIdStr := range trailIdStrs {
		trailId, err := server.ParseId(trailIdStr)
		if err != nil {
			server.Redis(ctx, func(r server.RedisClient) {
				r.ZRem(ctx, verifyReapKey, trailIdStr)
			})
			continue
		}

		trail := GetVerifyTrail(ctx, trailId)
		if trail == nil || trail.Status != VerifyTrailStatusActive {
			// ttl-expired or already terminal: drop the registry entry
			server.Redis(ctx, func(r server.RedisClient) {
				r.ZRem(ctx, verifyReapKey, trailIdStr)
			})
			continue
		}

		if trail.Pending != nil {
			deadlineMs := trail.Pending.AssignedMs + uint64((settings.StepTimeout+settings.StepTimeoutGrace)/time.Millisecond)
			if nowMs < deadlineMs {
				// activity since the registry score was written: re-score
				server.Redis(ctx, func(r server.RedisClient) {
					r.ZAdd(ctx, verifyReapKey, redis.Z{
						Score:  float64(deadlineMs),
						Member: trailIdStr,
					})
				})
				continue
			}
		}

		ExpireVerifyTrail(ctx, trailId)
		DecrVerifyActiveTrails(ctx, trail.Vpk)
		if !trail.Poison {
			InsertVerifyTrail(ctx, NewExpiredVerifyTrailRow(trail))
		}
		sweptCount += 1
		if glog.V(1) {
			glog.Infof("[verify]reaped expired trail %s (depth %d, poison=%t)\n", trailId, len(trail.Hops), trail.Poison)
		}
	}
	return
}

// NewExpiredVerifyTrailRow builds the durable expired record for a trail
// (§6.2: depth = confirmed hops, final_sig/verifier_sig null).
func NewExpiredVerifyTrailRow(trail *VerifyTrail) *VerifyTrailRow {
	type hopJson struct {
		ClientId server.Id `json:"client_id"`
		TimeMs   uint64    `json:"time_ms"`
	}
	hops := []hopJson{}
	for _, hop := range trail.Hops {
		hops = append(hops, hopJson{
			ClientId: hop.ClientId,
			TimeMs:   hop.ConfirmedMs,
		})
	}
	hopsJsonBytes, err := json.Marshal(hops)
	server.Raise(err)
	return &VerifyTrailRow{
		TrailId:     trail.TrailId,
		Vpk:         trail.Vpk,
		ServerKeyId: trail.ServerKeyId,
		ServerNonce: trail.ServerNonce,
		Depth:       len(trail.Hops),
		Status:      VerifyTrailRowStatusExpired,
		HopsJson:    string(hopsJsonBytes),
		CreateTime:  time.UnixMilli(int64(trail.CreateMs)).UTC(),
	}
}

// VerifyProviderStatsRow is one `verify_provider_stats` rollup row. The
// schema is consumed by the subnet scoring pipeline and must not change
// shape: (period_start, period_end, client_id, assignments, confirmations,
// latency_p50_ms, latency_p90_ms, latency_p99_ms).
type VerifyProviderStatsRow struct {
	PeriodStart   time.Time
	PeriodEnd     time.Time
	ClientId      server.Id
	Assignments   int64
	Confirmations int64
	LatencyP50Ms  *int
	LatencyP90Ms  *int
	LatencyP99Ms  *int
}

// RollupVerifyProviderStats drains the per-provider redis stats into
// `verify_provider_stats` (§7). For each client with pending stats it reads
// the current and previous period hashes and upserts absolute per-period
// values, so the rollup is idempotent and can run any number of times per
// period. Clients whose period hashes have fully aged out are removed from
// the pending set.
func RollupVerifyProviderStats(
	ctx context.Context,
	now time.Time,
	settings *VerifySettings,
) {
	var clientIdStrs []string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		clientIdStrs, err = r.SMembers(ctx, verifyStatClientsKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
	})

	currentPeriodStart := VerifyStatsPeriodStart(now, settings)
	periodStarts := []time.Time{
		currentPeriodStart.Add(-settings.StatsPeriod),
		currentPeriodStart,
	}

	for _, clientIdStr := range clientIdStrs {
		clientId, err := server.ParseId(clientIdStr)
		if err != nil {
			server.Redis(ctx, func(r server.RedisClient) {
				r.SRem(ctx, verifyStatClientsKey, clientIdStr)
			})
			continue
		}

		rows := []*VerifyProviderStatsRow{}
		anyPeriodPresent := false
		for _, periodStart := range periodStarts {
			var stats map[string]string
			server.Redis(ctx, func(r server.RedisClient) {
				var err error
				stats, err = r.HGetAll(ctx, verifyStatsKey(clientId, periodStart)).Result()
				if err != nil && !errors.Is(err, redis.Nil) {
					server.Raise(err)
				}
			})
			if len(stats) == 0 {
				continue
			}
			anyPeriodPresent = true

			row := &VerifyProviderStatsRow{
				PeriodStart: periodStart,
				PeriodEnd:   periodStart.Add(settings.StatsPeriod),
				ClientId:    clientId,
			}
			bucketCounts := map[int]int64{}
			for field, valueStr := range stats {
				var value int64
				fmt.Sscanf(valueStr, "%d", &value)
				switch field {
				case "assignments":
					row.Assignments = value
				case "confirmations":
					row.Confirmations = value
				default:
					var bucketIndex int
					if _, err := fmt.Sscanf(field, "lb_%d", &bucketIndex); err == nil {
						bucketCounts[bucketIndex] += value
					}
				}
			}
			row.LatencyP50Ms, row.LatencyP90Ms, row.LatencyP99Ms = VerifyLatencyPercentiles(bucketCounts)
			rows = append(rows, row)
		}

		if 0 < len(rows) {
			UpsertVerifyProviderStats(ctx, rows)
		}
		if !anyPeriodPresent {
			// both windows aged out: nothing more to roll up for this client
			server.Redis(ctx, func(r server.RedisClient) {
				r.SRem(ctx, verifyStatClientsKey, clientIdStr)
			})
		}
	}
}

// UpsertVerifyProviderStats writes rollup rows, overwriting the row for
// (period_start, client_id) with the latest absolute per-period values.
func UpsertVerifyProviderStats(
	ctx context.Context,
	rows []*VerifyProviderStatsRow,
) {
	if len(rows) == 0 {
		return
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, row := range rows {
				batch.Queue(
					`
					INSERT INTO verify_provider_stats (
						period_start,
						period_end,
						client_id,
						assignments,
						confirmations,
						latency_p50_ms,
						latency_p90_ms,
						latency_p99_ms
					)
					VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
					ON CONFLICT (period_start, client_id) DO UPDATE
					SET
						period_end = $2,
						assignments = $4,
						confirmations = $5,
						latency_p50_ms = $6,
						latency_p90_ms = $7,
						latency_p99_ms = $8
					`,
					row.PeriodStart.UTC(),
					row.PeriodEnd.UTC(),
					row.ClientId,
					row.Assignments,
					row.Confirmations,
					row.LatencyP50Ms,
					row.LatencyP90Ms,
					row.LatencyP99Ms,
				)
			}
		})
	})
}

// GetVerifyProviderStats loads all rollup rows for a client in ascending
// period order.
func GetVerifyProviderStats(
	ctx context.Context,
	clientId server.Id,
) (rows []*VerifyProviderStatsRow) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				period_start,
				period_end,
				assignments,
				confirmations,
				latency_p50_ms,
				latency_p90_ms,
				latency_p99_ms
			FROM verify_provider_stats
			WHERE client_id = $1
			ORDER BY period_start
			`,
			clientId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &VerifyProviderStatsRow{
					ClientId: clientId,
				}
				server.Raise(result.Scan(
					&row.PeriodStart,
					&row.PeriodEnd,
					&row.Assignments,
					&row.Confirmations,
					&row.LatencyP50Ms,
					&row.LatencyP90Ms,
					&row.LatencyP99Ms,
				))
				rows = append(rows, row)
			}
		})
	})
	return
}
