package controller

// verify_controller.go — the `/verify` routing-verification protocol
// (sn/VALIDATOR.md §§2-9): SEED and EXTEND per the §4.1/§4.2 checklists, the
// four Ed25519 signatures over the canonical messages of Appendix A (built
// and verified via package connect so server and validator can never disagree
// on bytes), poisoning per §9, idempotent retries per §4.3, and proof
// assembly/publication per §3.3/§6.2.
//
// Authentication is the protocol's own signatures, not a JWT (PLAN.md D-7):
// the SEED body carries the validator's client_id and the server checks the
// submitted vpk equals that client's registered Ed25519 key
// (model.GetClientPublicKey). The hop identity is never asserted by the
// caller — it is derived from the request source ip via the egress index
// (§8.1).
//
// Poisoning (§9): a SEED whose source ip does not resolve to a provider, or
// whose caller is unknown/ineligible/softly-over-limit, still creates a
// normal-looking trail with valid ASSIGNs, carried all the way to depth M
// with server-stamped times, then a normal-looking signed proof — but the
// trail is never written to `verify_trail`. Its pending hops are selected
// from the same eligible, routable provider set as a real trail (a shadow
// route), but no provider statistics are changed; pad writes keep response
// timing close. A synthetic id is used only for an already-confirmed,
// unresolved seed or when the routable population is empty.
// Only past the hard rate limits (the DoS bound on state creation) is a
// request refused outright.

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urfoundation/sn/protocol"
	"github.com/urnetwork/connect"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// VerifyServerKey is one server Ed25519 signing key, addressed by the 1-byte
// `server_key_id` carried in every ASSIGN/FINAL message (§3.5).
type VerifyServerKey struct {
	ServerKeyId byte
	PrivateKey  ed25519.PrivateKey
}

// verifyServerKeysFromVault loads the signing keys from the `verify.yml`
// vault resource (peer of `jwt.yml`):
//
//	keys:
//	  - server_key_id: 0
//	    seed: <base64 std 32-byte ed25519 seed>
//
// The first entry signs new trails; all entries are published via
// `GET /verify/keys` so historical proofs verify across rotations.
var verifyServerKeysFromVault = sync.OnceValue(func() []*VerifyServerKey {
	res := server.Vault.RequireSimpleResource("verify.yml")
	var conf struct {
		Keys []struct {
			ServerKeyId int    `yaml:"server_key_id"`
			Seed        string `yaml:"seed"`
		} `yaml:"keys"`
	}
	res.UnmarshalYaml(&conf)
	keys := []*VerifyServerKey{}
	seenIds := map[int]bool{}
	for _, confKey := range conf.Keys {
		if confKey.ServerKeyId < 0 || 255 < confKey.ServerKeyId {
			panic(fmt.Errorf("verify.yml server_key_id out of range: %d", confKey.ServerKeyId))
		}
		if seenIds[confKey.ServerKeyId] {
			panic(fmt.Errorf("verify.yml reuses server_key_id %d", confKey.ServerKeyId))
		}
		seenIds[confKey.ServerKeyId] = true
		seed, err := base64.StdEncoding.DecodeString(confKey.Seed)
		if err != nil {
			panic(fmt.Errorf("verify.yml bad key seed: %s", err))
		}
		if len(seed) != ed25519.SeedSize {
			panic(fmt.Errorf("verify.yml key seed must be %d bytes: %d", ed25519.SeedSize, len(seed)))
		}
		keys = append(keys, &VerifyServerKey{
			ServerKeyId: byte(confKey.ServerKeyId),
			PrivateKey:  ed25519.NewKeyFromSeed(seed),
		})
	}
	if len(keys) == 0 {
		panic(fmt.Errorf("verify.yml must define at least one key"))
	}
	return keys
})

// verifyServerKeysInstance, when set, overrides the vault keys (the
// swappable-instance pattern of `SetCoinbaseClient`, for tests).
var verifyServerKeysInstance []*VerifyServerKey

func SetVerifyServerKeys(keys []*VerifyServerKey) {
	verifyServerKeysInstance = keys
}

func verifyServerKeys() []*VerifyServerKey {
	if verifyServerKeysInstance != nil {
		return verifyServerKeysInstance
	}
	return verifyServerKeysFromVault()
}

// verifySigningKey is the newest key (first entry), which signs new trails.
func verifySigningKey() *VerifyServerKey {
	return verifyServerKeys()[0]
}

// verifySyntheticSeedContext domain-separates the synthetic-seed-id HMAC from
// any other use of the server signing seed.
const verifySyntheticSeedContext = "urnetwork/verify/synthetic-seed/v1"

// verifySyntheticSeedId derives a deterministic synthetic seed-hop id from a
// source `anchor`, so an unresolved-source poison seed returns a STABLE
// `trail[0]` across identical repeated seeds (§9, V2). Before this the poison
// seed used a fresh `server.NewId()` per call, so a caller could seed twice and
// compare `trail[0]` — changes → poison, stable → real — a cheap oracle. A
// deterministic id per source mimics how a real source resolves to one stable
// provider id.
//
// The anchor is the caller's canonical (port-stripped) source ip, NOT the raw
// `ClientAddress`: nginx forces `X-UR-Forwarded-For = $remote_addr:$remote_port`
// (V11), so the ephemeral source port varies per connection and anchoring on it
// would reopen the oracle. `ResolveVerifyEgress` keys on the ip alone, so the
// synthetic id must too.
//
// The HMAC secret is the server signing seed (verify.yml), so the id is stable
// across a deployment but unguessable — an outside observer can neither
// precompute a source's synthetic id nor invert one back to a source ip.
func verifySyntheticSeedId(anchor string) server.Id {
	mac := hmac.New(sha256.New, verifySigningKey().PrivateKey.Seed())
	mac.Write([]byte(verifySyntheticSeedContext))
	mac.Write([]byte(anchor))
	sum := mac.Sum(nil)
	var id server.Id
	copy(id[:], sum[:16])
	return id
}

