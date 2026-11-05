package model

// Unique-exit hosted proxies (HOSTED-PROXY-UNIQUE-EXIT.md). A network that
// creates a hosted proxy with `proxy_config.unique_exit` gets a proxy pinned to
// one provider whose egress is unused by the network's other unique-exit
// proxies. N such proxies are N simultaneous, pairwise distinct exit IPs.
//
// The owner's decisions (2026-11-05), all binding here:
//   - the exit IP is never exposed: the customer sees an opaque exit_id (the
//     assignment id), the exit's country, region and city, exit_since and a
//     state;
//   - Pro only, inside the unchanged hosted proxy caps;
//   - uniqueness is the exact IPv4 address or the IPv6 /64, by a keyed hash;
//     the address itself is never stored;
//   - at most UniqueExitSettings.PinsPerProvider live pins per provider across
//     networks (3);
//   - replacement is automatic after a 120 s grace for a lost provider, and
//     immediate for a bad verdict or a key collision, capped at 6 an hour per
//     proxy and 10% an hour per network; `manual` holds the exit until the
//     customer rotates it;
//   - a slot-like provider (a proxy miner's exits share one connection
//     address) counts only once its key was stable across 2 probes;
//   - a pinned proxy gives at most half its provider's estimated throughput
//     (proxy/proxy_unique_exit.go).
//
// Who writes what:
//   - the api creates assignments (auth-client) and rotates them;
//   - the egress location ingest stores each provider's egress key;
//   - the proxy device confirms its exit through its own tunnel and reports
//     failures;
//   - the taskworker sweeper replaces failed exits and keeps the rest honest.
//
// Every writer of a network's assignments takes the network's advisory lock,
// so the read-then-write checks below are serial per network. The partial
// unique index on (network_id, egress_key) is the backstop: Postgres refuses a
// second live assignment of a key whatever any code path does.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

const (
	UniqueExitReplaceAuto   = "auto"
	UniqueExitReplaceManual = "manual"
)

// The states a customer sees (ProxyExit.State).
const (
	ProxyExitStateAssigned  = "assigned"
	ProxyExitStateReplacing = "replacing"
	ProxyExitStateNoSupply  = "no_supply"
)

// The states of an assignment row. Pending and active hold the network's key;
// replacing is a proxy still pinned to an exit that failed, waiting for a
// replacement (over a cap, without supply, or in manual mode).
const (
	proxyExitAssignmentPending   = "pending"
	proxyExitAssignmentActive    = "active"
	proxyExitAssignmentReplacing = "replacing"
	proxyExitAssignmentReleased  = "released"
)

// Failure and release reasons.
const (
	ProxyExitReasonOffline      = "offline"
	ProxyExitReasonUnreachable  = "unreachable"
	ProxyExitReasonVerdict      = "verdict"
	ProxyExitReasonCollision    = "collision"
	ProxyExitReasonRotate       = "rotate"
	ProxyExitReasonProxyRemoved = "proxy_removed"
	ProxyExitReasonCap          = "cap"
	ProxyExitReasonNoSupply     = "no_supply"
	ProxyExitReasonManual       = "manual"
)

// The failures that count against the replacement caps. A rotate is the
// customer's own and has its own rate limit; a removed proxy is not a failure.
var proxyExitFailureReasons = []string{
	ProxyExitReasonOffline,
	ProxyExitReasonUnreachable,
	ProxyExitReasonVerdict,
	ProxyExitReasonCollision,
}

// ProxyExit is a unique-exit proxy's exit, as the api reports it. There is
// deliberately no address: providers' IPs are not published.
type ProxyExit struct {
	// ExitId is opaque and stable while the proxy keeps one exit.
	ExitId      *server.Id `json:"exit_id,omitempty"`
	CountryCode string     `json:"country_code,omitempty"`
	Region      string     `json:"region,omitempty"`
	City        string     `json:"city,omitempty"`
	ExitSince   *time.Time `json:"exit_since,omitempty"`
	// assigned, replacing or no_supply
	State string `json:"state"`
	// why a proxy is replacing or without supply, e.g. offline, cap, manual
	StateReason string `json:"state_reason,omitempty"`
	ReplaceMode string `json:"replace_mode"`
}

// The tunables, one default in one place (connect/CODESTYLE.md "Tunables live
// in settings").
type UniqueExitSettings struct {
	// live unique-exit pins one provider may carry across all networks
	PinsPerProvider int
	// how long a lost provider may be away before its proxies are moved
	ReplacementGrace time.Duration
	// automatic replacements per proxy per hour
	MaxReplacementsPerProxyPerHour int
	// of a network's unique-exit proxies, the share that may be replaced
	// automatically per hour (at least one)
	MaxNetworkReplacementFractionPerHour float64
	// a proxy is not pinned again to a provider it left within this
	ReleasedProviderExclusion time.Duration
	// a provider's egress key older than this is not trusted for allocation
	EgressKeyMaxAge time.Duration
	// a provider whose connected address carries at least this many connected
	// clients is slot-like, and needs StableProbeCount consecutive probes with
	// one key
	SharedAddressSlotThreshold int
	StableProbeCount           int
	// providers asked of find-providers2 per draw, and draws per allocation
	CandidateCount int
	CandidateDraws int
	// the device re-confirms its exit this often, and polls its assignment
	// this often
	ConfirmInterval        time.Duration
	AssignmentPollInterval time.Duration
	// consecutive failed confirms before the device reports its exit
	// unreachable
	UnreachableConfirmFailures int
	// a pinned proxy's throughput, as a share of its provider's estimate
	ThroughputShare float64
	// rotate-exit limits per proxy
	RotateMinInterval time.Duration
	RotatesPerDay     int
	// released assignments are the audit trail for abuse reports
	ReleasedRetention time.Duration
	// the sweeper's cadence and the assignments it walks per run
	SweepInterval  time.Duration
	SweepBatchSize int
	// how often the sweeper recomputes the supply gauge
	SupplyGaugeInterval time.Duration
}

func DefaultUniqueExitSettings() *UniqueExitSettings {
	return &UniqueExitSettings{
		PinsPerProvider:                      3,
		ReplacementGrace:                     120 * time.Second,
		MaxReplacementsPerProxyPerHour:       6,
		MaxNetworkReplacementFractionPerHour: 0.10,
		ReleasedProviderExclusion:            24 * time.Hour,
		EgressKeyMaxAge:                      24 * time.Hour,
		SharedAddressSlotThreshold:           10,
		StableProbeCount:                     2,
		CandidateCount:                       128,
		CandidateDraws:                       3,
		ConfirmInterval:                      30 * time.Minute,
		AssignmentPollInterval:               15 * time.Second,
		UnreachableConfirmFailures:           3,
		ThroughputShare:                      0.5,
		RotateMinInterval:                    time.Minute,
		RotatesPerDay:                        60,
		ReleasedRetention:                    90 * 24 * time.Hour,
		SweepInterval:                        30 * time.Second,
		SweepBatchSize:                       10000,
		SupplyGaugeInterval:                  10 * time.Minute,
	}
}

var uniqueExitSettingsOverride atomic.Pointer[UniqueExitSettings]

func GetUniqueExitSettings() *UniqueExitSettings {
	if settings := uniqueExitSettingsOverride.Load(); settings != nil {
		return settings
	}
	return DefaultUniqueExitSettings()
}

func Testing_SetUniqueExitSettings(settings *UniqueExitSettings) func() {
	previous := uniqueExitSettingsOverride.Swap(settings)
	return func() {
		uniqueExitSettingsOverride.Store(previous)
	}
}

var (
	proxyUniqueExitAllocations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_proxy_unique_exit_allocations_total",
		Help: "Unique-exit allocations by result: assigned, no_supply, or collision_retry for a candidate another allocation took first",
	}, []string{"result"})
	proxyUniqueExitReplacements = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_proxy_unique_exit_replacements_total",
		Help: "Unique-exit proxies moved to a new exit, by reason",
	}, []string{"reason"})
	proxyUniqueExitConfirms = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "urnetwork_proxy_unique_exit_confirm_total",
		Help: "Exit confirms through a unique-exit proxy's tunnel, by result: match, changed_unique, changed_collision, stale",
	}, []string{"result"})
	proxyUniqueExitAssignments = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "urnetwork_proxy_unique_exit_assignments",
		Help: "Unique-exit assignments that are not released, by state, as the sweeper last saw them",
	}, []string{"state"})
	proxyUniqueExitSupplyKeys = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "urnetwork_proxy_unique_exit_supply_keys",
		Help: "Distinct fresh provider egress keys by country: the most unique exits one network could hold there",
	}, []string{"country"})
	proxyUniqueExitAssignmentLifetime = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "urnetwork_proxy_unique_exit_assignment_lifetime_seconds",
		Help:    "How long a unique-exit assignment lived, observed when it is released",
		Buckets: prometheus.ExponentialBuckets(60, 4, 8),
	})
)

