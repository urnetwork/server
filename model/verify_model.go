package model

// verify_model.go — state model for the `/verify` routing-verification
// protocol (sn/VALIDATOR.md §§5-8). Redis holds hot per-trail/provider state.
// PostgreSQL retains each signed transition before publication, terminal proofs
// and the derived provider-stat projection. Missing historical originals remain
// unknown rather than being reconstructed from that mutable projection.
//
// Redis key map (cluster rule: keys touched in one pipeline share a `{...}`
// hash tag):
//
//	{vtr_<trailId>}h        trail header hash (vpk, server_nonce, m, status, ...)
//	{vtr_<trailId>}hops     list of confirmed hop records (json), seed first
//	{vtr_<trailId>}pending  the pending hop record (json) awaiting arrival
//	{vtr_<trailId>}resp     last response (json) for idempotent retries (§4.3)
//	verify_reap             zset trailId -> step deadline unix ms (reaper index)
//	verify_egress_v2_<ip>   address index (an exact address, or an IPv6 /64): ip -> clientId, or `!` if ambiguous (§8.1)
//	{vce4_<clientId>}       reverse address hash: <ip>_4 or <ip>_6_<its /64> ->
//	                        entry expiry unix ms _ the address's egress-IP-hash (§8.2)
//	{vce4_<clientId>}leases_<ip>  reverse connection/proxy owner -> expiry
//	{vce2_<clientId>}       v2 reverse hash: exact ip -> expiry; read and withdrawn, never fed
//	{verify_egress_v2_<ip>}leases forward client/owner -> expiry
//	{vhead_<clientId>}<window> egress-IP-hash a provider's hops record in one scoring window (§8.1)
//	{vtr_<trailId>}window   the scoring window the trail was seeded in
//	verify_eligible_v2      set of currently eligible provider clientIds (§5.1)
//	{velig_<clientId>}      eligibility token spend counter (INCR+EXPIRE, §5.3)
//	{vstat_<clientId>}p<t>  per-period stats hash: assignments, confirmations,
//	                        log-spaced latency buckets lb_<i> (§7)
//	verify_stat_clients     set of clientIds with stats pending rollup
//	verify_seed_ip_<hash>_<window> seed fixed-window counter per source ip (§9)
//	verify_seed_vpk_<vpk>_<window> seed fixed-window counter per vpk (§9)
//	verify_{seed,extend}_provider_<clientId>_<window> counters per resolved provider (§9)
//	verify_extend_pad_<ip hash>_<window> scratch counter an unresolved EXTEND source pays (§9)
//	{vtr_<trailId>}unattributed_<depth> retryable answers to one step's unattributed
//	                        source: their count, and the last one's unix ms
//	verify_seed_unattributed_<sig hash> retryable answers to one SEED's unresolved source
//	verify_trails_<vpk>     active trail count per vpk (concurrent-trail cap, §9)
//	{verify_cohort:<deployment>}e<epoch>       providers admitted for one settlement epoch
//	{verify_cohort:<deployment>}e<epoch>draws  next hops drawn from that cohort
//	{verify_cohort:<deployment>}lifetime       every provider ever admitted (no ttl)
//
// The egress index is fed by one bijection-gated feeder
// (`feedVerifyEgressLease`, applied a chunk at a time by the proxy refresh's
// `feedVerifyProxyEgresses`) with two independently owned sources: observed connection source ips
// (connect/transport_announce.go) and proxy-allocated egresses (the API
// controller's post-allocation feed + periodic `RefreshVerifyProxyEgress`). The §8.2
// invariant — one provider ⇄ one egress ip per address family, an IPv6 /64
// counting as one ip — is enforced at write time (an ip observed backing a
// second client becomes ambiguous and resolves to nothing) and re-checked at
// read time (`ResolveVerifyEgress` requires the forward and reverse entries to
// agree and the client to have exactly one live egress of the source's
// family). An IPv6 address resolves by its own forward entry, and by its /64's
// only when no client holds the address and one client holds the /64.
//
// All functions are safe for concurrent use.

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"slices"
	"strconv"
	"strings"
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
		TrailLockTtl:     35 * time.Second,
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
		ReliabilityAMin:        8,
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
// DefaultVerifySettings); the hard tier is always on. The per-source seed and
// EXTEND limits also apply per resolved provider, so a provider's source ips
// in the two families share one budget.
type VerifySettings struct {
	StepTimeout      time.Duration
	StepTimeoutGrace time.Duration
	TrailTtlGrace    time.Duration
	// TrailLockTtl is the configured floor for the per-trail EXTEND mutation
	// lock (V3). VerifyTrailMutationLockTtl raises it to the full trail TTL so a
	// loaded mutation remains fenced from the expiry sweeper; Redis expiry still
	// self-heals a holder crash.
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
	// SeedRateWindow (V4), also applied per resolved provider (§9).
	// Legitimate per-provider EXTEND volume is bounded by the eligibility
	// burst (a provider is the confirming hop only for trails it was assigned
	// to), so this is set generously — it exists to shed a single-ip flood of
	// the mutating path, not to shape normal traffic.
	ExtendRateHardLimit   int64
	ActiveTrailsSoftLimit int64
	ActiveTrailsHardLimit int64
	StatsPeriod           time.Duration
	// ReliabilityAMin is the Wilson denominator floor published with audit
	// rollups and used by the payout artifact builder.
	ReliabilityAMin        int64
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
	// EgressHashKey is shared by authorized operators and validators under a
	// versioned subnet policy. It must never appear in public artifacts; only
	// EgressHashKeyId is published.
	EgressHashKey   []byte
	EgressHashKeyId string
	// CohortSize, when positive, limits next-hop sampling to at most this many
	// distinct providers per coordinator settlement epoch, and
	// CohortLifetimeLimit to at most that many distinct providers over the
	// deployment's life (verify_cohort_model.go). Zero keeps uniform sampling
	// over every eligible provider. CohortDeploymentKey names the st epoch
	// mirror that supplies the epoch, which also windows a provider's head
	// hash (PinVerifyHopEgressIpHash).
	CohortSize          int
	CohortLifetimeLimit int
	CohortDeploymentKey StDeploymentKey
	// HopLatencyFromAttributedAttempt counts a hop confirmed after a
	// retryable answer from the step's last such answer: its AssignedMs is
	// that time (§7.5). Off by default, a confirmed hop keeps its ASSIGN time
	// as before, the only record a validator's receipt check accepts until it
	// takes the moved one; turn it on once every validator does.
	HopLatencyFromAttributedAttempt bool
}