// verifySyntheticEgressContext domain-separates the synthetic egress-IP-hash
// HMAC from verifySyntheticSeedId and every other use of the signing seed.
const verifySyntheticEgressContext = "urnetwork/verify/synthetic-egress/v1"

// verifySyntheticEgressIpHash derives a deterministic synthetic egress-IP-hash
// for a poison seed hop (D27, §9 indistinguishability): a poison hop must carry
// a synthetic, deterministic egress-IP-hash — shaped like a real one (32 bytes)
// and STABLE per source (the verifySyntheticSeedId V2 treatment) — so poison
// and real proofs are shaped identically and there is no observable branch on
// poison for this field. Keyed on the server signing seed so it is unguessable
// and never collides with a real provider's peppered egress-IP-hash.
func verifySyntheticEgressIpHash(anchor string) [32]byte {
	mac := hmac.New(sha256.New, verifySigningKey().PrivateKey.Seed())
	mac.Write([]byte(verifySyntheticEgressContext))
	mac.Write([]byte(anchor))
	return [32]byte(mac.Sum(nil))
}

// verifyServerKeyById selects the key a trail was started under, so a trail
// keeps one `server_key_id` across a rotation. Falls back to the newest key
// if the id is unknown (removed from the vault mid-trail).
func verifyServerKeyById(serverKeyId byte) *VerifyServerKey {
	for _, key := range verifyServerKeys() {
		if key.ServerKeyId == serverKeyId {
			return key
		}
	}
	return verifySigningKey()
}

// verifySettingsInstance holds the protocol parameters (§5.5). Swappable for
// tests via `SetVerifySettings`.
var verifySettingsInstance *model.VerifySettings

var verifySettingsFromVault = sync.OnceValue(func() *model.VerifySettings {
	res := server.Vault.RequireSimpleResource("verify.yml")
	var conf struct {
		Profile       string `yaml:"profile"`
		PolicyHash    string `yaml:"policy_hash"`
		EgressHashKey string `yaml:"egress_hash_key"`
		Settings      struct {
			StepTimeoutSeconds           int64  `yaml:"step_timeout_seconds"`
			StepTimeoutGraceSeconds      int64  `yaml:"step_timeout_grace_seconds"`
			TrailTtlGraceSeconds         int64  `yaml:"trail_ttl_grace_seconds"`
			EgressTtlSeconds             int64  `yaml:"egress_ttl_seconds"`
			EgressRefreshSeconds         int64  `yaml:"egress_refresh_seconds"`
			ReliabilityAMin              int64  `yaml:"reliability_a_min"`
			StatsPeriodSeconds           int64  `yaml:"stats_period_seconds"`
			EgressIpv4Prefix             int    `yaml:"egress_ipv4_prefix"`
			EgressIpv6Prefix             int    `yaml:"egress_ipv6_prefix"`
			EgressHashKeyId              string `yaml:"egress_hash_key_id"`
			SoftGuardrailsEnabled        bool   `yaml:"soft_guardrails_enabled"`
			HardSeedPerMinutePerSource   int64  `yaml:"hard_seed_per_minute_per_source"`
			HardExtendPerMinutePerSource int64  `yaml:"hard_extend_per_minute_per_source"`
			HardActiveTrailsPerSource    int64  `yaml:"hard_active_trails_per_source"`
		} `yaml:"settings"`
	}
	res.UnmarshalYaml(&conf)
	if conf.Profile != "testnet" && conf.Profile != "mainnet" {
		panic(fmt.Errorf("verify.yml profile must be testnet or mainnet"))
	}
	if cfg := stConfig(); cfg != nil {
		if cfg.Profile != conf.Profile || !strings.EqualFold(conf.PolicyHash, fmt.Sprintf("0x%x", cfg.PolicyHash)) {
			panic(fmt.Errorf("verify.yml profile/policy_hash does not match st.yml"))
		}
	}
	key, err := base64.StdEncoding.DecodeString(conf.EgressHashKey)
	if err != nil || len(key) != 32 {
		panic(fmt.Errorf("verify.yml egress_hash_key must be base64 32 bytes"))
	}
	s := model.DefaultVerifySettings()
	v := conf.Settings
	if v.StepTimeoutSeconds <= 0 || v.StepTimeoutGraceSeconds < 0 || v.TrailTtlGraceSeconds <= 0 ||
		v.EgressTtlSeconds <= 0 || v.EgressRefreshSeconds <= 0 || v.StatsPeriodSeconds <= 0 || v.ReliabilityAMin <= 0 ||
		v.EgressIpv4Prefix < 0 || v.EgressIpv4Prefix > 32 || v.EgressIpv6Prefix < 0 || v.EgressIpv6Prefix > 128 ||
		v.EgressHashKeyId == "" || v.HardSeedPerMinutePerSource <= 0 || v.HardExtendPerMinutePerSource <= 0 || v.HardActiveTrailsPerSource <= 0 {
		panic(fmt.Errorf("verify.yml settings are incomplete or invalid"))
	}
	s.StepTimeout = time.Duration(v.StepTimeoutSeconds) * time.Second
	s.StepTimeoutGrace = time.Duration(v.StepTimeoutGraceSeconds) * time.Second
	s.TrailTtlGrace = time.Duration(v.TrailTtlGraceSeconds) * time.Second
	s.EgressTtl = time.Duration(v.EgressTtlSeconds) * time.Second
	s.EgressRefreshInterval = time.Duration(v.EgressRefreshSeconds) * time.Second
	s.StatsPeriod = time.Duration(v.StatsPeriodSeconds) * time.Second
	s.ReliabilityAMin = v.ReliabilityAMin
	s.EgressHashV4Prefix, s.EgressHashV6Prefix = v.EgressIpv4Prefix, v.EgressIpv6Prefix
	s.EgressHashKey, s.EgressHashKeyId = append([]byte(nil), key...), v.EgressHashKeyId
	s.SoftLimitsEnabled = v.SoftGuardrailsEnabled
	s.SeedRateHardLimit, s.ExtendRateHardLimit, s.ActiveTrailsHardLimit = v.HardSeedPerMinutePerSource, v.HardExtendPerMinutePerSource, v.HardActiveTrailsPerSource
	return s
})