func init() {
	prometheus.MustRegister(
		proxyUniqueExitAllocations,
		proxyUniqueExitReplacements,
		proxyUniqueExitConfirms,
		proxyUniqueExitAssignments,
		proxyUniqueExitSupplyKeys,
		proxyUniqueExitAssignmentLifetime,
	)
}

// The egress key

const (
	uniqueExitKeyDomainV4 = "urnetwork/unique-exit-ipv4/v1"
	uniqueExitKeyDomainV6 = "urnetwork/unique-exit-ipv6-64/v1"
)

// The key is keyed with verify.yml's egress hash key, the secret the verify
// index already keys exit addresses with, under its own domains so a unique-exit
// key can never equal a verify index entry. A deployment without verify.yml
// falls back to the server's address pepper.
var uniqueExitKeySecretFromVault = sync.OnceValue(func() []byte {
	res, err := server.Vault.SimpleResource("verify.yml")
	if err != nil {
		return nil
	}
	var conf struct {
		EgressHashKey string `yaml:"egress_hash_key"`
	}
	res.UnmarshalYaml(&conf)
	key, err := base64.StdEncoding.DecodeString(conf.EgressHashKey)
	if err != nil || len(key) != 32 {
		return nil
	}
	return key
})

var uniqueExitKeySecretOverride atomic.Pointer[[]byte]

func uniqueExitKeySecret() []byte {
	if secret := uniqueExitKeySecretOverride.Load(); secret != nil {
		return *secret
	}
	return uniqueExitKeySecretFromVault()
}

func Testing_SetUniqueExitKeySecret(secret []byte) func() {
	previous := uniqueExitKeySecretOverride.Swap(&secret)
	return func() {
		uniqueExitKeySecretOverride.Store(previous)
	}
}

// UniqueExitEgressKey is what makes two exits the same exit: the exact IPv4
// address, or the IPv6 /64, since a residential IPv6 host rotates addresses
// inside its /64. Providers behind one address (a farm) share a key, so the
// allocator gives a network at most one of them.
func UniqueExitEgressKey(ip netip.Addr) []byte {
	ip = ip.Unmap()
	domain := uniqueExitKeyDomainV4
	subject := ip
	if ip.Is6() {
		domain = uniqueExitKeyDomainV6
		subject = netip.PrefixFrom(ip, 64).Masked().Addr()
	}
	secret := uniqueExitKeySecret()
	if len(secret) == 0 {
		peppered := server.ClientIpHashForAddrPrefix(subject, 32, 64)
		h := sha256.New()
		h.Write([]byte(domain))
		h.Write(peppered[:])
		return h.Sum(nil)
	}
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(domain))
	h.Write(subject.AsSlice())
	return h.Sum(nil)
}