// TrailTtl is the redis lifetime of one trail's state (VALIDATOR.md §5.5:
// `M·T` + grace).
func (self *VerifySettings) TrailTtl(m int) time.Duration {
	return time.Duration(m)*self.StepTimeout + self.TrailTtlGrace
}

// VerifyTrailHop is one hop record of a trail. `AssignedMs`/`AssignN` are zero
// for the validator-chosen seed hop, which is never a server assignment and is
// excluded from statistics (§7.6). With HopLatencyFromAttributedAttempt a
// confirmed hop's `AssignedMs` is the start of the attempt that was
// attributed: its ASSIGN time, or the step's last retryable answer (§8.2), so
// `ConfirmedMs − AssignedMs` is that attempt's latency (§7.5); without it, its
// ASSIGN time. `ConfirmedMs` is zero while pending.
// `EgressIpHash` is the hop's egress-IP-hash at the configured subnet
// granularity (§8.1, §3.3, D27) — the head routable-IP-score input, set at
// confirmation (zero while pending) and carried verbatim into the published
// FINAL proof. Every hop of one provider in a trail's scoring window records
// the hash the provider pinned for the window, so it scores as one prefix
// whatever family each lands on (PinVerifyHopEgressIpHash).
type VerifyTrailHop struct {
	ClientId     server.Id  `json:"client_id"`
	NetworkId    *server.Id `json:"network_id,omitempty"`
	NetworkIssue string     `json:"network_issue,omitempty"`
	AssignedMs   uint64     `json:"assigned_ms"`
	ConfirmedMs  uint64     `json:"confirmed_ms"`
	AssignN      int        `json:"assign_n"`
	Seed         bool       `json:"seed,omitempty"`
	EgressIpHash [32]byte   `json:"egress_ip_hash"`
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

// Keyed by the peppered hash of an address (VerifyEgressIndexHashWithSettings)
// or of an IPv6 /64 (verifyEgressNetworkHash), never the raw IP. Eligibility
// is an address-level bijection; the separately signed trail hash is
// deliberately coarser (/29 or /48) so different eligible providers can
// contribute one shared prefix that head scoring splits between fleets. v2
// key names prevent a rolling upgrade from mixing the old prefix-keyed
// reverse entries with exact-address entries during their TTL. An address's
// forward entry kept its format through the v4 reverse index, so writers of
// both versions share it and see each other's claims on an address.
func verifyEgressKey(egressHash [32]byte) string {
	return verifyEgressKeyFromHex(hex.EncodeToString(egressHash[:]))
}

func verifyEgressKeyFromHex(egressHashHex string) string {
	return fmt.Sprintf("verify_egress_v2_%s", egressHashHex)
}

// The v4 reverse index. Each field names one address and its family, so a
// provider may hold one live egress per family (§8.2).
func verifyClientEgressKey(clientId server.Id) string {
	return fmt.Sprintf("{vce4_%s}", clientId)
}

// The v2 reverse index, whose fields are bare exact-address hashes. No writer
// feeds it; it is read and withdrawn until its entries lapse, one EgressTtl
// after the last v2 writer.
func verifyClientEgressV2Key(clientId server.Id) string {
	return fmt.Sprintf("{vce2_%s}", clientId)
}

// Both reverse index versions, the current one first.
func verifyClientEgressKeys(clientId server.Id) []string {
	return []string{verifyClientEgressKey(clientId), verifyClientEgressV2Key(clientId)}
}

// verifyEgressField is one field of a reverse index: an address hash, its
// family, and the egress the family counts it as, the address itself or, for
// IPv6, its /64. A bare v2 field has family 0.
type verifyEgressField struct {
	address string
	family  int
	network string
}

// A v4 reverse field names one address: `<address hash>_4`, or
// `<address hash>_6_<its /64's hash>`. The /64 follows from the address, so a
// field never changes for one address.
func verifyClientEgressField(ip netip.Addr, settings *VerifySettings) (string, verifyEgressField) {
	ip = ip.Unmap()
	addressHash := VerifyEgressIndexHashWithSettings(ip, settings)
	networkHash := verifyEgressNetworkHash(ip, settings)
	field := verifyEgressField{
		address: hex.EncodeToString(addressHash[:]),
		family:  server.IpVersionForAddr(ip),
		network: hex.EncodeToString(networkHash[:]),
	}
	if field.family == 6 {
		return fmt.Sprintf("%s_6_%s", field.address, field.network), field
	}
	return fmt.Sprintf("%s_%d", field.address, field.family), field
}

// Reads a reverse field of either version. Anything but a v4 field is a bare
// address with family 0.
func parseVerifyClientEgressField(field string) verifyEgressField {
	parts := strings.Split(field, "_")
	parsed := verifyEgressField{address: parts[0], network: parts[0]}
	switch {
	case len(parts) == 2 && parts[1] == "4":
		parsed.family = 4
	case len(parts) == 3 && parts[1] == "6":
		parsed.family, parsed.network = 6, parts[2]
	}
	return parsed
}

// The forward entries an address's leases feed: its own, and its /64's.
func (self verifyEgressField) forwardHashes() []string {
	if self.network != self.address {
		return []string{self.address, self.network}
	}
	return []string{self.address}
}

// A v4 field's value: the expiry of its latest lease, and the egress-IP-hash
// of the address (VerifyEgressIpHashWithSettings) its feeder computed. A v2
// value is the expiry alone.
func verifyClientEgressValueSuffix(ip netip.Addr, settings *VerifySettings) string {
	scoreHash := VerifyEgressIpHashWithSettings(ip, settings)
	return "_" + hex.EncodeToString(scoreHash[:])
}

func parseVerifyClientEgressValue(value string) (expireMs uint64, scoreHex string) {
	expire, scoreHex, _ := strings.Cut(value, "_")
	expireMs, _ = strconv.ParseUint(expire, 10, 64)
	return
}

// verifyLiveEgressAddress is one live address with its stored egress-IP-hash,
// "" for a v2 entry.
type verifyLiveEgressAddress struct {
	verifyEgressField
	score string
}

// verifyLiveEgress is a client's live addresses: its v4 entries by family,
// and the bare hashes a v2 writer left. The two versions are judged apart: a
// v2 entry keeps the old rule, one live address in all.
type verifyLiveEgress struct {
	byFamily map[int][]verifyLiveEgressAddress
	legacy   []string
}

// The distinct egresses of a family, sorted: IPv4 addresses, or IPv6 /64s.
// Only a family with exactly one may attribute (§8.2).
func (self verifyLiveEgress) networks(family int) []string {
	networks := []string{}
	for _, address := range self.byFamily[family] {
		if !slices.Contains(networks, address.network) {
			networks = append(networks, address.network)
		}
	}
	slices.Sort(networks)
	return networks
}

// Splits one reverse hash's fields into the live ones and the expired ones,
// each with its value.
func splitVerifyEgressFields(fields map[string]string, nowMs uint64) (live map[string]string, expired map[string]string) {
	live, expired = map[string]string{}, map[string]string{}
	for field, value := range fields {
		if expireMs, _ := parseVerifyClientEgressValue(value); nowMs < expireMs {
			live[field] = value
		} else {
			expired[field] = value
		}
	}
	return
}

// Groups the live fields of a client's v4 and v2 reverse hashes.
func newVerifyLiveEgress(fields map[string]string, legacyFields map[string]string) verifyLiveEgress {
	live := verifyLiveEgress{byFamily: map[int][]verifyLiveEgressAddress{}}
	for field, value := range fields {
		if parsed := parseVerifyClientEgressField(field); parsed.family != 0 {
			_, score := parseVerifyClientEgressValue(value)
			live.byFamily[parsed.family] = append(live.byFamily[parsed.family], verifyLiveEgressAddress{verifyEgressField: parsed, score: score})
		}
	}
	for _, addresses := range live.byFamily {
		slices.SortFunc(addresses, func(a, b verifyLiveEgressAddress) int { return strings.Compare(a.address, b.address) })
	}
	for field := range legacyFields {
		live.legacy = append(live.legacy, parseVerifyClientEgressField(field).address)
	}
	slices.Sort(live.legacy)
	return live
}

const verifyEligibleKey = "verify_eligible_v2"

// refreshed on every eligibility grant; see updateVerifyEligibleMembership
const verifyEligibleKeyTtl = 30 * 24 * time.Hour

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

func verifySeedProviderRateKey(clientId server.Id) string {
	return fmt.Sprintf("verify_seed_provider_%s", clientId)
}

func verifyExtendProviderRateKey(clientId server.Id) string {
	return fmt.Sprintf("verify_extend_provider_%s", clientId)
}

func verifyExtendPadRateKey(ipHash [32]byte) string {
	return fmt.Sprintf("verify_extend_pad_%s", hex.EncodeToString(ipHash[:]))
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

func VerifyEgressIpHashWithSettings(ip netip.Addr, settings *VerifySettings) [32]byte {
	if len(settings.EgressHashKey) == 0 {
		return VerifyEgressIpHash(ip, settings.EgressHashV4Prefix, settings.EgressHashV6Prefix)
	}
	ip = ip.Unmap()
	if ip.Is4() {
		ip = netip.PrefixFrom(ip, settings.EgressHashV4Prefix).Masked().Addr()
	} else if ip.Is6() {
		ip = netip.PrefixFrom(ip, settings.EgressHashV6Prefix).Masked().Addr()
	}
	h := hmac.New(sha256.New, settings.EgressHashKey)
	h.Write([]byte("urnetwork/egress-prefix/v1"))
	h.Write(ip.AsSlice())
	return [32]byte(h.Sum(nil))
}

// VerifyEgressIndexHashWithSettings is the privacy-preserving exact-address
// identity used only by the server's eligibility bijection, in either family.
// It must not use the subnet scoring prefix: two distinct addresses in one
// /29 or /48 are two independently attributable providers but intentionally
// produce one shared VerifyEgressIpHashWithSettings value in a signed trail.
// Domain separation also prevents an index key from being confused with a
// published score hash. A v2 writer keyed both families by it.
func VerifyEgressIndexHashWithSettings(ip netip.Addr, settings *VerifySettings) [32]byte {
	ip = ip.Unmap()
	if len(settings.EgressHashKey) == 0 {
		return server.ClientIpHashForAddrPrefix(ip, 32, 128)
	}
	h := hmac.New(sha256.New, settings.EgressHashKey)
	h.Write([]byte("urnetwork/egress-address-index/v1"))
	h.Write(ip.AsSlice())
	return [32]byte(h.Sum(nil))
}

// verifyEgressNetworkHash is the egress a family counts an address as: an
// IPv4 address itself, or an IPv6 address's /64. A host's temporary IPv6
// addresses rotate inside its /64 (RFC 8981), so the /64 is one egress, as an
// IPv4 address behind NAT is. Its own domain keeps a /64 apart from any
// address.
func verifyEgressNetworkHash(ip netip.Addr, settings *VerifySettings) [32]byte {
	ip = ip.Unmap()
	if !ip.Is6() {
		return VerifyEgressIndexHashWithSettings(ip, settings)
	}
	prefix := netip.PrefixFrom(ip, 64).Masked().Addr()
	if len(settings.EgressHashKey) == 0 {
		return server.ClientIpHashForAddrPrefix(prefix, 32, 64)
	}
	h := hmac.New(sha256.New, settings.EgressHashKey)
	h.Write([]byte("urnetwork/egress-ipv6-64-index/v1"))
	h.Write(prefix.AsSlice())
	return [32]byte(h.Sum(nil))
}

// Feeds the durable proxy allocation lease. Connection announcements use their
// distinct connection ids so one transport cannot withdraw another's lease.
func FeedVerifyEgress(ctx context.Context, clientId server.Id, ip netip.Addr, settings *VerifySettings) {
	feedVerifyEgressLease(ctx, clientId, ip, verifyProxyEgressLeaseOwner, settings)
}

// Withdraws the proxy allocation lease without clearing live transport leases.
func ClearVerifyEgress(ctx context.Context, clientId server.Id, ip netip.Addr, settings *VerifySettings) {
	clearVerifyEgressLease(ctx, clientId, ip, verifyProxyEgressLeaseOwner, settings)
}

// RemoveVerifyEgressForClient drops all egress entries for a client, in both
// reverse index versions — called when a client_id is reaped
// (`RemoveDisconnectedNetworkClients`) so a recycled ip is not miscredited to
// the previous holder (§8.2).
func RemoveVerifyEgressForClient(
	ctx context.Context,
	clientId server.Id,
) {
	server.Redis(ctx, func(r server.RedisClient) {
		for _, reverseKey := range verifyClientEgressKeys(clientId) {
			entries, err := r.HGetAll(ctx, reverseKey).Result()
			if err != nil && !errors.Is(err, redis.Nil) {
				server.Raise(err)
			}
			for field := range entries {
				parsed := parseVerifyClientEgressField(field)
				for _, egressHashHex := range parsed.forwardHashes() {
					server.Raise(removeVerifyEgressAddressForClient(ctx, r, clientId, egressHashHex).Err())
				}
				server.Raise(r.Del(ctx, verifyClientEgressLeaseKey(reverseKey, parsed.address)).Err())
			}
			r.Del(ctx, reverseKey)
		}
		r.SRem(ctx, verifyEligibleKey, clientId.String())
	})
}

// readVerifyLiveEgress reads the client's live addresses from both reverse
// index versions, opportunistically deleting expired v4 fields. The v2 index
// is only read: no writer feeds it, so it empties within one EgressTtl of the
// last v2 writer.
func readVerifyLiveEgress(
	ctx context.Context,
	r server.RedisClient,
	clientId server.Id,
	nowMs uint64,
) verifyLiveEgress {
	versions := []map[string]string{}
	for _, reverseKey := range verifyClientEgressKeys(clientId) {
		fields, err := r.HGetAll(ctx, reverseKey).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		live, expired := splitVerifyEgressFields(fields, nowMs)
		if 0 < len(expired) && reverseKey == verifyClientEgressKey(clientId) {
			pruneVerifyExpiredEgressHashes(ctx, r, clientId, expired)
		}
		versions = append(versions, live)
	}
	return newVerifyLiveEgress(versions[0], versions[1])
}

// updateVerifyEligibleMembership recomputes one client's membership in the
// eligible-provider set (§5.1): a family whose one live egress a forward
// entry attributes to the client, and a non-empty provide mode set.
// Membership is also lazily re-checked at sampling time, so a small staleness
// window here is safe.
func updateVerifyEligibleMembership(
	ctx context.Context,
	clientId server.Id,
) {
	nowMs := uint64(server.NowUtc().UnixMilli())

	eligible := false
	server.Redis(ctx, func(r server.RedisClient) {
		eligible = verifyClientHasLiveEgress(ctx, r, clientId, nowMs)
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
			// hygiene bound only: the whole set drops after 30d without a
			// single eligibility grant (i.e., the system is effectively off)
			// and rebuilds organically from announce-path re-checks. Member
			// staleness is handled by the sampled-stale removal at sampling
			// time; without a ttl this key is unevictable under volatile-ttl.
			r.Expire(ctx, verifyEligibleKey, verifyEligibleKeyTtl)
		} else {
			r.SRem(ctx, verifyEligibleKey, clientId.String())
		}
	})
}

// An eligible-set entry is only a candidate index. Expiry and connection
// changes must pass the same bijection again before assignment. One family
// that attributes to the client suffices: a hop that lands on a shared,
// ambiguous or refused family, or on an address another client holds, is
// answered as retryable and resent on the other (§8.2, IPV6.md C7). A v2
// entry keeps the old rule, one live address in all.
func verifyClientHasLiveEgress(ctx context.Context, r server.RedisClient, clientId server.Id, nowMs uint64) bool {
	live := readVerifyLiveEgress(ctx, r, clientId, nowMs)
	return live.attributes(clientId, readVerifyEgressForwards(ctx, r, live.eligibilityForwardHashes()))
}

// Whether one family attributes to the client (verifyLiveEgress.familyAttributes).
func verifyFamilyAttributes(ctx context.Context, r server.RedisClient, clientId server.Id, live verifyLiveEgress, family int) bool {
	return live.familyAttributes(family, clientId, readVerifyEgressForwards(ctx, r, live.familyForwardHashes(family)))
}

// The forward entries whose owners decide whether a family attributes to the
// client: when its live addresses are one egress, an IPv4 address or
// addresses of one IPv6 /64, the egress's entry and each of its addresses'.
// A family of several egresses reads none, as it cannot attribute.
func (self verifyLiveEgress) familyForwardHashes(family int) []string {
	networks := self.networks(family)
	if len(networks) != 1 {
		return nil
	}
	hashes := []string{networks[0]}
	for _, address := range self.byFamily[family] {
		if address.address != address.network {
			hashes = append(hashes, address.address)
		}
	}
	return hashes
}

// Every forward entry eligibility reads: both families' and, when it is the
// client's one, its v2 entry's.
func (self verifyLiveEgress) eligibilityForwardHashes() []string {
	hashes := append(self.familyForwardHashes(4), self.familyForwardHashes(6)...)
	if len(self.legacy) == 1 {
		hashes = append(hashes, self.legacy[0])
	}
	return hashes
}

// A family attributes to the client when one of its forward entries, read
// into `forwards`, names the client. Two providers in one /64 make the /64's
// entry ambiguous, while each one's own address still names it.
func (self verifyLiveEgress) familyAttributes(family int, clientId server.Id, forwards map[string]string) bool {
	for _, egressHashHex := range self.familyForwardHashes(family) {
		if forwards[egressHashHex] == clientId.String() {
			return true
		}
	}
	return false
}

// Eligibility's egress rule (§5.1, §8.2) on the forward owners read for
// eligibilityForwardHashes: a family attributes to the client, or its one v2
// entry's forward entry names it.
func (self verifyLiveEgress) attributes(clientId server.Id, forwards map[string]string) bool {
	return self.familyAttributes(4, clientId, forwards) || self.familyAttributes(6, clientId, forwards) ||
		(len(self.legacy) == 1 && forwards[self.legacy[0]] == clientId.String())
}

// The owners of forward entries, each the client it names, the ambiguous
// marker, or "".
func readVerifyEgressForwards(ctx context.Context, r server.RedisClient, egressHashHexes []string) map[string]string {
	forwards := map[string]string{}
	for _, egressHashHex := range egressHashHexes {
		forwards[egressHashHex] = verifyEgressForward(ctx, r, egressHashHex)
	}
	return forwards
}

// The client a forward entry names, the ambiguous marker, or "".
func verifyEgressForward(ctx context.Context, r server.RedisClient, egressHashHex string) string {
	forward, err := r.Get(ctx, verifyEgressKeyFromHex(egressHashHex)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		server.Raise(err)
	}
	return forward
}

// ResolveVerifyEgress maps a request source ip to the provider currently
// egressing from it (§8.1), or nil. Resolution requires the bijection both
// ways (§8.2): a forward entry names the client, and the client's reverse
// hash has the source's egress live in its family and no other egress of
// that family. The exact address comes first; an IPv6 source falls back to
// its /64 only when no client holds the address, and resolves by it only when
// one client holds the /64, so two providers in one /64 each keep their own
// address while a temporary address rotated inside a provider's /64 still
// resolves. A v2 entry resolves by the old rule, as the client's one live
// address, under its exact-address hash. An ambiguous ip resolves to nothing —
// never guess. The lookup does the same number of redis round trips on hit
// and miss so it is not a timing oracle for "is this ip a provider" (§9).
func ResolveVerifyEgress(
	ctx context.Context,
	ip netip.Addr,
	settings *VerifySettings,
) (clientId *server.Id) {
	return resolveVerifyEgressAt(ctx, ip, settings, uint64(server.NowUtc().UnixMilli()))
}

// The explicit clock lets tests read past an entry's expiry without sleeping.
func resolveVerifyEgressAt(
	ctx context.Context,
	ip netip.Addr,
	settings *VerifySettings,
	nowMs uint64,
) (clientId *server.Id) {
	if !ip.IsValid() {
		return
	}
	_, source := verifyClientEgressField(ip, settings)

	server.Redis(ctx, func(r server.RedisClient) {
		// both entries are read whichever names a client
		owner := verifyEgressForward(ctx, r, source.address)
		if source.network != source.address {
			if networkOwner := verifyEgressForward(ctx, r, source.network); owner == "" {
				owner = networkOwner
			}
		}

		resolvedId, parseErr := server.ParseId(owner)
		if owner == "" || owner == verifyEgressAmbiguous || parseErr != nil {
			if settings.ResolveConstantTimePad {
				// pad the miss path to the same round-trip count as a hit
				for _, reverseKey := range verifyClientEgressKeys(server.Id{}) {
					r.HGetAll(ctx, reverseKey).Result()
				}
			}
			return
		}

		live := readVerifyLiveEgress(ctx, r, resolvedId, nowMs)
		networks := live.networks(source.family)
		if (len(networks) == 1 && networks[0] == source.network) || (len(live.legacy) == 1 && live.legacy[0] == source.address) {
			clientId = &resolvedId
		}
	})
	return
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
//
// With a configured cohort (CohortSize > 0) every draw comes from the bounded
// settlement-epoch cohort instead, and nothing falls back to this uniform draw.
func SampleVerifyNextHop(
	ctx context.Context,
	excludeClientIds []server.Id,
	settings *VerifySettings,
) (nextHop *server.Id, n int) {
	if 0 < settings.CohortSize {
		return sampleVerifyCohortNextHop(ctx, excludeClientIds, settings)
	}
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

			candidateId, ok := verifyCandidateAssignable(ctx, r, candidate)
			if !ok {
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

// verifyCandidateAssignable re-checks one drawn provider before it can be
// assigned: a parseable id, live attributable egresses with at most one per
// family, and an active provide mode. A failed candidate is lazily evicted
// from the eligible set, which may outlive an address lease or race a
// disconnect; it must never spend a token or be assigned as an
// unattributable source.
func verifyCandidateAssignable(ctx context.Context, r server.RedisClient, candidate string) (server.Id, bool) {
	candidateId, err := server.ParseId(candidate)
	if err != nil {
		r.SRem(ctx, verifyEligibleKey, candidate)
		return server.Id{}, false
	}
	if !verifyClientHasLiveEgress(ctx, r, candidateId, uint64(server.NowUtc().UnixMilli())) {
		server.Raise(r.SRem(ctx, verifyEligibleKey, candidate).Err())
		return server.Id{}, false
	}
	provideModes, err := GetProvideModes(ctx, candidateId)
	if err != nil || len(provideModes) == 0 {
		// no longer a provider: lazily evict
		r.SRem(ctx, verifyEligibleKey, candidate)
		return server.Id{}, false
	}
	return candidateId, true
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
	rateLimitClient, err := server.NewRateLimitClientIp(clientIp)
	server.Raise(err)
	return incrVerifySeedRatesForClient(
		ctx,
		rateLimitClient.IpHash(),
		rateLimitClient.Excluded(),
		vpk,
		settings,
	)
}

func incrVerifySeedRatesForClient(
	ctx context.Context,
	ipHash [32]byte,
	ipExcluded bool,
	vpk []byte,
	settings *VerifySettings,
) (ipCount int64, vpkCount int64) {
	if !ipExcluded {
		client := server.NewStoredRateLimitClient(ipHash)
		var err error
		ipCount, err = server.IncrementIpRateLimit(
			ctx,
			client,
			verifySeedIpRateKey(ipHash),
			settings.SeedRateWindow,
		)
		server.Raise(err)
	}
	vpkCount, err := server.IncrementRateLimitWindow(ctx, verifySeedVpkRateKey(vpk), settings.SeedRateWindow)
	server.Raise(err)
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
	rateLimitClient, err := server.NewRateLimitClientIp(clientIp)
	server.Raise(err)
	return incrVerifyExtendRateForClient(
		ctx,
		rateLimitClient.IpHash(),
		rateLimitClient.Excluded(),
		settings,
	)
}

func incrVerifyExtendRateForClient(
	ctx context.Context,
	ipHash [32]byte,
	ipExcluded bool,
	settings *VerifySettings,
) (ipCount int64) {
	if ipExcluded {
		return 0
	}
	client := server.NewStoredRateLimitClient(ipHash)
	ipCount, err := server.IncrementIpRateLimit(
		ctx,
		client,
		verifyExtendIpRateKey(ipHash),
		settings.SeedRateWindow,
	)
	server.Raise(err)
	return ipCount
}

// IncrVerifySeedProviderRate bumps and returns the seed counter of the
// provider a seed's source ip resolved to (§9). A provider with an address in
// each family has two source ips, so the caller holds this count to the same
// per-source limits: the provider keeps one budget, not one per family. An
// excluded source skips it, as it skips the source-ip counter.
func IncrVerifySeedProviderRate(
	ctx context.Context,
	clientIp string,
	clientId server.Id,
	settings *VerifySettings,
) int64 {
	return incrVerifyProviderRate(ctx, clientIp, verifySeedProviderRateKey(clientId), settings)
}

// IncrVerifyExtendProviderRate is IncrVerifySeedProviderRate for EXTEND (V4).
func IncrVerifyExtendProviderRate(
	ctx context.Context,
	clientIp string,
	clientId server.Id,
	settings *VerifySettings,
) int64 {
	return incrVerifyProviderRate(ctx, clientIp, verifyExtendProviderRateKey(clientId), settings)
}

func incrVerifyProviderRate(
	ctx context.Context,
	clientIp string,
	key string,
	settings *VerifySettings,
) int64 {
	rateLimitClient, err := server.NewRateLimitClientIp(clientIp)
	server.Raise(err)
	return incrVerifyProviderRateForClient(ctx, rateLimitClient.Excluded(), key, settings)
}

func incrVerifyProviderRateForClient(
	ctx context.Context,
	ipExcluded bool,
	key string,
	settings *VerifySettings,
) int64 {
	if ipExcluded {
		return 0
	}
	count, err := server.IncrementRateLimitWindow(ctx, key, settings.SeedRateWindow)
	server.Raise(err)
	return count
}

// PadVerifyExtendProviderRate does the work of IncrVerifyExtendProviderRate
// on a scratch counter for an EXTEND whose source resolved to no provider, so
// what an unknown trail's EXTEND costs does not depend on resolution (§9),
// and returns its count. The counter is the source's own, keyed by its
// rate-limit subnet like its EXTEND meter, so the pads of many sources share
// no one key.
func PadVerifyExtendProviderRate(
	ctx context.Context,
	clientIp string,
	settings *VerifySettings,
) int64 {
	rateLimitClient, err := server.NewRateLimitClientIp(clientIp)
	server.Raise(err)
	return incrVerifyProviderRateForClient(ctx, rateLimitClient.Excluded(), verifyExtendPadRateKey(rateLimitClient.IpHash()), settings)
}

// VerifyUnattributedAttemptLimit is how many times one step answers a
// well-formed request whose source the step cannot attribute as retryable
// before it treats the source as today: an EXTEND fails the trail and a SEED
// is poisoned. A validator sends each step at most three times, alternating
// the family it tries first (IPV6.md C7), so two retries can reach the
// provider's other family.
const VerifyUnattributedAttemptLimit = 2

func verifyUnattributedExtendKey(trailId server.Id, depth int) string {
	return fmt.Sprintf("{vtr_%s}unattributed_%d", trailId, depth)
}

func verifyUnattributedSeedKey(seedSig []byte) string {
	return fmt.Sprintf("verify_seed_unattributed_%x", sha256.Sum256(seedSig))
}

// IncrVerifyUnattributedExtend counts an EXTEND of the step at `depth` whose
// source resolved to no provider or to another than the pending one, notes
// `answeredMs` as the step's last such answer, and returns the count. Both
// live as long as the step can. Past the limit the trail fails, so the time a
// later confirmation reads is always a retryable answer's.
func IncrVerifyUnattributedExtend(
	ctx context.Context,
	trailId server.Id,
	depth int,
	answeredMs uint64,
	settings *VerifySettings,
) (count int64) {
	key := verifyUnattributedExtendKey(trailId, depth)
	server.Redis(ctx, func(r server.RedisClient) {
		var countCmd *redis.IntCmd
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			countCmd = pipe.HIncrBy(ctx, key, "attempts", 1)
			pipe.HSet(ctx, key, "answered_ms", answeredMs)
			pipe.PExpire(ctx, key, settings.StepTimeout+settings.StepTimeoutGrace)
			return nil
		})
		server.Raise(err)
		count = countCmd.Val()
	})
	return
}

// GetVerifyUnattributedExtendAnsweredMs is when the step at `depth` last
// answered an EXTEND as retryable, or 0 when it never did. The attributed
// resend started from that answer, as a first attempt starts from the ASSIGN,
// so the hop's latency counts from it (§7.5).
func GetVerifyUnattributedExtendAnsweredMs(
	ctx context.Context,
	trailId server.Id,
	depth int,
) (answeredMs uint64) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.HGet(ctx, verifyUnattributedExtendKey(trailId, depth), "answered_ms").Result()
		if errors.Is(err, redis.Nil) {
			return
		}
		server.Raise(err)
		answeredMs, err = strconv.ParseUint(value, 10, 64)
		server.Raise(err)
	})
	return
}