func SetVerifySettings(settings *model.VerifySettings) {
	verifySettingsInstance = settings
}

func verifySettings() *model.VerifySettings {
	if verifySettingsInstance != nil {
		return verifySettingsInstance
	}
	return verifySettingsFromVault()
}

// VerifyArgs is the decoded `POST /verify` body — the superset of the two
// wire shapes `connect.VerifySeedArgs` and `connect.VerifyExtendArgs`.
// `TrailId` presence dispatches: nil → SEED, non-nil → EXTEND.
type VerifyArgs struct {
	ClientId server.Id `json:"client_id"`
	// seed shape (§4.1)
	Vpk         []byte `json:"vpk"`
	ClientNonce []byte `json:"client_nonce"`
	SeedSig     []byte `json:"seed_sig"`
	M           int    `json:"M"`
	// extend shape (§4.2)
	TrailId   *server.Id  `json:"trail_id"`
	Trail     []server.Id `json:"trail"`
	ExtendSig []byte      `json:"extend_sig"`
}

// Verify backs `POST /verify`. The result is one of the two connect wire
// shapes: `*connect.VerifyAssignResult` (non-final) or
// `*connect.VerifyFinalResult` (depth M).
func Verify(
	verify *VerifyArgs,
	clientSession *session.ClientSession,
) (any, error) {
	if verify.TrailId == nil {
		return verifySeed(verify, clientSession)
	}
	return verifyExtend(verify, clientSession)
}

// verifyClampM clamps a requested trail depth into [MMin, MMax]; a missing M
// takes the default (§5.5).
func verifyClampM(m int) int {
	if m == 0 {
		return connect.VerifyMDefault
	}
	if m < connect.VerifyMMin {
		return connect.VerifyMMin
	}
	if connect.VerifyMMax < m {
		return connect.VerifyMMax
	}
	return m
}

// verifyConnectIds converts hop ids to the wire id type for the canonical
// message builders.
func verifyConnectIds(clientIds []server.Id) []connect.Id {
	connectIds := make([]connect.Id, len(clientIds))
	for i, clientId := range clientIds {
		connectIds[i] = connect.Id(clientId)
	}
	return connectIds
}

// verifyConfirmedIds returns the trail's confirmed hop ids in order (seed
// first).
func verifyConfirmedIds(trail *model.VerifyTrail) []server.Id {
	clientIds := make([]server.Id, len(trail.Hops))
	for i, hop := range trail.Hops {
		clientIds[i] = hop.ClientId
	}
	return clientIds
}

// verifyCachedResponse is the `{vtr_<trailId>}resp` envelope for idempotent
// retries (§4.3): exactly one of the two response shapes.
type verifyCachedResponse struct {
	Assign *connect.VerifyAssignResult `json:"assign,omitempty"`
	Final  *connect.VerifyFinalResult  `json:"final,omitempty"`
}

func verifyEncodeCachedResponse(cached *verifyCachedResponse) string {
	cachedJson, err := json.Marshal(cached)
	server.Raise(err)
	return string(cachedJson)
}

func verifyDecodeCachedResponse(responseJson string) (any, error) {
	var cached verifyCachedResponse
	err := json.Unmarshal([]byte(responseJson), &cached)
	if err != nil {
		return nil, err
	}
	if cached.Final != nil {
		return cached.Final, nil
	}
	if cached.Assign != nil {
		return cached.Assign, nil
	}
	return nil, fmt.Errorf("empty cached response")
}