// SetProviderEgressKey stores the keyed egress of a probe the location ingest
// just placed. The address is hashed here and dropped. A probe that sees the
// same key again counts it stable; a new key restarts the count. Older probes
// never overwrite newer ones.
func SetProviderEgressKey(ctx context.Context, clientId server.Id, exitIp netip.Addr, observedAt time.Time) {
	key := UniqueExitEgressKey(exitIp)
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
				UPDATE provider_egress_location
				SET
					egress_key_stable_count = CASE
						WHEN egress_key = $2 THEN egress_key_stable_count + 1
						ELSE 1
					END,
					egress_key = $2,
					egress_key_observed_at = $3
				WHERE
					client_id = $1 AND
					(egress_key_observed_at IS NULL OR egress_key_observed_at < $3)
			`,
			clientId,
			key,
			observedAt.UTC(),
		))
	})
}

// Candidates

// A UniqueExitCandidateSource draws providers in rank order. Production uses
// find-providers2's quality bucket: providers whose latest egress health run
// passed, with blackhole and TLS-authentication verdicts excluded.
type UniqueExitCandidateSource func(
	clientSession *session.ClientSession,
	specs []*ProviderSpec,
	excludeClientIds []server.Id,
	count int,
) ([]*FindProvidersProvider, error)

func findProvidersUniqueExitCandidates(
	clientSession *session.ClientSession,
	specs []*ProviderSpec,
	excludeClientIds []server.Id,
	count int,
) ([]*FindProvidersProvider, error) {
	result, err := FindProviders2(
		&FindProviders2Args{
			Specs:            specs,
			Count:            count,
			ExcludeClientIds: excludeClientIds,
			RankMode:         RankModeQuality,
		},
		clientSession,
	)
	if err != nil {
		return nil, err
	}
	return result.Providers, nil
}

var uniqueExitCandidateSourceLock sync.Mutex
var uniqueExitCandidateSource UniqueExitCandidateSource = findProvidersUniqueExitCandidates

func getUniqueExitCandidateSource() UniqueExitCandidateSource {
	uniqueExitCandidateSourceLock.Lock()
	defer uniqueExitCandidateSourceLock.Unlock()
	return uniqueExitCandidateSource
}

func Testing_SetUniqueExitCandidateSource(source UniqueExitCandidateSource) func() {
	uniqueExitCandidateSourceLock.Lock()
	defer uniqueExitCandidateSourceLock.Unlock()
	previous := uniqueExitCandidateSource
	uniqueExitCandidateSource = source
	return func() {
		uniqueExitCandidateSourceLock.Lock()
		defer uniqueExitCandidateSourceLock.Unlock()
		uniqueExitCandidateSource = previous
	}
}

var errUniqueExitNamedProvider = errors.New("A unique exit picks its own provider: use a country, region, city, group or best available location.")

// UniqueExitProviderSpecs turns a proxy's location into the provider specs its
// exits are drawn from. A location naming one provider is refused: the
// allocator, not the caller, picks a unique exit's provider.
func UniqueExitProviderSpecs(location *sdk.ConnectLocation) ([]*ProviderSpec, error) {
	if location == nil || location.ConnectLocationId == nil {
		return nil, errors.New("Invalid location")
	}
	locationId := location.ConnectLocationId
	switch {
	case locationId.ClientId != nil:
		return nil, errUniqueExitNamedProvider
	case locationId.LocationGroupId != nil:
		groupId := server.RequireIdFromBytes(locationId.LocationGroupId.Bytes())
		return []*ProviderSpec{{LocationGroupId: &groupId}}, nil
	case locationId.LocationId != nil:
		id := server.RequireIdFromBytes(locationId.LocationId.Bytes())
		return []*ProviderSpec{{LocationId: &id}}, nil
	case locationId.BestAvailable:
		return []*ProviderSpec{{BestAvailable: true}}, nil
	}
	return nil, errors.New("Invalid location")
}

type uniqueExitCandidate struct {
	ProviderClientId        server.Id
	EgressKey               []byte
	LocationId              *server.Id
	CountryCode             string
	EstimatedBytesPerSecond int64
	Stable                  bool
}

// drawUniqueExitCandidates returns up to want providers, in order of
// preference, whose egress key the network does not hold, each with a distinct
// key. Stable keys come first, then find-providers2's rank.
func drawUniqueExitCandidates(
	ctx context.Context,
	clientSession *session.ClientSession,
	networkId server.Id,
	specs []*ProviderSpec,
	excludeProviderIds []server.Id,
	settings *UniqueExitSettings,
	now time.Time,
	want int,
) ([]*uniqueExitCandidate, error) {
	excluded := map[server.Id]bool{}
	excludeList := []server.Id{}
	exclude := func(clientId server.Id) {
		if !excluded[clientId] {
			excluded[clientId] = true
			excludeList = append(excludeList, clientId)
		}
	}
	for _, clientId := range excludeProviderIds {
		exclude(clientId)
	}
	// the network's own exits are excluded from the draw, so a draw is spent on
	// providers the network could actually take
	for _, clientId := range networkLiveExitProviderIds(ctx, networkId) {
		exclude(clientId)
	}

	source := getUniqueExitCandidateSource()
	candidates := []*uniqueExitCandidate{}
	seenKeys := map[string]bool{}
	for draw := 0; draw < settings.CandidateDraws && len(candidates) < want; draw += 1 {
		providers, err := source(clientSession, specs, excludeList, settings.CandidateCount)
		if err != nil {
			return candidates, err
		}
		if len(providers) == 0 {
			break
		}
		fresh := []*FindProvidersProvider{}
		for _, provider := range providers {
			if excluded[provider.ClientId] {
				continue
			}
			exclude(provider.ClientId)
			fresh = append(fresh, provider)
		}
		if len(fresh) == 0 {
			break
		}
		for _, candidate := range filterUniqueExitCandidates(ctx, networkId, fresh, settings, now) {
			keyString := string(candidate.EgressKey)
			if seenKeys[keyString] {
				continue
			}
			seenKeys[keyString] = true
			candidates = append(candidates, candidate)
		}
	}
	sort.SliceStable(candidates, func(i int, j int) bool {
		return candidates[i].Stable && !candidates[j].Stable
	})
	if want < len(candidates) {
		candidates = candidates[:want]
	}
	return candidates, nil
}

func networkLiveExitProviderIds(ctx context.Context, networkId server.Id) []server.Id {
	clientIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT provider_client_id
				FROM proxy_exit_assignment
				WHERE
					network_id = $1 AND
					state IN ('pending', 'active', 'replacing')
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return clientIds
}

type providerEgressKeyRow struct {
	key         []byte
	stableCount int
	locationId  *server.Id
	countryCode string
}

// filterUniqueExitCandidates keeps, in the given order, the providers with a
// fresh key the network does not hold, under the pin cap, and either stable or
// not slot-like.
func filterUniqueExitCandidates(
	ctx context.Context,
	networkId server.Id,
	providers []*FindProvidersProvider,
	settings *UniqueExitSettings,
	now time.Time,
) []*uniqueExitCandidate {
	clientIds := make([]server.Id, 0, len(providers))
	for _, provider := range providers {
		clientIds = append(clientIds, provider.ClientId)
	}

	keyRows := map[server.Id]*providerEgressKeyRow{}
	held := map[string]bool{}
	pins := map[server.Id]int{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					client_id,
					egress_key,
					egress_key_stable_count,
					location_id,
					country_code
				FROM provider_egress_location
				WHERE
					client_id = ANY($1) AND
					egress_key IS NOT NULL AND
					$2 <= egress_key_observed_at
			`,
			clientIds,
			now.UTC().Add(-settings.EgressKeyMaxAge),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				row := &providerEgressKeyRow{}
				var locationId server.Id
				server.Raise(result.Scan(&clientId, &row.key, &row.stableCount, &locationId, &row.countryCode))
				row.locationId = &locationId
				keyRows[clientId] = row
			}
		})

		keys := [][]byte{}
		for _, row := range keyRows {
			keys = append(keys, row.key)
		}
		if 0 < len(keys) {
			result, err = conn.Query(
				ctx,
				`
					SELECT egress_key
					FROM proxy_exit_assignment
					WHERE
						network_id = $1 AND
						state IN ('pending', 'active') AND
						egress_key = ANY($2)
				`,
				networkId,
				keys,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var key []byte
					server.Raise(result.Scan(&key))
					held[string(key)] = true
				}
			})
		}

		result, err = conn.Query(
			ctx,
			`
				SELECT provider_client_id, count(*)
				FROM proxy_exit_assignment
				WHERE
					provider_client_id = ANY($1) AND
					state IN ('pending', 'active')
				GROUP BY provider_client_id
			`,
			clientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var count int
				server.Raise(result.Scan(&clientId, &count))
				pins[clientId] = count
			}
		})
	})

	unstable := []server.Id{}
	for clientId, row := range keyRows {
		if row.stableCount < settings.StableProbeCount {
			unstable = append(unstable, clientId)
		}
	}
	slotLike := uniqueExitSlotLikeProviders(ctx, unstable, settings)

	candidates := []*uniqueExitCandidate{}
	for _, provider := range providers {
		row, ok := keyRows[provider.ClientId]
		if !ok {
			continue
		}
		if held[string(row.key)] {
			continue
		}
		if settings.PinsPerProvider <= pins[provider.ClientId] {
			continue
		}
		stable := settings.StableProbeCount <= row.stableCount
		if !stable && slotLike[provider.ClientId] {
			continue
		}
		var estimate int64
		if provider.HasEstimatedBytesPerSecond {
			estimate = int64(provider.EstimatedBytesPerSecond)
		}
		candidates = append(candidates, &uniqueExitCandidate{
			ProviderClientId:        provider.ClientId,
			EgressKey:               row.key,
			LocationId:              row.locationId,
			CountryCode:             row.countryCode,
			EstimatedBytesPerSecond: estimate,
			Stable:                  stable,
		})
	}
	return candidates
}