// IncrVerifyUnattributedSeed is IncrVerifyUnattributedExtend for a SEED,
// identified by its signature: the validator resends the identical body. A
// seed hop has no latency, so no time is noted.
func IncrVerifyUnattributedSeed(
	ctx context.Context,
	seedSig []byte,
	settings *VerifySettings,
) (count int64) {
	key := verifyUnattributedSeedKey(seedSig)
	server.Redis(ctx, func(r server.RedisClient) {
		var countCmd *redis.IntCmd
		_, err := r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			countCmd = pipe.Incr(ctx, key)
			pipe.PExpire(ctx, key, settings.StepTimeout+settings.StepTimeoutGrace)
			return nil
		})
		server.Raise(err)
		count = countCmd.Val()
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
// the lock was acquired as a nonempty ownership token; on contention it returns
// empty and the caller returns 409. The token makes release compare-and-delete,
// so an expired old holder cannot erase a successor's lease. The lock self-heals
// via `ttl` if the holder crashes.
func AcquireVerifyTrailLock(
	ctx context.Context,
	trailId server.Id,
	ttl time.Duration,
) (token string) {
	if ttl <= 0 {
		return ""
	}
	candidate := server.NewId().String()
	server.Redis(ctx, func(r server.RedisClient) {
		ok, err := r.SetNX(ctx, verifyTrailLockKey(trailId), candidate, ttl).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
		if ok {
			token = candidate
		}
	})
	return
}

// VerifyTrailMutationLockTtl keeps an in-flight mutation fenced for at least
// the lifetime of the trail state it loaded. The configured floor is useful
// for small test values; the full trail TTL prevents the expiry sweeper from
// taking over merely because durable completion is slower than one hop's
// response deadline.
func VerifyTrailMutationLockTtl(settings *VerifySettings, m int) time.Duration {
	if settings == nil {
		return 0
	}
	ttl := settings.TrailLockTtl
	if trailTtl := settings.TrailTtl(m); trailTtl > ttl {
		ttl = trailTtl
	}
	return ttl
}

// ReleaseVerifyTrailLock drops only the caller-owned per-trail mutation lock.
// A missing, expired, or replaced token is a no-op.
func ReleaseVerifyTrailLock(
	ctx context.Context,
	trailId server.Id,
	token string,
) {
	if token == "" {
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		_, err := server.RedisRemoveIfEqual(r, ctx, verifyTrailLockKey(trailId), []byte(token)).Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			server.Raise(err)
		}
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
		server.Raise(r.HSet(ctx, verifyTrailHeaderKey(trailId), "status", VerifyTrailStatusExpired).Err())
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
	TrailId       server.Id
	Vpk           []byte
	ServerKeyId   byte
	ServerNonce   []byte
	Depth         int
	Status        int
	HopsJson      string
	FinalSig      []byte
	VerifierSig   []byte
	CreateTime    time.Time
	CompleteTime  *time.Time
	OriginalState []byte
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
				complete_time,
				original_state
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
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
			row.OriginalState,
		))
		server.RaisePgResult(tx.Exec(ctx, `DELETE FROM verify_original_pending WHERE trail_id=$1`, row.TrailId))
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
				complete_time,
				original_state
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
					&r.OriginalState,
				))
				r.ServerKeyId = byte(serverKeyId)
				row = r
			}
		})
	})
	return
}