// verifySeed implements the §4.1 SEED checklist. The checks are evaluated in
// full before branching so real and poisoned seeds share one code path and
// one response shape.
func verifySeed(
	verify *VerifyArgs,
	clientSession *session.ClientSession,
) (any, error) {
	ctx := clientSession.Ctx
	settings := verifySettings()

	// input shape (a caller that cannot even form the message gets a plain
	// error — signature validity does not depend on any provider state, so
	// this is not an oracle)
	if len(verify.Vpk) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("400 vpk must be %d bytes", ed25519.PublicKeySize)
	}
	if len(verify.ClientNonce) != connect.VerifyNonceSize {
		return nil, fmt.Errorf("400 client_nonce must be %d bytes", connect.VerifyNonceSize)
	}
	if len(verify.SeedSig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("400 seed_sig must be %d bytes", ed25519.SignatureSize)
	}
	if verify.M < 0 {
		return nil, fmt.Errorf("400 M must be non-negative")
	}

	// §9 (V5): shed floods before any crypto or provider-state lookup. the
	// per-source-ip/vpk hard rate limit is the DoS bound on trail-state
	// creation, so its increment + hard-limit early-reject run first, right
	// after the input-shape checks — ahead of the Ed25519 verify and the
	// egress / public-key Redis lookups. only the HARD-limit reject moves up;
	// the soft-limit (poison) `rateOver` is computed later where it feeds
	// `poison`.
	//
	// the source ip is the nginx-forced `$remote_addr` (V11): see
	// session.NewClientSessionFromRequest. §8 attribution and these per-ip
	// limits are sound only because no normal-ingress caller can spoof it; if
	// any ingress path ever reaches POST /verify without that override, this
	// breaks — it is a load-bearing deploy invariant (runbook check).
	sourceIp, sourceIpOk := model.ParseVerifyEgressIp(clientSession.ClientAddress)
	sourceIpStr := "0.0.0.0"
	if sourceIpOk {
		sourceIpStr = sourceIp.String()
	}
	ipCount, vpkCount := model.IncrVerifySeedRates(ctx, sourceIpStr, verify.Vpk, settings)
	if settings.SeedRateHardLimit < ipCount || settings.SeedRateHardLimit < vpkCount {
		// per-ip is the real limit; per-vpk is best-effort only (vpk is
		// unauthenticated caller input, rotatable to reset its counter — V4)
		return nil, fmt.Errorf("429 rate limit exceeded")
	}

	// clamp M (§5.5)
	m := verifyClampM(verify.M)

	// §4.1 step 2: verify seed_sig under vpk (Appendix A.1).
	// note the signed message carries the *requested* M, not the clamp
	seedMessage, err := connect.BuildVerifySeedMessage(verify.Vpk, verify.ClientNonce, byte(verify.M))
	if err != nil {
		return nil, fmt.Errorf("400 %s", err)
	}
	if !connect.VerifyVerifyMessageSignature(verify.Vpk, seedMessage, verify.SeedSig) {
		return nil, fmt.Errorf("400 invalid seed signature")
	}

	// §4.1 step 1: resolve source ip → seed hop (§8.1); unresolved → poison
	var seedClientId *server.Id
	if sourceIpOk {
		seedClientId = model.ResolveVerifyEgress(ctx, sourceIp, settings)
	}

	// §4.1 step 3: vpk must be the caller client_id's registered key (D-7)
	registeredKey, err := model.GetClientPublicKey(ctx, verify.ClientId)
	if err != nil {
		return nil, err
	}
	keyOk := len(registeredKey) == ed25519.PublicKeySize && slices.Equal(registeredKey, verify.Vpk)

	// §9: over the soft rate limit → poison (computed from the early counters).
	// per-vpk is best-effort (unauthenticated input); per-source-ip is the real
	// control (V4). D26: the soft tier is validator-configurable and default off
	// (SoftLimitsEnabled=false) so each validator drives its own trail rate; the
	// hard per-source-ip reject above stays as the DoS backstop.
	rateOver := settings.SoftLimitsEnabled &&
		(settings.SeedRateSoftLimit < ipCount || settings.SeedRateSoftLimit < vpkCount)

	// §9: concurrent active trail cap per vpk (soft → poison, hard → refuse).
	// The hard cap always applies; D26 gates only the soft → poison tier.
	activeTrails := model.IncrVerifyActiveTrails(ctx, verify.Vpk, settings)
	if settings.ActiveTrailsHardLimit < activeTrails {
		model.DecrVerifyActiveTrails(ctx, verify.Vpk)
		return nil, fmt.Errorf("429 rate limit exceeded")
	}
	capOver := settings.SoftLimitsEnabled && settings.ActiveTrailsSoftLimit < activeTrails

	// §4.1 steps 3-4: the validator may not seed through itself, and the
	// seed hop must be an eligible provider (active provide mode + holds an
	// eligibility token; resolution itself already enforced the §8.2
	// bijection)
	seedEligible := false
	if seedClientId != nil && *seedClientId != verify.ClientId {
		provideModes, err := model.GetProvideModes(ctx, *seedClientId)
		if err == nil && 0 < len(provideModes) {
			seedEligible = model.CheckVerifyEligibilityToken(ctx, *seedClientId, settings)
		}
	}

	poison := seedClientId == nil || !keyOk || !seedEligible || rateOver || capOver

	// §4.1 step 5: create the trail. the seed hop is the validator-chosen
	// entry — excluded from statistics (§7.6): AssignedMs/AssignN stay zero
	now := server.NowUtc()
	nowMs := uint64(now.UnixMilli())
	trailId := server.NewId()
	serverNonce := make([]byte, connect.VerifyNonceSize)
	_, err = rand.Read(serverNonce)
	server.Raise(err)

	var seedHopClientId server.Id
	var seedHopEgressHash [32]byte
	if seedClientId != nil {
		// use the resolved hop even when poisoned for another reason, so a
		// caller probing with its own known client_id cannot distinguish
		seedHopClientId = *seedClientId
		// the seed request's source ip is the seed provider's egress ip;
		// record its egress-IP-hash at the configured granularity (§8.1, D27).
		// sourceIp is valid here — seedClientId resolved from it.
		seedHopEgressHash = model.VerifyEgressIpHashWithSettings(sourceIp, settings)
	} else {
		// synthetic: never touches a real provider (§9). deterministic per
		// source ip (V2) so repeated identical seeds return a STABLE trail[0],
		// matching how a real source resolves to one stable provider — a fresh
		// server.NewId() here was a seed-twice-and-compare oracle
		seedHopClientId = verifySyntheticSeedId(sourceIpStr)
		// §9: the poison seed hop carries a synthetic, deterministic
		// egress-IP-hash shaped like a real one (D27) — no observable branch on
		// poison for this field
		seedHopEgressHash = verifySyntheticEgressIpHash(sourceIpStr)
	}

	// §4.1 step 6: both normal and poison trails sample a real routable next
	// hop. Poison replaces only the provider-stat mutation with a timing pad.
	var nextHopClientId server.Id
	var assignN int
	nextHop, n := model.SampleVerifyNextHop(
		ctx,
		[]server.Id{seedHopClientId, verify.ClientId},
		settings,
	)
	if nextHop == nil {
		if !poison {
			// no eligible provider to assign: degrade to poison so the
			// response stays indistinguishable from the working network case
			poison = true
		}
		assignN = model.PadVerifySample(ctx, settings)
		nextHopClientId = server.NewId()
		model.IncrVerifyPoisonCounter(ctx, "assign")
	} else {
		nextHopClientId = *nextHop
		assignN = n
		if poison {
			model.PadVerifySample(ctx, settings)
			model.IncrVerifyPoisonCounter(ctx, "assign")
		} else {
			model.RecordVerifyAssignment(ctx, nextHopClientId, now, settings)
		}
	}

	signingKey := verifySigningKey()
	trail := &model.VerifyTrail{
		TrailId:     trailId,
		ClientId:    verify.ClientId,
		Vpk:         verify.Vpk,
		ServerNonce: serverNonce,
		M:           m,
		ServerKeyId: signingKey.ServerKeyId,
		Status:      model.VerifyTrailStatusActive,
		Poison:      poison,
		CreateMs:    nowMs,
		ActivityMs:  nowMs,
		Hops: []*model.VerifyTrailHop{
			{
				ClientId:     seedHopClientId,
				ConfirmedMs:  nowMs,
				Seed:         true,
				EgressIpHash: seedHopEgressHash,
			},
		},
		Pending: &model.VerifyTrailHop{
			ClientId:   nextHopClientId,
			AssignedMs: nowMs,
			AssignN:    assignN,
		},
	}

	// §4.1 step 6: ASSIGN over the confirmed hops plus the pending hop
	// (Appendix A.3)
	assignMessage, err := connect.BuildVerifyAssignMessage(
		signingKey.ServerKeyId,
		connect.Id(trailId),
		serverNonce,
		verify.Vpk,
		byte(m),
		verifyConnectIds([]server.Id{seedHopClientId, nextHopClientId}),
	)
	server.Raise(err)
	assignSig := connect.SignVerifyMessage(signingKey.PrivateKey, assignMessage)

	result := &connect.VerifyAssignResult{
		TrailId:     connect.Id(trailId),
		ServerNonce: serverNonce,
		Trail:       verifyConnectIds([]server.Id{seedHopClientId}),
		NextHop:     connect.Id(nextHopClientId),
		M:           m,
		ServerKeyId: signingKey.ServerKeyId,
		AssignSig:   assignSig,
	}

	// §4.1 step 7: persist trail + cached response (§4.3)
	model.CreateVerifyTrail(
		ctx,
		trail,
		verifyEncodeCachedResponse(&verifyCachedResponse{Assign: result}),
		settings,
	)

	if glog.V(2) {
		glog.Infof("[verify]seed trail %s depth 1/%d (poison=%t)\n", trailId, m, poison)
	}
	return result, nil
}