// uniqueExitSlotLikeProviders is the set of providers whose connected address
// carries at least SharedAddressSlotThreshold connected clients. The proxy
// miner gives each SOCKS5 exit its own provider slot on one address, and such
// an exit may egress through a rotating upstream. Each address is counted to
// the threshold and no further, so a host with tens of thousands of slots
// costs no more than one with ten.
func uniqueExitSlotLikeProviders(ctx context.Context, clientIds []server.Id, settings *UniqueExitSettings) map[server.Id]bool {
	slotLike := map[server.Id]bool{}
	if len(clientIds) == 0 || settings.SharedAddressSlotThreshold <= 0 {
		return slotLike
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					c.client_id,
					(
						SELECT count(*)
						FROM (
							SELECT 1
							FROM network_client_connection s
							WHERE
								s.client_address_hash = c.client_address_hash AND
								s.connected = true
							LIMIT $2
						) bounded
					)
				FROM network_client_connection c
				WHERE
					c.client_id = ANY($1) AND
					c.connected = true
			`,
			clientIds,
			settings.SharedAddressSlotThreshold,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				var count int
				server.Raise(result.Scan(&clientId, &count))
				if settings.SharedAddressSlotThreshold <= count {
					slotLike[clientId] = true
				}
			}
		})
	})
	return slotLike
}

// Assignments

type proxyExitAssignment struct {
	AssignmentId            server.Id
	NetworkId               server.Id
	ProxyClientId           server.Id
	ProviderClientId        server.Id
	EgressKey               []byte
	State                   string
	LocationId              *server.Id
	CountryCode             string
	EstimatedBytesPerSecond int64
	AssignedAt              time.Time
	ConfirmedAt             *time.Time
	FailingSince            *time.Time
	FailingReason           string
}

const proxyExitAssignmentColumns = `
	assignment_id,
	network_id,
	proxy_client_id,
	provider_client_id,
	egress_key,
	state,
	location_id,
	country_code,
	estimated_bytes_per_second,
	assigned_at,
	confirmed_at,
	failing_since,
	failing_reason
`

func scanProxyExitAssignment(scan func(dest ...any) error) *proxyExitAssignment {
	a := &proxyExitAssignment{}
	server.Raise(scan(
		&a.AssignmentId,
		&a.NetworkId,
		&a.ProxyClientId,
		&a.ProviderClientId,
		&a.EgressKey,
		&a.State,
		&a.LocationId,
		&a.CountryCode,
		&a.EstimatedBytesPerSecond,
		&a.AssignedAt,
		&a.ConfirmedAt,
		&a.FailingSince,
		&a.FailingReason,
	))
	return a
}

func lockUniqueExitNetworkInTx(ctx context.Context, tx server.PgTx, networkId server.Id) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		"proxy-unique-exit-network:"+networkId.String(),
	))
}

// A provider's pin count is decided under its lock. The lock is tried, never
// waited for: two allocations for different networks may want the same
// providers in different orders, and waiting could deadlock them. A provider
// another allocation holds is simply skipped.
func tryLockUniqueExitProviderInTx(ctx context.Context, tx server.PgTx, providerClientId server.Id) (locked bool) {
	server.Raise(tx.QueryRow(
		ctx,
		`SELECT pg_try_advisory_xact_lock(hashtextextended($1, 0))`,
		"proxy-unique-exit-provider:"+providerClientId.String(),
	).Scan(&locked))
	return
}

// insertProxyExitAssignmentInTx pins proxyClientId to the first candidate it
// can take. A candidate is lost to the pin cap, or to a concurrent allocation
// of its key, which the unique index refuses (ON CONFLICT DO NOTHING).
func insertProxyExitAssignmentInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	proxyClientId server.Id,
	candidates []*uniqueExitCandidate,
	settings *UniqueExitSettings,
	now time.Time,
) *proxyExitAssignment {
	lockUniqueExitNetworkInTx(ctx, tx, networkId)
	for _, candidate := range candidates {
		if !tryLockUniqueExitProviderInTx(ctx, tx, candidate.ProviderClientId) {
			proxyUniqueExitAllocations.WithLabelValues("collision_retry").Inc()
			continue
		}
		var pins int
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT count(*)
				FROM proxy_exit_assignment
				WHERE
					provider_client_id = $1 AND
					state IN ('pending', 'active')
			`,
			candidate.ProviderClientId,
		).Scan(&pins))
		if settings.PinsPerProvider <= pins {
			continue
		}
		assignment := &proxyExitAssignment{
			AssignmentId:            server.NewId(),
			NetworkId:               networkId,
			ProxyClientId:           proxyClientId,
			ProviderClientId:        candidate.ProviderClientId,
			EgressKey:               candidate.EgressKey,
			State:                   proxyExitAssignmentPending,
			LocationId:              candidate.LocationId,
			CountryCode:             candidate.CountryCode,
			EstimatedBytesPerSecond: candidate.EstimatedBytesPerSecond,
			AssignedAt:              now.UTC(),
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO proxy_exit_assignment (
					assignment_id,
					network_id,
					proxy_client_id,
					provider_client_id,
					egress_key,
					state,
					location_id,
					country_code,
					estimated_bytes_per_second,
					assigned_at
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
				ON CONFLICT DO NOTHING
			`,
			assignment.AssignmentId,
			assignment.NetworkId,
			assignment.ProxyClientId,
			assignment.ProviderClientId,
			assignment.EgressKey,
			assignment.State,
			assignment.LocationId,
			assignment.CountryCode,
			assignment.EstimatedBytesPerSecond,
			assignment.AssignedAt,
		))
		if tag.RowsAffected() == 1 {
			proxyUniqueExitAllocations.WithLabelValues("assigned").Inc()
			return assignment
		}
		proxyUniqueExitAllocations.WithLabelValues("collision_retry").Inc()
	}
	return nil
}

// UniqueExitPinnedLocation is the device location of a pinned proxy: the one
// provider, which the sdk makes a fixed window of one client
// (sdk/device_local.go, FixedDestinationSize).
func UniqueExitPinnedLocation(providerClientId server.Id, countryCode string) *sdk.ConnectLocation {
	return &sdk.ConnectLocation{
		ConnectLocationId: &sdk.ConnectLocationId{
			ClientId: sdk.RequireIdFromBytes(providerClientId.Bytes()),
		},
		CountryCode: countryCode,
	}
}

type proxyUniqueExitConfig struct {
	ProxyClientId server.Id
	NetworkId     server.Id
	ReplaceMode   string
	Specs         []*ProviderSpec
	State         string
	StateReason   string
}

func insertProxyUniqueExitInTx(
	ctx context.Context,
	tx server.PgTx,
	proxyClientId server.Id,
	networkId server.Id,
	replaceMode string,
	specs []*ProviderSpec,
	now time.Time,
) {
	specsJson, err := json.Marshal(specs)
	server.Raise(err)
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO proxy_unique_exit (
				proxy_client_id,
				network_id,
				replace_mode,
				specs_json,
				state,
				create_time,
				update_time
			)
			VALUES ($1, $2, $3, $4, $5, $6, $6)
			ON CONFLICT (proxy_client_id) DO UPDATE SET
				replace_mode = EXCLUDED.replace_mode,
				specs_json = EXCLUDED.specs_json,
				state = EXCLUDED.state,
				state_reason = '',
				update_time = EXCLUDED.update_time
		`,
		proxyClientId,
		networkId,
		replaceMode,
		string(specsJson),
		ProxyExitStateAssigned,
		now.UTC(),
	))
}

func loadProxyUniqueExitConfigs(ctx context.Context, proxyClientIds []server.Id) map[server.Id]*proxyUniqueExitConfig {
	configs := map[server.Id]*proxyUniqueExitConfig{}
	if len(proxyClientIds) == 0 {
		return configs
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT
					proxy_client_id,
					network_id,
					replace_mode,
					specs_json,
					state,
					state_reason
				FROM proxy_unique_exit
				WHERE proxy_client_id = ANY($1)
			`,
			proxyClientIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				config := &proxyUniqueExitConfig{}
				var specsJson string
				server.Raise(result.Scan(
					&config.ProxyClientId,
					&config.NetworkId,
					&config.ReplaceMode,
					&specsJson,
					&config.State,
					&config.StateReason,
				))
				if err := json.Unmarshal([]byte(specsJson), &config.Specs); err != nil {
					glog.Errorf("[upe]unreadable specs for proxy %s: %s\n", config.ProxyClientId, err)
					config.Specs = []*ProviderSpec{{BestAvailable: true}}
				}
				configs[config.ProxyClientId] = config
			}
		})
	})
	return configs
}

func setProxyUniqueExitStateInTx(ctx context.Context, tx server.PgTx, proxyClientId server.Id, state string, reason string, now time.Time) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE proxy_unique_exit
			SET state = $2, state_reason = $3, update_time = $4
			WHERE proxy_client_id = $1
		`,
		proxyClientId,
		state,
		reason,
		now.UTC(),
	))
}

func loadLiveProxyExitAssignmentInTx(ctx context.Context, tx server.PgTx, proxyClientId server.Id) *proxyExitAssignment {
	var assignment *proxyExitAssignment
	result, err := tx.Query(
		ctx,
		`
			SELECT `+proxyExitAssignmentColumns+`
			FROM proxy_exit_assignment
			WHERE
				proxy_client_id = $1 AND
				state IN ('pending', 'active', 'replacing')
			FOR UPDATE
		`,
		proxyClientId,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			assignment = scanProxyExitAssignment(result.Scan)
		}
	})
	return assignment
}

func loadLiveProxyExitAssignment(ctx context.Context, proxyClientId server.Id) *proxyExitAssignment {
	var assignment *proxyExitAssignment
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+proxyExitAssignmentColumns+`
				FROM proxy_exit_assignment
				WHERE
					proxy_client_id = $1 AND
					state IN ('pending', 'active', 'replacing')
			`,
			proxyClientId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				assignment = scanProxyExitAssignment(result.Scan)
			}
		})
	})
	return assignment
}

func releaseProxyExitAssignmentInTx(ctx context.Context, tx server.PgTx, assignment *proxyExitAssignment, reason string, now time.Time) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE proxy_exit_assignment
			SET
				state = 'released',
				released_at = $2,
				release_reason = $3
			WHERE
				assignment_id = $1 AND
				state <> 'released'
		`,
		assignment.AssignmentId,
		now.UTC(),
		reason,
	))
	if tag.RowsAffected() == 1 {
		proxyUniqueExitAssignmentLifetime.Observe(max(0, now.Sub(assignment.AssignedAt).Seconds()))
	}
}

// setProxyDeviceConfigLocationInTx repins a proxy's stored device config, so a
// device created from it starts on the new exit. The redis mirror is written
// after the commit with the json returned here.
func setProxyDeviceConfigLocationInTx(
	ctx context.Context,
	tx server.PgTx,
	proxyClientId server.Id,
	location *sdk.ConnectLocation,
) (proxyIds []server.Id, configJsons [][]byte) {
	type row struct {
		proxyId    server.Id
		configJson string
	}
	rows := []row{}
	result, err := tx.Query(
		ctx,
		`
			SELECT proxy_id, config_json
			FROM proxy_device_config
			WHERE client_id = $1
			FOR UPDATE
		`,
		proxyClientId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var r row
			server.Raise(result.Scan(&r.proxyId, &r.configJson))
			rows = append(rows, r)
		}
	})
	for _, r := range rows {
		config := &ProxyDeviceConfig{}
		if err := json.Unmarshal([]byte(r.configJson), config); err != nil {
			glog.Errorf("[upe]unreadable device config for proxy %s: %s\n", r.proxyId, err)
			continue
		}
		if config.InitialDeviceState == nil {
			config.InitialDeviceState = &ProxyDeviceState{}
		}
		config.InitialDeviceState.Location = location
		configJson, err := json.Marshal(config)
		server.Raise(err)
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE proxy_device_config SET config_json = $2 WHERE proxy_id = $1`,
			r.proxyId,
			string(configJson),
		))
		proxyIds = append(proxyIds, r.proxyId)
		configJsons = append(configJsons, configJson)
	}
	return
}