// ListVerifyTrailRows exposes the durable, non-poison proof history in stable
// order for the public audit API. Limits are mandatory at the model boundary
// so an unauthenticated caller cannot trigger an unbounded scan.
func ListVerifyTrailRows(ctx context.Context, minTime, maxTime time.Time, limit int) (rows []*VerifyTrailRow) {
	if limit <= 0 || limit > 10_000 {
		panic(fmt.Errorf("verify proof history limit out of range: %d", limit))
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT trail_id, vpk, server_key_id, server_nonce, depth, status,
				hops_json, final_sig, verifier_sig, create_time, complete_time, original_state
			FROM verify_trail
			WHERE create_time >= $1 AND create_time < $2
			ORDER BY create_time, trail_id
			LIMIT $3
		`, minTime.UTC(), maxTime.UTC(), limit)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &VerifyTrailRow{}
				var serverKeyId int16
				server.Raise(result.Scan(&row.TrailId, &row.Vpk, &serverKeyId,
					&row.ServerNonce, &row.Depth, &row.Status, &row.HopsJson,
					&row.FinalSig, &row.VerifierSig, &row.CreateTime, &row.CompleteTime, &row.OriginalState))
				row.ServerKeyId = byte(serverKeyId)
				rows = append(rows, row)
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

	seenTrailIds := map[string]bool{}
	for _, trailId := range trailIdStrs {
		seenTrailIds[trailId] = true
	}
	for _, trailId := range dueVerifyOriginalTrails(ctx, now, settings.SweepLimit) {
		if !seenTrailIds[trailId] {
			trailIdStrs = append(trailIdStrs, trailId)
			seenTrailIds[trailId] = true
		}
	}
	for _, trailIdStr := range trailIdStrs {
		trailId, err := server.ParseId(trailIdStr)
		if err != nil {
			server.Redis(ctx, func(r server.RedisClient) {
				r.ZRem(ctx, verifyReapKey, trailIdStr)
			})
			continue
		}

		trail := GetVerifyTrail(ctx, trailId)
		originalRecovery := false
		if trail == nil {
			if original := GetLatestVerifyOriginal(ctx, trailId); original != nil {
				body, err := DecodeVerifyOriginal(original)
				server.Raise(err)
				trail = body.Trail
				originalRecovery = true
			}
		}
		if trail == nil || (trail.Status != VerifyTrailStatusActive && !originalRecovery) {
			// ttl-expired or already terminal: drop the registry entry
			server.Redis(ctx, func(r server.RedisClient) {
				r.ZRem(ctx, verifyReapKey, trailIdStr)
			})
			continue
		}

		// Serialize expiry with EXTEND. A request may have loaded and validated
		// this same active pending hop before the grace deadline; expiring it in
		// parallel would leave a confirmed hop on an expired trail and double
		// decrement the active counter. Contention is not an error: the due zset
		// entry remains and the next sweep retries after the holder commits.
		lockToken := AcquireVerifyTrailLock(ctx, trailId, VerifyTrailMutationLockTtl(settings, trail.M))
		if lockToken == "" {
			continue
		}
		func() {
			defer ReleaseVerifyTrailLock(ctx, trailId, lockToken)
			// The pre-lock snapshot may have changed while waiting. All expiry
			// decisions and durable evidence below use the fenced reload.
			trail = GetVerifyTrail(ctx, trailId)
			var originalDeadlineMs uint64
			if original := GetLatestVerifyOriginal(ctx, trailId); original != nil {
				body, err := DecodeVerifyOriginal(original)
				server.Raise(err)
				if trail != nil {
					server.Raise(VerifyOriginalMatchesTrail(body, trail))
				}
				if trail == nil || len(trail.Hops) < len(body.Trail.Hops) {
					trail = PublishVerifyOriginal(ctx, original, settings).Trail
				}
				if trail.Pending != nil && body.Trail.Pending != nil && trail.Pending.ClientId == body.Trail.Pending.ClientId && len(trail.Hops) == len(body.Trail.Hops) {
					originalDeadlineMs = body.RecoveryMs
				}
			}
			if trail == nil || trail.Status != VerifyTrailStatusActive {
				server.Redis(ctx, func(r server.RedisClient) {
					r.ZRem(ctx, verifyReapKey, trailIdStr)
				})
				return
			}

			if trail.Pending != nil {
				deadlineMs := trail.Pending.AssignedMs + uint64((settings.StepTimeout+settings.StepTimeoutGrace)/time.Millisecond)
				if originalDeadlineMs != 0 {
					deadlineMs = originalDeadlineMs
				}
				if nowMs < deadlineMs {
					// activity since the registry score was written: re-score
					server.Redis(ctx, func(r server.RedisClient) {
						r.ZAdd(ctx, verifyReapKey, redis.Z{
							Score:  float64(deadlineMs),
							Member: trailIdStr,
						})
					})
					return
				}
			}

			if !trail.Poison {
				InsertVerifyTrail(ctx, NewExpiredVerifyTrailRow(trail))
			}
			ExpireVerifyTrail(ctx, trailId)
			DecrVerifyActiveTrails(ctx, trail.Vpk)
			sweptCount += 1
			if glog.V(1) {
				glog.Infof("[verify]reaped expired trail %s (depth %d, poison=%t)\n", trailId, len(trail.Hops), trail.Poison)
			}
		}()
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
	originalState, err := json.Marshal(trail)
	server.Raise(err)
	return &VerifyTrailRow{
		TrailId:       trail.TrailId,
		Vpk:           trail.Vpk,
		ServerKeyId:   trail.ServerKeyId,
		ServerNonce:   trail.ServerNonce,
		Depth:         len(trail.Hops),
		Status:        VerifyTrailRowStatusExpired,
		HopsJson:      string(hopsJsonBytes),
		CreateTime:    time.UnixMilli(int64(trail.CreateMs)).UTC(),
		OriginalState: originalState,
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

// ListVerifyProviderStats returns all provider rows overlapping a bounded
// time range. It backs the public reproducibility API; scoring continues to
// use validator-local observations, never this operator-supplied index.
func ListVerifyProviderStats(ctx context.Context, minTime, maxTime time.Time, limit int) (rows []*VerifyProviderStatsRow) {
	if limit <= 0 || limit > 100_000 {
		panic(fmt.Errorf("verify stats history limit out of range: %d", limit))
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `
			SELECT period_start, period_end, client_id, assignments, confirmations,
				latency_p50_ms, latency_p90_ms, latency_p99_ms
			FROM verify_provider_stats
			WHERE period_end > $1 AND period_start < $2
			ORDER BY period_start, client_id
			LIMIT $3
		`, minTime.UTC(), maxTime.UTC(), limit)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				row := &VerifyProviderStatsRow{}
				server.Raise(result.Scan(&row.PeriodStart, &row.PeriodEnd, &row.ClientId,
					&row.Assignments, &row.Confirmations, &row.LatencyP50Ms,
					&row.LatencyP90Ms, &row.LatencyP99Ms))
				rows = append(rows, row)
			}
		})
	})
	return
}

// VerifyStatsRetention is how long rolled-up verify_provider_stats rows are
// kept. RemoveOldVerifyProviderStats deletes anything older. Mirrors
// SearchStatsRetention -- verify_provider_stats grows per period x provider and
// is read only for recent windows (GetStEpochClientReliability, the /stats
// APIs), so old rows are pure storage.
const VerifyStatsRetention = 30 * 24 * time.Hour

// RemoveOldVerifyProviderStats deletes rolled-up rows older than
// VerifyStatsRetention, in bounded batches so it never holds a long lock.
// Driven by the RemoveOldVerifyProviderStats task. Mirrors
// RemoveOldSearchProviderStats.
func RemoveOldVerifyProviderStats(ctx context.Context, maxTime time.Time, limit int) {
	minTime := maxTime.Add(-VerifyStatsRetention)
	server.MaintenanceTx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
			DELETE FROM verify_provider_stats
			USING (
			    SELECT period_start, client_id
			    FROM verify_provider_stats
			    WHERE period_start < $1
			    ORDER BY period_start
			    LIMIT $2
			) t
			WHERE
			    verify_provider_stats.period_start = t.period_start AND
			    verify_provider_stats.client_id = t.client_id
			`,
			minTime.UTC(),
			limit,
		))
	})
}