// verifyExtend implements the §4.2 EXTEND checklist plus the §4.3 idempotent
// replay and §4.4 failure handling. Poison trails run the identical path
// except that provider-stat writes are replaced by pads. Pending poison hops
// are real eligible providers selected by the same sampler, so source
// attribution and the complete depth-M routing path remain executable.
func verifyExtend(
	verify *VerifyArgs,
	clientSession *session.ClientSession,
) (any, error) {
	ctx := clientSession.Ctx
	settings := verifySettings()

	if len(verify.ExtendSig) != ed25519.SignatureSize {
		return nil, fmt.Errorf("400 extend_sig must be %d bytes", ed25519.SignatureSize)
	}
	if len(verify.Trail) == 0 || connect.VerifyMMax < len(verify.Trail) {
		return nil, fmt.Errorf("400 trail must have 1 to %d hops", connect.VerifyMMax)
	}
	trailId := *verify.TrailId

	// §9 (V4): meter the EXTEND path per source ip before the trail load and
	// lock. EXTEND is otherwise unmetered and drives the mutating
	// read-modify-write, so this is its per-source-ip DoS bound, complementing
	// the SEED per-ip limit. the source ip is the nginx-forced `$remote_addr`
	// (V11): see session.NewClientSessionFromRequest — §8 attribution and this
	// limit are sound only because normal-ingress callers cannot spoof it.
	sourceIp, sourceIpOk := model.ParseVerifyEgressIp(clientSession.ClientAddress)
	sourceIpStr := "0.0.0.0"
	if sourceIpOk {
		sourceIpStr = sourceIp.String()
	}
	if settings.ExtendRateHardLimit < model.IncrVerifyExtendRate(ctx, sourceIpStr, settings) {
		return nil, fmt.Errorf("429 rate limit exceeded")
	}

	// §4.2 step 1: resolve source ip → hop (§8.1), unconditionally, so the
	// lookup cost does not depend on what follows
	var sourceClientId *server.Id
	if sourceIpOk {
		sourceClientId = model.ResolveVerifyEgress(ctx, sourceIp, settings)
	}

	// §4.2 step 2: load the trail
	trail := model.GetVerifyTrail(ctx, trailId)
	if trail == nil {
		return nil, fmt.Errorf("400 trail not found")
	}

	confirmedIds := verifyConfirmedIds(trail)

	// §4.3 idempotency: a resend whose trail matches the already-confirmed
	// hops replays the cached response — no double count, no depth advance.
	// the signature is still required so only the vpk holder can fetch it.
	if slices.Equal(verify.Trail, confirmedIds) {
		replayMessage, err := connect.BuildVerifyExtendMessage(
			connect.Id(trailId),
			trail.ServerNonce,
			trail.Vpk,
			byte(trail.M),
			verifyConnectIds(verify.Trail),
		)
		if err != nil {
			return nil, fmt.Errorf("400 %s", err)
		}
		if !connect.VerifyVerifyMessageSignature(trail.Vpk, replayMessage, verify.ExtendSig) {
			return nil, fmt.Errorf("400 invalid extend signature")
		}
		responseJson, ok := model.GetVerifyTrailResponse(ctx, trailId)
		if !ok {
			return nil, fmt.Errorf("400 trail not found")
		}
		return verifyDecodeCachedResponse(responseJson)
	}

	// V3: serialize the mutating read-modify-write below (confirm+assign /
	// complete) with a short per-trail lock, so two concurrent identical valid
	// EXTENDs cannot both confirm the same pending hop (double-counting c_Y,
	// corrupting the hop list, and stuffing the latency histogram). The
	// idempotent-replay branch above is read-only and deliberately stays BEFORE
	// the lock, so a retry after completion still serves the cached response
	// without contending. On contention return 409; the caller retries
	// idempotently (the retry replays once this holder commits). The single
	// defer covers every early-return in the mutating path.
	if !model.AcquireVerifyTrailLock(ctx, trailId, settings.TrailLockTtl) {
		return nil, fmt.Errorf("409 trail busy")
	}
	defer model.ReleaseVerifyTrailLock(ctx, trailId)

	// §4.2 step 2: require active and not expired
	if trail.Status != model.VerifyTrailStatusActive {
		return nil, fmt.Errorf("400 trail not active")
	}
	if trail.Pending == nil {
		return nil, fmt.Errorf("400 trail not active")
	}

	now := server.NowUtc()
	nowMs := uint64(now.UnixMilli())

	// §4.4: the pending hop must confirm within StepTimeout
	deadlineMs := trail.Pending.AssignedMs + uint64(settings.StepTimeout.Milliseconds())
	if deadlineMs < nowMs {
		verifyFailTrail(ctx, trail)
		return nil, fmt.Errorf("400 trail failed")
	}

	// §4.2 step 3: the submitted trail must equal the confirmed hops plus
	// the single pending hop — history cannot be rewritten
	expectedTrail := append(slices.Clone(confirmedIds), trail.Pending.ClientId)
	if !slices.Equal(verify.Trail, expectedTrail) {
		verifyFailTrail(ctx, trail)
		return nil, fmt.Errorf("400 trail failed")
	}

	// §4.2 step 4: verify extend_sig under the trail's vpk over the
	// canonical path (Appendix A.2)
	extendMessage, err := connect.BuildVerifyExtendMessage(
		connect.Id(trailId),
		trail.ServerNonce,
		trail.Vpk,
		byte(trail.M),
		verifyConnectIds(verify.Trail),
	)
	if err != nil {
		verifyFailTrail(ctx, trail)
		return nil, fmt.Errorf("400 trail failed")
	}
	if !connect.VerifyVerifyMessageSignature(trail.Vpk, extendMessage, verify.ExtendSig) {
		verifyFailTrail(ctx, trail)
		return nil, fmt.Errorf("400 trail failed")
	}

	// §4.2 step 5: the request truly egressed from the assigned provider. This
	// check applies identically to normal and poison shadow routes.
	if sourceClientId == nil || *sourceClientId != trail.Pending.ClientId {
		verifyFailTrail(ctx, trail)
		return nil, fmt.Errorf("400 trail failed")
	}

	// §4.2 step 6: confirm the hop, stamp server time (§3.4), record
	// per-step latency at the instant of confirmation (§7.5)
	confirmedHop := &model.VerifyTrailHop{
		ClientId:    trail.Pending.ClientId,
		AssignedMs:  trail.Pending.AssignedMs,
		ConfirmedMs: nowMs,
		AssignN:     trail.Pending.AssignN,
		// §4.2 step 5 passed, so the confirming request's source ip is this
		// provider's egress ip — record its egress-IP-hash at the configured
		// granularity (§8.1, §3.3, D27). Computed unconditionally: a poison
		// trail's synthetic pending fails step 5 before here, so there is no
		// observable branch on poison.
		EgressIpHash: model.VerifyEgressIpHashWithSettings(sourceIp, settings),
	}
	if !trail.Poison {
		latencyMs := int64(confirmedHop.ConfirmedMs - confirmedHop.AssignedMs)
		model.RecordVerifyConfirmation(ctx, confirmedHop.ClientId, latencyMs, now, settings)
	} else {
		model.IncrVerifyPoisonCounter(ctx, "confirm")
	}

	confirmedHops := append(slices.Clone(trail.Hops), confirmedHop)
	depth := len(confirmedHops)
	confirmedHopIds := make([]server.Id, depth)
	for i, hop := range confirmedHops {
		confirmedHopIds[i] = hop.ClientId
	}
	serverKey := verifyServerKeyById(trail.ServerKeyId)

	// §4.2 step 7: depth == M → FINAL + publish; else assign the next hop
	if trail.M <= depth {
		proofHops := make([]connect.VerifyProofHop, depth)
		for i, hop := range confirmedHops {
			proofHops[i] = connect.VerifyProofHop{
				ClientId: connect.Id(hop.ClientId),
				TimeMs:   hop.ConfirmedMs,
				// each hop's stored egress-IP-hash goes into the FINAL proof; it
				// is inside the signed FINAL message (BuildVerifyFinalMessage
				// already appends it), so the server attests which IP-prefix
				// each hop egressed from and a fleet cannot forge its IPs (D27)
				EgressIpHash: hop.EgressIpHash,
			}
		}
		finalMessage, err := connect.BuildVerifyFinalMessage(
			serverKey.ServerKeyId,
			connect.Id(trailId),
			trail.ServerNonce,
			trail.Vpk,
			byte(trail.M),
			proofHops,
		)
		server.Raise(err)
		// §7.6: v1 coverage is the M server-confirmed hops with the validator's
		// seed hop excluded (M-1). Guard M>=1 so the unsigned subtraction cannot
		// underflow (BuildVerifyFinalMessage above already rejects M==0).
		if trail.M < 1 {
			return nil, fmt.Errorf("400 trail M must be at least 1")
		}
		coverage := uint64(trail.M - 1)
		// Release 1.0 signs the canonical FINAL bytes directly. Coverage is
		// deterministic M-1 measurement metadata; the parked effort-bounty
		// digest is not part of the active protocol.
		finalSig := connect.SignVerifyMessage(serverKey.PrivateKey, finalMessage)

		proof := &connect.VerifyProof{
			Header: connect.VerifyProofHeader{
				TrailId:     connect.Id(trailId),
				ServerNonce: trail.ServerNonce,
				Vpk:         trail.Vpk,
				M:           trail.M,
			},
			Hops:        proofHops,
			ServerKeyId: serverKey.ServerKeyId,
			// deterministic measurement metadata (seed excluded)
			Coverage: coverage,
			FinalSig: finalSig,
			// the depth-M EXTEND endorses the entire path (§3.2); its wire
			// name is verifier_sig (§3.3)
			VerifierSig: verify.ExtendSig,
		}
		result := &connect.VerifyFinalResult{
			Status: connect.VerifyStatusComplete,
			Proof:  proof,
		}

		model.CompleteVerifyTrail(
			ctx,
			trail,
			confirmedHop,
			verifyEncodeCachedResponse(&verifyCachedResponse{Final: result}),
			settings,
		)
		model.DecrVerifyActiveTrails(ctx, trail.Vpk)

		if !trail.Poison {
			// §3.3/§6.2: publish the proof durably; a poison trail is
			// silently never published (§9)
			hopsJson, err := json.Marshal(proofHops)
			server.Raise(err)
			completeTime := now.UTC()
			model.InsertVerifyTrail(ctx, &model.VerifyTrailRow{
				TrailId:      trailId,
				Vpk:          trail.Vpk,
				ServerKeyId:  serverKey.ServerKeyId,
				ServerNonce:  trail.ServerNonce,
				Depth:        trail.M,
				Status:       model.VerifyTrailRowStatusComplete,
				HopsJson:     string(hopsJson),
				FinalSig:     finalSig,
				VerifierSig:  verify.ExtendSig,
				CreateTime:   time.UnixMilli(int64(trail.CreateMs)).UTC(),
				CompleteTime: &completeTime,
			})
		}

		if glog.V(2) {
			glog.Infof("[verify]complete trail %s depth %d (poison=%t)\n", trailId, depth, trail.Poison)
		}
		return result, nil
	}

	// sample the next hop (§5.1): without replacement within the trail,
	// excluding the validator's own client id. Poison uses the same routable
	// population and substitutes a pad for the stats write.
	var nextHopClientId server.Id
	var assignN int
	sampled := false
	excludeClientIds := append(slices.Clone(confirmedHopIds), trail.ClientId)
	nextHop, n := model.SampleVerifyNextHop(ctx, excludeClientIds, settings)
	if nextHop != nil {
		nextHopClientId = *nextHop
		assignN = n
		sampled = true
		if trail.Poison {
			model.PadVerifySample(ctx, settings)
			model.IncrVerifyPoisonCounter(ctx, "assign")
		} else {
			model.RecordVerifyAssignment(ctx, nextHopClientId, now, settings)
		}
	}
	if !sampled {
		if !trail.Poison {
			// a real trail with no assignable provider cannot continue and
			// cannot be completed honestly: fail it without blame (there is
			// no unreached assigned hop)
			verifyFailTrail(ctx, trail)
			return nil, fmt.Errorf("400 trail failed")
		}
		assignN = model.PadVerifySample(ctx, settings)
		nextHopClientId = server.NewId()
		model.IncrVerifyPoisonCounter(ctx, "assign")
	}

	newPending := &model.VerifyTrailHop{
		ClientId:   nextHopClientId,
		AssignedMs: nowMs,
		AssignN:    assignN,
	}

	assignMessage, err := connect.BuildVerifyAssignMessage(
		serverKey.ServerKeyId,
		connect.Id(trailId),
		trail.ServerNonce,
		trail.Vpk,
		byte(trail.M),
		verifyConnectIds(append(slices.Clone(confirmedHopIds), nextHopClientId)),
	)
	server.Raise(err)
	assignSig := connect.SignVerifyMessage(serverKey.PrivateKey, assignMessage)

	result := &connect.VerifyAssignResult{
		TrailId:     connect.Id(trailId),
		ServerNonce: trail.ServerNonce,
		Trail:       verifyConnectIds(confirmedHopIds),
		NextHop:     connect.Id(nextHopClientId),
		M:           trail.M,
		ServerKeyId: serverKey.ServerKeyId,
		AssignSig:   assignSig,
	}

	model.ConfirmVerifyHopAndAssign(
		ctx,
		trail,
		confirmedHop,
		newPending,
		verifyEncodeCachedResponse(&verifyCachedResponse{Assign: result}),
		settings,
	)

	if glog.V(2) {
		glog.Infof("[verify]extend trail %s depth %d/%d (poison=%t)\n", trailId, depth, trail.M, trail.Poison)
	}
	return result, nil
}