// The device's view

// ProxyExitForDevice is what a unique-exit proxy device needs to stay pinned:
// the provider, and its estimate for the throughput share.
type ProxyExitForDevice struct {
	AssignmentId            server.Id
	ProviderClientId        server.Id
	CountryCode             string
	State                   string
	EstimatedBytesPerSecond int64
}

func GetProxyExitForDevice(ctx context.Context, proxyClientId server.Id) *ProxyExitForDevice {
	assignment := loadLiveProxyExitAssignment(ctx, proxyClientId)
	if assignment == nil {
		return nil
	}
	return &ProxyExitForDevice{
		AssignmentId:            assignment.AssignmentId,
		ProviderClientId:        assignment.ProviderClientId,
		CountryCode:             assignment.CountryCode,
		State:                   assignment.State,
		EstimatedBytesPerSecond: assignment.EstimatedBytesPerSecond,
	}
}

const (
	ProxyExitConfirmMatch            = "match"
	ProxyExitConfirmChangedUnique    = "changed_unique"
	ProxyExitConfirmChangedCollision = "changed_collision"
	ProxyExitConfirmStale            = "stale"
)

// ConfirmProxyExit records what the device saw through its own tunnel. The
// same key activates the assignment. A new key the network does not hold
// replaces the stored one (a residential IP moved). A new key another of the
// network's exits holds is a collision, which the sweeper replaces without a
// grace. An assignment that is no longer the proxy's is stale and changes
// nothing.
func ConfirmProxyExit(
	ctx context.Context,
	proxyClientId server.Id,
	assignmentId server.Id,
	observedKey []byte,
	now time.Time,
) (outcome string) {
	outcome = ProxyExitConfirmStale
	server.Tx(ctx, func(tx server.PgTx) {
		assignment := loadLiveProxyExitAssignmentInTx(ctx, tx, proxyClientId)
		if assignment == nil || assignment.AssignmentId != assignmentId || assignment.State == proxyExitAssignmentReplacing {
			outcome = ProxyExitConfirmStale
			return
		}
		lockUniqueExitNetworkInTx(ctx, tx, assignment.NetworkId)
		if bytes.Equal(assignment.EgressKey, observedKey) {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE proxy_exit_assignment
					SET
						state = 'active',
						confirmed_at = $2,
						failing_since = CASE WHEN failing_reason IN ('unreachable', 'offline') THEN NULL ELSE failing_since END,
						failing_reason = CASE WHEN failing_reason IN ('unreachable', 'offline') THEN '' ELSE failing_reason END
					WHERE assignment_id = $1
				`,
				assignmentId,
				now.UTC(),
			))
			outcome = ProxyExitConfirmMatch
			return
		}
		var taken bool
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM proxy_exit_assignment
					WHERE
						network_id = $1 AND
						egress_key = $2 AND
						state IN ('pending', 'active') AND
						assignment_id <> $3
				)
			`,
			assignment.NetworkId,
			observedKey,
			assignmentId,
		).Scan(&taken))
		if taken {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE proxy_exit_assignment
					SET
						failing_since = COALESCE(failing_since, $2),
						failing_reason = 'collision'
					WHERE assignment_id = $1
				`,
				assignmentId,
				now.UTC(),
			))
			outcome = ProxyExitConfirmChangedCollision
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE proxy_exit_assignment
				SET
					egress_key = $2,
					state = 'active',
					confirmed_at = $3,
					failing_since = NULL,
					failing_reason = ''
				WHERE assignment_id = $1
			`,
			assignmentId,
			observedKey,
			now.UTC(),
		))
		outcome = ProxyExitConfirmChangedUnique
	})
	proxyUniqueExitConfirms.WithLabelValues(outcome).Inc()
	return
}

// ReportProxyExitFailing marks the proxy's exit failing since now, unless it
// already is. The sweeper replaces it once the grace has passed.
func ReportProxyExitFailing(ctx context.Context, proxyClientId server.Id, assignmentId server.Id, reason string, now time.Time) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
				UPDATE proxy_exit_assignment
				SET
					failing_since = COALESCE(failing_since, $3),
					failing_reason = CASE WHEN failing_reason = '' THEN $4 ELSE failing_reason END
				WHERE
					assignment_id = $1 AND
					proxy_client_id = $2 AND
					state IN ('pending', 'active')
			`,
			assignmentId,
			proxyClientId,
			now.UTC(),
			reason,
		))
	})
}

// The replacement

func proxyExitFailuresSince(ctx context.Context, column string, id server.Id, since time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			fmt.Sprintf(
				`
					SELECT count(*)
					FROM proxy_exit_assignment
					WHERE
						%s = $1 AND
						state = 'released' AND
						$2 <= released_at AND
						release_reason = ANY($3)
				`,
				column,
			),
			id,
			since.UTC(),
			proxyExitFailureReasons,
		).Scan(&count))
	})
	return
}

func networkUniqueExitProxyCount(ctx context.Context, networkId server.Id) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`SELECT count(*) FROM proxy_unique_exit WHERE network_id = $1`,
			networkId,
		).Scan(&count))
	})
	return
}

// networkReplacementBudget is how many automatic replacements a network may
// spend per hour: the share of its unique-exit proxies, at least one, so a
// regional outage does not churn the whole set at once.
func networkReplacementBudget(proxyCount int, settings *UniqueExitSettings) int {
	return max(1, int(math.Ceil(settings.MaxNetworkReplacementFractionPerHour*float64(proxyCount))))
}

func recentlyReleasedProviderIds(ctx context.Context, proxyClientId server.Id, since time.Time) []server.Id {
	clientIds := []server.Id{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT provider_client_id
				FROM proxy_exit_assignment
				WHERE
					proxy_client_id = $1 AND
					state = 'released' AND
					$2 <= released_at
			`,
			proxyClientId,
			since.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var clientId server.Id
				server.Raise(result.Scan(&clientId))
				clientIds = append(clientIds, clientId)
			}
		})
	})
	return clientIds
}

// holdProxyExit keeps the proxy pinned to its failing exit, in the replacing
// state, with the reason the customer sees. The exit no longer holds the
// network's key.
func holdProxyExit(ctx context.Context, assignment *proxyExitAssignment, failingReason string, state string, stateReason string, now time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE proxy_exit_assignment
				SET
					state = 'replacing',
					failing_since = COALESCE(failing_since, $2),
					failing_reason = CASE WHEN $3 = '' THEN failing_reason ELSE $3 END
				WHERE
					assignment_id = $1 AND
					state IN ('pending', 'active', 'replacing')
			`,
			assignment.AssignmentId,
			now.UTC(),
			failingReason,
		))
		setProxyUniqueExitStateInTx(ctx, tx, assignment.ProxyClientId, state, stateReason, now)
	})
}

// replaceProxyExit moves a proxy to a new unique exit and returns it, or nil
// when the proxy stays where it is: manual mode, over a cap, or no supply. A
// rotate is the customer's own request: it skips the caps and manual mode, and
// without supply it leaves the working exit untouched.
func replaceProxyExit(
	ctx context.Context,
	clientSession *session.ClientSession,
	config *proxyUniqueExitConfig,
	current *proxyExitAssignment,
	reason string,
	rotate bool,
	settings *UniqueExitSettings,
	now time.Time,
) *proxyExitAssignment {
	if !rotate {
		if config.ReplaceMode == UniqueExitReplaceManual {
			holdProxyExit(ctx, current, reason, ProxyExitStateReplacing, ProxyExitReasonManual, now)
			return nil
		}
		hourAgo := now.Add(-time.Hour)
		if settings.MaxReplacementsPerProxyPerHour <= proxyExitFailuresSince(ctx, "proxy_client_id", current.ProxyClientId, hourAgo) {
			holdProxyExit(ctx, current, reason, ProxyExitStateReplacing, ProxyExitReasonCap, now)
			return nil
		}
		budget := networkReplacementBudget(networkUniqueExitProxyCount(ctx, current.NetworkId), settings)
		if budget <= proxyExitFailuresSince(ctx, "network_id", current.NetworkId, hourAgo) {
			holdProxyExit(ctx, current, reason, ProxyExitStateReplacing, ProxyExitReasonCap, now)
			return nil
		}
	}

	exclude := append(
		[]server.Id{current.ProviderClientId},
		recentlyReleasedProviderIds(ctx, current.ProxyClientId, now.Add(-settings.ReleasedProviderExclusion))...,
	)
	candidates, err := drawUniqueExitCandidates(ctx, clientSession, current.NetworkId, config.Specs, exclude, settings, now, 8)
	if err != nil {
		glog.Infof("[upe]draw for proxy %s: %s\n", current.ProxyClientId, err)
	}
	if len(candidates) == 0 {
		proxyUniqueExitAllocations.WithLabelValues("no_supply").Inc()
		if !rotate {
			holdProxyExit(ctx, current, reason, ProxyExitStateNoSupply, ProxyExitReasonNoSupply, now)
		}
		return nil
	}

	var next *proxyExitAssignment
	var proxyIds []server.Id
	var configJsons [][]byte
	server.Tx(ctx, func(tx server.PgTx) {
		next = nil
		proxyIds, configJsons = nil, nil
		lockUniqueExitNetworkInTx(ctx, tx, current.NetworkId)
		live := loadLiveProxyExitAssignmentInTx(ctx, tx, current.ProxyClientId)
		if live == nil || live.AssignmentId != current.AssignmentId {
			// another writer moved the proxy first
			return
		}
		releaseProxyExitAssignmentInTx(ctx, tx, live, reason, now)
		next = insertProxyExitAssignmentInTx(ctx, tx, current.NetworkId, current.ProxyClientId, candidates, settings, now)
		if next == nil {
			// every candidate was taken meanwhile: the proxy keeps its exit, as
			// it was, and holds the network lock, so its key is still free
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					UPDATE proxy_exit_assignment
					SET state = $2, released_at = NULL, release_reason = ''
					WHERE assignment_id = $1
				`,
				live.AssignmentId,
				live.State,
			))
			return
		}
		proxyIds, configJsons = setProxyDeviceConfigLocationInTx(
			ctx,
			tx,
			current.ProxyClientId,
			UniqueExitPinnedLocation(next.ProviderClientId, next.CountryCode),
		)
		setProxyUniqueExitStateInTx(ctx, tx, current.ProxyClientId, ProxyExitStateAssigned, "", now)
	})
	for i, proxyId := range proxyIds {
		server.HandleError(func() {
			setProxyDeviceConfigMirror(ctx, proxyId, configJsons[i])
		})
	}
	if next != nil {
		proxyUniqueExitReplacements.WithLabelValues(reason).Inc()
	}
	return next
}

// The api's view

// GetProxyExits reports the exit of each unique-exit proxy among
// proxyClientIds. A proxy without unique exit is absent.
func GetProxyExits(ctx context.Context, proxyClientIds []server.Id) map[server.Id]*ProxyExit {
	exits := map[server.Id]*ProxyExit{}
	configs := loadProxyUniqueExitConfigs(ctx, proxyClientIds)
	if len(configs) == 0 {
		return exits
	}
	configIds := make([]server.Id, 0, len(configs))
	for proxyClientId := range configs {
		configIds = append(configIds, proxyClientId)
	}
	assignments := map[server.Id]*proxyExitAssignment{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+proxyExitAssignmentColumns+`
				FROM proxy_exit_assignment
				WHERE
					proxy_client_id = ANY($1) AND
					state IN ('pending', 'active', 'replacing')
			`,
			configIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				assignment := scanProxyExitAssignment(result.Scan)
				assignments[assignment.ProxyClientId] = assignment
			}
		})
	})
	locationIds := []server.Id{}
	for _, assignment := range assignments {
		if assignment.LocationId != nil {
			locationIds = append(locationIds, *assignment.LocationId)
		}
	}
	locations := map[server.Id]*Location{}
	if 0 < len(locationIds) {
		locations = GetLocations(ctx, locationIds)
	}
	for proxyClientId, config := range configs {
		exit := &ProxyExit{
			State:       ProxyExitStateNoSupply,
			ReplaceMode: config.ReplaceMode,
			StateReason: config.StateReason,
		}
		if assignment, ok := assignments[proxyClientId]; ok {
			exitId := assignment.AssignmentId
			exitSince := assignment.AssignedAt
			exit.ExitId = &exitId
			exit.ExitSince = &exitSince
			exit.CountryCode = assignment.CountryCode
			if assignment.LocationId != nil {
				if location, ok := locations[*assignment.LocationId]; ok && location != nil {
					exit.City = location.City
					exit.Region = location.Region
					if exit.CountryCode == "" {
						exit.CountryCode = location.CountryCode
					}
				}
			}
			switch assignment.State {
			case proxyExitAssignmentPending, proxyExitAssignmentActive:
				exit.State = ProxyExitStateAssigned
				exit.StateReason = ""
			default:
				exit.State = ProxyExitStateReplacing
				if config.State == ProxyExitStateNoSupply {
					exit.State = ProxyExitStateNoSupply
				}
				if exit.StateReason == "" {
					exit.StateReason = assignment.FailingReason
				}
			}
		}
		exits[proxyClientId] = exit
	}
	return exits
}

// Creation, from auth-client

type uniqueExitCreatePlan struct {
	replaceMode string
	specs       []*ProviderSpec
	candidates  []*uniqueExitCandidate
	settings    *UniqueExitSettings
}

func uniqueExitNoSupplyError() *AuthNetworkClientError {
	return &AuthNetworkClientError{
		NoUniqueSupply: true,
		Message:        "No unique exit IP is available in this location right now. Try another location, or best available.",
	}
}

// planUniqueExitCreate decides a unique-exit create before the create is
// counted: Pro only, a location that is not one provider, and at least one exit
// the network can take. A refusal here spends neither the network's proxy cap
// nor its create rate.
func planUniqueExitCreate(
	clientSession *session.ClientSession,
	proxyConfig *ProxyConfig,
	deviceState *ProxyDeviceState,
) (*uniqueExitCreatePlan, *AuthNetworkClientError) {
	var location *sdk.ConnectLocation
	if deviceState != nil {
		location = deviceState.Location
	}
	replaceMode := proxyConfig.UniqueExitReplace
	switch replaceMode {
	case "":
		replaceMode = UniqueExitReplaceAuto
	case UniqueExitReplaceAuto, UniqueExitReplaceManual:
	default:
		return nil, &AuthNetworkClientError{
			Message: `unique_exit_replace must be "auto" or "manual".`,
		}
	}
	networkId := clientSession.ByJwt.NetworkId
	if !IsProFresh(clientSession.Ctx, &networkId) {
		return nil, &AuthNetworkClientError{
			UpgradeRequired: true,
			Message:         "Unique exit IPs are a UR Pro feature. Upgrade to UR Pro to create unique-exit proxies.",
		}
	}
	specs, err := UniqueExitProviderSpecs(location)
	if err != nil {
		return nil, &AuthNetworkClientError{Message: err.Error()}
	}
	settings := GetUniqueExitSettings()
	candidates, err := drawUniqueExitCandidates(
		clientSession.Ctx,
		clientSession,
		networkId,
		specs,
		nil,
		settings,
		server.NowUtc(),
		16,
	)
	if err != nil {
		glog.Infof("[upe]draw for network %s: %s\n", networkId, err)
	}
	if len(candidates) == 0 {
		proxyUniqueExitAllocations.WithLabelValues("no_supply").Inc()
		return nil, uniqueExitNoSupplyError()
	}
	return &uniqueExitCreatePlan{
		replaceMode: replaceMode,
		specs:       specs,
		candidates:  candidates,
		settings:    settings,
	}, nil
}