// verifyFailTrail applies §4.4: mark the trail expired, release the vpk's
// active-trail slot, and, for real trails, persist the expired record. The
// failure is attributed to the pending hop by construction (its assignment
// was counted at ASSIGN time and no confirmation ever will be, §7.2).
func verifyFailTrail(ctx context.Context, trail *model.VerifyTrail) {
	model.ExpireVerifyTrail(ctx, trail.TrailId)
	model.DecrVerifyActiveTrails(ctx, trail.Vpk)
	if !trail.Poison {
		model.InsertVerifyTrail(ctx, model.NewExpiredVerifyTrailRow(trail))
	}
	if glog.V(2) {
		glog.Infof("[verify]failed trail %s depth %d (poison=%t)\n", trail.TrailId, len(trail.Hops), trail.Poison)
	}
}

// VerifyKey is one published server verification key (§3.5).
type VerifyKey struct {
	ServerKeyId byte   `json:"server_key_id"`
	PublicKey   []byte `json:"public_key"`
}

// GetVerifyKeysResult backs `GET /verify/keys`: all historical server public
// keys by `server_key_id`, so third parties can verify published proofs
// across rotations (§3.5). Unauthenticated by design.
type GetVerifyKeysResult struct {
	Keys []*VerifyKey `json:"keys"`
}

type GetVerifyEvidenceArgs struct {
	From  time.Time
	To    time.Time
	Limit int
}

type PublicVerifyStatsRow struct {
	PeriodStart    time.Time `json:"period_start"`
	PeriodEnd      time.Time `json:"period_end"`
	ClientId       server.Id `json:"client_id"`
	Assignments    uint64    `json:"assignments"`
	Confirmations  uint64    `json:"confirmations"`
	ReliabilityPPM uint32    `json:"reliability_ppm"`
	LatencyP50Ms   *int      `json:"latency_p50_ms,omitempty"`
	LatencyP90Ms   *int      `json:"latency_p90_ms,omitempty"`
	LatencyP99Ms   *int      `json:"latency_p99_ms,omitempty"`
}

type GetVerifyStatsResult struct {
	Schema          string                  `json:"schema"`
	Profile         string                  `json:"profile"`
	PolicyHash      string                  `json:"policy_hash"`
	EgressHashKeyId string                  `json:"egress_hash_key_id"`
	Rows            []*PublicVerifyStatsRow `json:"rows"`
}

type PublicVerifyProofRow struct {
	TrailId      server.Id       `json:"trail_id"`
	Vpk          []byte          `json:"vpk"`
	ServerKeyId  byte            `json:"server_key_id"`
	ServerNonce  []byte          `json:"server_nonce"`
	Depth        int             `json:"depth"`
	Status       int             `json:"status"`
	Hops         json.RawMessage `json:"hops"`
	FinalSig     []byte          `json:"final_sig,omitempty"`
	VerifierSig  []byte          `json:"verifier_sig,omitempty"`
	CreateTime   time.Time       `json:"create_time"`
	CompleteTime *time.Time      `json:"complete_time,omitempty"`
}