func proxyExitFromAssignment(ctx context.Context, assignment *proxyExitAssignment, replaceMode string) *ProxyExit {
	exitId := assignment.AssignmentId
	exitSince := assignment.AssignedAt
	exit := &ProxyExit{
		ExitId:      &exitId,
		CountryCode: assignment.CountryCode,
		ExitSince:   &exitSince,
		State:       ProxyExitStateAssigned,
		ReplaceMode: replaceMode,
	}
	if assignment.LocationId != nil {
		if location, ok := GetLocations(ctx, []server.Id{*assignment.LocationId})[*assignment.LocationId]; ok && location != nil {
			exit.City = location.City
			exit.Region = location.Region
			if exit.CountryCode == "" {
				exit.CountryCode = location.CountryCode
			}
		}
	}
	return exit
}

// Rotate

type RotateProxyExitArgs struct {
	ClientId server.Id `json:"client_id"`
}

type RotateProxyExitResult struct {
	Exit  *ProxyExit            `json:"exit,omitempty"`
	Error *RotateProxyExitError `json:"error,omitempty"`
}

type RotateProxyExitError struct {
	Message        string `json:"message"`
	NoUniqueSupply bool   `json:"no_unique_supply,omitempty"`
	RateLimited    bool   `json:"rate_limited,omitempty"`
}

var uniqueExitRotateNow = server.NowUtc

func Testing_SetUniqueExitRotateNow(now func() time.Time) func() {
	previous := uniqueExitRotateNow
	uniqueExitRotateNow = now
	return func() {
		uniqueExitRotateNow = previous
	}
}

// rotateAllowed spends one of the proxy's rotates: at most one per
// RotateMinInterval and RotatesPerDay a day, counted in redis time buckets like
// the proxy create rate. A redis failure allows the rotate, as the create rate
// does.
func rotateAllowed(ctx context.Context, proxyClientId server.Id, now time.Time, settings *UniqueExitSettings) (allowed bool) {
	allowed = true
	intervalSeconds := max(1, int64(settings.RotateMinInterval/time.Second))
	intervalKey := fmt.Sprintf("upe-rot-%s-%d", proxyClientId, now.Unix()/intervalSeconds)
	dayKey := fmt.Sprintf("upe-rotd-%s-%d", proxyClientId, now.Unix()/86400)
	server.HandleError(func() {
		server.Redis(ctx, func(r server.RedisClient) {
			intervalCount, err := r.Incr(ctx, intervalKey).Result()
			server.Raise(err)
			r.Expire(ctx, intervalKey, 2*time.Duration(intervalSeconds)*time.Second)
			dayCount, err := r.Incr(ctx, dayKey).Result()
			server.Raise(err)
			r.Expire(ctx, dayKey, 48*time.Hour)
			if 1 < intervalCount || int64(settings.RotatesPerDay) < dayCount {
				allowed = false
			}
		})
	})
	return
}

// RotateProxyExit moves a unique-exit proxy to a new unique exit on the
// customer's request. Without another exit to take, the proxy keeps its
// current one.
func RotateProxyExit(rotate *RotateProxyExitArgs, clientSession *session.ClientSession) (*RotateProxyExitResult, error) {
	ctx := clientSession.Ctx
	settings := GetUniqueExitSettings()
	now := uniqueExitRotateNow()
	config := loadProxyUniqueExitConfigs(ctx, []server.Id{rotate.ClientId})[rotate.ClientId]
	if config == nil || config.NetworkId != clientSession.ByJwt.NetworkId {
		return &RotateProxyExitResult{
			Error: &RotateProxyExitError{Message: "Not a unique-exit proxy of this network."},
		}, nil
	}
	if callerClientId := clientSession.ByJwt.ClientId; callerClientId != nil && *callerClientId != rotate.ClientId {
		// a client token may rotate only itself; the network token rotates any
		// of the network's proxies
		return &RotateProxyExitResult{
			Error: &RotateProxyExitError{Message: "Not a unique-exit proxy of this network."},
		}, nil
	}
	current := loadLiveProxyExitAssignment(ctx, rotate.ClientId)
	if current == nil {
		return &RotateProxyExitResult{
			Error: &RotateProxyExitError{Message: "Not a unique-exit proxy of this network."},
		}, nil
	}
	if !rotateAllowed(ctx, rotate.ClientId, now, settings) {
		return &RotateProxyExitResult{
			Error: &RotateProxyExitError{
				RateLimited: true,
				Message:     fmt.Sprintf("A proxy's exit can be rotated once a minute and %d times a day.", settings.RotatesPerDay),
			},
		}, nil
	}
	next := replaceProxyExit(ctx, clientSession, config, current, ProxyExitReasonRotate, true, settings, now)
	if next == nil {
		return &RotateProxyExitResult{
			Error: &RotateProxyExitError{
				NoUniqueSupply: true,
				Message:        "No other unique exit IP is available in this location right now. The proxy keeps its current exit.",
			},
		}, nil
	}
	return &RotateProxyExitResult{
		Exit: GetProxyExits(ctx, []server.Id{rotate.ClientId})[rotate.ClientId],
	}, nil
}

// The sweeper

// Liveness and verdicts, injectable for tests. Production reads the provider
// liveness lease (offline providers, failing open on unknown) and the hard
// exclusions find-providers2 applies (blackhole and TLS authentication).
var uniqueExitProviderOffline = func(ctx context.Context, clientIds []server.Id, now time.Time) (map[server.Id]bool, error) {
	offline, _, err := getProviderLivenessExclusions(ctx, clientIds, now)
	return offline, err
}

var uniqueExitProviderExcluded = getProviderHardExclusions

func Testing_SetUniqueExitProviderSignals(
	offline func(ctx context.Context, clientIds []server.Id, now time.Time) (map[server.Id]bool, error),
	excluded func(ctx context.Context, clientIds []server.Id) (map[server.Id]bool, error),
) func() {
	previousOffline, previousExcluded := uniqueExitProviderOffline, uniqueExitProviderExcluded
	uniqueExitProviderOffline, uniqueExitProviderExcluded = offline, excluded
	return func() {
		uniqueExitProviderOffline, uniqueExitProviderExcluded = previousOffline, previousExcluded
	}
}

type ProxyExitSweepResult struct {
	LiveCount          int `json:"live_count"`
	MarkedFailingCount int `json:"marked_failing_count"`
	ClearedCount       int `json:"cleared_count"`
	ReplacedCount      int `json:"replaced_count"`
	HeldCount          int `json:"held_count"`
	KeyUpdatedCount    int `json:"key_updated_count"`
	ReleasedCount      int `json:"released_count"`
	DeletedCount       int `json:"deleted_count"`
}

var uniqueExitSupplyGaugeAt atomic.Int64

func loadLiveProxyExitAssignments(ctx context.Context, limit int) []*proxyExitAssignment {
	assignments := []*proxyExitAssignment{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+proxyExitAssignmentColumns+`
				FROM proxy_exit_assignment
				WHERE state IN ('pending', 'active', 'replacing')
				ORDER BY assigned_at
				LIMIT $1
			`,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				assignments = append(assignments, scanProxyExitAssignment(result.Scan))
			}
		})
	})
	return assignments
}

// SweepProxyExits keeps every live unique exit honest:
//   - a removed proxy's exit is released;
//   - a provider with a bad verdict, or an exit whose key now collides, is
//     replaced at once;
//   - a provider that went offline, or that the device cannot reach, is marked
//     failing, and replaced once the grace has passed;
//   - a recovered provider is cleared;
//   - a provider whose probes report a new key gets it, or is replaced when
//     the new key collides;
//   - a held proxy (replacing, auto) is retried.
//
// Released rows past the retention are deleted, and the gauges are set.
func SweepProxyExits(ctx context.Context, now time.Time, settings *UniqueExitSettings) *ProxyExitSweepResult {
	sweep := &ProxyExitSweepResult{}
	live := loadLiveProxyExitAssignments(ctx, settings.SweepBatchSize)
	sweep.LiveCount = len(live)

	stateCounts := map[string]int{
		proxyExitAssignmentPending:   0,
		proxyExitAssignmentActive:    0,
		proxyExitAssignmentReplacing: 0,
	}
	providerIds := []server.Id{}
	proxyIds := []server.Id{}
	seenProviders := map[server.Id]bool{}
	for _, assignment := range live {
		stateCounts[assignment.State] += 1
		proxyIds = append(proxyIds, assignment.ProxyClientId)
		if !seenProviders[assignment.ProviderClientId] {
			seenProviders[assignment.ProviderClientId] = true
			providerIds = append(providerIds, assignment.ProviderClientId)
		}
	}

	activeProxies := map[server.Id]bool{}
	currentKeys := map[server.Id]*struct {
		key        []byte
		observedAt time.Time
	}{}
	if 0 < len(live) {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`SELECT client_id FROM network_client WHERE client_id = ANY($1) AND active = true`,
				proxyIds,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					server.Raise(result.Scan(&clientId))
					activeProxies[clientId] = true
				}
			})
			result, err = conn.Query(
				ctx,
				`
					SELECT client_id, egress_key, egress_key_observed_at
					FROM provider_egress_location
					WHERE client_id = ANY($1) AND egress_key IS NOT NULL
				`,
				providerIds,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var clientId server.Id
					current := &struct {
						key        []byte
						observedAt time.Time
					}{}
					server.Raise(result.Scan(&clientId, &current.key, &current.observedAt))
					currentKeys[clientId] = current
				}
			})
		})
	}
	offline := map[server.Id]bool{}
	excluded := map[server.Id]bool{}
	if 0 < len(providerIds) {
		if o, err := uniqueExitProviderOffline(ctx, providerIds, now); err == nil {
			offline = o
		} else {
			glog.Infof("[upe]liveness: %s\n", err)
		}
		if e, err := uniqueExitProviderExcluded(ctx, providerIds); err == nil {
			excluded = e
		} else {
			glog.Infof("[upe]exclusions: %s\n", err)
		}
	}
	configs := loadProxyUniqueExitConfigs(ctx, proxyIds)
	sessions := map[server.Id]*session.ClientSession{}
	networkSession := func(networkId server.Id) *session.ClientSession {
		if s, ok := sessions[networkId]; ok {
			return s
		}
		s := session.NewLocalClientSession(ctx, "0.0.0.0:0", &session.ByJwt{NetworkId: networkId})
		sessions[networkId] = s
		return s
	}
	defer func() {
		for _, s := range sessions {
			s.Cancel()
		}
	}()

	replace := func(config *proxyUniqueExitConfig, assignment *proxyExitAssignment, reason string) {
		if next := replaceProxyExit(ctx, networkSession(assignment.NetworkId), config, assignment, reason, false, settings, now); next != nil {
			sweep.ReplacedCount += 1
		} else {
			sweep.HeldCount += 1
		}
	}

	for _, assignment := range live {
		if !activeProxies[assignment.ProxyClientId] {
			server.Tx(ctx, func(tx server.PgTx) {
				releaseProxyExitAssignmentInTx(ctx, tx, assignment, ProxyExitReasonProxyRemoved, now)
				server.RaisePgResult(tx.Exec(
					ctx,
					`DELETE FROM proxy_unique_exit WHERE proxy_client_id = $1`,
					assignment.ProxyClientId,
				))
			})
			sweep.ReleasedCount += 1
			continue
		}
		config, ok := configs[assignment.ProxyClientId]
		if !ok {
			continue
		}

		if assignment.State == proxyExitAssignmentReplacing {
			if config.ReplaceMode == UniqueExitReplaceAuto {
				reason := assignment.FailingReason
				if reason == "" {
					reason = ProxyExitReasonOffline
				}
				replace(config, assignment, reason)
			}
			continue
		}

		if excluded[assignment.ProviderClientId] {
			replace(config, assignment, ProxyExitReasonVerdict)
			continue
		}
		if assignment.FailingReason == ProxyExitReasonCollision {
			replace(config, assignment, ProxyExitReasonCollision)
			continue
		}

		if current, ok := currentKeys[assignment.ProviderClientId]; ok && !bytes.Equal(current.key, assignment.EgressKey) {
			since := assignment.AssignedAt
			if assignment.ConfirmedAt != nil && since.Before(*assignment.ConfirmedAt) {
				since = *assignment.ConfirmedAt
			}
			if since.Before(current.observedAt) {
				if updateProxyExitKey(ctx, assignment, current.key) {
					sweep.KeyUpdatedCount += 1
				} else {
					replace(config, assignment, ProxyExitReasonCollision)
					continue
				}
			}
		}

		failing := offline[assignment.ProviderClientId] || assignment.FailingReason == ProxyExitReasonUnreachable
		switch {
		case failing && assignment.FailingSince == nil:
			reason := ProxyExitReasonOffline
			if !offline[assignment.ProviderClientId] {
				reason = assignment.FailingReason
			}
			ReportProxyExitFailing(ctx, assignment.ProxyClientId, assignment.AssignmentId, reason, now)
			sweep.MarkedFailingCount += 1
		case failing && settings.ReplacementGrace <= now.Sub(*assignment.FailingSince):
			reason := assignment.FailingReason
			if reason == "" {
				reason = ProxyExitReasonOffline
			}
			replace(config, assignment, reason)
		case !failing && assignment.FailingReason == ProxyExitReasonOffline:
			clearProxyExitFailing(ctx, assignment)
			sweep.ClearedCount += 1
		}
	}

	server.Db(ctx, func(conn server.PgConn) {
		tag := server.RaisePgResult(conn.Exec(
			ctx,
			`
				DELETE FROM proxy_exit_assignment
				WHERE assignment_id IN (
					SELECT assignment_id
					FROM proxy_exit_assignment
					WHERE state = 'released' AND released_at < $1
					LIMIT 5000
				)
			`,
			now.UTC().Add(-settings.ReleasedRetention),
		))
		sweep.DeletedCount = int(tag.RowsAffected())
	})

	for state, count := range stateCounts {
		proxyUniqueExitAssignments.WithLabelValues(state).Set(float64(count))
	}
	if last := uniqueExitSupplyGaugeAt.Load(); last == 0 || settings.SupplyGaugeInterval <= now.Sub(time.Unix(0, last)) {
		uniqueExitSupplyGaugeAt.Store(now.UnixNano())
		setUniqueExitSupplyGauge(ctx, now, settings)
	}
	return sweep
}

// updateProxyExitKey moves an exit to its provider's new key, unless another of
// the network's live exits holds it.
func updateProxyExitKey(ctx context.Context, assignment *proxyExitAssignment, key []byte) (updated bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		lockUniqueExitNetworkInTx(ctx, tx, assignment.NetworkId)
		var taken bool
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT EXISTS (
					SELECT 1
					FROM proxy_exit_assignment
					WHERE
						network_id = $1 AND
						egress_key = $2 AND
						state IN ('pending', 'active') AND
						assignment_id <> $3
				)
			`,
			assignment.NetworkId,
			key,
			assignment.AssignmentId,
		).Scan(&taken))
		if taken {
			return
		}
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE proxy_exit_assignment
				SET egress_key = $2
				WHERE assignment_id = $1 AND state IN ('pending', 'active')
			`,
			assignment.AssignmentId,
			key,
		))
		updated = tag.RowsAffected() == 1
	})
	return
}

func clearProxyExitFailing(ctx context.Context, assignment *proxyExitAssignment) {
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(
			ctx,
			`
				UPDATE proxy_exit_assignment
				SET failing_since = NULL, failing_reason = ''
				WHERE
					assignment_id = $1 AND
					failing_reason = 'offline' AND
					state IN ('pending', 'active')
			`,
			assignment.AssignmentId,
		))
	})
}

func setUniqueExitSupplyGauge(ctx context.Context, now time.Time, settings *UniqueExitSettings) {
	server.HandleError(func() {
		counts := map[string]int{}
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT country_code, count(DISTINCT egress_key)
					FROM provider_egress_location
					WHERE
						egress_key IS NOT NULL AND
						$1 <= egress_key_observed_at
					GROUP BY country_code
				`,
				now.UTC().Add(-settings.EgressKeyMaxAge),
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var countryCode string
					var count int
					server.Raise(result.Scan(&countryCode, &count))
					counts[countryCode] = count
				}
			})
		})
		proxyUniqueExitSupplyKeys.Reset()
		for countryCode, count := range counts {
			proxyUniqueExitSupplyKeys.WithLabelValues(countryCode).Set(float64(count))
		}
	})
}