type GetVerifyProofsResult struct {
	Schema     string                  `json:"schema"`
	Profile    string                  `json:"profile"`
	PolicyHash string                  `json:"policy_hash"`
	Rows       []*PublicVerifyProofRow `json:"rows"`
}

func verifyEvidenceRange(args *GetVerifyEvidenceArgs, defaultLimit, maxLimit int) (time.Time, time.Time, int, error) {
	if args == nil {
		args = &GetVerifyEvidenceArgs{}
	}
	to := args.To.UTC()
	if to.IsZero() {
		to = server.NowUtc()
	}
	from := args.From.UTC()
	if from.IsZero() {
		from = to.Add(-31 * 24 * time.Hour)
	}
	if !from.Before(to) || to.Sub(from) > 93*24*time.Hour {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("400 evidence range must be positive and at most 93 days")
	}
	limit := args.Limit
	if limit == 0 {
		limit = defaultLimit
	}
	if limit < 1 || limit > maxLimit {
		return time.Time{}, time.Time{}, 0, fmt.Errorf("400 evidence limit must be 1..%d", maxLimit)
	}
	return from, to, limit, nil
}

// GetVerifyStats exposes operator-observed rollups for audit/reconstruction.
// Validators intentionally do not consume this endpoint for Q_n.
func GetVerifyStats(args *GetVerifyEvidenceArgs, clientSession *session.ClientSession) (*GetVerifyStatsResult, error) {
	from, to, limit, err := verifyEvidenceRange(args, 10_000, 100_000)
	if err != nil {
		return nil, err
	}
	settings := verifySettings()
	result := &GetVerifyStatsResult{Schema: "urnetwork-verify-stats-index-v1", EgressHashKeyId: settings.EgressHashKeyId}
	if cfg := stConfig(); cfg != nil {
		result.Profile = cfg.Profile
		result.PolicyHash = fmt.Sprintf("0x%x", cfg.PolicyHash)
	}
	for _, row := range model.ListVerifyProviderStats(clientSession.Ctx, from, to, limit) {
		assignments, confirmations := uint64(0), uint64(0)
		if row.Assignments > 0 {
			assignments = uint64(row.Assignments)
		}
		if row.Confirmations > 0 {
			confirmations = uint64(row.Confirmations)
		}
		result.Rows = append(result.Rows, &PublicVerifyStatsRow{
			PeriodStart: row.PeriodStart.UTC(), PeriodEnd: row.PeriodEnd.UTC(), ClientId: row.ClientId,
			Assignments: assignments, Confirmations: confirmations,
			ReliabilityPPM: protocol.ReliabilityPPM(confirmations, assignments, uint64(settings.ReliabilityAMin)),
			LatencyP50Ms:   row.LatencyP50Ms, LatencyP90Ms: row.LatencyP90Ms, LatencyP99Ms: row.LatencyP99Ms,
		})
	}
	return result, nil
}

// GetVerifyProofs exposes signed, non-poison FINAL/expired records. Each
// completed row is independently verifiable with /verify/keys.
func GetVerifyProofs(args *GetVerifyEvidenceArgs, clientSession *session.ClientSession) (*GetVerifyProofsResult, error) {
	from, to, limit, err := verifyEvidenceRange(args, 1_000, 10_000)
	if err != nil {
		return nil, err
	}
	result := &GetVerifyProofsResult{Schema: "urnetwork-verify-proof-index-v1"}
	if cfg := stConfig(); cfg != nil {
		result.Profile = cfg.Profile
		result.PolicyHash = fmt.Sprintf("0x%x", cfg.PolicyHash)
	}
	for _, row := range model.ListVerifyTrailRows(clientSession.Ctx, from, to, limit) {
		hops := json.RawMessage(row.HopsJson)
		if !json.Valid(hops) {
			return nil, fmt.Errorf("stored verify proof %s has invalid hops JSON", row.TrailId)
		}
		result.Rows = append(result.Rows, &PublicVerifyProofRow{
			TrailId: row.TrailId, Vpk: row.Vpk, ServerKeyId: row.ServerKeyId,
			ServerNonce: row.ServerNonce, Depth: row.Depth, Status: row.Status, Hops: hops,
			FinalSig: row.FinalSig, VerifierSig: row.VerifierSig,
			CreateTime: row.CreateTime.UTC(), CompleteTime: row.CompleteTime,
		})
	}
	return result, nil
}

func GetVerifyKeys(
	clientSession *session.ClientSession,
) (*GetVerifyKeysResult, error) {
	keys := []*VerifyKey{}
	for _, serverKey := range verifyServerKeys() {
		keys = append(keys, &VerifyKey{
			ServerKeyId: serverKey.ServerKeyId,
			PublicKey:   serverKey.PrivateKey.Public().(ed25519.PublicKey),
		})
	}
	return &GetVerifyKeysResult{
		Keys: keys,
	}, nil
}
