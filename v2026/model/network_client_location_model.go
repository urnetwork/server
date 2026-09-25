package model

import (
	"cmp"
	"context"
	// "encoding/hex"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"bytes"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand"
	"net"
	"slices"
	"unicode/utf8"

	"github.com/urnetwork/glog/v2026"

	"maps"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/geo"
	"github.com/urnetwork/server/v2026/search"
	"github.com/urnetwork/server/v2026/session"
	"github.com/urnetwork/server/v2026/stats"
)

const (
	// One UpdateClientScores export contains thousands of cross-slot SETs for
	// each caller location. Sending all of them through one ClusterClient
	// pipeline from each of 48 workers can fill a node socket until the 15s
	// write deadline; generating a complete operation list before batching also
	// retained tens of GiB across those workers. Produce and flush one bounded
	// batch at a time; completed chunks are not replayed when a later chunk
	// needs a transient retry.
	clientScoreExportBatchSize   = 512
	clientScoreExportBatchBytes  = 8 << 20
	clientScoreExportMaxAttempts = 3

	// A caller-target alias is intentionally tiny. "1" selects the shared
	// zero-caller baseline and "0" selects the caller-specific override. A
	// missing alias means the cache was written by a legacy process and must
	// continue to select the caller-specific payload.
	clientScoreAliasBaselineValue = "1"
	clientScoreAliasCallerValue   = "0"
	clientScoreAliasReadyKey      = "client_score_alias_v1_ready"
	// The score writer only visits targets present in its current SQL result.
	// Persist the last complete target set so a location that loses its final
	// eligible provider receives an explicit empty payload instead of leaving
	// its prior providers selectable for the cache TTL.
	clientScoreTargetManifestKey = "client_score_target_manifest_v1"
	// Published only after a complete score export whose SQL excludes derived
	// window clients and inactive top-level clients. Monitoring uses this
	// durable boundary to distinguish harmless raw candidate rows from caches
	// written by the legacy unfiltered query.
	clientScoreProviderEligibilityReadyKey   = "client_score_provider_eligibility_v1_ready"
	clientScoreProviderEligibilityReadyValue = "1"
	// Published only after a complete score export that wrote the per-family
	// facets (connect/IPV6.md A8). Until then every export also writes
	// today's un-faceted counts and samples, so an api process that predates
	// the facets keeps reading a live cache. Deploy the api before the
	// taskworker: once this is set the un-faceted payloads expire with the
	// cache ttl and only a faceted reader finds providers.
	clientScoreIpFamilyReadyKey   = "client_score_ip_family_v1_ready"
	clientScoreIpFamilyReadyValue = "1"
)

type clientScoreRedisSet struct {
	key   string
	value []byte
}

type clientScoreTargetManifestDocument struct {
	LocationIds      []string `json:"location_ids"`
	LocationGroupIds []string `json:"location_group_ids"`
}

type clientScoreTargetManifest struct {
	locationIds      map[server.Id]bool
	locationGroupIds map[server.Id]bool
}

// clientScoreTargetKeys builds the payload and alias key families for one
// target location or location group. The caller location stays in each hash
// tag to preserve cluster distribution; aliases let equivalent callers share
// the zero-caller payload instead of duplicating it.
type clientScoreTargetKeys struct {
	counts func(server.Id) string
	filter func(server.Id) string
	sample func(server.Id, int) string
	alias  func(server.Id) string
	// the per-family facets of counts and sample (connect/IPV6.md A8)
	facetCounts func(server.Id, ipFamilyFacet) string
	facetSample func(server.Id, ipFamilyFacet, int) string
}

// clientScoreFacetPayload is one family facet's encoded counts and on-demand
// sample encoder.
type clientScoreFacetPayload struct {
	countsBytes  []byte
	counts       []int
	encodeSample func(int) []byte
}

// clientScoreExportPayload is one target's encoded cache payload: today's
// un-faceted counts and samples over every provider, the public
// ClientFilter, and one facet per family category that find-providers2 reads
// by preference. Every facet is present, an empty category included, so a
// reader can tell "written without facets" from "no providers in this
// family". Samples are encoded on demand so exporters retain one at a time.
type clientScoreExportPayload struct {
	countsBytes  []byte
	filterBytes  []byte
	counts       []int
	encodeSample func(int) []byte
	facets       map[ipFamilyFacet]clientScoreFacetPayload
}

type clientScoreTargetEncode func(map[server.Id]*ClientScore) clientScoreExportPayload

type clientScoreExportBatchExec func([]clientScoreRedisSet) error
type clientScoreExportRetryWait func(context.Context, int) error
type clientScoreExportProduce func(func(clientScoreRedisSet) error) error

func clientScoreNetworkIds(clientScores map[server.Id]*ClientScore) map[server.Id]bool {
	networkIds := make(map[server.Id]bool)
	for _, clientScore := range clientScores {
		networkIds[clientScore.NetworkId] = true
	}
	return networkIds
}

func networkSetsIntersect(a, b map[server.Id]bool) bool {
	if len(a) > len(b) {
		a, b = b, a
	}
	for networkId := range a {
		if b[networkId] {
			return true
		}
	}
	return false
}

func filterClientScoresByNetwork(
	clientScores map[server.Id]*ClientScore,
	excludedNetworkIds map[server.Id]bool,
) map[server.Id]*ClientScore {
	activeClientScores := make(map[server.Id]*ClientScore, len(clientScores))
	for clientId, clientScore := range clientScores {
		if !excludedNetworkIds[clientScore.NetworkId] {
			activeClientScores[clientId] = clientScore
		}
	}
	return activeClientScores
}

func filterClientScoresByNetworkWithMetrics(
	clientScores map[server.Id]*ClientScore,
	excludedNetworkIds map[server.Id]bool,
	metrics *updateClientScoresPhaseMetricSet,
) map[server.Id]*ClientScore {
	span := metrics.start(updateClientScoresPhaseTargetMap)
	defer span.finish()
	activeClientScores := filterClientScoresByNetwork(clientScores, excludedNetworkIds)
	metrics.addWork(updateClientScoresPhaseTargetMap, len(clientScores), 0)
	return activeClientScores
}

// emitClientScoreTargetFanout stores one complete zero-caller baseline for a
// target. Callers whose blocked-network set leaves the target unchanged get a
// one-byte baseline alias; callers that really remove a provider get a complete
// override and a caller alias. A missing alias remains the legacy-reader
// sentinel, so a rolling deployment can read cache entries from either writer.
//
// The first alias-aware export can additionally refresh the legacy duplicate
// payloads for unchanged callers. Once that complete export publishes the
// schema-ready marker, later exports omit those duplicates and let their normal
// TTL reclaim the memory without a production delete.
//
// This changes storage sharing, not score contents. loadClientScores already
// randomizes the sample-key order and FindProviders2 performs a final weighted
// selection, so equivalent callers do not require independently shuffled gob
// payloads.
func emitClientScoreTargetFanout(
	clientLocationIds []server.Id,
	clientScores map[server.Id]*ClientScore,
	excludeLocationNetworkIds map[server.Id]map[server.Id]bool,
	keys clientScoreTargetKeys,
	encode clientScoreTargetEncode,
	writeLegacyUnchanged bool,
	writeUnfacetedPayload bool,
	emit func(clientScoreRedisSet) error,
) error {
	return emitClientScoreTargetFanoutWithMetrics(
		clientLocationIds,
		clientScores,
		excludeLocationNetworkIds,
		keys,
		encode,
		writeLegacyUnchanged,
		writeUnfacetedPayload,
		emit,
		updateClientScoresPhaseMetrics,
	)
}

func emitClientScoreTargetFanoutWithMetrics(
	clientLocationIds []server.Id,
	clientScores map[server.Id]*ClientScore,
	excludeLocationNetworkIds map[server.Id]map[server.Id]bool,
	keys clientScoreTargetKeys,
	encode clientScoreTargetEncode,
	writeLegacyUnchanged bool,
	writeUnfacetedPayload bool,
	emit func(clientScoreRedisSet) error,
	metrics *updateClientScoresPhaseMetricSet,
) error {
	targetSpan := metrics.start(updateClientScoresPhaseTargetExport)
	defer targetSpan.finish()
	metrics.addWork(updateClientScoresPhaseTargetExport, 1, 0)

	mapSpan := metrics.start(updateClientScoresPhaseTargetMap)
	defer mapSpan.finish()
	targetNetworkIds := clientScoreNetworkIds(clientScores)
	unchangedClientLocationIds := make([]server.Id, 0, len(clientLocationIds))
	changedClientLocationIds := make([]server.Id, 0)
	for _, clientLocationId := range clientLocationIds {
		if clientLocationId == (server.Id{}) {
			continue
		}
		if networkSetsIntersect(targetNetworkIds, excludeLocationNetworkIds[clientLocationId]) {
			changedClientLocationIds = append(changedClientLocationIds, clientLocationId)
		} else {
			unchangedClientLocationIds = append(unchangedClientLocationIds, clientLocationId)
		}
	}
	metrics.addWork(updateClientScoresPhaseTargetMap, len(clientScores)+len(clientLocationIds), 0)
	mapSpan.finish()

	emitPayload := func(
		callerIds []server.Id,
		payload clientScoreExportPayload,
	) error {
		for _, callerId := range callerIds {
			sets := []clientScoreRedisSet{
				{key: keys.filter(callerId), value: payload.filterBytes},
			}
			if writeUnfacetedPayload {
				sets = append(sets, clientScoreRedisSet{key: keys.counts(callerId), value: payload.countsBytes})
			}
			for _, facet := range ipFamilyFacets {
				sets = append(sets, clientScoreRedisSet{
					key:   keys.facetCounts(callerId, facet),
					value: payload.facets[facet].countsBytes,
				})
			}
			for _, set := range sets {
				if err := emit(set); err != nil {
					return err
				}
			}
		}
		if writeUnfacetedPayload {
			for sampleIndex := range payload.counts {
				// Encode before the caller loop: every equivalent caller gets
				// the same immutable payload under its own key.
				value := payload.encodeSample(sampleIndex)
				for _, callerId := range callerIds {
					if err := emit(clientScoreRedisSet{
						key:   keys.sample(callerId, sampleIndex),
						value: value,
					}); err != nil {
						return err
					}
				}
			}
		}
		for _, facet := range ipFamilyFacets {
			facetPayload := payload.facets[facet]
			for sampleIndex := range facetPayload.counts {
				value := facetPayload.encodeSample(sampleIndex)
				for _, callerId := range callerIds {
					if err := emit(clientScoreRedisSet{
						key:   keys.facetSample(callerId, facet, sampleIndex),
						value: value,
					}); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}

	// The zero caller is the canonical baseline even when the caller list does
	// not explicitly contain it. During the one-time compatibility pass, the
	// same immutable bytes also refresh every legacy unchanged-caller key.
	payload := encode(clientScores)
	baselineAndLegacyCallers := []server.Id{{}}
	if writeLegacyUnchanged {
		baselineAndLegacyCallers = append(baselineAndLegacyCallers, unchangedClientLocationIds...)
	}
	if err := emitPayload(baselineAndLegacyCallers, payload); err != nil {
		return err
	}
	for _, clientLocationId := range unchangedClientLocationIds {
		if err := emit(clientScoreRedisSet{
			key:   keys.alias(clientLocationId),
			value: []byte(clientScoreAliasBaselineValue),
		}); err != nil {
			return err
		}
	}
	for _, clientLocationId := range changedClientLocationIds {
		activeClientScores := filterClientScoresByNetworkWithMetrics(
			clientScores,
			excludeLocationNetworkIds[clientLocationId],
			metrics,
		)
		if err := emitPayload([]server.Id{clientLocationId}, encode(activeClientScores)); err != nil {
			return err
		}
		if err := emit(clientScoreRedisSet{
			key:   keys.alias(clientLocationId),
			value: []byte(clientScoreAliasCallerValue),
		}); err != nil {
			return err
		}
	}
	return nil
}

// selectClientScorePayload resolves one alias-aware read. Missing/unknown
// aliases deliberately preserve the legacy caller-key behavior. If a baseline
// alias races a missing/evicted baseline value, the caller payload is the safe
// fallback because it preserves exclusions.
func selectClientScorePayload(
	clientLocationId server.Id,
	aliasBytes []byte,
	callerBytes []byte,
	baselineBytes []byte,
) (effectiveClientLocationId server.Id, payload []byte) {
	if clientLocationId != (server.Id{}) &&
		string(aliasBytes) == clientScoreAliasBaselineValue &&
		0 < len(baselineBytes) {
		return server.Id{}, baselineBytes
	}
	return clientLocationId, callerBytes
}

func clientScoreCommandBytes(cmd *redis.StringCmd) []byte {
	if cmd == nil {
		return nil
	}
	value, _ := cmd.Bytes()
	return value
}

func transientClientScoreExportError(err error) bool {
	if err == nil {
		return false
	}
	// Pool exhaustion is local backpressure. Retrying it in-place adds more
	// demand to the same saturated pool, matching server.Redis's fail-fast
	// rule for this class.
	if strings.Contains(err.Error(), "redis: connection pool timeout") {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	message := err.Error()
	for _, marker := range []string{
		"i/o timeout",
		"connection reset by peer",
		"cannot assign requested address",
		"redis: client is closed",
		"CLUSTERDOWN",
		"LOADING",
		"READONLY",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// runClientScoreExportStream is the deterministic bounded-working-set and
// retry core. Produce cannot get past either the command-count or payload-byte
// budget ahead of execBatch: a full batch is synchronously written and cleared
// before emit returns. SET is idempotent and every value carries the same ttl, so retrying
// exactly the failed chunk is safe; successful earlier chunks are deliberately
// not replayed. retryWait is injected so tests never depend on wall-clock
// sleeps.
func runClientScoreExportStream(
	ctx context.Context,
	batchSize int,
	maxBatchBytes int,
	maxAttempts int,
	produce clientScoreExportProduce,
	execBatch clientScoreExportBatchExec,
	retryWait clientScoreExportRetryWait,
) error {
	if batchSize <= 0 || maxBatchBytes <= 0 || maxAttempts <= 0 || produce == nil || execBatch == nil || retryWait == nil {
		return fmt.Errorf("client score export requires positive batch size, byte budget, and attempts")
	}
	batch := make([]clientScoreRedisSet, 0, batchSize)
	batchBytes := 0
	batchIndex := 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		batchIndex++
		var err error
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			err = execBatch(batch)
			if err == nil {
				break
			}
			if !transientClientScoreExportError(err) || attempt == maxAttempts {
				return fmt.Errorf("client score export batch %d attempt %d/%d: %w", batchIndex, attempt, maxAttempts, err)
			}
			if waitErr := retryWait(ctx, attempt); waitErr != nil {
				return waitErr
			}
		}
		// Release every encoded payload before produce resumes. Re-slicing alone
		// leaves the old []byte references in the backing array and lets a short
		// tail retain almost a full prior batch until the worker exits.
		clear(batch)
		batch = batch[:0]
		batchBytes = 0
		return nil
	}
	emit := func(set clientScoreRedisSet) error {
		setBytes := len(set.key) + len(set.value)
		// Count and bytes are independent guards. Encoded samples vary with
		// provider population, so a 512-command batch alone is not a bounded
		// working set. Flush before adding a value that would cross the byte
		// budget. One individually oversized value is unavoidable, but is sent
		// alone immediately instead of being combined with other payloads.
		if 0 < len(batch) && (maxBatchBytes-batchBytes < setBytes) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, set)
		batchBytes += setBytes
		if len(batch) == batchSize || maxBatchBytes <= batchBytes {
			return flush()
		}
		return nil
	}
	if err := produce(emit); err != nil {
		return err
	}
	return flush()
}

// runClientScoreExportBatches retains the slice-based seam for focused retry
// tests and small callers. Production uses the streaming producer below, so a
// whole caller-location export is never materialized at once.
func runClientScoreExportBatches(
	ctx context.Context,
	sets []clientScoreRedisSet,
	batchSize int,
	maxBatchBytes int,
	maxAttempts int,
	execBatch clientScoreExportBatchExec,
	retryWait clientScoreExportRetryWait,
) error {
	return runClientScoreExportStream(
		ctx,
		batchSize,
		maxBatchBytes,
		maxAttempts,
		func(emit func(clientScoreRedisSet) error) error {
			for _, set := range sets {
				if err := emit(set); err != nil {
					return err
				}
			}
			return nil
		},
		execBatch,
		retryWait,
	)
}

func writeClientScoreRedisStream(ctx context.Context, r server.RedisClient, ttl time.Duration, produce clientScoreExportProduce) error {
	return runClientScoreExportStream(
		ctx,
		clientScoreExportBatchSize,
		clientScoreExportBatchBytes,
		clientScoreExportMaxAttempts,
		produce,
		func(batch []clientScoreRedisSet) error {
			span := updateClientScoresPhaseMetrics.start(updateClientScoresPhaseCacheWrite)
			defer span.finish()
			pipe := r.Pipeline()
			batchBytes := 0
			for _, set := range batch {
				batchBytes += len(set.key) + len(set.value)
				pipe.Set(ctx, set.key, set.value, ttl)
			}
			updateClientScoresPhaseMetrics.addWork(updateClientScoresPhaseCacheWrite, len(batch), batchBytes)
			_, err := pipe.Exec(ctx)
			return err
		},
		func(ctx context.Context, failedAttempt int) error {
			timer := time.NewTimer(time.Duration(failedAttempt) * 250 * time.Millisecond)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
	)
}

func clientScoreAliasReady(ctx context.Context) (ready bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, clientScoreAliasReadyKey).Result()
		if err == redis.Nil {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		ready = value == clientScoreAliasBaselineValue
	})
	return
}

func markClientScoreAliasReady(ctx context.Context) (returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = r.Set(ctx, clientScoreAliasReadyKey, clientScoreAliasBaselineValue, 0).Err()
	})
	return
}

func clientScoreProviderEligibilityReady(ctx context.Context) (ready bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, clientScoreProviderEligibilityReadyKey).Result()
		if err == redis.Nil {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		ready = value == clientScoreProviderEligibilityReadyValue
	})
	return
}

func markClientScoreProviderEligibilityReady(ctx context.Context) (returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = r.Set(ctx, clientScoreProviderEligibilityReadyKey, clientScoreProviderEligibilityReadyValue, 0).Err()
	})
	return
}

func clientScoreIpFamilyReady(ctx context.Context) (ready bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, clientScoreIpFamilyReadyKey).Result()
		if err == redis.Nil {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		ready = value == clientScoreIpFamilyReadyValue
	})
	return
}

func markClientScoreIpFamilyReady(ctx context.Context) (returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = r.Set(ctx, clientScoreIpFamilyReadyKey, clientScoreIpFamilyReadyValue, 0).Err()
	})
	return
}

func init() {
	resetCountryCodeLocationIds()
	server.OnReset(func() {
		resetCountryCodeLocationIds()
	})
	server.OnWarmup(server.WarmupTargetCountryLocations, func() {
		countryCodeLocationIds()
	})

	server.OnReset(func() {
		resetLocationDirectory()
	})
	server.OnWarmup(server.WarmupTargetLocationDirectory, func() {
		loadLocationDirectory()
	})
}

func resetCountryCodeLocationIds() {
	countryCodeLocationIds = sync.OnceValue(func() map[string]server.Id {
		ctx := context.Background()

		countryCodeLocationIds := map[string]server.Id{}

		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					country_code,
					location_id
				FROM location
				WHERE
					location_type = 'country'
				`,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var countryCode string
					var locationId server.Id
					server.Raise(result.Scan(
						&countryCode,
						&locationId,
					))
					countryCode = strings.ToLower(countryCode)
					countryCodeLocationIds[countryCode] = locationId
				}
			})
		})

		return countryCodeLocationIds
	})
}

// country code is lowercase
var countryCodeLocationIds func() map[string]server.Id

type locationDirectoryEntry struct {
	Name        string
	CountryCode string
	Latitude    *float64
	Longitude   *float64
}

type locationDirectorySnapshot struct {
	entries  map[server.Id]*locationDirectoryEntry
	loadTime time.Time
}

// Each reset installs a unique non-zero-sized token. A loader may publish only
// while its captured token is still current, and may clear only its own loading
// claim. This matters when Reset replaces the database/redis resources while a
// non-blocking directory refresh from the previous environment is still in
// flight: the previous bool allowed that old refresh to overwrite the new
// snapshot and to clear a newer refresh's claim after reset.
type locationDirectoryGeneration struct {
	marker byte
	ctx    context.Context
	cancel context.CancelFunc
}

type locationDirectoryState struct {
	generation  atomic.Pointer[locationDirectoryGeneration]
	loading     atomic.Pointer[locationDirectoryGeneration]
	snapshot    atomic.Pointer[locationDirectorySnapshot]
	publishLock sync.Mutex
}

func newLocationDirectoryState() *locationDirectoryState {
	state := &locationDirectoryState{}
	state.reset()
	return state
}

func (self *locationDirectoryState) reset() {
	generationCtx, generationCancel := context.WithCancel(context.Background())
	generation := &locationDirectoryGeneration{
		ctx:    generationCtx,
		cancel: generationCancel,
	}

	// Cancel the old generation before replacing it. Every external operation
	// carries that context, so an old loader cannot move from a cache miss into
	// the replacement environment's Redis or PostgreSQL resources after reset.
	// Cancellation is process-local and non-blocking; no external operation is
	// performed while this lock is held.
	self.publishLock.Lock()
	defer self.publishLock.Unlock()

	if previous := self.generation.Load(); previous != nil {
		previous.cancel()
	}
	self.generation.Store(generation)
	self.snapshot.Store(nil)
	self.loading.Store(nil)
}

func (self *locationDirectoryState) startLoad() (*locationDirectoryGeneration, bool) {
	// Pair the claim with reset's generation/loading swap. This path runs only
	// on a missing or 30-minute-stale snapshot, so the short lock is not on the
	// steady-state request path.
	self.publishLock.Lock()
	defer self.publishLock.Unlock()

	generation := self.generation.Load()
	if generation == nil || generation.ctx.Err() != nil || !self.loading.CompareAndSwap(nil, generation) {
		return nil, false
	}
	return generation, true
}

func (self *locationDirectoryState) finishLoad(generation *locationDirectoryGeneration) {
	// An old generation must not clear a newer generation's in-flight claim.
	self.loading.CompareAndSwap(generation, nil)
}

func (self *locationDirectoryState) current(generation *locationDirectoryGeneration) bool {
	return generation != nil && generation.ctx.Err() == nil && self.generation.Load() == generation
}

func (self *locationDirectoryState) publish(
	generation *locationDirectoryGeneration,
	entries map[server.Id]*locationDirectoryEntry,
) bool {
	self.publishLock.Lock()
	defer self.publishLock.Unlock()

	if !self.current(generation) {
		return false
	}
	self.snapshot.Store(&locationDirectorySnapshot{
		entries:  entries,
		loadTime: server.NowUtc(),
	})
	return true
}

// refresh matches the `clientLocationKey` ttl
const locationDirectoryStaleAfter = 30 * time.Minute

var currentLocationDirectory = newLocationDirectoryState()

func resetLocationDirectory() {
	currentLocationDirectory.reset()
}

// the current location directory without blocking the caller.
// nil until the first load completes (callers omit locations), and a stale
// snapshot is served while a single background reload runs.
func locationDirectory() map[server.Id]*locationDirectoryEntry {
	snapshot := currentLocationDirectory.snapshot.Load()
	if snapshot == nil || locationDirectoryStaleAfter <= time.Since(snapshot.loadTime) {
		if generation, started := currentLocationDirectory.startLoad(); started {
			go connect.HandleError(func() {
				defer currentLocationDirectory.finishLoad(generation)
				loadLocationDirectoryForGeneration(generation)
			})
		}
	}
	if snapshot == nil {
		return nil
	}
	return snapshot.entries
}

// locationDirectoryRedisKey shares one computed directory across the fleet.
// The query behind it scans the whole ~53M-row reliability table, and every
// process used to run it independently on its own staleness timer: ~1,550
// executions per 48h, ~2% of all db time, to produce the same ~9k rows each
// time (2026-08-11 audit). The ttl is the same staleness bound the per-process
// snapshot already had, so nothing is served staler than before — the scan is
// just paid once per window for the fleet instead of once per process.
const locationDirectoryRedisKey = "location_directory"

// locationDirectoryRow is the wire form of one directory entry. The directory is
// keyed by location id in memory, but server.Id implements MarshalJSON and not
// MarshalText, so it cannot be a json map key — the cache carries a list and the
// map is rebuilt on read.
type locationDirectoryRow struct {
	LocationId  server.Id `json:"location_id"`
	Name        string    `json:"name"`
	CountryCode string    `json:"country_code"`
	Latitude    *float64  `json:"latitude,omitempty"`
	Longitude   *float64  `json:"longitude,omitempty"`
}

// getLocationDirectoryCache reads the fleet-shared directory, or nil on miss.
// A redis failure is a miss, not an error: the caller falls back to querying pg,
// which is exactly the pre-cache behavior.
func getLocationDirectoryCache(ctx context.Context) map[server.Id]*locationDirectoryEntry {
	var entries map[server.Id]*locationDirectoryEntry
	server.Redis(ctx, func(r server.RedisClient) {
		rowsJson, err := r.Get(ctx, locationDirectoryRedisKey).Result()
		if err != nil {
			// miss, or redis is unavailable; fall back to pg
			return
		}
		rows := []*locationDirectoryRow{}
		if err := json.Unmarshal([]byte(rowsJson), &rows); err != nil {
			glog.V(1).Infof("[nclm]location directory cache decode err = %v\n", err)
			return
		}
		entries = map[server.Id]*locationDirectoryEntry{}
		for _, row := range rows {
			entries[row.LocationId] = &locationDirectoryEntry{
				Name:        row.Name,
				CountryCode: row.CountryCode,
				Latitude:    row.Latitude,
				Longitude:   row.Longitude,
			}
		}
	})
	return entries
}

func setLocationDirectoryCache(
	ctx context.Context,
	entries map[server.Id]*locationDirectoryEntry,
	ttl time.Duration,
) {
	rows := make([]*locationDirectoryRow, 0, len(entries))
	for locationId, entry := range entries {
		rows = append(rows, &locationDirectoryRow{
			LocationId:  locationId,
			Name:        entry.Name,
			CountryCode: entry.CountryCode,
			Latitude:    entry.Latitude,
			Longitude:   entry.Longitude,
		})
	}
	rowsJson, err := json.Marshal(rows)
	if err != nil {
		glog.V(1).Infof("[nclm]location directory cache encode err = %v\n", err)
		return
	}
	server.Redis(ctx, func(r server.RedisClient) {
		if err := r.Set(ctx, locationDirectoryRedisKey, string(rowsJson), ttl).Err(); err != nil {
			// the directory is still usable in-process; the next loader just
			// pays the query again
			glog.V(1).Infof("[nclm]location directory cache write err = %v\n", err)
		}
	})
}

func loadLocationDirectory() {
	loadLocationDirectoryForGeneration(currentLocationDirectory.generation.Load())
}

func loadLocationDirectoryForGeneration(generation *locationDirectoryGeneration) {
	loadLocationDirectoryForGenerationWith(
		currentLocationDirectory,
		generation,
		getLocationDirectoryCache,
		queryLocationDirectory,
		setLocationDirectoryCache,
	)
}

func loadLocationDirectoryForGenerationWith(
	state *locationDirectoryState,
	generation *locationDirectoryGeneration,
	readShared func(context.Context) map[server.Id]*locationDirectoryEntry,
	readDatabase func(context.Context) map[server.Id]*locationDirectoryEntry,
	writeShared func(context.Context, map[server.Id]*locationDirectoryEntry, time.Duration),
) {
	if !state.current(generation) {
		return
	}
	ctx := generation.ctx

	entries := readShared(ctx)
	if !state.current(generation) {
		return
	}
	publishShared := false
	if entries == nil {
		entries = readDatabase(ctx)
		publishShared = true
	}
	if !state.publish(generation, entries) {
		return
	}
	// The local immutable snapshot is available before the optional shared
	// cache write. A slow Redis write therefore cannot block stale/nil readers
	// or reset; reset cancellation stops it before replacement resources exist.
	if publishShared && state.current(generation) {
		writeShared(ctx, entries, locationDirectoryStaleAfter)
	}
}

func queryLocationDirectory(ctx context.Context) map[server.Id]*locationDirectoryEntry {
	entries := map[server.Id]*locationDirectoryEntry{}

	server.Db(ctx, func(conn server.PgConn) {
		// The seeded city list can be ~10^6 rows, so bound the directory to
		// locations referenced by providers that are usable now. Historical
		// disconnected rows dominate this table (~58M on main) but cannot appear
		// in a current provider result; excluding them lets the existing
		// (valid,connected,client_id) index drive a tiny materialized set.
		//
		// The referenced set is collected as DISTINCT (city, region, country)
		// TRIPLES in a single pass, then unnested. Selecting each column's
		// DISTINCT separately and UNIONing them reads the same table three
		// times — the planner runs three independent parallel seq scans, which
		// measured 3x the buffers and ~3.5x the cpu of this shape (2026-08-11).
		// The triple set is tiny (~7.8k rows against ~53M scanned), so the
		// unnest above it is free; unnesting the three columns per ROW instead
		// would push ~159M rows through the function scan and cost far more
		// than the scan it saves.
		result, err := conn.Query(
			ctx,
			`
			SELECT
				location.location_id,
				location.location_name,
				location.country_code,
				location.latitude,
				location.longitude
			FROM location
			WHERE location.location_id IN (
				SELECT DISTINCT loc.id
					FROM (
						SELECT DISTINCT
							city_location_id AS c,
							region_location_id AS r,
							country_location_id AS n
						FROM network_client_location_reliability
						WHERE
							network_client_location_reliability.valid = true AND
							network_client_location_reliability.connected = true
					) triples
				CROSS JOIN LATERAL unnest(ARRAY[triples.c, triples.r, triples.n]) AS loc(id)
				WHERE loc.id IS NOT NULL
			)
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var locationId server.Id
				entry := &locationDirectoryEntry{}
				server.Raise(result.Scan(
					&locationId,
					&entry.Name,
					&entry.CountryCode,
					&entry.Latitude,
					&entry.Longitude,
				))
				entry.CountryCode = strings.ToLower(entry.CountryCode)
				entries[locationId] = entry
			}
		})
	})

	return entries
}

const DefaultMaxDistanceFraction = float32(0.2)

const StrongPrivacyLaws = "Strong Privacy Laws and Internet Freedom"

// The canonical place list the seeder reads
// (connect/GEOMAP.md §4.1). server/cli/geolite2export writes it into the dated
// directory of the GeoLite2 database it is exported from, and the deploy
// flattens that directory as it does for the database.
const placesResource = "mmdb/places.yml"

// called from db_migrations to add default locations and groups
//
// Countries, regions and cities come from the canonical place list and are
// matched, created and refreshed by geoname id (connect/GEOMAP.md §4.2): a row
// seeded from the old city list, or created by a lookup before geoname ids, is
// matched by name and gets its id filled in, and nothing is deleted. Every
// country is seeded. `cityLimit` caps the cities -- 0 for none, negative for
// every city -- taken in the list's order, so a limited run seeds the same
// cities every time; each city's region is created with it.
func AddDefaultLocations(ctx context.Context, cityLimit int) {
	// The process's list, loaded here unless a use before this loaded it: the
	// seeded rows and every stored row are resolved against it
	// (location_match.go), and every later use in this process shares this
	// one copy. A deployment without one cannot seed, and panics rather than
	// seeding nothing.
	names, err := currentLocationPlaceNamesLoad().get()
	if err != nil {
		panic(err)
	}
	places := names.places

	createCountry := func(country *geo.Country) {
		location := &Location{
			LocationType:     LocationTypeCountry,
			Country:          country.Name,
			CountryCode:      country.Code,
			CountryGeonameId: country.GeonameId,
		}
		if location.Country == "" {
			if _, ok := resolveCountryName(country.Code); !ok {
				// nothing names it, and no row is stored without a name
				glog.Infof("[loc]skip unnamed country %s\n", country.Code)
				return
			}
		}
		seedLocation(ctx, location)
	}

	// exact is the first pass: the city is seeded only when its geoname id or
	// its exact name finds a row, and false is returned otherwise
	createCity := func(place *geo.Place, exact bool) bool {
		location := &Location{
			LocationType:    LocationTypeCity,
			City:            place.City,
			Region:          place.Region,
			CountryCode:     place.CountryCode,
			Latitude:        place.Latitude,
			Longitude:       place.Longitude,
			Timezone:        place.TimeZone,
			CityGeonameId:   place.GeonameId,
			RegionGeonameId: place.RegionGeonameId,
		}
		if country := places.Country(place.CountryCode); country != nil {
			location.Country = country.Name
			location.CountryGeonameId = country.GeonameId
		}
		if exact {
			return seedLocationExactly(ctx, location)
		}
		seedLocation(ctx, location)
		return true
	}

	// a location-group member takes the place list's ids where the list has
	// the place, so it resolves to the seeded row by id
	withPlaceGeonameIds := func(location *Location) *Location {
		if country := places.Country(location.CountryCode); country != nil && location.CountryGeonameId == 0 {
			location.CountryGeonameId = country.GeonameId
		}
		if location.Region != "" && location.RegionGeonameId == 0 {
			location.RegionGeonameId = places.RegionGeonameId(location.CountryCode, location.Region)
		}
		return location
	}

	createLocationGroup := func(promoted bool, name string, members ...any) {
		// member can be a country code, Location, or *Location,

		memberLocationIds := []server.Id{}

		var add func(member any)
		add = func(member any) {
			switch v := member.(type) {
			case []any:
				for _, a := range v {
					add(a)
				}
			case string:
				// country code
				//
				// The name MUST be resolved here. This branch used to build a
				// `&Location{LocationType: LocationTypeCountry, CountryCode: v}`
				// with no `Country` at all, and `CreateLocation` writes
				// `location_name` from `Country` -- so every country reachable
				// only through a location group (i.e. every code below that
				// the deployment's country list did not name) was inserted with
				// an empty name. That single omission produced 161 blank-named
				// country rows on the live beta deployment. The place list names
				// every country GeoLite2 knows, and the ISO table the rest.
				countryCode := strings.ToLower(v)
				location := &Location{
					LocationType: LocationTypeCountry,
					CountryCode:  countryCode,
				}
				if country := places.Country(countryCode); country != nil && country.Name != "" {
					location.Country = country.Name
					location.CountryGeonameId = country.GeonameId
				} else if country, ok := resolveCountryName(countryCode); ok {
					location.Country = country
				} else {
					// deliberately fatal: the member lists below are hardcoded
					// country codes, so an unresolvable one is a typo in this
					// file, not a data condition to tolerate.
					panic(fmt.Errorf(
						"location group \"%s\" member \"%s\" is not a known country code",
						name,
						v,
					))
				}
				CreateLocation(ctx, location)
				memberLocationIds = append(memberLocationIds, location.LocationId)
			case Location:
				CreateLocation(ctx, withPlaceGeonameIds(&v))
				memberLocationIds = append(memberLocationIds, v.LocationId)
			case *Location:
				CreateLocation(ctx, withPlaceGeonameIds(v))
				memberLocationIds = append(memberLocationIds, v.LocationId)
			}
		}
		for _, member := range members {
			add(member)
		}

		locationGroup := &LocationGroup{
			Name:              name,
			Promoted:          promoted,
			MemberLocationIds: memberLocationIds,
		}
		CreateLocationGroup(ctx, locationGroup)
	}

	func() {
		// countries
		countries := places.Countries()
		for i, country := range countries {
			glog.Infof("[loc][%d/%d] %s, %s\n", i+1, len(countries), country.Code, country.Name)
			createCountry(country)
		}
	}()

	func() {
		// cities, in two passes: first every city that its geoname id or a
		// stored row anchored to it finds, then the rest, which adopt a row
		// that resolves to them loosely or are created. Resolution already
		// keeps one city from taking another's row; the order keeps every
		// exact match settled before any loose one is considered.
		cityCount := places.CityCount()
		if 0 <= cityLimit {
			cityCount = min(cityCount, cityLimit)
		}
		unmatched := []*geo.Place{}
		cityIndex := 0
		for place := range places.Cities() {
			if cityCount <= cityIndex {
				break
			}
			cityIndex += 1
			glog.Infof("[loc][%d/%d] %s, %s, %s\n", cityIndex, cityCount, place.CountryCode, place.Region, place.City)
			if !createCity(place, true) {
				unmatched = append(unmatched, place)
			}
		}
		for i, place := range unmatched {
			glog.Infof("[loc][%d/%d unmatched] %s, %s, %s\n", i+1, len(unmatched), place.CountryCode, place.Region, place.City)
			createCity(place, false)
		}
	}()

	// values can be country code or *Location
	eu := []any{
		"at",
		"be",
		"bg",
		"hr",
		"cy",
		"cz",
		"dk",
		"ee",
		"fi",
		"fr",
		"de",
		"gr",
		"hu",
		"ie",
		"it",
		"lv",
		"lt",
		"lu",
		"mt",
		"nl",
		"pl",
		"pt",
		"ro",
		"sk",
		"si",
		"es",
		"se",
	}
	nordic := []any{
		"dk",
		"fi",
		"is",
		"no",
		"se",
	}

	customRegions := map[string][]any{
		// https://www.gov.uk/eu-eea
		"European Union (EU)": eu,
		"Nordic":              nordic,
		StrongPrivacyLaws: []any{
			// all EU/EEA countries adhere to the General Data Protection Regulation (GDPR), which establishes a globally recognized high standard for individual privacy and data rights
			// Strong national framework complementing GDPR, good overall human rights ranking.
			"at",
			// Adheres to GDPR, central location for many EU institutions with strong historical focus on privacy.
			"be",
			// High ranking in civil liberties and democratic institutions.
			"cz",
			// Consistently ranked globally for strong democracy, civil liberties, and robust privacy culture (Nordic model/GDPR).
			"dk",
			// Highly digitized state with a strong emphasis on digital freedom, transparency, and data protection via the GDPR.
			"ee",
			// Consistently ranked globally for human rights, democracy, and strong national privacy laws (Data Protection Act) supplementing GDPR.
			"fi",
			// Strong independent supervisory authority (CNIL) and firm commitment to the GDPR framework.
			"fr",
			// The strongest national legal tradition of data protection (Informational Self-Determination) which led to the GDPR; Bundesdatenschutzgesetz (BDSG) reinforces privacy protections.
			"de",
			// As the main establishment for many global tech companies, the Data Protection Commission (DPC) has a central role in GDPR enforcement.
			"ie",
			// Part of the EEA, adhering to GDPR, and consistently ranked top globally for human rights and press freedom.
			"is",
			// Strong privacy law tradition and active national data protection authority (Garante).
			"it",
			// High ranking in political rights and civil liberties, strong adherence to GDPR.
			"lt",
			// Active role in EU policy and GDPR compliance, high economic stability.
			"lu",
			// High ranking in civil liberties and democracy, with an independent and active data protection authority.
			"nl",
			// Part of the EEA, adhering to GDPR, and consistently ranked top globally for human rights and rule of law.
			"no",
			// High ranking in civil liberties and strong adherence to GDPR.
			"pt",
			// Highly active in GDPR enforcement (highest number of fines in EU), strong focus on citizen data protection.
			"es",
			// Consistently ranked globally for human rights, transparency, and robust privacy culture (Nordic model/GDPR).
			"se",

			// these countries also rank high in privacy, human rights, and internet freedom
			"ch",
			"jp",
			"ca",
			"kr",
			"nz",
			"ar",
			"br",
			"sg",

			// Within the US, since there is no national privacy law, strong privacy is at the state level
			// https://www.ncsl.org/technology-and-communication/state-laws-related-to-digital-privacy
			// https://pro.bloomberglaw.com/insights/privacy/state-privacy-legislation-tracker
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "California",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Colorado",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Connecticut",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Delaware",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Maryland",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Minnesota",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Oregon",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Virginia",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Texas",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "New Hampshire",
				Country:      "United States",
				CountryCode:  "us",
			},
			&Location{
				LocationType: LocationTypeRegion,
				Region:       "Montana",
				Country:      "United States",
				CountryCode:  "us",
			},
		},
	}
	for name, members := range customRegions {
		// server.Logger().Printf("Create promoted group %s\n", name)
		createLocationGroup(false, name, members...)
	}

	// subregions
	// https://en.wikipedia.org/wiki/Subregion
	unSubregions := map[string][]any{
		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_Africa
		"Northern Africa": []any{
			"dz",
			"eg",
			"ly",
			"ma",
			"sd",
			"tn",
			"eh",
		},
		"Eastern Africa": []any{
			"io",
			"bi",
			"km",
			"dj",
			"er",
			"et",
			"tf",
			"ke",
			"mg",
			"mw",
			"mu",
			"yt",
			"mz",
			"re",
			"rw",
			"sc",
			"so",
			"ss",
			"ug",
			"tz",
			"zw",
		},
		"Central Africa": []any{
			"ao",
			"cm",
			"cf",
			"td",
			"cg",
			"cd",
			"gq",
			"ga",
			"st",
		},
		"Southern Africa": []any{
			"bw",
			"sz",
			"ls",
			"na",
			"za",
		},
		"Western Africa": []any{
			"bj",
			"bf",
			"cv",
			"ci",
			"gm",
			"gh",
			"gn",
			"gw",
			"lr",
			"ml",
			"mr",
			"ne",
			"ng",
			"sh",
			"sn",
			"sl",
			"tg",
		},

		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_Asia
		"Central Asia": []any{
			"kz",
			"kg",
			"tj",
			"tm",
			"uz",
		},
		"Eastern Asia": []any{
			"cn",
			"hk",
			"mo",
			"kp",
			"jp",
			"mn",
			"kr",
		},
		"Southeastern Asia": []any{
			"bn",
			"kh",
			"id",
			"la",
			"my",
			"mm",
			"ph",
			"sg",
			"th",
			"tl",
			"vn",
		},
		"Southern Asia": []any{
			"af",
			"bd",
			"bt",
			"in",
			"ir",
			"mv",
			"np",
			"pk",
			"lk",
		},
		"Western Asia": []any{
			"am",
			"az",
			"bh",
			"cy",
			"ge",
			"iq",
			"il",
			"jo",
			"kw",
			"lb",
			"om",
			"qa",
			"sa",
			"ps",
			"sy",
			"tr",
			"ae",
			"ye",
		},

		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_Europe
		"Eastern Europe": []any{
			"by",
			"bg",
			"cz",
			"hu",
			"pl",
			"md",
			"ro",
			"ru",
			"sk",
			"ua",
		},
		"Northern Europe": []any{
			"ax",
			"dk",
			"ee",
			"fo",
			"fi",
			"is",
			"ie",
			"im",
			"lv",
			"lt",
			"no",
			"sj",
			"se",
			"gb",
		},
		"Southern Europe": []any{
			"al",
			"ad",
			"ba",
			"hr",
			"gi",
			"gr",
			"va",
			"it",
			"mt",
			"me",
			"mk",
			"pt",
			"sm",
			"rs",
			"si",
			"es",
		},
		"Western Europe": []any{
			"at",
			"be",
			"fr",
			"de",
			"li",
			"lu",
			"mc",
			"nl",
			"ch",
		},

		// https://en.wikipedia.org/wiki/United_Nations_geoscheme_for_the_Americas
		"Caribbean": []any{
			"ai",
			"ag",
			"aw",
			"bs",
			"bb",
			"bq",
			"vg",
			"ky",
			"cu",
			"cw",
			"dm",
			"do",
			"gd",
			"gp",
			"ht",
			"jm",
			"mq",
			"ms",
			"pr",
			"bl",
			"kn",
			"lc",
			"mf",
			"vc",
			"sx",
			"tt",
			"tc",
			"vi",
		},
		"Central America": []any{
			"bz",
			"cr",
			"sv",
			"gt",
			"hn",
			"mx",
			"ni",
			"pa",
		},
		"South America": []any{
			"ar",
			"bo",
			"bv",
			"br",
			"cl",
			"co",
			"ec",
			"fk",
			"gf",
			"gy",
			"py",
			"pe",
			"gs",
			"sr",
			"uy",
			"ve",
		},
		"Northern America": []any{
			"bm",
			"ca",
			"gl",
			"pm",
			"us",
		},

		"Antarctica": []any{
			"aq",
		},
	}
	for name, members := range unSubregions {
		// server.Logger().Printf("Create group %s\n", name)
		createLocationGroup(false, name, members...)
	}

	// merge the rows that resolve to one place, the seeded ones included
	// (connect/GEOMAP.md §4.2); a refusal leaves the table as it is and the
	// seeding done
	if _, err := deduplicateLocations(ctx, names); err != nil {
		glog.Errorf("[loc]location de-duplication refused: %s\n", err)
	}
}

type LocationType = string

const (
	LocationTypeCity    LocationType = "city"
	LocationTypeRegion  LocationType = "region"
	LocationTypeCountry LocationType = "country"
)

type Location struct {
	LocationType      LocationType
	City              string
	Region            string
	Country           string
	CountryCode       string
	Continent         string
	ContinentCode     string
	LocationId        server.Id
	CityLocationId    server.Id
	RegionLocationId  server.Id
	CountryLocationId server.Id
	Latitude          float64
	Longitude         float64
	Timezone          string
	// The GeoNames ids GeoLite2 carries for the city, region and country
	// (connect/GEOMAP.md §4), the canonical key a place is matched on. Zero
	// when the location did not come from the database.
	CityGeonameId    uint32
	RegionGeonameId  uint32
	CountryGeonameId uint32
}

func (self *Location) GuessLocationType() (LocationType, error) {
	if self.City != "" {
		return LocationTypeCity, nil
	}
	if self.Region != "" {
		return LocationTypeRegion, nil
	}
	if self.CountryCode != "" {
		return LocationTypeCountry, nil
	}
	return "", fmt.Errorf("Unknown location type.")
}

func (self *Location) SearchStrings() []string {
	switch self.LocationType {
	case LocationTypeCity:
		return []string{
			fmt.Sprintf("%s, %s", self.City, self.Country),
			fmt.Sprintf("%s (%s)", self.City, self.CountryCode),
			fmt.Sprintf("%s, %s", self.City, self.Region),
		}
	case LocationTypeRegion:
		return []string{
			fmt.Sprintf("%s, %s", self.Region, self.Country),
			fmt.Sprintf("%s (%s)", self.Region, self.CountryCode),
		}
	default:
		return []string{
			fmt.Sprintf("%s (%s)", self.Country, self.CountryCode),
			fmt.Sprintf("%s", self.CountryCode),
		}
	}
}

func (self *Location) CountryLocation() (*Location, error) {
	return &Location{
		LocationType:      LocationTypeCountry,
		Country:           self.Country,
		CountryCode:       self.CountryCode,
		LocationId:        self.CountryLocationId,
		CountryLocationId: self.CountryLocationId,
	}, nil
}

func (self *Location) RegionLocation() (*Location, error) {
	switch self.LocationType {
	case LocationTypeCity, LocationTypeRegion:
		return &Location{
			LocationType:      LocationTypeRegion,
			Region:            self.Region,
			Country:           self.Country,
			CountryCode:       self.CountryCode,
			LocationId:        self.RegionLocationId,
			RegionLocationId:  self.RegionLocationId,
			CountryLocationId: self.CountryLocationId,
		}, nil
	default:
		return nil, fmt.Errorf("Cannot get region from %s.", self.LocationType)
	}
}

func (self *Location) CityLocation() (*Location, error) {
	switch self.LocationType {
	case LocationTypeCity:
		return &Location{
			LocationType:      LocationTypeCity,
			City:              self.City,
			Region:            self.Region,
			Country:           self.Country,
			CountryCode:       self.CountryCode,
			LocationId:        self.CityLocationId,
			CityLocationId:    self.CityLocationId,
			RegionLocationId:  self.RegionLocationId,
			CountryLocationId: self.CountryLocationId,
		}, nil
	default:
		return nil, fmt.Errorf("Cannot get city from %s.", self.LocationType)
	}
}

// resolveCountryName resolves the display name of an ISO-3166-1 alpha-2
// country code from the built-in ISO table.
//
// A deployment's names come from the canonical place list instead
// (connect/GEOMAP.md §4): AddDefaultLocations seeds every country GeoLite2
// names, under GeoLite2's name, and a country row is matched on its code, so a
// code-only location resolves to the seeded row and keeps that name (see
// countryLocationInTx). This table only names a country row that does not
// exist yet -- a location-group member on a database the seeder has not run
// on, or a location created before it.
//
// ok is false when the code is not in the table. Callers must NOT substitute
// the code for the name -- a location row named "cn" is the same bug wearing
// a different hat, just quieter.
func resolveCountryName(countryCode string) (string, bool) {
	code := strings.ToLower(countryCode)
	if len(code) != 2 {
		return "", false
	}
	return ISOCountryName(code)
}

// Finds or creates the rows of a location -- its country, its region and its
// city, as far as its type goes -- and replaces the location in place with the
// resolved one, whose ids, names and geoname ids are the stored rows'.
//
// A place from GeoLite2 carries GeoNames ids (connect/GEOMAP.md §4.2), which
// are matched before any name: a city already stored under its id resolves in
// one indexed read, whatever its row is named. A stored row that predates the
// ids is adopted, and gets the id filled in, when it resolves to the place by
// the rule of location_match.go -- its name anchors to the place in the
// canonical place list, or is within reach of that place alone -- or, when no
// place list is loaded, when its name is exactly the lookup's. A new row stores
// the id. A row that carries a different id is a different place, even under
// the same name; the newcomer's row takes the " (geoname <id>)" suffix the
// place list uses to tell such places apart. Rows are never deleted or moved.
func CreateLocation(ctx context.Context, location *Location) {
	createLocation(ctx, location, createLocationOptions{})
}

// CreateLocation for a place of the canonical place list (AddDefaultLocations).
// A row keyed by the place's geoname id -- matched on it, or given it here --
// also takes the list's name and coordinates. A lookup never overwrites those:
// it carries one network's coordinate, where the list carries the one most of
// the city's networks share.
func seedLocation(ctx context.Context, location *Location) {
	createLocation(ctx, location, createLocationOptions{seed: true})
}

// The seeder's first pass: seedLocation for a city that its geoname id finds,
// or a stored row that anchors to it (location_match.go), and false, with no
// city row adopted loosely and none created, for one that neither finds. The
// seeder takes the rest only after every exact match is settled.
func seedLocationExactly(ctx context.Context, location *Location) bool {
	return createLocation(ctx, location, createLocationOptions{seed: true, exactCity: true})
}

// How createLocation resolves a location. The zero value is a lookup's
// (CreateLocation); the seeder sets seed, and its first pass exactCity too.
type createLocationOptions struct {
	// refresh a row keyed by the place's geoname id from the place list
	seed bool
	// match a city by geoname id or exact name only, and create none
	exactCity bool
}

// Resolves the location in place and reports whether it did; only an exactCity
// city that nothing exactly matches is left unresolved.
func createLocation(ctx context.Context, location *Location, options createLocationOptions) bool {
	var countryCode string
	if location.CountryCode != "" {
		countryCode = strings.ToLower(location.CountryCode)
	} else {
		// use the country name
		countryCode = strings.ToLower(string([]rune(location.Country)))
	}
	if 2 < len(countryCode) {
		countryCode = countryCode[0:2]
	}

	// No location row may be inserted with an empty `location_name`. Each
	// insert below writes the name straight from the field the row is named
	// after -- `Country`, `Region`, `City` -- so an empty field is an empty
	// name, and the `location_full_name` built from it comes out shaped like
	// ", hk". Both were observed on the live beta deployment.
	//
	// A country with no name is resolved from its code. Everything else
	// resolves to its nearest NAMED ancestor rather than erroring, because the
	// unnamed-region case is reached on the connect-announce hot path: mmdb
	// returns no subdivision for the subdivision-less countries (hk, sg, mc,
	// va, ...), `GuessLocationType` still classifies those as a city because
	// the city is set, and refusing the whole location there would turn a
	// cosmetically-bad row into a failed connection for every client in those
	// countries. Degrading loses city precision for them; it does not lose the
	// connection, and it never writes a blank name.
	switch location.LocationType {
	case LocationTypeCountry, LocationTypeRegion, LocationTypeCity:
	default:
		// the inserts below are selected by `LocationType`, and an unrecognized
		// one (including the zero value, from a `Location` built without the
		// field) falls through every early return and lands on the city insert
		// with whatever the caller left empty. That is a blank name by another
		// route, so it is refused here rather than resolved.
		glog.Errorf(
			"[loc]refusing to create a location: \"%s\" is not a known location type.\n",
			location.LocationType,
		)
		server.Raise(fmt.Errorf("Unknown location type \"%s\".", location.LocationType))
	}
	if location.Country == "" {
		if country, ok := resolveCountryName(countryCode); ok {
			location.Country = country
		} else if len(countryCode) != 2 {
			// not a country code at all. There is no ancestor to fall back to,
			// and inventing a name (or reusing the code as one) is what made
			// this class of bug invisible in the first place.
			refuseUnknownCountryCode(countryCode)
		}
		// Otherwise a code the ISO table does not name (GeoLite2's `xk`). The
		// stored country row names it when there is one, and the transaction
		// refuses it when there is not (countryLocationInTx).
	}
	if location.LocationType == LocationTypeCity &&
		location.City != "" &&
		location.Region == "" &&
		location.CityGeonameId != 0 &&
		location.Country != "" {
		// GeoLite2 files some cities under no subdivision (hk, sg, pr, mc,
		// ...). The place list keeps them under a region named for the
		// country, the convention the blank-region backfill set
		// (db_migrations.go), so a lookup of one resolves to the city the
		// seeder stored instead of degrading to the country. That region is not
		// a subdivision and takes no subdivision's id. A city without a geoname
		// id did not come from GeoLite2, and degrades below as before.
		location.Region = location.Country
		location.RegionGeonameId = 0
	}
	if location.LocationType == LocationTypeCity && location.City == "" {
		glog.Infof(
			"[loc]unnamed city in \"%s\"; resolving at region granularity.\n",
			countryCode,
		)
		location.LocationType = LocationTypeRegion
	}
	if location.LocationType != LocationTypeCountry && location.Region == "" {
		// a city row is keyed under a region row, so a city whose region has no
		// name cannot be created either
		glog.Infof(
			"[loc]unnamed region in \"%s\"; resolving at country granularity.\n",
			countryCode,
		)
		location.LocationType = LocationTypeCountry
	}

	// The transaction reads only this copy and publishes into `resolved`, so
	// an attempt that server.Tx re-runs starts from the caller's location
	// rather than from what a failed attempt resolved it to.
	input := *location
	// A located lookup resolves stored rows against the place list, which is
	// read once per process -- before the transaction, so the first read never
	// holds a database connection open -- and only for a lookup whose place is
	// not yet stored under its geoname id, the one case the transaction
	// consults it for (regionLocationInTx, cityLocationInTx). So a process
	// whose lookups all find their place by id never reads the list, whose
	// parse keeps ~300 MB resident (location_place_names.go). A place removed
	// between the check and the transaction is matched as without a list, by
	// geoname id and exact name.
	var names *locationPlaceNames
	if input.CityGeonameId != 0 || input.RegionGeonameId != 0 {
		// the boolean an EXISTS query answers
		exists := func(sql string, args ...any) bool {
			exists := false
			server.Db(ctx, func(conn server.PgConn) {
				result, err := conn.Query(ctx, sql, args...)
				server.WithPgResult(result, err, func() {
					if result.Next() {
						server.Raise(result.Scan(&exists))
					}
				})
			})
			return exists
		}
		// Whether the transaction resolves the place by its geoname id alone:
		// a country, a city stored under its id with its region and country
		// rows (cityLocationByGeonameIdInTx, which a seed skips), or a region
		// stored under its id beneath the country row the transaction takes
		// (countryLocationInTx, regionLocationInTx).
		storedByGeonameId := func() bool {
			switch {
			case input.LocationType == LocationTypeCountry:
				return true
			case input.LocationType == LocationTypeCity && input.CityGeonameId != 0 && !options.seed:
				return exists(
					`
						SELECT EXISTS (
							SELECT 1
							FROM location AS city
							INNER JOIN location AS region ON region.location_id = city.region_location_id
							INNER JOIN location AS country ON country.location_id = city.country_location_id
							WHERE
								city.geoname_id = $1 AND
								city.location_type = $2
						)
					`,
					int64(input.CityGeonameId),
					LocationTypeCity,
				)
			case input.LocationType == LocationTypeRegion && input.RegionGeonameId != 0:
				return exists(
					`
						SELECT EXISTS (
							SELECT 1
							FROM location AS region
							WHERE
								region.location_type = $1 AND
								region.geoname_id = $2 AND
								region.country_location_id = (
									SELECT location_id
									FROM location
									WHERE
										location_type = $3 AND
										(
											country_code = $4 OR
											geoname_id = $5
										)
									ORDER BY
										COALESCE(geoname_id = $5, false) DESC,
										location_id
									LIMIT 1
								)
						)
					`,
					LocationTypeRegion,
					int64(input.RegionGeonameId),
					LocationTypeCountry,
					countryCode,
					geonameIdArg(input.CountryGeonameId),
				)
			default:
				return false
			}
		}
		load := currentLocationPlaceNamesLoad()
		if placeNames, loaded := load.peek(); loaded {
			names = placeNames
		} else if !storedByGeonameId() {
			names, _ = load.get()
		}
	}
	var resolved *Location
	server.Tx(ctx, func(tx server.PgTx) {
		resolved = nil

		if !options.seed && input.LocationType == LocationTypeCity && input.CityGeonameId != 0 {
			if cityLocation := cityLocationByGeonameIdInTx(ctx, tx, &input); cityLocation != nil {
				resolved = cityLocation
				return
			}
		}

		countryLocation := countryLocationInTx(ctx, tx, &input, countryCode, options.seed)
		if input.LocationType == LocationTypeCountry {
			resolved = countryLocation
			return
		}

		regionLocation := regionLocationInTx(ctx, tx, &input, countryCode, countryLocation, names, options.seed)
		if input.LocationType == LocationTypeRegion {
			resolved = regionLocation
			return
		}

		resolved = cityLocationInTx(ctx, tx, &input, countryCode, countryLocation, regionLocation, names, options)
	})
	if resolved == nil {
		return false
	}
	*location = *resolved
	return true
}

// Logs and raises (panics) for a country code with no known country name,
// rather than create a location row without one.
func refuseUnknownCountryCode(countryCode string) {
	glog.Errorf(
		"[loc]refusing to create a location: \"%s\" is not a known country code.\n",
		countryCode,
	)
	server.Raise(fmt.Errorf("Unknown country code \"%s\".", countryCode))
}

// A geoname id as a query argument: NULL for 0, which means the location did
// not come from GeoLite2.
func geonameIdArg(geonameId uint32) *int64 {
	if geonameId == 0 {
		return nil
	}
	arg := int64(geonameId)
	return &arg
}

// Reads a stored geoname id: 0 for NULL or a value out of range.
func geonameIdValue(geonameId *int64) uint32 {
	if geonameId == nil || *geonameId <= 0 || math.MaxUint32 < *geonameId {
		return 0
	}
	return uint32(*geonameId)
}

// Gives a row that predates geoname ids its id, and reports whether the row
// now holds it. Another row already holding the id (a place GeoNames moved, or
// bad data) leaves this row as it was rather than failing the transaction on
// the unique index -- which server.Tx would retry until its deadline, since a
// constraint violation reads as transient. A holder committed concurrently,
// after this transaction's snapshot, is the one case that does meet the index,
// and its retry sees the holder.
func backfillGeonameIdInTx(ctx context.Context, tx server.PgTx, locationId server.Id, geonameId uint32) bool {
	tag, err := tx.Exec(
		ctx,
		`
			UPDATE location
			SET geoname_id = $2
			WHERE
				location_id = $1 AND
				geoname_id IS NULL AND
				NOT EXISTS (
					SELECT 1
					FROM location AS holder
					WHERE holder.geoname_id = $2
				)
		`,
		locationId,
		int64(geonameId),
	)
	server.Raise(err)
	return tag.RowsAffected() == 1
}

// The geoname id a new row stores: NULL when the place has none, or when a row
// the lookups did not match already holds it -- another kind of place, or a
// region under another country row (bad data, which the new row survives the
// way a legacy row does, found by name). server.Tx runs at repeatable read, so
// the lookups and this check see one snapshot: a row of this very place that a
// concurrent transaction commits after it is invisible here, and meets this
// row on the unique index instead, which server.Tx retries with a fresh
// snapshot that finds it.
func newRowGeonameIdInTx(ctx context.Context, tx server.PgTx, geonameId uint32) *int64 {
	if geonameId == 0 {
		return nil
	}
	held := false
	result, err := tx.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM location WHERE geoname_id = $1)`,
		int64(geonameId),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&held))
		}
	})
	if held {
		return nil
	}
	return geonameIdArg(geonameId)
}

// Whether any location row already holds the full name (`location_full_name`
// is unique).
func locationFullNameTakenInTx(ctx context.Context, tx server.PgTx, fullName string) bool {
	taken := false
	result, err := tx.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM location WHERE location_full_name = $1)`,
		fullName,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&taken))
		}
	})
	return taken
}

// Picks the name and full name of a new region or city row.
// `location_full_name` is unique, and a place with a geoname id whose plain
// full name is already stored is a different place of the same name (a row of
// the same id, or a legacy row of the name, would have matched first), so it
// takes the suffix the place list gives the later of two such places. A place
// without an id keeps the plain name, as before.
func newRowNameInTx(
	ctx context.Context,
	tx server.PgTx,
	name string,
	geonameId uint32,
	fullName func(name string) string,
) (string, string) {
	if geonameId == 0 || !locationFullNameTakenInTx(ctx, tx, fullName(name)) {
		return name, fullName(name)
	}
	suffixedName := geo.GeonameSuffixedName(name, geonameId)
	if locationFullNameTakenInTx(ctx, tx, fullName(suffixedName)) {
		// a row neither of this id nor of this name holds it; refuse plainly
		// rather than retry against the unique index until the deadline
		server.Raise(fmt.Errorf("Location \"%s\" is taken by another place.", fullName(suffixedName)))
	}
	return suffixedName, fullName(suffixedName)
}

// Gives a seeded row keyed by its geoname id the place list's name, and its
// search strings with it, and reports whether it did. It leaves the row as it
// is when another row already holds the full name the new name composes: that
// is a row of the same place from before geoname ids, and both stay
// resolvable.
func renameLocationInTx(
	ctx context.Context,
	tx server.PgTx,
	locationId server.Id,
	name string,
	fullName string,
	searchLocation *Location,
) bool {
	tag, err := tx.Exec(
		ctx,
		`
			UPDATE location
			SET
				location_name = $2,
				location_full_name = $3
			WHERE
				location_id = $1 AND
				(location_name <> $2 OR location_full_name <> $3) AND
				NOT EXISTS (
					SELECT 1
					FROM location AS holder
					WHERE
						holder.location_full_name = $3 AND
						holder.location_id <> $1
				)
		`,
		locationId,
		name,
		fullName,
	)
	server.Raise(err)
	if tag.RowsAffected() == 0 {
		return false
	}
	locationSearch().RemoveInTx(ctx, locationId, tx)
	for i, searchStr := range searchLocation.SearchStrings() {
		locationSearch().AddInTx(ctx, searchStr, locationId, i, tx)
	}
	return true
}

// Finds or creates the country row. A country code names one country, so a
// row is matched on the country's geoname id or its code, the id first; a row
// matched on its code alone gets the id filled in. The stored name is the
// name: the seeder writes GeoLite2's name, and a lookup that names the country
// another way does not rename it.
func countryLocationInTx(
	ctx context.Context,
	tx server.PgTx,
	location *Location,
	countryCode string,
	seed bool,
) *Location {
	var countryLocation *Location
	result, err := tx.Query(
		ctx,
		`
			SELECT
				location_id,
				location_name,
				country_code,
				geoname_id
			FROM location
			WHERE
				location_type = $1 AND
				(
					country_code = $2 OR
					geoname_id = $3
				)
			ORDER BY
				COALESCE(geoname_id = $3, false) DESC,
				location_id
			LIMIT 1
		`,
		LocationTypeCountry,
		countryCode,
		geonameIdArg(location.CountryGeonameId),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			var locationId server.Id
			var name string
			var storedCountryCode string
			var geonameId *int64
			server.Raise(result.Scan(
				&locationId,
				&name,
				&storedCountryCode,
				&geonameId,
			))
			countryLocation = &Location{
				LocationType:      LocationTypeCountry,
				Country:           name,
				CountryCode:       strings.ToLower(storedCountryCode),
				LocationId:        locationId,
				CountryLocationId: locationId,
				CountryGeonameId:  geonameIdValue(geonameId),
			}
		}
	})

	if countryLocation != nil {
		keyed := countryLocation.CountryGeonameId != 0 && countryLocation.CountryGeonameId == location.CountryGeonameId
		if countryLocation.CountryGeonameId == 0 && location.CountryGeonameId != 0 {
			if backfillGeonameIdInTx(ctx, tx, countryLocation.LocationId, location.CountryGeonameId) {
				countryLocation.CountryGeonameId = location.CountryGeonameId
				keyed = true
			}
		}
		if seed && keyed && location.Country != "" && location.Country != countryLocation.Country {
			renamed := &Location{
				LocationType: LocationTypeCountry,
				Country:      location.Country,
				CountryCode:  countryLocation.CountryCode,
			}
			// a country row's full name is its code
			if renameLocationInTx(ctx, tx, countryLocation.LocationId, location.Country, countryLocation.CountryCode, renamed) {
				countryLocation.Country = location.Country
			}
		}
		return countryLocation
	}

	if location.Country == "" {
		refuseUnknownCountryCode(countryCode)
	}

	locationId := server.NewId()
	geonameId := newRowGeonameIdInTx(ctx, tx, location.CountryGeonameId)
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO location (
				location_id,
				location_type,
				location_name,
				country_location_id,
				country_code,
				location_full_name,
				geoname_id
			)
			VALUES ($1, $2, $3, $1, $4, $5, $6)
		`,
		locationId,
		LocationTypeCountry,
		location.Country,
		countryCode,
		countryCode,
		geonameId,
	))

	countryLocation = &Location{
		LocationType:      LocationTypeCountry,
		Country:           location.Country,
		CountryCode:       countryCode,
		LocationId:        locationId,
		CountryLocationId: locationId,
		CountryGeonameId:  geonameIdValue(geonameId),
	}

	// add to the search
	for i, searchStr := range countryLocation.SearchStrings() {
		locationSearch().AddInTx(ctx, searchStr, locationId, i, tx)
	}
	return countryLocation
}

// Finds or creates the region row under the country row: by the subdivision's
// geoname id first, then by name. A row of the same name that carries another
// subdivision's id is a different region.
func regionLocationInTx(
	ctx context.Context,
	tx server.PgTx,
	location *Location,
	countryCode string,
	countryLocation *Location,
	names *locationPlaceNames,
	seed bool,
) *Location {
	var regionLocation *Location
	scanRegion := func(result server.PgResult) {
		if result.Next() {
			var locationId server.Id
			var name string
			var geonameId *int64
			server.Raise(result.Scan(
				&locationId,
				&name,
				&geonameId,
			))
			regionLocation = &Location{
				LocationType:      LocationTypeRegion,
				Region:            name,
				Country:           countryLocation.Country,
				CountryCode:       countryCode,
				LocationId:        locationId,
				RegionLocationId:  locationId,
				CountryLocationId: countryLocation.LocationId,
				RegionGeonameId:   geonameIdValue(geonameId),
				CountryGeonameId:  countryLocation.CountryGeonameId,
			}
		}
	}

	if location.RegionGeonameId != 0 {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					location_id,
					location_name,
					geoname_id
				FROM location
				WHERE
					location_type = $1 AND
					geoname_id = $2 AND
					country_location_id = $3
			`,
			LocationTypeRegion,
			int64(location.RegionGeonameId),
			countryLocation.LocationId,
		)
		server.WithPgResult(result, err, func() {
			scanRegion(result)
		})
	}
	matchedByGeonameId := regionLocation != nil

	// The list's region this lookup names, when the place list is loaded and
	// knows it: a stored row without an id is adopted only when it resolves to
	// that region (location_match.go), which an exact name alone does not show.
	var listRegion *geo.RegionNames
	if regionLocation == nil && names != nil {
		listRegion = names.lookupRegion(countryCode, location)
	}
	if regionLocation == nil && listRegion != nil {
		regionLocation = adoptRegionInTx(ctx, tx, names, listRegion, countryCode, countryLocation)
	}

	if regionLocation == nil && listRegion == nil {
		// A blank-name backfill can leave a legacy row alongside the canonical
		// region when both full names would otherwise collide. Prefer the row
		// that owns the normally-composed full name; the id tie-breaker keeps
		// selection deterministic when only legacy rows exist.
		result, err := tx.Query(
			ctx,
			`
				SELECT
					location_id,
					location_name,
					geoname_id
				FROM location
				WHERE
					location_type = $1 AND
					country_code = $2 AND
					location_name = $3 AND
					country_location_id = $4 AND
					(
						geoname_id IS NULL OR
						$6::bigint IS NULL
					)
				ORDER BY
					(location_full_name = $5) DESC,
					location_id
				LIMIT 1
			`,
			LocationTypeRegion,
			countryCode,
			location.Region,
			countryLocation.LocationId,
			fmt.Sprintf("%s, %s", location.Region, countryCode),
			geonameIdArg(location.RegionGeonameId),
		)
		server.WithPgResult(result, err, func() {
			scanRegion(result)
		})
	}

	if regionLocation != nil {
		keyed := matchedByGeonameId
		if !matchedByGeonameId && regionLocation.RegionGeonameId == 0 && location.RegionGeonameId != 0 {
			if backfillGeonameIdInTx(ctx, tx, regionLocation.LocationId, location.RegionGeonameId) {
				regionLocation.RegionGeonameId = location.RegionGeonameId
				keyed = true
			}
		}
		if seed && keyed && location.Region != regionLocation.Region {
			renamed := &Location{
				LocationType: LocationTypeRegion,
				Region:       location.Region,
				Country:      countryLocation.Country,
				CountryCode:  countryCode,
			}
			fullName := fmt.Sprintf("%s, %s", location.Region, countryCode)
			if renameLocationInTx(ctx, tx, regionLocation.LocationId, location.Region, fullName, renamed) {
				regionLocation.Region = location.Region
			}
		}
		return regionLocation
	}

	// create a new location
	geonameId := newRowGeonameIdInTx(ctx, tx, location.RegionGeonameId)
	// named by the place's id even when the row cannot store it, so a region
	// of the same name elsewhere in the country never collides with it
	name, fullName := newRowNameInTx(ctx, tx, location.Region, location.RegionGeonameId, func(name string) string {
		return fmt.Sprintf("%s, %s", name, countryCode)
	})
	locationId := server.NewId()
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO location (
				location_id,
				location_type,
				location_name,
				region_location_id,
				country_location_id,
				country_code,
				location_full_name,
				geoname_id
			)
			VALUES ($1, $2, $3, $1, $4, $5, $6, $7)
		`,
		locationId,
		LocationTypeRegion,
		name,
		countryLocation.LocationId,
		countryCode,
		fullName,
		geonameId,
	))

	regionLocation = &Location{
		LocationType:      LocationTypeRegion,
		Region:            name,
		Country:           countryLocation.Country,
		CountryCode:       countryCode,
		LocationId:        locationId,
		RegionLocationId:  locationId,
		CountryLocationId: countryLocation.LocationId,
		RegionGeonameId:   geonameIdValue(geonameId),
		CountryGeonameId:  countryLocation.CountryGeonameId,
	}

	// add to the search
	for i, searchStr := range regionLocation.SearchStrings() {
		locationSearch().AddInTx(ctx, searchStr, locationId, i, tx)
	}
	return regionLocation
}

// The list's region a lookup names, or nil: the region of its subdivision's
// id, or, for a city GeoLite2 files under no subdivision, the region named for
// its country.
func (self *locationPlaceNames) lookupRegion(countryCode string, location *Location) *geo.RegionNames {
	country := self.names.Country(countryCode)
	if country == nil {
		return nil
	}
	if location.RegionGeonameId != 0 {
		return country.RegionByGeonameId(location.RegionGeonameId)
	}
	if region := country.Region(location.Region); region != nil && region.GeonameId == 0 {
		return region
	}
	return nil
}

// A stored row without a geoname id that resolves to the place a lookup names.
type adoptionCandidate struct {
	locationId server.Id
	name       string
	resolution placeResolution
	references int
}

// The stored region row, under the country row and without a geoname id, that
// resolves to the list's region, or nil.
func adoptRegionInTx(
	ctx context.Context,
	tx server.PgTx,
	names *locationPlaceNames,
	listRegion *geo.RegionNames,
	countryCode string,
	countryLocation *Location,
) *Location {
	candidates := []*adoptionCandidate{}
	result, err := tx.Query(
		ctx,
		`
			SELECT
				location_id,
				location_name
			FROM location
			WHERE
				location_type = $1 AND
				country_code = $2 AND
				country_location_id = $3 AND
				geoname_id IS NULL
		`,
		LocationTypeRegion,
		countryCode,
		countryLocation.LocationId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var locationId server.Id
			var name string
			server.Raise(result.Scan(&locationId, &name))
			resolution := names.resolveRegion(countryCode, name)
			if resolution.resolved() && resolution.candidate.region == listRegion {
				candidates = append(candidates, &adoptionCandidate{
					locationId: locationId,
					name:       name,
					resolution: resolution,
				})
			}
		}
	})
	candidate := chooseAdoptionInTx(ctx, tx, LocationTypeRegion, countryCode, candidates)
	if candidate == nil {
		return nil
	}
	glog.Infof("[loc]region \"%s\" in \"%s\" is %s (%s)\n", candidate.name, countryCode, listRegion.Name, candidate.resolution.kind)
	return &Location{
		LocationType:      LocationTypeRegion,
		Region:            candidate.name,
		Country:           countryLocation.Country,
		CountryCode:       countryCode,
		LocationId:        candidate.locationId,
		RegionLocationId:  candidate.locationId,
		CountryLocationId: countryLocation.LocationId,
		CountryGeonameId:  countryLocation.CountryGeonameId,
	}
}

// The stored city row, under the region row and without a geoname id, that
// resolves to the list's place, or nil. The seeder's first pass takes only a
// row that anchors to it.
func adoptCityRowInTx(
	ctx context.Context,
	tx server.PgTx,
	names *locationPlaceNames,
	listPlace *geo.Place,
	countryCode string,
	regionLocation *Location,
	anchoredOnly bool,
) *storedCityRow {
	listRegion := names.cityRegion(listPlace)
	rows := map[server.Id]*storedCityRow{}
	candidates := []*adoptionCandidate{}
	result, err := tx.Query(
		ctx,
		storedCityRowSelect+`
			WHERE
				city.location_type = $1 AND
				city.country_code = $2 AND
				city.region_location_id = $3 AND
				city.geoname_id IS NULL
		`,
		LocationTypeCity,
		countryCode,
		regionLocation.LocationId,
	)
	server.WithPgResult(result, err, func() {
		for {
			row := scanStoredCityRow(result)
			if row == nil {
				return
			}
			resolution := names.resolveCity(countryCode, listRegion, row.name)
			if !resolution.resolved() || resolution.candidate.city.Place.GeonameId != listPlace.GeonameId {
				continue
			}
			if anchoredOnly && resolution.kind != placeAnchored {
				continue
			}
			rows[row.locationId] = row
			candidates = append(candidates, &adoptionCandidate{
				locationId: row.locationId,
				name:       row.name,
				resolution: resolution,
			})
		}
	})
	candidate := chooseAdoptionInTx(ctx, tx, LocationTypeCity, countryCode, candidates)
	if candidate == nil {
		return nil
	}
	glog.Infof("[loc]city \"%s\" in \"%s\", \"%s\" is %s (%s)\n", candidate.name, regionLocation.Region, countryCode, listPlace.City, candidate.resolution.kind)
	return rows[candidate.locationId]
}

// Picks, of several stored rows that resolve to one place, the one a lookup
// adopts, or nil for none: an anchor before a loose match, then the nearer,
// then the more referenced, then the older (location ids are time-ordered).
// Counting every reference would read the connection history tables in full,
// which a lookup on the connect path cannot afford, so a lookup counts only
// what an index answers -- a region's cities, location group memberships and
// network exclusions -- and the de-duplication, which counts everything and
// merges the rest into the row adopted here, settles the others.
func chooseAdoptionInTx(
	ctx context.Context,
	tx server.PgTx,
	locationType LocationType,
	countryCode string,
	candidates []*adoptionCandidate,
) *adoptionCandidate {
	if len(candidates) == 0 {
		return nil
	}
	closer := func(a *adoptionCandidate, b *adoptionCandidate) int {
		if aAnchored, bAnchored := a.resolution.kind == placeAnchored, b.resolution.kind == placeAnchored; aAnchored != bAnchored {
			if aAnchored {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.resolution.distance, b.resolution.distance)
	}
	slices.SortFunc(candidates, closer)
	tied := 1
	for tied < len(candidates) && closer(candidates[0], candidates[tied]) == 0 {
		tied += 1
	}
	candidates = candidates[:tied]
	if 1 < len(candidates) {
		locationIds := make([]server.Id, 0, len(candidates))
		for _, candidate := range candidates {
			locationIds = append(locationIds, candidate.locationId)
		}
		references := map[server.Id]int{}
		count := func(sql string, args ...any) {
			result, err := tx.Query(ctx, sql, args...)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var locationId server.Id
					var n int
					server.Raise(result.Scan(&locationId, &n))
					references[locationId] += n
				}
			})
		}
		if locationType == LocationTypeRegion {
			count(
				`
					SELECT region_location_id, COUNT(*)
					FROM location
					WHERE
						location_type = $1 AND
						country_code = $2 AND
						region_location_id = ANY($3::uuid[])
					GROUP BY region_location_id
				`,
				LocationTypeCity,
				countryCode,
				locationIds,
			)
		}
		count(
			`
				SELECT location_id, COUNT(*)
				FROM location_group_member
				WHERE location_id = ANY($1::uuid[])
				GROUP BY location_id
			`,
			locationIds,
		)
		count(
			`
				SELECT client_location_id, COUNT(*)
				FROM exclude_network_client_location
				WHERE client_location_id = ANY($1::uuid[])
				GROUP BY client_location_id
			`,
			locationIds,
		)
		for _, candidate := range candidates {
			candidate.references = references[candidate.locationId]
		}
	}
	slices.SortFunc(candidates, func(a *adoptionCandidate, b *adoptionCandidate) int {
		if c := cmp.Compare(b.references, a.references); c != 0 {
			return c
		}
		return a.locationId.Cmp(b.locationId)
	})
	return candidates[0]
}

// A city row with the region and country rows it is filed under.
type storedCityRow struct {
	locationId        server.Id
	name              string
	fullName          string
	geonameId         uint32
	latitude          *float64
	longitude         *float64
	countryCode       string
	regionLocationId  server.Id
	regionName        string
	regionGeonameId   uint32
	countryLocationId server.Id
	countryName       string
	countryGeonameId  uint32
}

const storedCityRowSelect = `
	SELECT
		city.location_id,
		city.location_name,
		city.location_full_name,
		city.geoname_id,
		city.latitude,
		city.longitude,
		city.country_code,
		region.location_id,
		region.location_name,
		region.geoname_id,
		country.location_id,
		country.location_name,
		country.geoname_id
	FROM location AS city
	LEFT JOIN location AS region ON region.location_id = city.region_location_id
	LEFT JOIN location AS country ON country.location_id = city.country_location_id
`

// Reads the next row of a storedCityRowSelect, or nil when there is none. A
// parent row that is missing leaves its fields zero.
func scanStoredCityRow(result server.PgResult) *storedCityRow {
	if !result.Next() {
		return nil
	}
	row := &storedCityRow{}
	var geonameId *int64
	var regionLocationId *server.Id
	var regionName *string
	var regionGeonameId *int64
	var countryLocationId *server.Id
	var countryName *string
	var countryGeonameId *int64
	server.Raise(result.Scan(
		&row.locationId,
		&row.name,
		&row.fullName,
		&geonameId,
		&row.latitude,
		&row.longitude,
		&row.countryCode,
		&regionLocationId,
		&regionName,
		&regionGeonameId,
		&countryLocationId,
		&countryName,
		&countryGeonameId,
	))
	row.geonameId = geonameIdValue(geonameId)
	row.countryCode = strings.ToLower(row.countryCode)
	if regionLocationId != nil {
		row.regionLocationId = *regionLocationId
		row.regionName = *regionName
		row.regionGeonameId = geonameIdValue(regionGeonameId)
	}
	if countryLocationId != nil {
		row.countryLocationId = *countryLocationId
		row.countryName = *countryName
		row.countryGeonameId = geonameIdValue(countryGeonameId)
	}
	return row
}

// Whether the region and country rows the city is filed under both exist.
func (self *storedCityRow) complete() bool {
	return self.regionLocationId != (server.Id{}) && self.countryLocationId != (server.Id{})
}

// Fills a missing region or country row with the ones the top-down lookup
// resolved, so the returned hierarchy is never partly zero.
func (self *storedCityRow) withParents(countryLocation *Location, regionLocation *Location) *storedCityRow {
	if self.regionLocationId == (server.Id{}) {
		self.regionLocationId = regionLocation.LocationId
		self.regionName = regionLocation.Region
		self.regionGeonameId = regionLocation.RegionGeonameId
	}
	if self.countryLocationId == (server.Id{}) {
		self.countryLocationId = countryLocation.LocationId
		self.countryName = countryLocation.Country
		self.countryGeonameId = countryLocation.CountryGeonameId
	}
	return self
}

// The row as a city Location, with the ids, names and geoname ids of the region
// and country rows it is filed under. Its coordinates are not carried.
func (self *storedCityRow) location() *Location {
	return &Location{
		LocationType:      LocationTypeCity,
		City:              self.name,
		Region:            self.regionName,
		Country:           self.countryName,
		CountryCode:       self.countryCode,
		LocationId:        self.locationId,
		CityLocationId:    self.locationId,
		RegionLocationId:  self.regionLocationId,
		CountryLocationId: self.countryLocationId,
		CityGeonameId:     self.geonameId,
		RegionGeonameId:   self.regionGeonameId,
		CountryGeonameId:  self.countryGeonameId,
	}
}

// The city row stored under the geoname id, whatever it is filed under, or nil.
func cityRowByGeonameIdInTx(ctx context.Context, tx server.PgTx, geonameId uint32) *storedCityRow {
	var row *storedCityRow
	result, err := tx.Query(
		ctx,
		storedCityRowSelect+`
			WHERE
				city.geoname_id = $1 AND
				city.location_type = $2
		`,
		int64(geonameId),
		LocationTypeCity,
	)
	server.WithPgResult(result, err, func() {
		row = scanStoredCityRow(result)
	})
	return row
}

// the mmdb uses 0,0 for unknown coordinates, and a genuine 0,0 city is
// effectively impossible, so 0,0 is stored as NULL (unknown)
func hasLocationCoordinates(location *Location) bool {
	return location.Latitude != 0 || location.Longitude != 0
}

// Fills the lookup's coordinates into a row stored without any, such as one
// created before coordinates were stored. It never overwrites stored ones.
func healCityCoordinatesInTx(ctx context.Context, tx server.PgTx, row *storedCityRow, location *Location) {
	if row.latitude != nil || !hasLocationCoordinates(location) {
		return
	}
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			UPDATE location
			SET
				latitude = $2,
				longitude = $3
			WHERE
				location_id = $1 AND
				latitude IS NULL
		`,
		row.locationId,
		location.Latitude,
		location.Longitude,
	))
}

// Resolves a city already stored under its geoname id with one indexed read,
// whatever region and country rows it is filed under, or nil. This is the
// steady state of the connect-announce path once a city has been seen: no name
// matching and no parent lookups.
func cityLocationByGeonameIdInTx(ctx context.Context, tx server.PgTx, location *Location) *Location {
	row := cityRowByGeonameIdInTx(ctx, tx, location.CityGeonameId)
	if row == nil || !row.complete() {
		// a city whose parents are gone resolves through the lookups below,
		// which supply them
		return nil
	}
	healCityCoordinatesInTx(ctx, tx, row, location)
	return row.location()
}

// Finds or creates the city row: by geoname id first (anywhere, as the fast
// path above), then by name under the region. A row of the same name that
// carries another city's id is a different city. With exactCity it creates
// none, and returns nil when nothing matches exactly.
func cityLocationInTx(
	ctx context.Context,
	tx server.PgTx,
	location *Location,
	countryCode string,
	countryLocation *Location,
	regionLocation *Location,
	names *locationPlaceNames,
	options createLocationOptions,
) *Location {
	seed := options.seed
	var row *storedCityRow
	if location.CityGeonameId != 0 {
		row = cityRowByGeonameIdInTx(ctx, tx, location.CityGeonameId)
	}
	matchedByGeonameId := row != nil

	// When the place list is loaded and knows the place, a stored row without
	// an id is adopted only when it resolves to it (location_match.go): a row
	// spelled exactly like this lookup may still be another place, as a
	// suffixed twin's lookup shows ("Springfield" is the Springfield of the
	// smaller id).
	var listPlace *geo.Place
	if row == nil && location.CityGeonameId != 0 && names != nil {
		if listPlace = names.places.CityByGeonameId(location.CityGeonameId); listPlace != nil {
			row = adoptCityRowInTx(ctx, tx, names, listPlace, countryCode, regionLocation, options.exactCity)
		}
	}

	if row == nil && listPlace == nil {
		// A non-conflicting legacy city may have had its full name normalized
		// while retaining its legacy region id. The globally-unique full name is
		// therefore a safe fallback when the canonical region lookup above does
		// not find the city under that exact region id. The stored region is
		// read back so the returned hierarchy remains internally consistent.
		result, err := tx.Query(
			ctx,
			storedCityRowSelect+`
				WHERE
					city.location_type = $1 AND
					city.country_code = $2 AND
					city.location_name = $3 AND
					city.country_location_id = $5 AND
					(
						city.region_location_id = $4 OR
						city.location_full_name = $6
					) AND
					(
						city.geoname_id IS NULL OR
						$7::bigint IS NULL
					)
				ORDER BY
					(city.region_location_id = $4) DESC,
					city.location_id
				LIMIT 1
			`,
			LocationTypeCity,
			countryCode,
			location.City,
			regionLocation.LocationId,
			countryLocation.LocationId,
			fmt.Sprintf("%s, %s, %s", location.City, regionLocation.Region, countryCode),
			geonameIdArg(location.CityGeonameId),
		)
		server.WithPgResult(result, err, func() {
			row = scanStoredCityRow(result)
		})
	}

	if row == nil && options.exactCity {
		return nil
	}

	if row != nil {
		row.withParents(countryLocation, regionLocation)
		keyed := matchedByGeonameId
		if !matchedByGeonameId && row.geonameId == 0 && location.CityGeonameId != 0 {
			if backfillGeonameIdInTx(ctx, tx, row.locationId, location.CityGeonameId) {
				row.geonameId = location.CityGeonameId
				keyed = true
			}
		}
		if seed && keyed {
			// the place list's coordinate is canonical for its city
			if hasLocationCoordinates(location) &&
				(row.latitude == nil || *row.latitude != location.Latitude ||
					row.longitude == nil || *row.longitude != location.Longitude) {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						UPDATE location
						SET
							latitude = $2,
							longitude = $3
						WHERE location_id = $1
					`,
					row.locationId,
					location.Latitude,
					location.Longitude,
				))
			}
			// the full name is composed with the region the row is filed
			// under, which may not be the region the list names
			fullName := fmt.Sprintf("%s, %s, %s", location.City, row.regionName, row.countryCode)
			if location.City != row.name || fullName != row.fullName {
				renamed := &Location{
					LocationType: LocationTypeCity,
					City:         location.City,
					Region:       row.regionName,
					Country:      row.countryName,
					CountryCode:  row.countryCode,
				}
				if renameLocationInTx(ctx, tx, row.locationId, location.City, fullName, renamed) {
					row.name = location.City
				}
			}
		} else {
			healCityCoordinatesInTx(ctx, tx, row, location)
		}
		return row.location()
	}

	// create a new location

	var latitude *float64
	var longitude *float64
	if hasLocationCoordinates(location) {
		latitude = &location.Latitude
		longitude = &location.Longitude
	}

	geonameId := newRowGeonameIdInTx(ctx, tx, location.CityGeonameId)
	name, fullName := newRowNameInTx(ctx, tx, location.City, location.CityGeonameId, func(name string) string {
		return fmt.Sprintf("%s, %s, %s", name, regionLocation.Region, countryCode)
	})
	locationId := server.NewId()
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO location (
				location_id,
				location_type,
				location_name,
				city_location_id,
				region_location_id,
				country_location_id,
				country_code,
				location_full_name,
				latitude,
				longitude,
				geoname_id
			)
			VALUES ($1, $2, $3, $1, $4, $5, $6, $7, $8, $9, $10)
		`,
		locationId,
		LocationTypeCity,
		name,
		regionLocation.LocationId,
		countryLocation.LocationId,
		countryCode,
		fullName,
		latitude,
		longitude,
		geonameId,
	))

	cityLocation := &Location{
		LocationType:      LocationTypeCity,
		City:              name,
		Region:            regionLocation.Region,
		Country:           countryLocation.Country,
		CountryCode:       countryLocation.CountryCode,
		LocationId:        locationId,
		CityLocationId:    locationId,
		RegionLocationId:  regionLocation.LocationId,
		CountryLocationId: countryLocation.LocationId,
		CityGeonameId:     geonameIdValue(geonameId),
		RegionGeonameId:   regionLocation.RegionGeonameId,
		CountryGeonameId:  countryLocation.CountryGeonameId,
	}

	// add to the search
	for i, searchStr := range cityLocation.SearchStrings() {
		locationSearch().AddInTx(ctx, searchStr, locationId, i, tx)
	}
	return cityLocation
}

// Reads a location row with the city, region and
// country rows it is filed under: their ids, names and geoname ids, and the
// row's own coordinates.
const locationHierarchySelect = `
	SELECT
		location.location_id,
		location.location_type,
		location.country_code,
		location.latitude,
		location.longitude,
		city.location_id,
		city.location_name,
		city.geoname_id,
		region.location_id,
		region.location_name,
		region.geoname_id,
		country.location_id,
		country.location_name,
		country.geoname_id
	FROM location
	LEFT JOIN location AS city ON city.location_id = location.city_location_id
	LEFT JOIN location AS region ON region.location_id = location.region_location_id
	LEFT JOIN location AS country ON country.location_id = location.country_location_id
`

// Reads the one location row a condition on `location` selects, as a full
// Location, or nil. The hierarchy ids of a granularity the row does not reach
// (the city of a country row) stay zero.
func getLocationWhere(ctx context.Context, where string, args ...any) *Location {
	var location *Location
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			locationHierarchySelect+" WHERE "+where,
			args...,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				location = scanLocationHierarchy(result)
			}
		})
	})
	return location
}

// Scans the current row of a locationHierarchySelect into a Location.
func scanLocationHierarchy(result server.PgResult) *Location {
	location := &Location{}
	var latitude *float64
	var longitude *float64
	// city_location_id/region_location_id are only set once the row's
	// hierarchy reaches that granularity (a country row has both NULL),
	// and server.Id.Scan errors on a nil source, so every joined column
	// scans through a pointer
	var cityLocationId *server.Id
	var city *string
	var cityGeonameId *int64
	var regionLocationId *server.Id
	var region *string
	var regionGeonameId *int64
	var countryLocationId *server.Id
	var country *string
	var countryGeonameId *int64
	server.Raise(result.Scan(
		&location.LocationId,
		&location.LocationType,
		&location.CountryCode,
		&latitude,
		&longitude,
		&cityLocationId,
		&city,
		&cityGeonameId,
		&regionLocationId,
		&region,
		&regionGeonameId,
		&countryLocationId,
		&country,
		&countryGeonameId,
	))
	location.CountryCode = strings.ToLower(location.CountryCode)
	if latitude != nil && longitude != nil {
		location.Latitude = *latitude
		location.Longitude = *longitude
	}
	if cityLocationId != nil {
		location.CityLocationId = *cityLocationId
		location.City = *city
		location.CityGeonameId = geonameIdValue(cityGeonameId)
	}
	if regionLocationId != nil {
		location.RegionLocationId = *regionLocationId
		location.Region = *region
		location.RegionGeonameId = geonameIdValue(regionGeonameId)
	}
	if countryLocationId != nil {
		location.CountryLocationId = *countryLocationId
		location.Country = *country
		location.CountryGeonameId = geonameIdValue(countryGeonameId)
	}
	return location
}

// Reads many location rows at once, with the hierarchy GetLocation reads for
// one, keyed by location id. An id with no row is absent.
func GetLocations(ctx context.Context, locationIds []server.Id) map[server.Id]*Location {
	locations := map[server.Id]*Location{}
	if len(locationIds) == 0 {
		return locations
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			locationHierarchySelect+" WHERE location.location_id = ANY($1)",
			locationIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				location := scanLocationHierarchy(result)
				locations[location.LocationId] = location
			}
		})
	})
	return locations
}

// Returns the location row a GeoNames id is stored on -- a city, region or
// country -- with its hierarchy, or nil.
func GetLocationByGeonameId(ctx context.Context, geonameId uint32) *Location {
	if geonameId == 0 {
		return nil
	}
	return getLocationWhere(ctx, "location.geoname_id = $1", int64(geonameId))
}

type LocationGroup struct {
	LocationGroupId   server.Id
	Name              string
	Promoted          bool
	MemberLocationIds []server.Id
}

func (self *LocationGroup) SearchStrings() []string {
	return []string{
		self.Name,
	}
}

func CreateLocationGroup(ctx context.Context, locationGroup *LocationGroup) {
	uniqueMemberLocationIds := map[server.Id]bool{}
	for _, memberLocationId := range locationGroup.MemberLocationIds {
		if uniqueMemberLocationIds[memberLocationId] {
			glog.Infof("[nclm]duplicate member[%s] found in group \"%s\". Ignoring.\n", memberLocationId, locationGroup.Name)
		}
		uniqueMemberLocationIds[memberLocationId] = true
	}
	server.Tx(ctx, func(tx server.PgTx) {
		ok := false
		var locationGroupId server.Id

		result, err := tx.Query(
			ctx,
			`
            SELECT location_group_id FROM location_group
            WHERE location_group_name = $1
            `,
			locationGroup.Name,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&locationGroupId))
				ok = true
			}
		})

		if ok {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
	                UPDATE location_group
	                SET
	                	location_group_name = $2,
	                	promoted = $3
	                WHERE
	                	location_group_id = $1
	            `,
				locationGroupId,
				locationGroup.Name,
				locationGroup.Promoted,
			))

			server.RaisePgResult(tx.Exec(
				ctx,
				`
	            DELETE FROM location_group_member
	            WHERE 
	                location_group_id = $1
	            `,
				locationGroupId,
			))

		} else {
			locationGroupId = server.NewId()

			server.RaisePgResult(tx.Exec(
				ctx,
				`
	                INSERT INTO location_group (
	                    location_group_id,
	                    location_group_name,
	                    promoted
	                )
	                VALUES ($1, $2, $3)
	            `,
				locationGroupId,
				locationGroup.Name,
				locationGroup.Promoted,
			))
		}

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for memberLocationId, _ := range uniqueMemberLocationIds {
				batch.Queue(
					`
                        INSERT INTO location_group_member (
                            location_group_id,
                            location_id
                        )
                        VALUES ($1, $2)
                    `,
					locationGroupId,
					memberLocationId,
				)
			}
		})

		locationGroup.LocationGroupId = locationGroupId

		for i, searchStr := range locationGroup.SearchStrings() {
			locationGroupSearch().AddInTx(ctx, searchStr, locationGroupId, i, tx)
		}
	})
}

func UpdateLocationGroup(ctx context.Context, locationGroup *LocationGroup) bool {
	success := false

	server.Tx(ctx, func(tx server.PgTx) {
		tag, err := tx.Exec(
			ctx,
			`
                UPDATE location_group
                SET
                    location_group_name = $2,
                    promoted = $3
                WHERE
                    location_group_id = $1
            `,
			locationGroup.LocationGroupId,
			locationGroup.Name,
			locationGroup.Promoted,
		)
		server.Raise(err)
		if tag.RowsAffected() != 1 {
			// does not exist
			return
		}

		tag, err = tx.Exec(
			ctx,
			`
                DELETE FROM location_group_member
                WHERE location_group_id = $1
            `,
			locationGroup.LocationGroupId,
		)
		server.Raise(err)

		server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
			for _, locationId := range locationGroup.MemberLocationIds {
				batch.Queue(
					`
                        INSERT INTO location_group_member (
                            location_group_id,
                            location_id
                        )
                        VALUES ($1, $2)
                    `,
					locationGroup.LocationGroupId,
					locationId,
				)
			}
		})

		success = true
	})

	return success
}

type ConnectionLocationScores struct {
	NetTypeHosting int
	NetTypePrivacy int
	NetTypeVirtual int
	NetTypeForeign int
	// the lookup's accuracy radius in km, the genesis confidence of
	// connect/GEOMAP.md §5.1; nil (NULL) for a location with none, such as an
	// egress-probed one. It describes the genesis location below when one is
	// stored, else the stored location.
	AccuracyKm *float32
	// The location the connection's own address lookup resolved to, its
	// genesis (connect/GEOMAP.md §5.1), stored beside the location the
	// connection is published at. The two differ once a derived location is
	// published (§6): the derive phase must then still read the lookup as the
	// node's genesis, and never its own last answer, or each derivation would
	// anchor to the one before and a node could be walked away from where
	// GeoLite2 places it one run at a time. nil for a location the egress
	// probe placed, whose genesis the derive phase reads from the probe.
	GenesisLocationId *server.Id
}

func SetConnectionLocation(
	ctx context.Context,
	connectionId server.Id,
	locationId server.Id,
	connectionLocationScores *ConnectionLocationScores,
) (returnErr error) {
	server.Tx(ctx, func(tx server.PgTx) {
		// note the network_id is allowed to be nil for a connection without an associated client
		result, err := tx.Query(
			ctx,
			`
                SELECT
                    network_client_connection.client_id,
                    network_client.network_id
                FROM network_client_connection 
                LEFT JOIN network_client ON network_client.client_id = network_client_connection.client_id
                WHERE network_client_connection.connection_id = $1
            `,
			connectionId,
		)
		var clientId *server.Id
		var networkId *server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&clientId,
					&networkId,
				))
			}
		})

		if clientId == nil {
			returnErr = fmt.Errorf("Missing client connection.")
			return
		}

		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location.city_location_id,
                    location.region_location_id,
                    location.country_location_id
                FROM location 
                WHERE location_id = $1
            `,
			locationId,
		)
		var cityLocationId *server.Id
		var regionLocationId *server.Id
		var countryLocationId *server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&cityLocationId,
					&regionLocationId,
					&countryLocationId,
				))
			}
		})

		// fix(beta): the free-tier ipinfo geo db resolves many IPs --
		// datacenter, mobile, and VPN egress especially -- to country or
		// region granularity only, with no city. network_client_location
		// requires city_location_id and region_location_id NOT NULL, so a
		// country-only location's NULL city/region made this INSERT panic
		// inside server.Tx. That panic propagated out of the connection
		// announce goroutine (connect/transport_announce.go), whose
		// HandleError wrapper then cancelled the whole connection context --
		// tearing down every country-only client's connect connection right
		// after auth (the app itself included, whichever egress it resolved
		// to), and, because the panic hit before the disconnect-cleanup
		// defer was registered, orphaning the connection row as
		// connected=true forever. Fall back to the coarsest available
		// granularity so the columns are always non-null: a country-only
		// location stores its country id for city/region too, which keeps
		// the provider locatable at country level instead of crashing the
		// connection. If even the country id is missing (location row absent
		// or malformed), return a clean error so the caller's existing
		// graceful retry path handles it -- never panic here.
		if countryLocationId == nil {
			returnErr = fmt.Errorf("Location %s has no country granularity.", locationId)
			return
		}
		if cityLocationId == nil {
			cityLocationId = countryLocationId
		}
		if regionLocationId == nil {
			regionLocationId = countryLocationId
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
                INSERT INTO network_client_location (
                    connection_id,
                    client_id,
                    city_location_id,
                    region_location_id,
                    country_location_id,
		            net_type_hosting,
		            net_type_privacy,
		            net_type_virtual,
		            net_type_foreign,
		            network_id,
		            accuracy_km,
		            genesis_location_id
                )
                VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
                ON CONFLICT (connection_id) DO UPDATE
                SET
                    client_id = $2,
                    city_location_id = $3,
                    region_location_id = $4,
                    country_location_id = $5,
                    net_type_hosting = $6,
                    net_type_privacy = $7,
                    net_type_virtual = $8,
                    net_type_foreign = $9,
                    network_id = $10,
                    accuracy_km = $11,
                    genesis_location_id = $12
            `,
			connectionId,
			clientId,
			cityLocationId,
			regionLocationId,
			countryLocationId,
			connectionLocationScores.NetTypeHosting,
			connectionLocationScores.NetTypePrivacy,
			connectionLocationScores.NetTypeVirtual,
			connectionLocationScores.NetTypeForeign,
			networkId,
			connectionLocationScores.AccuracyKm,
			connectionLocationScores.GenesisLocationId,
		))
	})
	return
}

type LocationGroupResult struct {
	LocationGroupId server.Id `json:"location_group_id"`
	Name            string    `json:"name"`
	ProviderCount   int       `json:"provider_count,omitempty"`
	Promoted        bool      `json:"promoted,omitempty"`
	MatchDistance   int       `json:"match_distance,omitempty"`
}

type LocationResult struct {
	LocationId        server.Id    `json:"location_id"`
	LocationType      LocationType `json:"location_type"`
	Name              string       `json:"name"`
	City              string       `json:"city,omitempty"`
	Region            string       `json:"region,omitempty"`
	Country           string       `json:"country,omitempty"`
	CityLocationId    *server.Id   `json:"city_location_id,omitempty"`
	RegionLocationId  *server.Id   `json:"region_location_id,omitempty"`
	CountryLocationId *server.Id   `json:"country_location_id,omitempty"`
	CountryCode       string       `json:"country_code"`
	ProviderCount     int          `json:"provider_count,omitempty"`
	MatchDistance     int          `json:"match_distance,omitempty"`

	Stable        bool `json:"stable"`
	StrongPrivacy bool `json:"strong_privacy"`
}

// clientLocationName resolves a parent location id (city/region/country) to its
// display name from the location set, or "" if the parent is not present.
func clientLocationName(byId map[server.Id]*ClientLocation, id *server.Id) string {
	if id == nil {
		return ""
	}
	if cl, ok := byId[*id]; ok {
		return cl.Name
	}
	return ""
}

type LocationDeviceResult struct {
	ClientId   server.Id `json:"client_id"`
	DeviceName string    `json:"device_name"`
}

type FindLocationsArgs struct {
	Query string `json:"query"`
	// the max search distance is `MaxDistanceFraction * len(Query)`
	// in other words `len(Query) * (1 - MaxDistanceFraction)` length the query must match
	MaxDistanceFraction       float32 `json:"max_distance_fraction,omitempty"`
	EnableMaxDistanceFraction bool    `json:"enable_max_distance_fraction,omitempty"`
	RankMode                  string  `json:"rank_mode"`
}

type FindLocationsResult struct {
	// this includes groups that show up in the location results
	// all `ProviderCount` are from inside the location results
	// groups are suggestions that can be used to broaden the search
	Groups []*LocationGroupResult `json:"groups"`
	// this includes all parent locations that show up in the location results
	// every `CityId`, `RegionId`, `CountryId` will have an entry
	Locations []*LocationResult `json:"locations"`
	// direct devices
	Devices []*LocationDeviceResult `json:"devices"`

	// location stats
	CountryCount       int `json:"country_count"`
	RegionCount        int `json:"region_count"`
	CityCount          int `json:"city_count"`
	StableCount        int `json:"stable_count"`
	StrongPrivacyCount int `json:"strong_privacy_count"`
}

func (self *FindLocationsResult) SetStats() {
	countryCount := 0
	regionCount := 0
	cityCount := 0
	stableCount := 0
	strongPrivacyCount := 0

	for _, location := range self.Locations {
		switch location.LocationType {
		case LocationTypeCountry:
			countryCount += 1
		case LocationTypeRegion:
			regionCount += 1
		case LocationTypeCity:
			cityCount += 1
		}
		if location.Stable {
			stableCount += 1
		}
		if location.StrongPrivacy {
			strongPrivacyCount += 1
		}
	}

	self.CountryCount = countryCount
	self.RegionCount = regionCount
	self.CityCount = cityCount
	self.StableCount = stableCount
	self.StrongPrivacyCount = strongPrivacyCount
}

// used for debugging
func SearchLocations(ctx context.Context, query string, distance int) []*search.SearchResult {
	s := locationSearch()
	s.WaitForInitialSync(ctx)

	startTime := time.Now()
	r := s.Around(
		ctx,
		query,
		distance,
		search.OptMostLikley(10),
	)
	endTime := time.Now()
	glog.Infof("Search took %.2fms\n", float64(endTime.Sub(startTime)/time.Microsecond)/1000.0)

	return r
}

type ClientLocation struct {
	LocationId  server.Id
	ClientCount int

	Name              string
	LocationType      LocationType
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id
	CountryCode       string

	// location id -> client count
	TopCityLocationIdCounts map[server.Id]int
	// location id -> client count
	TopRegionLocationIdCounts map[server.Id]int

	StrongPrivacy bool
}

type ClientLocationGroup struct {
	LocationGroupId server.Id

	Name     string
	Promoted bool
}

type InitialClientLocations struct {
	Locations      []*ClientLocation
	LocationGroups []*ClientLocationGroup
}

// the hash tag is per location so that the family spreads across cluster slots.
// a single shared tag would concentrate the entire cache on one node.
func clientLocationKey(locationId server.Id) string {
	return fmt.Sprintf("{cl_%s}l", locationId)
}

func initialClientLocationsKey() string {
	return fmt.Sprintf("{cl}i")
}

// returns the given ids with nils dropped and duplicates removed, preserving
// order.
//
// A client's city/region/country location ids are not guaranteed to differ: a
// country-only geo resolution stores the country id in all three columns and a
// region-only one stores it in two, because SetConnectionLocation falls back to
// the coarsest granularity available rather than writing a NULL into the NOT
// NULL city/region columns. Anything that fans a client out across the three
// columns has to treat them as a set, or the client is counted once per column
// instead of once per location it is in.
//
// Scope: that coarsest-granularity fallback is a beta change. upstream/main has
// no country-only fallback -- it passes the NULL through and the insert raises
// -- so on main today no row with city = region = country exists and this
// dedupe changes nothing there. It becomes load-bearing upstream when the
// fallback lands with PR #407, which carries its own copy of this helper.
func distinctIds(ids ...*server.Id) []server.Id {
	distinct := make([]server.Id, 0, len(ids))
	for _, id := range ids {
		if id == nil {
			continue
		}
		if !slices.Contains(distinct, *id) {
			distinct = append(distinct, *id)
		}
	}
	return distinct
}

// The share of destinations a provider must reach to count as healthy: 90%,
// as 9/10. Compared exactly as `10*ok >= 9*total` rather than through a float
// division, so the boundary is the same for every denominator.
//
// 90% because it cleanly separates working from broken on the real
// population: the healthy fleet measures 129-131 of 131 destinations, while
// a dead proxy measures 0 of 131. Nothing observed sits near the line, so
// the exact figure is not load bearing -- it only has to be far above 0 and
// below the ~98% a genuinely working provider always clears.
//
// Package scope, not function scope: both UpdateClientScores and
// UpdateClientLocations gate on this now, and two copies could drift.
const minEgressHealthOKNumerator = 9
const minEgressHealthOKDenominator = 10

const providerConfigResourceName = "provider.yml"

// The old rollout flag, which after connect/GEOMAP.md §10.3 speaks only for a
// rollup row written before the egress index existed: on, such a row is gated in both buckets on its 24 hour
// health run and counted only where a probe observed it where it is listed
// (decideProviderEgress). For every row the new rollup has written, the online
// bucket took over the flag's decision about the unprobed -- an unprobed
// provider is online when traffic shows it working, counted either way, and
// never fails closed -- and the hard exclusions, the country gate and the 90 %
// rule apply whatever the flag says.
//
// Defaulting to false is deliberate. A deployment can introduce the server
// side of the probe pipeline before any prober has populated its tables. In
// that state, treating every missing measurement as an individual failure
// publishes an empty cache even though the connected provider fleet is
// healthy.
func providerEgressTestEnabled() bool {
	return providerEgressTestEnabledFromResource(
		server.Config.SimpleResource(providerConfigResourceName),
	)
}

func providerEgressTestEnabledFromResource(resource *server.SimpleResource, err error) bool {
	if err != nil || resource == nil {
		return false
	}
	enabled := resource.Bool("enable_egress_test")
	return len(enabled) == 1 && enabled[0]
}

// providerCountFilter answers one question: does this provider count as real,
// reachable supply?
//
// It exists so the advertised provider_count (UpdateClientLocations) and the
// gated membership (UpdateClientScores) apply an IDENTICAL predicate. They ran
// different rules before: membership was gated on egress health while the count
// was not, so a location could survive the gate and still advertise providers
// that no probe had ever reached.
//
// Current hard-failure evidence is always loaded: an explicit blackhole verdict
// or an unauthenticated TLS identity is conclusive per-provider evidence, not a
// dependency on broad fleet probe coverage. So is the observed country, which
// the country gate reads whatever the rollout flag says (connect/GEOMAP.md
// §10.3). The 24 hour health counts are loaded only when the flag is on: they
// decide only rollup rows written before the egress index, whose own columns
// carry the health evidence for every other row. These loops run over the
// entire provider population, so a per-provider query here is one round trip
// per provider.
type providerCountFilter struct {
	healthCounts map[server.Id]ProviderEgressHealthCounts
	countryCodes map[server.Id]string
	// blackholed is the FAILING set only, not a verdict for every provider:
	// absent means "no current evidence this provider is dark", which covers
	// both a passing check and no check at all. See
	// GetAllProviderBlackholedClientIds for why it fails in that direction.
	blackholed map[server.Id]bool
	// tlsAuthenticationFailed is positive integrity-failure evidence. It is
	// loaded and enforced even while broad percentage/location qualification is
	// disabled, just like a current explicit blackhole verdict.
	tlsAuthenticationFailed map[server.Id]bool
}

func newProviderCountFilter(ctx context.Context, loadEgressEvidence bool) providerCountFilter {
	f := providerCountFilter{
		blackholed:              GetAllProviderBlackholedClientIds(ctx),
		tlsAuthenticationFailed: GetAllProviderEgressTLSAuthenticationFailedClientIds(ctx),
		countryCodes:            GetAllProviderEgressCountryCodes(ctx),
	}
	if loadEgressEvidence {
		f.healthCounts = GetAllProviderEgressHealthCounts(ctx)
	}
	return f
}

func (self providerCountFilter) isBlackholed(clientId server.Id) bool {
	return self.blackholed[clientId]
}

func (self providerCountFilter) hasHardEgressFailure(clientId server.Id) bool {
	return self.isBlackholed(clientId) || self.tlsAuthenticationFailed[clientId]
}

// passesHealth reports whether a probe has MEASURED this provider healthy.
// Fail closed: no record at all (never probed) does not pass, and neither does
// a record with no destinations in it, which is not a measurement of anything.
// Guarding total also keeps the ratio well defined.
//
// Compared exactly as 10*ok >= 9*total rather than through a float, so the 90%
// boundary cannot drift with rounding.
func (self providerCountFilter) passesHealth(clientId server.Id) bool {
	// Hard failures override a passing percentage. The hourly blackhole check
	// catches a provider that went dark after its last health sweep; the TLS bit
	// catches a provider for which one authenticated destination failed even if
	// enough unrelated destinations passed to clear 90%. Neither is a ranking
	// input: both only remove unsafe/unusable supply.
	if self.hasHardEgressFailure(clientId) {
		return false
	}

	counts, ok := self.healthCounts[clientId]
	if !ok {
		return false
	}
	if counts.Total <= 0 {
		return false
	}
	return minEgressHealthOKDenominator*counts.OKCount >= minEgressHealthOKNumerator*counts.Total
}

// countsTowardCountry reports whether this provider counts as supply for
// countryCode. It must both be measured healthy and have been OBSERVED
// egressing from that country.
//
// The two locations are different claims. network_client_location is where the
// provider says it is, derived from its own connection. provider_egress_location
// is where a probe actually watched its traffic leave. Counting on the claim
// alone advertises providers in countries they do not egress from -- measured
// on beta at 3 of 152 healthy providers claiming `at` while egressing from `gb`
// -- which is what an adversarial provider would exploit at scale.
//
// A provider with no observed location is not counted, matching the health rule.
func (self providerCountFilter) countsTowardCountry(clientId server.Id, countryCode string) bool {
	if !self.passesHealth(clientId) {
		return false
	}
	observed, ok := self.countryCodes[clientId]
	if !ok {
		return false
	}
	return observed == strings.ToLower(countryCode)
}

// Reports whether the rollout flag's qualification should be skipped for this
// count pass, counting an unprobed provider as though the
// flag were off while the hard exclusions and the country gate still apply.
//
// Both maps are checked, not just healthCounts, because they are fed by two
// INDEPENDENT pipelines that can stall separately: health arrives over the
// external push endpoint (api/handlers/provider_egress_health_handlers.go),
// while the observed egress location comes from a separate internal job
// (controller/provider_egress_location_controller.go). If only the health
// pipeline stalls (or vice versa), the healthy-but-unlocated -- or
// located-but-unhealthy -- provider still fails closed in countsTowardCountry
// and locationClientCounts still empties fleet-wide, which is exactly the
// wiped-list failure this gate exists to prevent. Do NOT collapse this back
// to a single condition: either map being empty is "we know nothing from that
// pipeline", which must not be treated as "everything failed."
func (self providerCountFilter) shouldSkipCountGate() bool {
	return len(self.healthCounts) == 0 || len(self.countryCodes) == 0
}

// shouldRecountUngated is the SECOND half of the fleet-wide floor, applied
// after a gated counting pass instead of before it: gated says the gate was
// actually applied to this pass, providerRows is how many connected + valid +
// Public rows the count query returned, and countedLocations is how many
// locations came out of it with any supply at all.
//
// This is NOT redundant with shouldSkipCountGate, and a future reader must not
// collapse the two. They answer different questions and neither implies the
// other:
//
//   - shouldSkipCountGate asks "did either probe pipeline produce ANY rows at
//     all". It reads the two input maps.
//   - this asks "did rows that exist produce ANY counted supply". It reads the
//     OUTPUT of the pass.
//
// Non-empty inputs can still yield an empty output, by more than one route: a
// fleet-wide mismatch between claimed and observed countries, a location-table
// anomaly that makes every claimed country NULL (see the countryCode == nil
// branch below), a partially drained egress-location table whose surviving rows
// all belong to churned clients, a fleet of rows written before the egress
// index under the rollout flag, or any future rule added to
// decideProviderEgress. In every one of those, the
// input maps are non-empty so shouldSkipCountGate stays false, and yet
// locationClientCounts comes out empty.
//
// An empty locationClientCounts is not a benign "no supply" result: every
// location then misses the lookup below and lands in removeClientLocations,
// which DELs every clientLocationKey from redis and publishes an empty
// initialClientLocations -- /network/provider-locations returns nothing to
// every app. Treat "rows existed but nothing counted" as "this pass learned
// nothing" and redo it with only the hard exclusions, which is at least the
// fallback shouldSkipCountGate selects.
//
// providerRows > 0 is what separates this from a genuinely empty fleet. If the
// count query returned no rows at all, there really is no connected + valid +
// Public supply and emptying the published list is the correct answer.
func shouldRecountUngated(gated bool, providerRows int, countedLocations int) bool {
	return gated && providerRows > 0 && countedLocations == 0
}

// providerCountRow is one connected + valid + Public provider row from the
// count query, held in memory so the pass can be counted twice (with the
// country gate and the rollout flag, then without them if the first pass came
// out empty) without issuing a second query. Both passes apply the hard
// exclusions.
type providerCountRow struct {
	clientId          server.Id
	cityLocationId    server.Id
	regionLocationId  server.Id
	countryLocationId server.Id
	// the country the provider CLAIMS. nil when the claimed country has no
	// `location` row to resolve it against.
	claimedCountryCode *string
	// the rollup's egress columns; a nil index is a row the new rollup has not
	// written
	egressIndex   *int
	egressQuality *bool
}

func UpdateClientLocations(ctx context.Context, ttl time.Duration) (returnErr error) {
	topCitiesPerRegion := 20
	topCitiesPerCountry := 10
	topRegionsPerCountry := 10

	clientLocations := map[server.Id]*ClientLocation{}
	removeClientLocations := map[server.Id]bool{}

	initialClientLocations := &InitialClientLocations{}

	// One bulk load per pass, outside the tx: this loop runs over the whole
	// provider population. Current blackhole and TLS-authentication verdicts and
	// the observed countries are always read; the 24 hour health counts only
	// while the rollout flag is on (see providerCountFilter).
	egressTestEnabled := providerEgressTestEnabled()
	egressSettings := egressIndexSettings()
	countFilter := newProviderCountFilter(ctx, egressTestEnabled)

	// The hard exclusions for FindProviders2 to read where it assembles a
	// result (see providerHardExclusionsKey): every provider a current
	// blackhole verdict or TLS-authentication failure excludes, from the same
	// load as the counts so the two agree. The set is replaced whole in one
	// transaction, so a reader sees the last set or this one and never part of
	// one, and with the counts' ttl, so a stalled pass lets both lapse
	// together. The marker distinguishes an authoritative empty publication
	// from missing coverage; missing or legacy sets use candidate-only SQL.
	hardExcludedMembers := []any{providerHardExclusionsReadyMember}
	for clientId, blackholed := range countFilter.blackholed {
		if blackholed {
			hardExcludedMembers = append(hardExcludedMembers, clientId.String())
		}
	}
	for clientId, failed := range countFilter.tlsAuthenticationFailed {
		if failed && !countFilter.blackholed[clientId] {
			hardExcludedMembers = append(hardExcludedMembers, clientId.String())
		}
	}
	var hardExclusionsErr error
	server.Redis(ctx, func(r server.RedisClient) {
		_, hardExclusionsErr = r.TxPipelined(ctx, func(pipe redis.Pipeliner) error {
			pipe.Del(ctx, providerHardExclusionsKey)
			pipe.SAdd(ctx, providerHardExclusionsKey, hardExcludedMembers...)
			pipe.Expire(ctx, providerHardExclusionsKey, ttl)
			return nil
		})
	})
	if hardExclusionsErr != nil {
		return fmt.Errorf("publish provider hard exclusions: %w", hardExclusionsErr)
	}

	// The rollout flag now speaks only for rollup rows written before the
	// egress index, and for those it keeps its old fail-closed count rule: a
	// row with no record does not count. An empty health or countryCodes map
	// means one of the two probe pipelines has told us nothing yet -- stalled
	// job, truncated table, cold environment -- not "every provider measured
	// unhealthy" or "every provider is mislocated", and applying the rule
	// fleet-wide then would empty locationClientCounts for a fleet of such
	// rows, which sends every single location through removeClientLocations
	// below and deletes every key from redis -- wiping the whole public
	// provider list because one prober died, not because supply is actually
	// gone. So count this pass as though the flag were off: only the hard
	// exclusions and the country gate apply. See shouldSkipCountGate for why
	// both maps are checked. Do not remove this as "redundant" with the
	// per-provider rules -- it is a fleet-wide floor, not a per-provider one.
	//
	// This is only the input-side half of that floor: empty inputs are not the
	// only way to reach an emptied count. See shouldRecountUngated, applied to
	// the counted result below, for the other half.
	countEgressTestEnabled := egressTestEnabled && !countFilter.shouldSkipCountGate()
	if !countEgressTestEnabled {
		if egressTestEnabled {
			glog.Infof("[nclm]egress health or location records are empty; counting rows written before the egress index without the rollout flag for this pass; hard egress exclusions and the country gate remain enabled\n")
		} else {
			glog.Infof("[nclm]provider egress test is disabled; counting rows written before the egress index without it; hard egress exclusions and the country gate remain enabled\n")
		}
	}

	server.Tx(ctx, func(tx server.PgTx) {

		providerCountRows := []providerCountRow{}

		result, err := tx.Query(
			ctx,
			`
	        SELECT
	        	network_client_location_reliability.client_id,
	        	network_client_location_reliability.city_location_id,
	        	network_client_location_reliability.region_location_id,
	        	network_client_location_reliability.country_location_id,
	        	-- the country the provider CLAIMS, to check against the country a
	        	-- probe observed it egressing from
	        	country_location.country_code,
	        	-- the rollup's egress verdict (connect/GEOMAP.md §10.4): a row
	        	-- it has written counts by the new rules, one it has not by the
	        	-- old ones
	        	network_client_location_reliability.egress_index,
	        	network_client_location_reliability.egress_quality

	        FROM network_client_location_reliability

	        INNER JOIN network_client ON
	            network_client.client_id = network_client_location_reliability.client_id

	        -- fix(beta): this was an INNER JOIN upstream, which requires a
	        -- client to already have a row in client_connection_reliability_score
	        -- (populated by a separate multi-stage rollup: raw events -> redis
	        -- drain -> client_reliability_running -> reliability scores) before
	        -- it counts toward any provider location at all. At small/cold-start
	        -- scale (this self-contained beta env) that rollup chain can go
	        -- indefinitely without producing a single row even though real,
	        -- currently-connected/valid clients exist -- the INNER JOIN then
	        -- discards every one of them, and /network/provider-locations comes
	        -- back completely empty despite real providers being connected.
	        -- LEFT JOIN counts a location from connected+valid alone, which is
	        -- real, already-verified data (see SetConnectionLocation), without
	        -- waiting on the reliability-scoring pipeline to catch up.
	        LEFT JOIN client_connection_reliability_score ON
	        	client_connection_reliability_score.client_id = network_client_location_reliability.client_id AND
				client_connection_reliability_score.lookback_index = 0

	        LEFT JOIN location AS country_location ON
	        	country_location.location_id = network_client_location_reliability.country_location_id

	        WHERE
	            network_client.active = true AND
	            network_client.source_client_id IS NULL AND
	        	network_client_location_reliability.connected = true AND
	        	network_client_location_reliability.valid = true AND
	        	-- this is the number shown to everyone, so count only providers
	        	-- a stranger can actually reach. GetProvideRelationship returns
	        	-- ProvideModePublic for a cross-network pair, so a Public
	        	-- provide key is what makes a provider generally reachable;
	        	-- without one it advertises supply nobody outside its own
	        	-- network can use.
	        	--
	        	-- Note this is deliberately narrower than the candidate pool
	        	-- UpdateClientScores builds. That pool also carries
	        	-- ProvideModeNetwork providers, which are real usable supply
	        	-- for sources inside their own network, and FindProviders2
	        	-- filters them per request against the caller's network. They
	        	-- do not belong in a public count.
	        	--
	        	-- Every other mode is excluded, not just Stream. In particular
	        	-- resolveNonCompanionProvideMode
	        	-- (controller/connect_controller.go) lets a Stream-only
	        	-- destination be resolved as a *companion* stream, but that
	        	-- dead-ends at CreateCompanionTransferEscrow, which requires a
	        	-- pre-existing reverse-direction origin contract -- so a
	        	-- Stream-only destination can never bootstrap a session and is
	        	-- correctly absent from both the count and the pool.
	        	EXISTS (
	        		SELECT 1 FROM provide_key
	        		WHERE
	        			provide_key.client_id = network_client_location_reliability.client_id AND
	        			provide_key.provide_mode = $1
	        	)
	        `,
			ProvideModePublic,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				// declared per iteration on purpose: claimedCountryCode is a
				// pointer, and hoisting these out of the loop would make every
				// retained row alias the last scanned value.
				var row providerCountRow
				server.Raise(result.Scan(
					&row.clientId,
					&row.cityLocationId,
					&row.regionLocationId,
					&row.countryLocationId,
					&row.claimedCountryCode,
					&row.egressIndex,
					&row.egressQuality,
				))
				providerCountRows = append(providerCountRows, row)
			}
		})

		// counted from the retained rows rather than inline in the scan loop, so
		// the same pass can be counted a second time with the gate off without
		// re-querying. See shouldRecountUngated.
		countProviderRows := func(gated bool) map[server.Id]int {
			locationClientCounts := map[server.Id]int{}
			for _, row := range providerCountRows {
				// A current blackhole or TLS-authentication verdict is conclusive
				// per-provider evidence and is never part of the fleet-wide
				// health/location fallback. Otherwise that fallback would
				// immediately resurrect the exact unsafe provider.
				if countFilter.hasHardEgressFailure(row.clientId) {
					continue
				}

				// This is the number every app shows when a user picks a
				// location, so it is the supply a user can actually use
				// (connect/GEOMAP.md §10.3): past the hard exclusions and the
				// country gate. A probed provider that fails the 90 % rule
				// still counts -- it is out of quality, not out of the market
				// -- and so does an unprobed one, which the online bucket
				// answers for once traffic shows it working and which never
				// fails closed.
				// A rollup row written before the egress index keeps the old
				// rule: with the flag on, counted only where a probe measured
				// it healthy and observed it egressing from the country it
				// claims, and a claimed country with no location row fails
				// closed, since it cannot be verified against anything.
				//
				// Unless the pass is ungated (see shouldRecountUngated below):
				// a fleet the rules emptied is "unknown", not "unusable", and
				// must not empty the public list.
				if gated {
					decision := decideProviderEgress(
						countFilter.egressFacts(
							row.clientId,
							row.claimedCountryCode,
							row.egressIndex,
							row.egressQuality,
							egressSettings,
						),
						countEgressTestEnabled,
					)
					if !decision.counted {
						continue
					}
				}

				// count each client at most once per distinct location id. A
				// client whose geo lookup resolved neither a city nor a region
				// is stored with city = region = country (see
				// SetConnectionLocation's country fallback), so incrementing
				// all three unconditionally counted that one client three
				// times in its own country. Distinct ids -- a real
				// city-granular client -- still roll up into their region and
				// country exactly as before.
				//
				// This is a live fix on beta, where that fallback exists, and a
				// forward guard against upstream/main, where it does not yet --
				// see distinctIds.
				for _, locationId := range distinctIds(
					&row.cityLocationId,
					&row.regionLocationId,
					&row.countryLocationId,
				) {
					locationClientCounts[locationId] += 1
				}
			}
			return locationClientCounts
		}

		// the country gate is not behind the flag, so every pass is gated
		gated := true
		locationClientCounts := countProviderRows(gated)

		// the output-side half of the fleet-wide floor. shouldSkipCountGate
		// guards the INPUTS (did a probe pipeline produce rows); this guards
		// the OUTPUT (did those rows produce any counted supply). Neither
		// implies the other -- see shouldRecountUngated for why they must not
		// be collapsed.
		if shouldRecountUngated(gated, len(providerCountRows), len(locationClientCounts)) {
			glog.Infof(
				"[nclm]count qualification emptied all %d connected provider rows fleet-wide; recounting without the country gate and the rollout flag; hard egress exclusions remain enabled\n",
				len(providerCountRows),
			)
			locationClientCounts = countProviderRows(false)
		}

		server.CreateTempTableInTx(
			ctx,
			tx,
			"temp_location_ids(location_id uuid)",
			slices.Collect(maps.Keys(locationClientCounts))...,
		)

		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_id,
                    location_type,
                    location_name,
                    city_location_id,
                    region_location_id,
                    country_location_id,
                    country_code
                FROM location
            `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				clientLocation := &ClientLocation{
					TopCityLocationIdCounts:   map[server.Id]int{},
					TopRegionLocationIdCounts: map[server.Id]int{},
				}
				server.Raise(result.Scan(
					&clientLocation.LocationId,
					&clientLocation.LocationType,
					&clientLocation.Name,
					&clientLocation.CityLocationId,
					&clientLocation.RegionLocationId,
					&clientLocation.CountryLocationId,
					&clientLocation.CountryCode,
				))
				if clientCount, ok := locationClientCounts[clientLocation.LocationId]; ok {
					clientLocation.ClientCount = clientCount
					clientLocations[clientLocation.LocationId] = clientLocation

					if clientLocation.LocationType == LocationTypeCountry {
						initialClientLocations.Locations = append(initialClientLocations.Locations, clientLocation)
					}
				} else {
					removeClientLocations[clientLocation.LocationId] = true
				}
			}
		})

		// create top links
		for locationId, clientLocation := range clientLocations {
			switch clientLocation.LocationType {
			case LocationTypeCity:
				regionClientLocation := clientLocations[*(clientLocation.RegionLocationId)]
				regionClientLocation.TopCityLocationIdCounts[locationId] = clientLocation.ClientCount

				countryClientLocation := clientLocations[*(clientLocation.CountryLocationId)]
				countryClientLocation.TopCityLocationIdCounts[locationId] = clientLocation.ClientCount
			case LocationTypeRegion:
				countryClientLocation := clientLocations[*(clientLocation.CountryLocationId)]
				countryClientLocation.TopRegionLocationIdCounts[locationId] = clientLocation.ClientCount
			}
		}
		filterTop := func(locationIdCounts map[server.Id]int, n int) map[server.Id]int {
			locationIds := slices.Collect(maps.Keys(locationIdCounts))
			slices.SortFunc(locationIds, func(a server.Id, b server.Id) int {
				d := locationIdCounts[b] - locationIdCounts[a]
				if d != 0 {
					return d
				}
				return a.Cmp(b)
			})
			filteredLocationIdCounts := map[server.Id]int{}
			for _, locationId := range locationIds[:min(n, len(locationIds))] {
				filteredLocationIdCounts[locationId] = locationIdCounts[locationId]
			}
			return filteredLocationIdCounts
		}
		for _, clientLocation := range clientLocations {
			switch clientLocation.LocationType {
			case LocationTypeRegion:
				clientLocation.TopCityLocationIdCounts = filterTop(clientLocation.TopCityLocationIdCounts, topCitiesPerRegion)
			case LocationTypeCountry:
				clientLocation.TopCityLocationIdCounts = filterTop(clientLocation.TopCityLocationIdCounts, topCitiesPerCountry)
				clientLocation.TopRegionLocationIdCounts = filterTop(clientLocation.TopRegionLocationIdCounts, topRegionsPerCountry)
			}
		}

		// fill in strong privacy flag based on membership in the `StrongPrivacyLaws` group
		// strong privacy is transitive to all sub-locations
		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_group_member.location_id
                FROM location_group_member

                INNER JOIN location_group ON
                	location_group.location_group_id = location_group_member.location_group_id AND
                	location_group.location_group_name = $1
            `,
			StrongPrivacyLaws,
		)
		strongPrivacyLocations := map[server.Id]bool{}
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var locationId server.Id
				server.Raise(result.Scan(&locationId))
				strongPrivacyLocations[locationId] = true
			}
		})
		for _, clientLocation := range clientLocations {
			strongPrivacy := false
			if clientLocation.CountryLocationId != nil {
				if strongPrivacyLocations[*clientLocation.CountryLocationId] {
					strongPrivacy = true
				}
			}
			if clientLocation.RegionLocationId != nil {
				if strongPrivacyLocations[*clientLocation.RegionLocationId] {
					strongPrivacy = true
				}
			}
			if clientLocation.CityLocationId != nil {
				if strongPrivacyLocations[*clientLocation.CityLocationId] {
					strongPrivacy = true
				}
			}
			clientLocation.StrongPrivacy = strongPrivacy
		}

		result, err = tx.Query(
			ctx,
			`
                SELECT
                    location_group_id,
                    location_group_name,
                    promoted
                FROM location_group
                WHERE promoted = true
            `,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				clientLocationGroup := &ClientLocationGroup{}
				server.Raise(result.Scan(
					&clientLocationGroup.LocationGroupId,
					&clientLocationGroup.Name,
					&clientLocationGroup.Promoted,
				))
				initialClientLocations.LocationGroups = append(initialClientLocations.LocationGroups, clientLocationGroup)
			}
		})
	})

	server.Redis(ctx, func(r server.RedisClient) {
		// plain pipeline instead of tx: the sets are independent and the keys
		// hash to different cluster slots, which multi/exec cannot span
		pipe := r.Pipeline()

		for locationId, clientLocation := range clientLocations {
			b := bytes.NewBuffer(nil)
			e := gob.NewEncoder(b)
			e.Encode(clientLocation)
			clientLocationBytes := b.Bytes()

			pipe.Set(ctx, clientLocationKey(locationId), clientLocationBytes, ttl)
			glog.V(2).Infof("[nclm]update client location (%s)\n", locationId)
		}
		for locationId, _ := range removeClientLocations {
			pipe.Del(ctx, clientLocationKey(locationId))
			glog.V(2).Infof("[nclm]remove client location (%s)\n", locationId)
		}

		b := bytes.NewBuffer(nil)
		e := gob.NewEncoder(b)
		e.Encode(initialClientLocations)
		initialClientLocationsBytes := b.Bytes()
		pipe.Set(ctx, initialClientLocationsKey(), initialClientLocationsBytes, ttl)
		glog.V(2).Infof("[nclm]update initial client locations\n")

		_, returnErr = pipe.Exec(ctx)
		if returnErr != nil {
			return
		}
	})

	glog.Infof("[nclm]updated %d client locations, removed %d, and updated initial\n", len(clientLocations), len(removeClientLocations))

	return
}

func loadClientLocations(
	ctx context.Context,
	locationIds map[server.Id]bool,
) (clientLocations map[server.Id]*ClientLocation, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		load := func(locationIds map[server.Id]bool, clientLocations map[server.Id]*ClientLocation) error {
			clientLocationCmds := map[server.Id]*redis.StringCmd{}

			// plain pipeline instead of tx: independent gets across cluster slots
			pipe := r.Pipeline()
			for locationId, _ := range locationIds {
				v := pipe.Get(ctx, clientLocationKey(locationId))
				clientLocationCmds[locationId] = v
			}
			// note ignore the error for GET since it will include missing key
			pipe.Exec(ctx)

			for locationId, clientLocationCmd := range clientLocationCmds {
				clientLocationBytes, _ := clientLocationCmd.Bytes()
				if len(clientLocationBytes) == 0 {
					continue
				}
				b := bytes.NewBuffer(clientLocationBytes)
				e := gob.NewDecoder(b)
				var clientLocation ClientLocation
				err := e.Decode(&clientLocation)
				if err != nil {
					return err
				}

				clientLocations[locationId] = &clientLocation
			}

			return nil
		}

		clientLocations = map[server.Id]*ClientLocation{}

		returnErr = load(locationIds, clientLocations)
		if returnErr != nil {
			return
		}

		expandedLocationIds := map[server.Id]bool{}

		for _, clientLocation := range clientLocations {
			if clientLocation.CityLocationId != nil {
				_, ok := locationIds[*clientLocation.CityLocationId]
				if !ok {
					expandedLocationIds[*clientLocation.CityLocationId] = true
				}
			}
			if clientLocation.RegionLocationId != nil {
				_, ok := locationIds[*clientLocation.RegionLocationId]
				if !ok {
					expandedLocationIds[*clientLocation.RegionLocationId] = true
				}
			}
			if clientLocation.CountryLocationId != nil {
				_, ok := locationIds[*clientLocation.CountryLocationId]
				if !ok {
					expandedLocationIds[*clientLocation.CountryLocationId] = true
				}
			}

			for locationId, _ := range clientLocation.TopCityLocationIdCounts {
				expandedLocationIds[locationId] = true
			}
			for locationId, _ := range clientLocation.TopRegionLocationIdCounts {
				expandedLocationIds[locationId] = true
			}
		}

		returnErr = load(expandedLocationIds, clientLocations)
		if returnErr != nil {
			return
		}
	})

	return
}

func loadInitialClientLocations(ctx context.Context) (initialClientLocations *InitialClientLocations, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {

		cmd := r.Get(ctx, initialClientLocationsKey())

		initialClientLocationsBytes, _ := cmd.Bytes()
		if len(initialClientLocationsBytes) == 0 {
			return
		}
		b := bytes.NewBuffer(initialClientLocationsBytes)
		e := gob.NewDecoder(b)
		var initialClientLocations_ InitialClientLocations
		returnErr = e.Decode(&initialClientLocations_)
		if returnErr != nil {
			return
		}

		initialClientLocations = &initialClientLocations_
	})
	return
}

// a missing entry means the location has no providers
func loadLocationStables(
	ctx context.Context,
	locationIds []server.Id,
	// forceMinimum selects which pre-computed key family to read. The writer
	// (UpdateClientScores) populates both, so this only chooses between them.
	// User-facing listing passes false and keeps today's behaviour; an operator
	// census passes true, because a location where every provider fails the
	// minimums gate is otherwise invisible and its providers can never be
	// probed or graduate probation.
	forceMinimum bool,
	rankMode RankMode,
	clientLocationId server.Id,
) (
	locationStables map[server.Id]bool,
	returnErr error,
) {
	locationStables = map[server.Id]bool{}

	server.Redis(ctx, func(r server.RedisClient) {
		type filterRead struct {
			alias    *redis.StringCmd
			caller   *redis.StringCmd
			baseline *redis.StringCmd
		}
		locationFilterCmds := map[server.Id]filterRead{}

		// plain pipeline instead of tx: independent gets across cluster slots
		pipe := r.Pipeline()
		for _, locationId := range locationIds {
			read := filterRead{
				caller: pipe.Get(ctx, clientScoreLocationFilterKey(forceMinimum, rankMode, locationId, clientLocationId)),
			}
			if clientLocationId != (server.Id{}) {
				read.alias = pipe.Get(ctx, clientScoreLocationAliasKey(forceMinimum, rankMode, locationId, clientLocationId))
				read.baseline = pipe.Get(ctx, clientScoreLocationFilterKey(forceMinimum, rankMode, locationId, server.Id{}))
			}
			locationFilterCmds[locationId] = read
		}
		// note ignore the error for GET since it will include missing key
		pipe.Exec(ctx)

		for locationId, read := range locationFilterCmds {
			_, filterBytes := selectClientScorePayload(
				clientLocationId,
				clientScoreCommandBytes(read.alias),
				clientScoreCommandBytes(read.caller),
				clientScoreCommandBytes(read.baseline),
			)
			if len(filterBytes) == 0 {
				// there are no providers
				continue
			}
			b := bytes.NewBuffer(filterBytes)
			e := gob.NewDecoder(b)
			var filter ClientFilter
			returnErr = e.Decode(&filter)
			if returnErr != nil {
				return
			}
			if 0 < filter.Count {
				stable := MinStableNetReliabilityWeight <= filter.NetReliabilityWeight
				locationStables[locationId] = stable
			}
			// else there are no providers
		}
	})
	return
}

func FindProviderLocations(
	findLocations *FindLocationsArgs,
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	query := strings.TrimSpace(findLocations.Query)
	if clientId, err := server.ParseId(query); err == nil {
		device := &LocationDeviceResult{
			ClientId:   clientId,
			DeviceName: fmt.Sprintf("%s", clientId),
		}

		deviceResults := []*LocationDeviceResult{
			device,
		}

		return &FindLocationsResult{
			Locations: []*LocationResult{},
			Groups:    []*LocationGroupResult{},
			Devices:   deviceResults,
		}, nil
	} else {
		// note group search is no longer supported

		rankMode := RankModeQuality
		if findLocations.RankMode != "" {
			rankMode = findLocations.RankMode
		}

		// the caller ip is used to match against provider excluded lists
		clientIp, _, err := session.ParseClientIpPort()
		if err != nil {
			return nil, err
		}

		ipInfo, err := server.GetIpInfo(clientIp)
		if err != nil {
			return nil, err
		}

		clientLocationId := countryCodeLocationIds()[ipInfo.CountryCode]

		var matchDistances map[server.Id]int
		var clientLocations map[server.Id]*ClientLocation
		if query == "" {
			initialClientLocations, err := loadInitialClientLocations(session.Ctx)
			if err != nil {
				return nil, err
			}
			matchDistances = map[server.Id]int{}
			clientLocations = map[server.Id]*ClientLocation{}
			for _, clientLocation := range initialClientLocations.Locations {
				clientLocations[clientLocation.LocationId] = clientLocation
			}
		} else {
			maxSearchDistance := 2
			locationSearchResults := locationSearch().AroundIds(
				session.Ctx,
				query,
				maxSearchDistance,
				search.OptMostLikley(30),
			)

			locationIds := map[server.Id]bool{}
			for locationId, _ := range locationSearchResults {
				locationIds[locationId] = true
			}
			var err error
			clientLocations, err = loadClientLocations(session.Ctx, locationIds)
			if err != nil {
				return nil, err
			}

			matchDistances = map[server.Id]int{}
			for locationId, _ := range clientLocations {
				if r, ok := locationSearchResults[locationId]; ok {
					matchDistances[locationId] = r.ValueDistance
				} else {
					matchDistances[locationId] = maxSearchDistance + 1
				}
			}
		}

		// ignore if this meta data can't be loaded
		// in that case, all locations will be considered unstable
		locationStables, _ := loadLocationStables(
			session.Ctx,
			slices.Collect(maps.Keys(clientLocations)),
			// user-facing search: only surface locations that meet the bar
			false,
			rankMode,
			clientLocationId,
		)
		if locationStables == nil {
			locationStables = map[server.Id]bool{}
		}

		locationResults := []*LocationResult{}

		for locationId, clientLocation := range clientLocations {
			stable, ok := locationStables[locationId]
			if ok {
				locationResult := &LocationResult{
					LocationId:        clientLocation.LocationId,
					LocationType:      clientLocation.LocationType,
					Name:              clientLocation.Name,
					CityLocationId:    clientLocation.CityLocationId,
					RegionLocationId:  clientLocation.RegionLocationId,
					CountryLocationId: clientLocation.CountryLocationId,
					CountryCode:       clientLocation.CountryCode,
					ProviderCount:     clientLocation.ClientCount,
					StrongPrivacy:     clientLocation.StrongPrivacy,
					Stable:            stable,
					MatchDistance:     matchDistances[locationId],
				}
				locationResults = append(locationResults, locationResult)
			}
		}

		for _, locationResult := range locationResults {
			locationResult.City = clientLocationName(clientLocations, locationResult.CityLocationId)
			locationResult.Region = clientLocationName(clientLocations, locationResult.RegionLocationId)
			locationResult.Country = clientLocationName(clientLocations, locationResult.CountryLocationId)
		}

		result := &FindLocationsResult{
			Locations: locationResults,
			Groups:    []*LocationGroupResult{},
			Devices:   []*LocationDeviceResult{},
		}
		result.SetStats()

		return result, nil
	}
}

// since there are no promoted groups, this call can be replaced with `FindProviderLocations` with an empty query
func GetProviderLocations(
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	rankMode := RankModeQuality

	// the caller ip is used to match against provider excluded lists
	clientIp, _, err := session.ParseClientIpPort()

	var clientLocationId server.Id
	if err == nil {
		ipInfo, err := server.GetIpInfo(clientIp)
		if err == nil {
			clientLocationId = countryCodeLocationIds()[ipInfo.CountryCode]
		} else {
			glog.V(2).Infof("[GetProviderLocations] could not get ip info for %s: %s\n", clientIp, err)
		}
	} else {
		glog.V(2).Infof("[GetProviderLocations] could not parse client ip: %s\n", err)
	}

	initialClientLocations, err := loadInitialClientLocations(session.Ctx)
	if err != nil {
		return nil, err
	}
	if initialClientLocations == nil {
		initialClientLocations = &InitialClientLocations{}
	}

	locationIds := []server.Id{}
	for _, clientLocation := range initialClientLocations.Locations {
		locationIds = append(locationIds, clientLocation.LocationId)
	}

	// ignore if this meta data can't be loaded
	// in that case, all locations will be considered unstable
	locationStables, _ := loadLocationStables(
		session.Ctx,
		locationIds,
		// user-facing listing: only surface locations that meet the bar
		false,
		rankMode,
		clientLocationId,
	)
	if locationStables == nil {
		locationStables = map[server.Id]bool{}
	}

	locationResults := []*LocationResult{}
	locationGroupResults := []*LocationGroupResult{}

	for _, clientLocation := range initialClientLocations.Locations {
		stable, ok := locationStables[clientLocation.LocationId]
		if ok {
			locationResult := &LocationResult{
				LocationId:        clientLocation.LocationId,
				LocationType:      clientLocation.LocationType,
				Name:              clientLocation.Name,
				CityLocationId:    clientLocation.CityLocationId,
				RegionLocationId:  clientLocation.RegionLocationId,
				CountryLocationId: clientLocation.CountryLocationId,
				CountryCode:       clientLocation.CountryCode,
				ProviderCount:     clientLocation.ClientCount,
				StrongPrivacy:     clientLocation.StrongPrivacy,
				Stable:            stable,
			}
			locationResults = append(locationResults, locationResult)
		}
	}
	for _, clientLocationGroup := range initialClientLocations.LocationGroups {
		locationGroupResult := &LocationGroupResult{
			LocationGroupId: clientLocationGroup.LocationGroupId,
			Name:            clientLocationGroup.Name,
			Promoted:        clientLocationGroup.Promoted,
		}
		locationGroupResults = append(locationGroupResults, locationGroupResult)
	}

	locationsById := map[server.Id]*ClientLocation{}
	for _, cl := range initialClientLocations.Locations {
		locationsById[cl.LocationId] = cl
	}
	for _, locationResult := range locationResults {
		locationResult.City = clientLocationName(locationsById, locationResult.CityLocationId)
		locationResult.Region = clientLocationName(locationsById, locationResult.RegionLocationId)
		locationResult.Country = clientLocationName(locationsById, locationResult.CountryLocationId)
	}

	result := &FindLocationsResult{
		Locations: locationResults,
		Groups:    locationGroupResults,
		Devices:   []*LocationDeviceResult{},
	}
	result.SetStats()

	return result, nil
}

// no longer supported
func FindLocations(
	findLocations *FindLocationsArgs,
	session *session.ClientSession,
) (*FindLocationsResult, error) {
	return &FindLocationsResult{
		Locations: []*LocationResult{},
		Groups:    []*LocationGroupResult{},
		Devices:   []*LocationDeviceResult{},
	}, nil
}

type FindProvidersArgs struct {
	LocationId       *server.Id  `json:"location_id,omitempty"`
	LocationGroupId  *server.Id  `json:"location_group_id,omitempty"`
	Count            int         `json:"count"`
	ExcludeClientIds []server.Id `json:"exclude_location_ids,omitempty"`
}

type FindProvidersResult struct {
	ClientIds []server.Id `json:"client_ids,omitempty"`
}

// no longer supported. See `FindProviders2`
func FindProviders(
	findProviders *FindProvidersArgs,
	session *session.ClientSession,
) (*FindProvidersResult, error) {
	return &FindProvidersResult{
		ClientIds: []server.Id{},
	}, nil
}

type ProviderSpec struct {
	LocationId      *server.Id `json:"location_id,omitempty"`
	LocationGroupId *server.Id `json:"location_group_id,omitempty"`
	ClientId        *server.Id `json:"client_id,omitempty"`
	BestAvailable   bool       `json:"best_available,omitempty"`
}

type RankMode = string

const (
	RankModeQuality RankMode = "quality"
	RankModeSpeed   RankMode = "speed"
)

type FindProviders2Args struct {
	Specs               []*ProviderSpec `json:"specs"`
	Count               int             `json:"count"`
	ForceCount          bool            `json:"force_count"`
	ExcludeClientIds    []server.Id     `json:"exclude_client_ids"`
	ExcludeDestinations [][]server.Id   `json:"exclude_destinations"`
	RankMode            RankMode        `json:"rank_mode"`
	ForceMinimum        bool            `json:"force_minimum"`
	// IpFamily filters providers by proven address family, in the connect
	// vocabulary: "" and "v4-capable" (dualstack first, then v4-only),
	// "v6-capable" (dualstack first, then v6-only), and the exact categories
	// "dualstack", "v4-only", "v6-only". See ipFamilyFacetsForFilter.
	IpFamily string `json:"ip_family"`
}

type FindProviders2Result struct {
	Providers []*FindProvidersProvider `json:"providers"`
}

type FindProvidersProvider struct {
	ClientId                   server.Id         `json:"client_id"`
	EstimatedBytesPerSecond    ByteCount         `json:"estimated_bytes_per_second"`
	HasEstimatedBytesPerSecond bool              `json:"has_estimated_bytes_per_second"`
	Tier                       int               `json:"tier"`
	IntermediaryIds            []server.Id       `json:"intermediary_ids"`
	NetworkOnly                bool              `json:"network_only,omitempty"`
	ReputationFailedNames      string            `json:"reputation_failed_names,omitempty"`
	Location                   *ProviderLocation `json:"location,omitempty"`
	// IpFamily is the provider's proven category: "dualstack", "v4-only" or
	// "v6-only". Empty for a fixed client-id spec, which bypasses discovery.
	IpFamily string `json:"ip_family,omitempty"`
}

type LocationCoordinates struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

type ProviderLocation struct {
	Country           string               `json:"country,omitempty"`
	CountryCode       string               `json:"country_code,omitempty"`
	Region            string               `json:"region,omitempty"`
	City              string               `json:"city,omitempty"`
	CountryLocationId *server.Id           `json:"country_location_id,omitempty"`
	RegionLocationId  *server.Id           `json:"region_location_id,omitempty"`
	CityLocationId    *server.Id           `json:"city_location_id,omitempty"`
	RegionCoordinates *LocationCoordinates `json:"region_coordinates,omitempty"`
	CityCoordinates   *LocationCoordinates `json:"city_coordinates,omitempty"`
}

type ClientScore struct {
	ClientId                     server.Id
	NetworkId                    server.Id
	Scores                       map[string]int
	ReliabilityWeight            float64
	IndependentReliabilityWeight float64
	Tiers                        map[string]int
	MinRelativeLatencyMillis     int
	MaxBytesPerSecond            ByteCount
	HasLatencyTest               bool
	HasSpeedTest                 bool

	// true when the provider holds a ProvideModeNetwork provide key but no
	// ProvideModePublic one, i.e. it can only settle a contract with a source
	// in its own network. FindProviders2 keeps such a provider only for callers
	// in NetworkId. It is stored negated on purpose: the score cache is gob
	// encoded with a 5h ttl, so entries written before this field existed decode
	// with the zero value, and the zero value has to mean "publicly usable" --
	// the pre-existing behaviour -- or every provider would be treated as
	// network-only until the cache turned over.
	NetworkOnly bool
	// ReputationFailedNames is the current external-probe domain/vendor
	// rejection set. It is intentionally separate from health scoring: a
	// hosted exit can carry traffic correctly while a particular publisher
	// refuses its egress IP. Like the location ids below, this is set only on
	// the top-level score so each lookback does not duplicate the string in
	// every gob cache blob.
	ReputationFailedNames string
	// IpFamilies is the bitmask of families this provider has proven with a
	// connected connection (ClientScoreIpFamilyV4 | ClientScoreIpFamilyV6).
	// Zero is legacy and reads as v4-only, for the same gob reason as
	// NetworkOnly: an entry written before this field existed must keep
	// today's behavior. See IpFamily and network_client_ip_family.go.
	IpFamilies uint8
	// marks a provider of the online bucket (connect/GEOMAP.md §10.3):
	// no probe verdict, past the exclusions and the gate, and within every
	// minimum the other buckets apply apart from the probe's -- the
	// reliability floors and the speed-mode score maximum, so a provider no
	// client has measured is not online. It sits in both modes' samples but
	// is native to neither -- no request names the online bucket, and a
	// short bucket borrows from it last. The zero value is "not online" for
	// the same gob reason as NetworkOnly: an entry written before this field
	// existed keeps its native place. Top-level only.
	Online bool

	// set only on the top-level score, never on the `LookbackClientScores`
	// copies: each score is gob-serialized into thousands of cache key
	// permutations, and gob transmits zero-valued arrays, so these are
	// pointers (omitted when nil) and stay nil on the nested lookback copies
	CityLocationId    *server.Id
	RegionLocationId  *server.Id
	CountryLocationId *server.Id

	LookbackIndex        int
	LookbackClientScores map[int]*ClientScore

	ScaledWeights  map[string]float32
	PassesMinimums map[string]bool

	// the score each mode's score minimum and selection weight read while
	// UpdateClientScores builds the pool. Unexported, so gob
	// never carries it into the cache. For a provider with an egress index it
	// is the score without the index: the index orders the quality bucket
	// through the tier, and must not also decide membership through the
	// minimum (connect/GEOMAP.md §10.3), or two failed loads -- which the
	// sites that refuse proxied requests hand most healthy providers (§10.5)
	// -- would put the score at the minimum's bar and take out of quality a
	// provider the 90 % rule admits. For a row written before the index it is
	// the score itself, as it always was.
	minimumScores map[string]int
}

type ClientFilter struct {
	Count                int
	NetReliabilityWeight float64
	Index                int
}

// scores are [0, max], where 0 is best
const MaxClientScore = 50
const ClientScoreSampleCount = 200

// The width of a tier in score points: a tier is a score's twentieths, so a
// missing latency or throughput test (two tiers) costs 40.
const ClientScorePerTier = 20

// The tier of a provider past a mode's latency or throughput cutoff: one past
// MaxNativeClientScoreTier, the highest tier a
// score within the cutoffs can reach. FindProviders2 draws a mode's own
// providers from those within its cutoffs, and treats the rest as backfill.
const ClientScoreCutoffTier = (MaxClientScore + ClientScorePerTier - 1) / ClientScorePerTier

// The highest tier a provider within a mode's cutoffs carries.
const MaxNativeClientScoreTier = MaxClientScore / ClientScorePerTier

// choose a filter that has at least this number of providers
// FIXME this scale based on traffic for region
// const MinExportNetReliabilityWeight = float64(400)

// the number of filtered providers to consider a location stable
const MinStableNetReliabilityWeight = float64(4)

// the client score cache keys hash tag on the (caller location, target) pair
// so that the family spreads across cluster slots. tagging only the caller
// location would concentrate all targets for a popular caller location
// (e.g. us) on a single node, which can exceed the node's memory.
// the sample index stays outside the tag.
func clientScoreLocationCountsKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}c_l", fm, rm, callerLocationId, locationId)
}

func clientScoreLocationGroupCountsKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}c_g", fm, rm, callerLocationId, locationGroupId)
}

func clientScoreLocationAliasKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}a_l", fm, rm, callerLocationId, locationId)
}

func clientScoreLocationGroupAliasKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}a_g", fm, rm, callerLocationId, locationGroupId)
}

func clientScoreLocationFilterKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}f_l", fm, rm, callerLocationId, locationId)
}

func clientScoreLocationGroupFilterKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}f_g", fm, rm, callerLocationId, locationGroupId)
}

func clientScoreLocationSampleKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}s_l_%d", fm, rm, callerLocationId, locationId, index)
}

func clientScoreLocationGroupSampleKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}s_g_%d", fm, rm, callerLocationId, locationGroupId, index)
}

// The per-family facets of the score cache (connect/IPV6.md A8). Under the
// same hash tag as the keys above, so a target's whole family stays on one
// cluster slot:
//
//	{cs_<fm>_<rm>_<caller>_<target>}c_l           un-faceted counts (every provider)
//	{cs_<fm>_<rm>_<caller>_<target>}s_l_<i>       un-faceted sample bucket i
//	{cs_<fm>_<rm>_<caller>_<target>}c_l_<facet>   counts of one family category
//	{cs_<fm>_<rm>_<caller>_<target>}s_l_<facet>_<i>  sample bucket i of that category
//
// with `_g` in place of `_l` for a location group, and <facet> one of "d"
// (dualstack), "4" (v4-only), "6" (v6-only). A faceted export writes every
// facet, an empty one included, so loadClientScores can tell a cache written
// without facets (fall back to the un-faceted keys) from a family with no
// providers. The filter (`f_l`) and alias (`a_l`) keys are shared: the
// stability filter counts public providers of every family, and an alias
// covers every key under its tag.
func clientScoreLocationFacetCountsKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id, facet ipFamilyFacet) string {
	return fmt.Sprintf("%s_%s", clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, callerLocationId), facet)
}

func clientScoreLocationGroupFacetCountsKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id, facet ipFamilyFacet) string {
	return fmt.Sprintf("%s_%s", clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, callerLocationId), facet)
}

func clientScoreLocationFacetSampleKey(forceMinimum bool, rankMode RankMode, locationId server.Id, callerLocationId server.Id, facet ipFamilyFacet, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}s_l_%s_%d", fm, rm, callerLocationId, locationId, facet, index)
}

func clientScoreLocationGroupFacetSampleKey(forceMinimum bool, rankMode RankMode, locationGroupId server.Id, callerLocationId server.Id, facet ipFamilyFacet, index int) string {
	fm := 0
	if forceMinimum {
		fm = 1
	}
	rm, _ := utf8.DecodeRuneInString(rankMode)
	return fmt.Sprintf("{cs_%d_%c_%s_%s}s_g_%s_%d", fm, rm, callerLocationId, locationGroupId, facet, index)
}

func UpdateClientScores(ctx context.Context, ttl time.Duration, parallel int) (returnErr error) {
	aliasesReady, err := clientScoreAliasReady(ctx)
	if err != nil {
		return fmt.Errorf("read client score alias migration state: %w", err)
	}
	writeLegacyUnchanged := !aliasesReady
	ipFamilyReady, err := clientScoreIpFamilyReady(ctx)
	if err != nil {
		return fmt.Errorf("read client score ip family migration state: %w", err)
	}
	writeUnfacetedPayload := !ipFamilyReady
	sourceLoadSpan := updateClientScoresPhaseMetrics.start(updateClientScoresPhaseSourceLoad)
	defer sourceLoadSpan.finish()
	sourceRows := 0

	addClientScore := func(lookbackClientScore *ClientScore, reputationFailedNames string, m map[server.Id]*ClientScore) *ClientScore {
		clientScore, ok := m[lookbackClientScore.ClientId]
		if !ok {
			clientScore = &ClientScore{
				ClientId:              lookbackClientScore.ClientId,
				NetworkId:             lookbackClientScore.NetworkId,
				NetworkOnly:           lookbackClientScore.NetworkOnly,
				ReputationFailedNames: reputationFailedNames,
				IpFamilies:            lookbackClientScore.IpFamilies,
				LookbackClientScores:  map[int]*ClientScore{},
			}
			m[lookbackClientScore.ClientId] = clientScore
		}
		clientScore.LookbackClientScores[lookbackClientScore.LookbackIndex] = lookbackClientScore
		return clientScore
	}

	locationClientScores := map[server.Id]map[server.Id]*ClientScore{}
	locationGroupClientScores := map[server.Id]map[server.Id]*ClientScore{}

	type performanceTarget struct {
		relativeLatencyMillisThreshold int
		relativeLatencyMillisCutoff    int
		relativeLatencyMillisPerScore  int
		bytesPerSecondThreshold        ByteCount
		bytesPerSecondCutoff           ByteCount
		bytesPerSecondPerScore         ByteCount
	}

	scorePerTier := ClientScorePerTier
	missingLatencyScore := 2 * scorePerTier
	missingSpeedScore := 2 * scorePerTier

	performanceTargets := map[RankMode]performanceTarget{
		RankModeQuality: performanceTarget{
			relativeLatencyMillisThreshold: 50,
			relativeLatencyMillisCutoff:    200,
			relativeLatencyMillisPerScore:  20,
			bytesPerSecondThreshold:        8 * Mib,
			bytesPerSecondCutoff:           800 * Kib,
			bytesPerSecondPerScore:         200 * Kib,
		},
		RankModeSpeed: performanceTarget{
			relativeLatencyMillisThreshold: 20,
			relativeLatencyMillisCutoff:    50,
			relativeLatencyMillisPerScore:  5,
			bytesPerSecondThreshold:        40 * Mib,
			bytesPerSecondCutoff:           4 * Mib,
			bytesPerSecondPerScore:         1 * Mib,
		},
	}

	// Per mode, score = min(20·base + adjust, MaxClientScore) and the tier its
	// twentieths, where the performance tests make the adjust and a cutoff
	// excludes (connect/GEOMAP.md §10.1). The base is the egress index in
	// quality and zero in speed (§10.3), or each mode's net-type score for a
	// row the new rollup has not written. rankModeMinimumBases is the base of
	// the score the minimum and the weight read (see
	// ClientScore.minimumScores).
	setScore := func(
		clientScore *ClientScore,
		rankModeBases map[RankMode]int,
		rankModeMinimumBases map[RankMode]int,
		minRelativeLatencyMillis int,
		maxBytesPerSecond ByteCount,
		hasLatencyTest bool,
		hasSpeedTest bool,
	) {
		clientScore.minimumScores = map[string]int{}
		for rankMode, target := range performanceTargets {
			exclude := false
			scoreAdjust := 0

			if hasLatencyTest {
				if target.relativeLatencyMillisCutoff < minRelativeLatencyMillis {
					exclude = true
				} else if d := minRelativeLatencyMillis - target.relativeLatencyMillisThreshold; 0 < d {
					scoreAdjust += (d + target.relativeLatencyMillisPerScore/2) / target.relativeLatencyMillisPerScore
				}
			} else {
				scoreAdjust += missingLatencyScore
			}

			if hasSpeedTest {
				if maxBytesPerSecond < target.bytesPerSecondCutoff {
					exclude = true
				} else if d := target.bytesPerSecondThreshold - maxBytesPerSecond; 0 < d {
					scoreAdjust += int((d + target.bytesPerSecondPerScore/2) / target.bytesPerSecondPerScore)
				}
			} else {
				scoreAdjust += missingSpeedScore
			}

			if !exclude {
				score := min(
					scorePerTier*rankModeBases[rankMode]+scoreAdjust,
					MaxClientScore,
				)
				clientScore.Scores[rankMode] = score
				clientScore.Tiers[rankMode] = score / scorePerTier
				clientScore.minimumScores[rankMode] = min(
					scorePerTier*rankModeMinimumBases[rankMode]+scoreAdjust,
					MaxClientScore,
				)
			} else {
				clientScore.Scores[rankMode] = 0
				clientScore.Tiers[rankMode] = ClientScoreCutoffTier
				clientScore.minimumScores[rankMode] = 0
			}
		}
	}

	// What the rules of connect/GEOMAP.md §10.3 read about a provider from its
	// rollup row, the same in both pool queries.
	type clientScoreEgress struct {
		egressIndex   *int
		egressQuality *bool
		// the country of the rollup's location, which the country gate
		// compares with a fresh probe's
		publishedCountryCode *string
	}

	loadClientScore := func(result server.PgResult) (lookbackClientScore *ClientScore, cityLocationXId *server.Id, regionLocationXId *server.Id, countryLocationXId *server.Id, reputationFailedNames string, egress *clientScoreEgress) {
		var clientId server.Id
		var networkId server.Id
		var netTypeScore int
		var netTypeScoreSpeed int
		var minRelativeLatencyMillis int
		var maxBytesPerSecond ByteCount
		var hasLatencyTest bool
		var hasSpeedTest bool
		var lookbackIndex int
		var reliabilityWeight float64
		var independentReliabilityWeight float64
		var publiclyUsable bool
		var ipv4Proven bool
		var ipv6Proven bool
		egress = &clientScoreEgress{}
		server.Raise(result.Scan(
			&cityLocationXId,
			&regionLocationXId,
			&countryLocationXId,
			&clientId,
			&networkId,
			&netTypeScore,
			&netTypeScoreSpeed,
			&minRelativeLatencyMillis,
			&maxBytesPerSecond,
			&hasLatencyTest,
			&hasSpeedTest,
			&lookbackIndex,
			&reliabilityWeight,
			&independentReliabilityWeight,
			&publiclyUsable,
			&reputationFailedNames,
			&ipv4Proven,
			&ipv6Proven,
			&egress.egressIndex,
			&egress.egressQuality,
			&egress.publishedCountryCode,
		))
		lookbackClientScore = &ClientScore{
			ClientId:                     clientId,
			LookbackIndex:                lookbackIndex,
			NetworkId:                    networkId,
			NetworkOnly:                  !publiclyUsable,
			IpFamilies:                   clientScoreIpFamilies(ipv4Proven, ipv6Proven),
			ReliabilityWeight:            reliabilityWeight,
			IndependentReliabilityWeight: independentReliabilityWeight,
			MinRelativeLatencyMillis:     minRelativeLatencyMillis,
			MaxBytesPerSecond:            maxBytesPerSecond,
			HasLatencyTest:               hasLatencyTest,
			HasSpeedTest:                 hasSpeedTest,
			Scores:                       map[string]int{},
			Tiers:                        map[string]int{},
		}

		// the old fields stay authoritative wherever the new one is empty
		// (connect/GEOMAP.md §10.4): a row the new rollup has not written
		// ranks exactly as it did, on its net-type score in both modes
		rankModeBases := map[RankMode]int{
			RankModeQuality: netTypeScore,
			RankModeSpeed:   netTypeScoreSpeed,
		}
		rankModeMinimumBases := rankModeBases
		if egress.egressIndex != nil {
			rankModeBases = map[RankMode]int{
				RankModeQuality: *egress.egressIndex,
				RankModeSpeed:   0,
			}
			rankModeMinimumBases = map[RankMode]int{
				RankModeQuality: 0,
				RankModeSpeed:   0,
			}
		}

		setScore(
			lookbackClientScore,
			rankModeBases,
			rankModeMinimumBases,
			minRelativeLatencyMillis,
			maxBytesPerSecond,
			hasLatencyTest,
			hasSpeedTest,
		)

		return
	}

	// The evidence of the rules of connect/GEOMAP.md §10.3 is loaded once for
	// the whole pass rather than per client: this walks every provider, and
	// each table is at most one row per ever-probed provider. It is loaded
	// before the pool so a hard-excluded provider never enters it: a current
	// blackhole verdict or TLS-authentication failure keeps a provider out of
	// every cached sample, force_minimum's included, and out of the stability
	// filter's counts. Shared with UpdateClientLocations, so the gated
	// membership and the advertised count can never disagree about a provider.
	//
	// # Staleness
	//
	// The index and its verdict are the rollup's, bounded by
	// EgressIndexSettings.EvidenceMaxAge; the observed country is bounded by
	// ProviderEgressLocationMaxAge and the blackhole verdict by
	// ProviderBlackholeCheckMaxAge. The TLS bit has no age: it is positive
	// evidence of an unsafe path, cleared only by a later clean run. A stale
	// *good* run stops being evidence and its provider is decided as unprobed;
	// a stale *bad* one likewise, and the full-probe queue stays independent of
	// every one of these rules, so an excluded provider is always re-measured
	// (TestProbeDueQueueIgnoresTheEgressHealthGate). Its only negative-evidence
	// exception is a current blackhole failure: that independently proves the
	// fixed tunnel cannot carry any destination, and the cheaper blackhole
	// queue retries it without full-probe backoff.
	egressTestEnabled := providerEgressTestEnabled()
	egressSettings := egressIndexSettings()
	countFilter := newProviderCountFilter(ctx, egressTestEnabled)
	clientIdEgresses := map[server.Id]*clientScoreEgress{}
	hardExcludedClientIds := map[server.Id]bool{}

	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
	        SELECT
	        	network_client_location_reliability.city_location_id,
	        	network_client_location_reliability.region_location_id,
	        	network_client_location_reliability.country_location_id,
	            network_client_location_reliability.client_id,
	            network_client_location_reliability.network_id,
	            network_client_location_reliability.max_net_type_score,
	            network_client_location_reliability.max_net_type_score_speed,
	            network_client_location_reliability.min_relative_latency_ms,
	            network_client_location_reliability.max_bytes_per_second,
	            network_client_location_reliability.has_latency_test,
	            network_client_location_reliability.has_speed_test,
	            -- fix(beta): see the LEFT JOIN comment below
	            COALESCE(client_connection_reliability_score.lookback_index, 0),
	            COALESCE(client_connection_reliability_score.reliability_weight, 1),
	            COALESCE(client_connection_reliability_score.independent_reliability_weight, 1),
	            -- publicly usable: holds a Public provide key, so a caller from
	            -- any network can settle a contract with it. A provider that is
	            -- in the pool without this is Network-only, and FindProviders2
	            -- hands it out to callers in its own network only.
	            EXISTS (
	            	SELECT 1 FROM provide_key
	            	WHERE
	            		provide_key.client_id = network_client_location_reliability.client_id AND
	            		provide_key.provide_mode = $1
	            ),
	            COALESCE(provider_egress_health.reputation_failed_names, ''),
	            network_client_location_reliability.ipv4_proven,
	            network_client_location_reliability.ipv6_proven,
	            -- the egress index (connect/GEOMAP.md §10.4) and the country
	            -- the provider is published under, which the country gate reads
	            network_client_location_reliability.egress_index,
	            network_client_location_reliability.egress_quality,
	            country_location.country_code

	        FROM network_client_location_reliability

	        INNER JOIN network_client ON
	            network_client.client_id = network_client_location_reliability.client_id

	        LEFT JOIN location AS country_location ON
	            country_location.location_id = network_client_location_reliability.country_location_id

	        -- fix(beta): same class of issue as UpdateClientLocations above --
	        -- an INNER JOIN here requires a reliability score to already exist
	        -- before a client counts toward its location's stability filter at
	        -- all, which the reliability-scoring pipeline may never produce at
	        -- this env's small/cold-start scale. LEFT JOIN plus the COALESCE
	        -- defaults above treats an unscored client as neutral (full
	        -- weight, lookback 0) rather than excluding it outright.
	        LEFT JOIN client_connection_reliability_score ON
	        	client_connection_reliability_score.client_id = network_client_location_reliability.client_id
	        LEFT JOIN provider_egress_health ON
	                provider_egress_health.client_id = network_client_location_reliability.client_id AND
	                provider_egress_health.measured_at >= $3
	        WHERE
	            network_client.active = true AND
	            network_client.source_client_id IS NULL AND
	        	network_client_location_reliability.connected = true AND
	        	network_client_location_reliability.valid = true AND
	        	-- the candidate pool, unlike the public count in
	        	-- UpdateClientLocations, carries every provider that can settle
	        	-- a contract with *someone*: Public for any caller, Network for
	        	-- callers in the provider's own network. GetProvideRelationship
	        	-- only ever returns one of those two, so this pair is exactly
	        	-- the set CreateContract can accept. FindProviders2 then decides
	        	-- eligibility per request -- it has to be done there and not
	        	-- here, because the score cache is keyed by (forceMinimum,
	        	-- rankMode, locationId, callerLocationId) and is not
	        	-- network-scoped.
	        	--
	        	-- Restricting this to Public would remove Network-only
	        	-- providers from their own network's discovery, which works
	        	-- today via CreateContractNoEscrow. Stream is still excluded:
	        	-- resolveNonCompanionProvideMode can resolve a Stream-only
	        	-- destination as a companion, but that dead-ends at
	        	-- CreateCompanionTransferEscrow, which needs a pre-existing
	        	-- reverse origin contract, so it can never bootstrap a session.
	        	--
	        	-- GetProviderLocations gates on loadLocationStables, populated
	        	-- from here; see exportClientScores for why the ClientFilter it
	        	-- reads stays Public-only.
	        	EXISTS (
	        		SELECT 1 FROM provide_key
	        		WHERE
	        			provide_key.client_id = network_client_location_reliability.client_id AND
	        			provide_key.provide_mode IN ($1, $2)
	        	)
	        `,
			ProvideModePublic,
			ProvideModeNetwork,
			server.NowUtc().Add(-ProviderEgressHealthMaxAge).UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				sourceRows++
				lookbackClientScore, cityLocationId, regionLocationId, countryLocationId, reputationFailedNames, egress := loadClientScore(result)
				if countFilter.hasHardEgressFailure(lookbackClientScore.ClientId) {
					hardExcludedClientIds[lookbackClientScore.ClientId] = true
					continue
				}
				clientIdEgresses[lookbackClientScore.ClientId] = egress

				// top-level only; the lookback copies stay nil (see `ClientScore`)
				setLocationIds := func(clientScore *ClientScore) {
					clientScore.CityLocationId = cityLocationId
					clientScore.RegionLocationId = regionLocationId
					clientScore.CountryLocationId = countryLocationId
				}

				// once per distinct location id: a country-only client stores
				// its country id in all three columns (see
				// SetConnectionLocation), and a client belongs in a location's
				// pool once. The per-location map is keyed by client id so a
				// repeat is already absorbed, but going through the set makes
				// the intent explicit and keeps this loop in step with the
				// counting loop in UpdateClientLocations.
				for _, locationId := range distinctIds(
					cityLocationId,
					regionLocationId,
					countryLocationId,
				) {
					clientScores, ok := locationClientScores[locationId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationClientScores[locationId] = clientScores
					}
					setLocationIds(addClientScore(lookbackClientScore, reputationFailedNames, clientScores))
				}
			}
		})

		result, err = conn.Query(
			ctx,
			`
	            SELECT
	            	location_group_member_city.location_group_id AS city_location_group_id,
	            	location_group_member_region.location_group_id AS region_location_group_id,
	            	location_group_member_country.location_group_id AS country_location_group_id,
	                network_client_location_reliability.client_id,
	                network_client_location_reliability.network_id,
	                network_client_location_reliability.max_net_type_score,
	                network_client_location_reliability.max_net_type_score_speed,
	                network_client_location_reliability.min_relative_latency_ms,
		            network_client_location_reliability.max_bytes_per_second,
		            network_client_location_reliability.has_latency_test,
		            network_client_location_reliability.has_speed_test,
		            COALESCE(client_connection_reliability_score.lookback_index, 0),  -- fix(beta): see LEFT JOIN comment below
	                COALESCE(client_connection_reliability_score.reliability_weight, 1),
	                COALESCE(client_connection_reliability_score.independent_reliability_weight, 1),
	                -- publicly usable; see the per-location query above
	                EXISTS (
	                	SELECT 1 FROM provide_key
	                	WHERE
	                		provide_key.client_id = network_client_location_reliability.client_id AND
	                		provide_key.provide_mode = $1
	                ),
	                COALESCE(provider_egress_health.reputation_failed_names, ''),
	                network_client_location_reliability.ipv4_proven,
	                network_client_location_reliability.ipv6_proven,
	                -- the egress index and the published country; see the
	                -- per-location query above
	                network_client_location_reliability.egress_index,
	                network_client_location_reliability.egress_quality,
	                country_location.country_code

	            FROM network_client_location_reliability

	            INNER JOIN network_client ON
	                network_client.client_id = network_client_location_reliability.client_id

	            LEFT JOIN location AS country_location ON
	                country_location.location_id = network_client_location_reliability.country_location_id

	            -- fix(beta): same class of issue as UpdateClientLocations/the query
            -- above this one -- treats an unscored client as neutral rather
            -- than excluding it, since the reliability-scoring pipeline may
            -- never populate at this env's small/cold-start scale
	            LEFT JOIN client_connection_reliability_score ON
	        		client_connection_reliability_score.client_id = network_client_location_reliability.client_id
	            LEFT JOIN provider_egress_health ON
	                    provider_egress_health.client_id = network_client_location_reliability.client_id AND
	                    provider_egress_health.measured_at >= $3

	            LEFT JOIN location_group_member location_group_member_city ON
	                location_group_member_city.location_id = network_client_location_reliability.city_location_id

	            LEFT JOIN location_group_member location_group_member_region ON
	                location_group_member_region.location_id = network_client_location_reliability.region_location_id

	            LEFT JOIN location_group_member location_group_member_country ON
	                location_group_member_country.location_id = network_client_location_reliability.country_location_id

	            WHERE
	                network_client.active = true AND
	                network_client.source_client_id IS NULL AND
	            	network_client_location_reliability.connected = true AND
	            	network_client_location_reliability.valid = true AND
	            	-- same rule as the per-location query above: Public or
	            	-- Network. This one fills locationGroupClientScores -> the
	            	-- clientScoreLocationGroup* redis keys -> loadClientScores
	            	-- -> FindProviders2 whenever a spec carries a
	            	-- LocationGroupId, so a user who picks a promoted group
	            	-- (e.g. "Strong Privacy Laws") must be filtered by the same
	            	-- request-time network check as a plain location.
	            	EXISTS (
	            		SELECT 1 FROM provide_key
	            		WHERE
	            			provide_key.client_id = network_client_location_reliability.client_id AND
	            			provide_key.provide_mode IN ($1, $2)
	            	)
	        `,
			ProvideModePublic,
			ProvideModeNetwork,
			server.NowUtc().Add(-ProviderEgressHealthMaxAge).UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				sourceRows++
				lookbackClientScore, cityLocationGroupId, regionLocationGroupId, countryLocationGroupId, reputationFailedNames, egress := loadClientScore(result)
				if countFilter.hasHardEgressFailure(lookbackClientScore.ClientId) {
					hardExcludedClientIds[lookbackClientScore.ClientId] = true
					continue
				}
				clientIdEgresses[lookbackClientScore.ClientId] = egress

				// once per distinct group id. The three location columns can
				// be the same id (a country-only client), in which case all
				// three group joins resolve to the same membership rows.
				for _, locationGroupId := range distinctIds(
					cityLocationGroupId,
					regionLocationGroupId,
					countryLocationGroupId,
				) {
					clientScores, ok := locationGroupClientScores[locationGroupId]
					if !ok {
						clientScores = map[server.Id]*ClientScore{}
						locationGroupClientScores[locationGroupId] = clientScores
					}
					addClientScore(lookbackClientScore, reputationFailedNames, clientScores)
				}
			}
		})
	})

	type filter struct {
		maxScore                         int
		minIndependentReliabilityWeights map[int]float64
		// minBytesPerSecond                ByteCount
		// maxRelativeLatencyMillis         int
	}
	// filters are tested in order of declaration for `MinExportNetReliabilityWeight`
	// to minimize the chance of bad providers in the `FindProviders2` randomized shuffle
	// the last filter represents the worst case the network will expose to users
	minFilter := filter{
		maxScore: 2 * scorePerTier,
	}
	// keyed by lookback index (see ClientLookbacks): 1 = the hour, 2 = 12h.
	// The hour threshold is the gate that decides whether a provider is in the
	// market at all. It sat at 0.99 -- less than one bad block in 60 -- which
	// left no slack for a reconnect that straddles a block boundary (the block
	// carrying only the new-connection sync has no established sync, so it is
	// invalid however tolerant the rule is). 0.95 allows three such blocks an
	// hour. Repeated reconnects still fail it: they are real user impact, and
	// `client_reliability_valid` only forgives ONE per block.
	if NormalNetworkConditions() {
		minFilter.minIndependentReliabilityWeights = map[int]float64{
			1: float64(0.95),
			2: float64(0.7),
			3: float64(0.6),
		}
	} else {
		// some abormal conditions, loosen the stats as they reset
		minFilter.minIndependentReliabilityWeights = map[int]float64{
			1: float64(0.8),
			2: float64(0.6),
			3: float64(0.6),
		}
	}
	minReliabilityWeightScale := 0.1
	maxReliabilityWeightScale := 1.0
	minScoreScale := 0.1
	maxScoreScale := 1.0

	// The rules of connect/GEOMAP.md §10.3 for every provider of the pool, once
	// each: a client sits in several location and group maps, each with its
	// own ClientScore, and must be decided the same way in all of them.
	clientIdEgressDecisions := map[server.Id]providerEgressDecision{}
	reasonCounts := map[string]int{}
	onlineCount := 0
	for clientId, egress := range clientIdEgresses {
		decision := decideProviderEgress(
			countFilter.egressFacts(
				clientId,
				egress.publishedCountryCode,
				egress.egressIndex,
				egress.egressQuality,
				egressSettings,
			),
			egressTestEnabled,
		)
		clientIdEgressDecisions[clientId] = decision
		if decision.reason != "" {
			reasonCounts[decision.reason] += 1
		}
		if decision.online {
			onlineCount += 1
		}
	}
	glog.Infof(
		"[nclm]egress rules: %d providers decided, %d hard excluded from the pool, out of a bucket by reason %v, %d unprobed for the online bucket\n",
		len(clientIdEgressDecisions),
		len(hardExcludedClientIds),
		reasonCounts,
		onlineCount,
	)

	// migration: set each client score to the lowest lookback index index
	migrateClientScore := func(clientScore *ClientScore) {
		lookbackIndexes := slices.Collect(maps.Keys(clientScore.LookbackClientScores))
		slices.Sort(lookbackIndexes)
		minLookbackIndex := lookbackIndexes[0]

		minClientScore := clientScore.LookbackClientScores[minLookbackIndex]

		clientScore.Scores = minClientScore.Scores
		clientScore.ReliabilityWeight = minClientScore.ReliabilityWeight
		clientScore.IndependentReliabilityWeight = minClientScore.IndependentReliabilityWeight
		clientScore.Tiers = minClientScore.Tiers
		clientScore.MinRelativeLatencyMillis = minClientScore.MinRelativeLatencyMillis
		clientScore.MaxBytesPerSecond = minClientScore.MaxBytesPerSecond
		clientScore.HasLatencyTest = minClientScore.HasLatencyTest
		clientScore.HasSpeedTest = minClientScore.HasSpeedTest
		clientScore.minimumScores = minClientScore.minimumScores

		clientScore.ScaledWeights = map[string]float32{}
		clientScore.PassesMinimums = map[string]bool{}

		// Bucket membership seeds each mode's minimum (decideProviderEgress):
		// quality and speed differ exactly where the 90 % rule removes a
		// provider from quality alone. It is a gate and nothing else: it can
		// only take a provider out of a mode, and the scaled-weight
		// arithmetic below reads the same minimum score for every provider
		// that qualifies.
		decision := clientIdEgressDecisions[clientScore.ClientId]
		rankModePassesBucket := map[RankMode]bool{
			RankModeQuality: decision.quality,
			RankModeSpeed:   decision.speed,
		}

		// The online bucket (connect/GEOMAP.md §10.3) is every provider with
		// no probe verdict past the exclusions and the gate that passes every
		// minimum the other buckets apply apart from the probe's: per
		// lookback, the independent weight floor that decides whether a
		// provider is in the market at all, and the score maximum over the
		// speed-mode score -- the performance adjustment with the missing-test
		// penalties, on a base of 0 as in the speed bucket. A provider no
		// client has measured carries both penalties, 80 capped at
		// MaxClientScore against a maximum of 40, so it stays out exactly as
		// it did before the buckets; one past a speed cutoff scores 0 there
		// and passes, as it passes the speed bucket's minimum.
		passesOnlineMinimums := true
		for lookbackIndex, lookbackClientScore := range clientScore.LookbackClientScores {
			if lookbackClientScore.IndependentReliabilityWeight < minFilter.minIndependentReliabilityWeights[lookbackIndex] {
				passesOnlineMinimums = false
				break
			}
			if minFilter.maxScore <= lookbackClientScore.minimumScores[RankModeSpeed] {
				passesOnlineMinimums = false
				break
			}
		}
		clientScore.Online = decision.online && passesOnlineMinimums

		for _, rankMode := range slices.Collect(maps.Keys(clientScore.Scores)) {
			passesMinimum := rankModePassesBucket[rankMode]
			// all lookback thresholds must pass
			for lookbackIndex, lookbackClientScore := range clientScore.LookbackClientScores {
				if lookbackClientScore.IndependentReliabilityWeight < minFilter.minIndependentReliabilityWeights[lookbackIndex] {
					passesMinimum = false
					break
				}
				if minFilter.maxScore <= lookbackClientScore.minimumScores[rankMode] {
					passesMinimum = false
					break
				}
			}

			if passesMinimum {
				u := float64(minClientScore.IndependentReliabilityWeight-minFilter.minIndependentReliabilityWeights[minLookbackIndex]) / (1.0 - minFilter.minIndependentReliabilityWeights[minLookbackIndex])
				reliabilityWeightScale := (1-u)*minReliabilityWeightScale + u*maxReliabilityWeightScale
				v := float64(minFilter.maxScore-clientScore.minimumScores[rankMode]) / float64(minFilter.maxScore)
				scoreScale := (1-v)*minScoreScale + v*maxScoreScale
				clientScore.ScaledWeights[rankMode] = float32(reliabilityWeightScale * clientScore.ReliabilityWeight * scoreScale)
				clientScore.PassesMinimums[rankMode] = true
			}
		}
	}
	for _, clientScores := range locationClientScores {
		for _, clientScore := range clientScores {
			migrateClientScore(clientScore)
		}
	}
	for _, clientScores := range locationGroupClientScores {
		for _, clientScore := range clientScores {
			migrateClientScore(clientScore)
		}
	}
	// splitClientScoreSamples shuffles and buckets one list of scores into
	// ClientScoreSampleCount-sized samples. Encode on demand so 48 parallel
	// caller-location exporters retain at most one sample each, rather than
	// every encoded sample for their current provider location. The returned
	// bytes move directly into the 512-item/8MiB streaming writer and are
	// cleared after the synchronous Exec.
	splitClientScoreSamples := func(clientScores []*ClientScore) (countsBytes []byte, counts []int, encodeSample func(int) []byte) {
		mathrand.Shuffle(len(clientScores), func(i int, j int) {
			clientScores[i], clientScores[j] = clientScores[j], clientScores[i]
		})

		n := (len(clientScores) + ClientScoreSampleCount - 1) / ClientScoreSampleCount

		counts = make([]int, n)
		clientsPerSample := 0
		if 0 < n {
			clientsPerSample = (len(clientScores) + n - 1) / n
			for i := range n {
				i0 := i * clientsPerSample
				i1 := min((i+1)*clientsPerSample, len(clientScores))
				counts[i] = i1 - i0
			}
		}
		encodeSample = func(i int) []byte {
			i0 := i * clientsPerSample
			i1 := min((i+1)*clientsPerSample, len(clientScores))
			return encodeClientScoreGobValue(updateClientScoresPhaseMetrics, clientScores[i0:i1])
		}

		countsBytes = encodeClientScoreGobValue(updateClientScoresPhaseMetrics, counts)
		return
	}

	exportClientScores := func(forceMinimum bool, rankMode RankMode, s map[server.Id]*ClientScore) clientScoreExportPayload {
		mapSpan := updateClientScoresPhaseMetrics.start(updateClientScoresPhaseTargetMap)
		defer mapSpan.finish()
		clientScores := []*ClientScore{}
		facetClientScores := map[ipFamilyFacet][]*ClientScore{}
		publicCount := 0
		publicNetReliabilityWeight := float64(0)
		for _, clientScore := range s {
			// an online provider sits in both modes' samples, native to
			// neither: FindProviders2 borrows from it last
			if clientScore.PassesMinimums[rankMode] || clientScore.Online || forceMinimum {
				clientScores = append(clientScores, clientScore)
				facet := clientScore.ipFamilyFacet()
				facetClientScores[facet] = append(facetClientScores[facet], clientScore)
				if !clientScore.NetworkOnly {
					publicCount += 1
					publicNetReliabilityWeight += clientScore.ReliabilityWeight
				}
			}
		}

		// the samples above carry Network-only providers too -- FindProviders2
		// filters them per request against the caller's network -- but the
		// ClientFilter does not. It is read only by loadLocationStables, which
		// decides the `Stable` flag GetProviderLocations publishes to every
		// user. That is a public surface, so like the provider count in
		// UpdateClientLocations it counts only providers a stranger can reach:
		// a location whose only supply is network-only is not stable, and with
		// zero public providers it reports no providers at all.
		filter := &ClientFilter{
			Count:                publicCount,
			NetReliabilityWeight: publicNetReliabilityWeight,
		}

		payload := clientScoreExportPayload{
			facets: map[ipFamilyFacet]clientScoreFacetPayload{},
		}
		updateClientScoresPhaseMetrics.addWork(updateClientScoresPhaseTargetMap, len(s), 0)
		mapSpan.finish()
		payload.countsBytes, payload.counts, payload.encodeSample = splitClientScoreSamples(clientScores)
		// every facet, an empty one included: see the key layout comment
		for _, facet := range ipFamilyFacets {
			facetPayload := clientScoreFacetPayload{}
			facetPayload.countsBytes, facetPayload.counts, facetPayload.encodeSample = splitClientScoreSamples(facetClientScores[facet])
			payload.facets[facet] = facetPayload
		}

		payload.filterBytes = encodeClientScoreGobValue(updateClientScoresPhaseMetrics, filter)

		return payload
	}

	// location id -> network id
	excludeLocationNetworkIds := map[server.Id]map[server.Id]bool{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				network_id,
				client_location_id
			FROM exclude_network_client_location
			`,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				sourceRows++
				var networkId server.Id
				var clientLocationId server.Id
				server.Raise(result.Scan(
					&networkId,
					&clientLocationId,
				))
				networkIds, ok := excludeLocationNetworkIds[clientLocationId]
				if !ok {
					networkIds = map[server.Id]bool{}
					excludeLocationNetworkIds[clientLocationId] = networkIds
				}
				networkIds[networkId] = true
			}
		})
	})

	clientLocationIds := []server.Id{
		// no client location match
		server.Id{},
	}
	clientLocationIds = append(clientLocationIds, slices.Collect(maps.Values(countryCodeLocationIds()))...)

	type clientScoreTarget struct {
		id            server.Id
		locationGroup bool
		clientScores  map[server.Id]*ClientScore
	}
	targets := make([]clientScoreTarget, 0, len(locationClientScores)+len(locationGroupClientScores))
	for locationId, clientScores := range locationClientScores {
		targets = append(targets, clientScoreTarget{id: locationId, clientScores: clientScores})
	}
	for locationGroupId, clientScores := range locationGroupClientScores {
		targets = append(targets, clientScoreTarget{
			id:            locationGroupId,
			locationGroup: true,
			clientScores:  clientScores,
		})
	}
	updateClientScoresPhaseMetrics.addWork(updateClientScoresPhaseSourceLoad, sourceRows, 0)
	sourceLoadSpan.finish()

	var wg sync.WaitGroup
	var exportCount atomic.Uint32
	workerLimit := max(1, parallel)
	returnErrs := make(chan error, workerLimit)
	targetExportTotal := 2 * len(performanceTargets) * len(targets)
	targetBlockSize := 0
	if len(targets) != 0 {
		targetBlockSize = (len(targets) + workerLimit - 1) / workerLimit
	}

	for i := 0; i < len(targets); i += targetBlockSize {
		blockTargets := targets[i:min(len(targets), i+targetBlockSize)]

		wg.Add(1)
		go func() {
			defer wg.Done()
			connect.HandleError(func() {
				server.Redis(ctx, func(r server.RedisClient) {
					for _, forceMinimum := range []bool{false, true} {
						for rankMode, _ := range performanceTargets {
							// The sets are independent and hash to different cluster slots, so
							// multi/exec cannot span them. Encode directly into the bounded
							// writer. Partitioning by target, rather than caller, lets one gob
							// payload fan out to every caller whose blocked-network set leaves
							// that target unchanged.
							err := writeClientScoreRedisStream(ctx, r, ttl, func(emit func(clientScoreRedisSet) error) error {
								for _, target := range blockTargets {
									exportIndex := exportCount.Add(1)
									kind := "location"
									keys := clientScoreTargetKeys{}
									if target.locationGroup {
										kind = "location_group"
										keys = clientScoreTargetKeys{
											counts: func(callerId server.Id) string {
												return clientScoreLocationGroupCountsKey(forceMinimum, rankMode, target.id, callerId)
											},
											filter: func(callerId server.Id) string {
												return clientScoreLocationGroupFilterKey(forceMinimum, rankMode, target.id, callerId)
											},
											sample: func(callerId server.Id, sampleIndex int) string {
												return clientScoreLocationGroupSampleKey(forceMinimum, rankMode, target.id, callerId, sampleIndex)
											},
											alias: func(callerId server.Id) string {
												return clientScoreLocationGroupAliasKey(forceMinimum, rankMode, target.id, callerId)
											},
											facetCounts: func(callerId server.Id, facet ipFamilyFacet) string {
												return clientScoreLocationGroupFacetCountsKey(forceMinimum, rankMode, target.id, callerId, facet)
											},
											facetSample: func(callerId server.Id, facet ipFamilyFacet, sampleIndex int) string {
												return clientScoreLocationGroupFacetSampleKey(forceMinimum, rankMode, target.id, callerId, facet, sampleIndex)
											},
										}
									} else {
										keys = clientScoreTargetKeys{
											counts: func(callerId server.Id) string {
												return clientScoreLocationCountsKey(forceMinimum, rankMode, target.id, callerId)
											},
											filter: func(callerId server.Id) string {
												return clientScoreLocationFilterKey(forceMinimum, rankMode, target.id, callerId)
											},
											sample: func(callerId server.Id, sampleIndex int) string {
												return clientScoreLocationSampleKey(forceMinimum, rankMode, target.id, callerId, sampleIndex)
											},
											alias: func(callerId server.Id) string {
												return clientScoreLocationAliasKey(forceMinimum, rankMode, target.id, callerId)
											},
											facetCounts: func(callerId server.Id, facet ipFamilyFacet) string {
												return clientScoreLocationFacetCountsKey(forceMinimum, rankMode, target.id, callerId, facet)
											},
											facetSample: func(callerId server.Id, facet ipFamilyFacet, sampleIndex int) string {
												return clientScoreLocationFacetSampleKey(forceMinimum, rankMode, target.id, callerId, facet, sampleIndex)
											},
										}
									}
									// Target-oriented export has thousands more progress units than
									// the old caller-oriented loop. Keep production progress useful
									// without turning the root CPU fix into a log-volume regression.
									if exportIndex == 1 || exportIndex%100 == 0 || int(exportIndex) == targetExportTotal {
										glog.Infof(
											"[nclm]export client score target[%d/%d] %s=%s callers=%d\n",
											exportIndex,
											targetExportTotal,
											kind,
											target.id,
											len(clientLocationIds),
										)
									}
									if err := emitClientScoreTargetFanout(
										clientLocationIds,
										target.clientScores,
										excludeLocationNetworkIds,
										keys,
										func(clientScores map[server.Id]*ClientScore) clientScoreExportPayload {
											return exportClientScores(forceMinimum, rankMode, clientScores)
										},
										writeLegacyUnchanged,
										writeUnfacetedPayload,
										emit,
									); err != nil {
										return err
									}
									glog.V(2).Infof(
										"[nclm]updated client score target %s=%s for %d callers\n",
										kind,
										target.id,
										len(clientLocationIds),
									)
								}
								return nil
							})
							if err != nil {
								select {
								case <-ctx.Done():
									return
								case returnErrs <- err:
									return
								}
							}
						}
					}
				})
			}, func(err error) {
				// A recovered worker panic must prevent publication of the alias
				// migration marker. Keep wg.Done outside HandleError so the error
				// reaches this bounded channel before the waiter can close it.
				select {
				case <-ctx.Done():
				case returnErrs <- fmt.Errorf("client score export worker panic: %w", err):
				}
			})
		}()
	}

	wg.Wait()
	close(returnErrs)

	func() {
		for {
			select {
			case <-ctx.Done():
				return
			case err, ok := <-returnErrs:
				if !ok {
					return
				}
				returnErr = errors.Join(returnErr, err)
			}
		}
	}()

	if returnErr == nil {
		if writeLegacyUnchanged {
			if err := markClientScoreAliasReady(ctx); err != nil {
				return fmt.Errorf("publish client score alias migration state: %w", err)
			}
			glog.Infof("[nclm]client score alias schema ready; legacy duplicate payloads will expire naturally\n")
		}
		if err := markClientScoreProviderEligibilityReady(ctx); err != nil {
			return fmt.Errorf("publish client score provider eligibility state: %w", err)
		}
		if writeUnfacetedPayload {
			if err := markClientScoreIpFamilyReady(ctx); err != nil {
				return fmt.Errorf("publish client score ip family migration state: %w", err)
			}
			glog.Infof("[nclm]client score ip family facets ready; un-faceted payloads will expire naturally\n")
		}
		glog.Infof("[nclm]client score provider eligibility ready; derived and inactive clients excluded\n")
		glog.Infof(
			"[nclm]update %d client locations x %d location scores, %d location group scores\n",
			len(clientLocationIds),
			len(locationClientScores),
			len(locationGroupClientScores),
		)
	} else {
		glog.Infof("[nclm]update err = %s\n", returnErr)
	}

	return
}

// loadClientScores draws up to n scores for the requested targets from the
// cache, facet by facet in preference order (connect/IPV6.md A8): every
// sample of the first facet is eligible before any of the second, so a
// v4-capable request that finds enough dualstack providers never reaches the
// v4-only buckets. A target whose facets are absent was written by an
// exporter without facets; its un-faceted buckets are drawn last and their
// legacy scores read as v4-only, so a v6 request finds nothing in them,
// which is the truth about what such a cache proves.
func loadClientScores(
	forceMinimum bool,
	rankMode RankMode,
	ctx context.Context,
	locationIds map[server.Id]bool,
	locationGroupIds map[server.Id]bool,
	clientLocationId server.Id,
	n int,
	facets []ipFamilyFacet,
) (clientScores map[server.Id]*ClientScore, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		type countsRead struct {
			caller   *redis.StringCmd
			baseline *redis.StringCmd
		}
		type targetRead struct {
			alias    *redis.StringCmd
			unfacted countsRead
			facets   map[ipFamilyFacet]countsRead
		}
		locationReads := map[server.Id]targetRead{}
		locationGroupReads := map[server.Id]targetRead{}

		// plain pipeline instead of tx: independent gets across cluster slots
		pipe := r.Pipeline()
		for locationId, _ := range locationIds {
			read := targetRead{
				unfacted: countsRead{
					caller: pipe.Get(ctx, clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, clientLocationId)),
				},
				facets: map[ipFamilyFacet]countsRead{},
			}
			if clientLocationId != (server.Id{}) {
				read.alias = pipe.Get(ctx, clientScoreLocationAliasKey(forceMinimum, rankMode, locationId, clientLocationId))
				read.unfacted.baseline = pipe.Get(ctx, clientScoreLocationCountsKey(forceMinimum, rankMode, locationId, server.Id{}))
			}
			for _, facet := range facets {
				facetRead := countsRead{
					caller: pipe.Get(ctx, clientScoreLocationFacetCountsKey(forceMinimum, rankMode, locationId, clientLocationId, facet)),
				}
				if clientLocationId != (server.Id{}) {
					facetRead.baseline = pipe.Get(ctx, clientScoreLocationFacetCountsKey(forceMinimum, rankMode, locationId, server.Id{}, facet))
				}
				read.facets[facet] = facetRead
			}
			locationReads[locationId] = read
		}
		for locationGroupId, _ := range locationGroupIds {
			read := targetRead{
				unfacted: countsRead{
					caller: pipe.Get(ctx, clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, clientLocationId)),
				},
				facets: map[ipFamilyFacet]countsRead{},
			}
			if clientLocationId != (server.Id{}) {
				read.alias = pipe.Get(ctx, clientScoreLocationGroupAliasKey(forceMinimum, rankMode, locationGroupId, clientLocationId))
				read.unfacted.baseline = pipe.Get(ctx, clientScoreLocationGroupCountsKey(forceMinimum, rankMode, locationGroupId, server.Id{}))
			}
			for _, facet := range facets {
				facetRead := countsRead{
					caller: pipe.Get(ctx, clientScoreLocationGroupFacetCountsKey(forceMinimum, rankMode, locationGroupId, clientLocationId, facet)),
				}
				if clientLocationId != (server.Id{}) {
					facetRead.baseline = pipe.Get(ctx, clientScoreLocationGroupFacetCountsKey(forceMinimum, rankMode, locationGroupId, server.Id{}, facet))
				}
				read.facets[facet] = facetRead
			}
			locationGroupReads[locationGroupId] = read
		}
		// note ignore the error for GET since it will include missing key
		pipe.Exec(ctx)

		// sample keys grouped by draw order: one group per requested facet,
		// then the un-faceted fallback group
		groupCount := len(facets) + 1
		groupSampleKeyCounts := make([]map[string]int, groupCount)
		for i := range groupSampleKeyCounts {
			groupSampleKeyCounts[i] = map[string]int{}
		}

		decodeCounts := func(alias *redis.StringCmd, read countsRead) (effectiveClientLocationId server.Id, counts []int, ok bool) {
			effectiveClientLocationId, countsBytes := selectClientScorePayload(
				clientLocationId,
				clientScoreCommandBytes(alias),
				clientScoreCommandBytes(read.caller),
				clientScoreCommandBytes(read.baseline),
			)
			if len(countsBytes) == 0 {
				return
			}
			b := bytes.NewBuffer(countsBytes)
			e := gob.NewDecoder(b)
			if err := e.Decode(&counts); err != nil {
				returnErr = err
				return
			}
			ok = true
			return
		}

		addTarget := func(
			read targetRead,
			facetSampleKey func(effectiveClientLocationId server.Id, facet ipFamilyFacet, index int) string,
			sampleKey func(effectiveClientLocationId server.Id, index int) string,
		) {
			faceted := false
			for facetIndex, facet := range facets {
				effectiveClientLocationId, counts, ok := decodeCounts(read.alias, read.facets[facet])
				if returnErr != nil {
					return
				}
				if !ok {
					continue
				}
				faceted = true
				for i, count := range counts {
					groupSampleKeyCounts[facetIndex][facetSampleKey(effectiveClientLocationId, facet, i)] = count
				}
			}
			if faceted {
				return
			}
			// written before the facets existed: fall back to the un-faceted buckets
			effectiveClientLocationId, counts, ok := decodeCounts(read.alias, read.unfacted)
			if returnErr != nil || !ok {
				return
			}
			for i, count := range counts {
				groupSampleKeyCounts[groupCount-1][sampleKey(effectiveClientLocationId, i)] = count
			}
		}

		for locationId, read := range locationReads {
			addTarget(
				read,
				func(effectiveClientLocationId server.Id, facet ipFamilyFacet, index int) string {
					return clientScoreLocationFacetSampleKey(forceMinimum, rankMode, locationId, effectiveClientLocationId, facet, index)
				},
				func(effectiveClientLocationId server.Id, index int) string {
					return clientScoreLocationSampleKey(forceMinimum, rankMode, locationId, effectiveClientLocationId, index)
				},
			)
			if returnErr != nil {
				return
			}
		}
		for locationGroupId, read := range locationGroupReads {
			addTarget(
				read,
				func(effectiveClientLocationId server.Id, facet ipFamilyFacet, index int) string {
					return clientScoreLocationGroupFacetSampleKey(forceMinimum, rankMode, locationGroupId, effectiveClientLocationId, facet, index)
				},
				func(effectiveClientLocationId server.Id, index int) string {
					return clientScoreLocationGroupSampleKey(forceMinimum, rankMode, locationGroupId, effectiveClientLocationId, index)
				},
			)
			if returnErr != nil {
				return
			}
		}

		samples := []*redis.StringCmd{}
		netCount := 0

		pipe = r.Pipeline()
		for _, sampleKeyCounts := range groupSampleKeyCounts {
			if n <= netCount {
				break
			}
			keys := slices.Collect(maps.Keys(sampleKeyCounts))
			mathrand.Shuffle(len(keys), func(i int, j int) {
				keys[i], keys[j] = keys[j], keys[i]
			})
			for _, key := range keys {
				if n <= netCount {
					break
				}
				c := sampleKeyCounts[key]
				v := pipe.Get(ctx, key)
				samples = append(samples, v)
				netCount += c
			}
		}
		// note ignore the error for GET since it will include missing key
		pipe.Exec(ctx)

		clientScores = map[server.Id]*ClientScore{}

		for _, sampleCmd := range samples {
			sampleBytes, _ := sampleCmd.Bytes()
			if len(sampleBytes) == 0 {
				continue
			}
			b := bytes.NewBuffer(sampleBytes)
			e := gob.NewDecoder(b)
			var sample []*ClientScore
			returnErr = e.Decode(&sample)
			if returnErr != nil {
				return
			}

			for _, clientScore := range sample {
				// a client can appear under multiple requested keys with
				// identical ranking fields, but only location-keyed samples
				// carry location ids. keep the copy that has them.
				if existing, ok := clientScores[clientScore.ClientId]; ok {
					if existing.CountryLocationId != nil && clientScore.CountryLocationId == nil {
						continue
					}
				}
				clientScores[clientScore.ClientId] = clientScore
			}
		}
	})

	return
}

// the response location from the score's cached location ids and the
// process-local directory. nil when the ids are unknown (cache blobs written
// before the ids were cached, group-keyed samples) or when the directory has
// not loaded yet or cannot resolve the country.
func resolveProviderLocation(
	directory map[server.Id]*locationDirectoryEntry,
	clientScore *ClientScore,
) *ProviderLocation {
	if clientScore.CountryLocationId == nil {
		return nil
	}
	countryEntry := directory[*clientScore.CountryLocationId]
	if countryEntry == nil {
		return nil
	}

	location := &ProviderLocation{
		Country:           countryEntry.Name,
		CountryCode:       countryEntry.CountryCode,
		CountryLocationId: clientScore.CountryLocationId,
	}
	if clientScore.RegionLocationId != nil {
		if regionEntry := directory[*clientScore.RegionLocationId]; regionEntry != nil {
			location.Region = regionEntry.Name
			location.RegionLocationId = clientScore.RegionLocationId
			if lat, lon, ok := centroidFor(countryEntry.CountryCode, regionEntry.Name); ok {
				location.RegionCoordinates = &LocationCoordinates{
					Lat: lat,
					Lon: lon,
				}
			}
		}
	}
	if clientScore.CityLocationId != nil {
		if cityEntry := directory[*clientScore.CityLocationId]; cityEntry != nil {
			location.City = cityEntry.Name
			location.CityLocationId = clientScore.CityLocationId
			if cityEntry.Latitude != nil && cityEntry.Longitude != nil {
				location.CityCoordinates = &LocationCoordinates{
					Lat: *cityEntry.Latitude,
					Lon: *cityEntry.Longitude,
				}
			}
		}
	}
	return location
}

// findProvidersProviderFromClientScore is the cache-to-wire boundary. Keep
// discovery metadata assembled in one pure helper so reputation and
// same-network eligibility cannot be silently dropped by one response path.
func findProvidersProviderFromClientScore(
	clientScore *ClientScore,
	rankMode RankMode,
	directory map[server.Id]*locationDirectoryEntry,
) *FindProvidersProvider {
	return &FindProvidersProvider{
		ClientId:                   clientScore.ClientId,
		Tier:                       clientScore.Tiers[rankMode],
		EstimatedBytesPerSecond:    clientScore.MaxBytesPerSecond,
		HasEstimatedBytesPerSecond: clientScore.HasSpeedTest,
		NetworkOnly:                clientScore.NetworkOnly,
		ReputationFailedNames:      clientScore.ReputationFailedNames,
		Location:                   resolveProviderLocation(directory, clientScore),
		IpFamily:                   string(clientScore.IpFamily()),
	}
}

func FindProviders2(
	findProviders2 *FindProviders2Args,
	session *session.ClientSession,
) (*FindProviders2Result, error) {
	providers := []*FindProvidersProvider{}
	callerCountryCode := ""

	// an unknown filter is refused before any spec is read: a filter this
	// server cannot interpret must not silently widen to "any family"
	facets, err := ipFamilyFacetsForFilter(findProviders2.IpFamily)
	if err != nil {
		return nil, err
	}

	locationIds := map[server.Id]bool{}
	locationGroupIds := map[server.Id]bool{}

	excludeFinalDestinations := sync.OnceValue(func() map[server.Id]bool {
		excludeFinalDestinations := map[server.Id]bool{}
		for _, clientId := range findProviders2.ExcludeClientIds {
			excludeFinalDestinations[clientId] = true
		}
		for _, destination := range findProviders2.ExcludeDestinations {
			excludeFinalDestinations[destination[len(destination)-1]] = true
		}
		return excludeFinalDestinations
	})

	// the providers specs name by client id, held until the hard exclusions
	// are read
	specClientIds := []server.Id{}
	for _, spec := range findProviders2.Specs {
		if spec.LocationId != nil {
			locationIds[*spec.LocationId] = true
		}
		if spec.LocationGroupId != nil {
			locationGroupIds[*spec.LocationGroupId] = true
		}
		if spec.ClientId != nil {
			clientId := *(spec.ClientId)
			if !excludeFinalDestinations()[clientId] {
				specClientIds = append(specClientIds, clientId)
			}
		}
		if spec.BestAvailable {
			homeLocationId, ok := countryCodeLocationIds()["us"]
			if ok {
				locationIds[homeLocationId] = true
			}
		}
	}

	// A provider named by client id bypasses discovery, and with it every
	// minimum: an explicit choice by the caller, such as reconnecting to a
	// known provider, reaches a provider no bucket admits. The hard exclusions
	// of connect/GEOMAP.md §10.3 are not minimums, so they hold here too: a
	// blackholed provider is unusable and a TLS-intercepting one unsafe, and no
	// caller's choice changes either. Named providers come first, in spec
	// order, as they always have.
	appendSpecProviders := func(hardExcludedClientIds map[server.Id]bool) {
		for _, clientId := range specClientIds {
			if hardExcludedClientIds[clientId] {
				continue
			}
			providers = append(providers, &FindProvidersProvider{
				ClientId: clientId,
			})
		}
	}

	if 0 < len(locationIds) || 0 < len(locationGroupIds) {
		// use a min block size to reduce db activity
		var count int
		if findProviders2.ForceCount {
			count = findProviders2.Count
		} else {
			count = max(findProviders2.Count, 20)
		}

		// the random process is
		// 1. load (ideally this would be all, but is truncated for performance)
		// 2. sample based on reliability * quality
		// 3. band based on tier and keep the top `count`
		minLoadCount := 1000
		loadMultiplier := 10

		rankMode := RankModeQuality
		if findProviders2.RankMode != "" {
			rankMode = findProviders2.RankMode
		}

		// the caller ip is used to match against provider excluded lists
		clientIp, _, err := session.ParseClientIpPort()
		if err != nil {
			return nil, err
		}

		ipInfo, err := server.GetIpInfo(clientIp)
		if err != nil {
			return nil, err
		}
		callerCountryCode = ipInfo.CountryCode

		clientLocationId := countryCodeLocationIds()[ipInfo.CountryCode]

		loadStartTime := time.Now()
		clientScores, err := loadClientScores(
			findProviders2.ForceMinimum,
			rankMode,
			session.Ctx,
			locationIds,
			locationGroupIds,
			clientLocationId,
			max(loadMultiplier*count, minLoadCount),
			facets,
		)
		if err != nil {
			return nil, err
		}
		loadEndTime := time.Now()
		loadDuration := loadEndTime.Sub(loadStartTime)
		loadMillis := float64(loadDuration) / float64(time.Millisecond)
		findProviders2LoadSeconds.Observe(loadDuration.Seconds())
		// one provider search per call makes this client-driven volume: the
		// histogram is the signal, the slow-case line is V(1) detail
		if 50*time.Millisecond <= loadDuration && glog.V(1) {
			glog.Infof(
				"[nclm]findproviders2 load %.2fms (%d)\n",
				loadMillis,
				len(clientScores),
			)
		}

		// The hard exclusions (connect/GEOMAP.md §10.3), read once for every
		// provider this call could return, and applied here where the result
		// is assembled rather than only where the pool's minimums were: the
		// cached pool already leaves out what its last export saw excluded,
		// but that export can be an hour old, a verdict issued since must hold
		// now, and force_minimum reads the same pool with no minimums at all.
		candidateClientIds := slices.Clone(specClientIds)
		for clientId := range clientScores {
			candidateClientIds = append(candidateClientIds, clientId)
		}
		hardExcludedClientIds, err := getProviderHardExclusions(session.Ctx, candidateClientIds)
		if err != nil {
			return nil, err
		}
		appendSpecProviders(hardExcludedClientIds)
		exclusionReadClientIds := map[server.Id]bool{}
		for _, clientId := range candidateClientIds {
			exclusionReadClientIds[clientId] = true
		}

		// drop providers this caller cannot contract with.
		//
		// UpdateClientScores puts both Public and Network-only providers in the
		// pool. A Network-only provider can only settle a contract with a
		// source in its own network (GetProvideRelationship ->
		// ProvideModeNetwork -> CreateContractNoEscrow); it is real, usable
		// supply for those users and has always been discoverable to them, so
		// it stays. For anyone else CreateContract would reject with
		// NoPermission, so it goes.
		//
		// This must happen here, after loadClientScores, and must never be
		// baked into the cached set: the client score redis entries are keyed
		// by (forceMinimum, rankMode, locationId, callerLocationId) with no
		// network component, so a single cached set is shared by callers from
		// every network.
		//
		// A session with no jwt yields the zero network id, which matches no
		// provider -- fail closed rather than leak.
		var callerNetworkId server.Id
		if session.ByJwt != nil {
			callerNetworkId = session.ByJwt.NetworkId
		}

		// Applies the request's eligibility to one mode's loaded pool, then the
		// request's weights in that mode. It runs on the requested mode's pool
		// and, when that comes up short, on the other mode's, so a borrowed
		// provider passes exactly what a native one does.
		filterPool := func(clientScores map[server.Id]*ClientScore, mode RankMode, hardExcludedClientIds map[server.Id]bool) {
			for clientId := range hardExcludedClientIds {
				delete(clientScores, clientId)
			}
			for clientId, clientScore := range clientScores {
				if clientScore.NetworkOnly && clientScore.NetworkId != callerNetworkId {
					delete(clientScores, clientId)
				}
			}

			// a cache written without facets fell back to its un-faceted
			// buckets, whose scores carry no proven family and read as v4-only
			for clientId, clientScore := range clientScores {
				if !slices.Contains(facets, clientScore.ipFamilyFacet()) {
					delete(clientScores, clientId)
				}
			}

			for clientId, _ := range excludeFinalDestinations() {
				delete(clientScores, clientId)
			}
			if findProviders2.ForceMinimum {
				for _, clientScore := range clientScores {
					clientScore.ScaledWeights[mode] = 1.0
				}
			}
			// the final hop is excluded
			// intermediaries have score reduced
			intermediaryScale := float32(0.5)
			for _, destination := range findProviders2.ExcludeDestinations {
				for _, clientId := range destination[:len(destination)-1] {
					if clientScore, ok := clientScores[clientId]; ok {
						clientScore.ScaledWeights[mode] *= intermediaryScale
					}
				}
			}
		}
		filterPool(clientScores, rankMode, hardExcludedClientIds)

		// Draws up to n of the candidates by their weight in `mode` and bands
		// the draw by their tier in `mode`. Weighted selection and tier banding
		// run within each facet, and the preferred facet fills n first: a
		// v4-capable request takes every dualstack provider it can before any
		// v4-only one, so the single-family category only ever tops up a
		// shortfall.
		selectProviders := func(candidateClientScores map[server.Id]*ClientScore, mode RankMode, n int) []server.Id {
			clientIds := []server.Id{}
			for _, facet := range facets {
				remainingCount := n - len(clientIds)
				if remainingCount <= 0 {
					break
				}
				facetClientIds := []server.Id{}
				for clientId, clientScore := range candidateClientScores {
					if clientScore.ipFamilyFacet() == facet {
						facetClientIds = append(facetClientIds, clientId)
					}
				}
				mathrand.Shuffle(len(facetClientIds), func(i int, j int) {
					facetClientIds[i], facetClientIds[j] = facetClientIds[j], facetClientIds[i]
				})

				connect.WeightedSelectFunc(facetClientIds, remainingCount, func(clientId server.Id) float32 {
					clientScore := candidateClientScores[clientId]
					return clientScore.ScaledWeights[mode]
				})
				facetClientIds = facetClientIds[:min(remainingCount, len(facetClientIds))]

				// band by tier
				slices.SortStableFunc(facetClientIds, func(a server.Id, b server.Id) int {
					clientScoreA := candidateClientScores[a]
					clientScoreB := candidateClientScores[b]

					return clientScoreA.Tiers[mode] - clientScoreB.Tiers[mode]
				})
				clientIds = append(clientIds, facetClientIds...)
			}
			return clientIds
		}

		// Takes up to n of the online bucket in its order
		// (connect/GEOMAP.md §10.3): reliability weight, highest first, then
		// the speed-mode performance adjustment -- the client-measured latency
		// and throughput, past the speed cutoffs last; a missing test's
		// penalty alone reaches the score maximum, so no online provider
		// carries one -- within each facet in the request's order.
		selectOnline := func(candidateClientScores map[server.Id]*ClientScore, n int) []server.Id {
			clientIds := []server.Id{}
			for _, facet := range facets {
				remainingCount := n - len(clientIds)
				if remainingCount <= 0 {
					break
				}
				facetClientIds := []server.Id{}
				for clientId, clientScore := range candidateClientScores {
					if clientScore.ipFamilyFacet() == facet {
						facetClientIds = append(facetClientIds, clientId)
					}
				}
				// full ties in no particular order
				mathrand.Shuffle(len(facetClientIds), func(i int, j int) {
					facetClientIds[i], facetClientIds[j] = facetClientIds[j], facetClientIds[i]
				})
				slices.SortStableFunc(facetClientIds, func(a server.Id, b server.Id) int {
					clientScoreA := candidateClientScores[a]
					clientScoreB := candidateClientScores[b]
					switch {
					case clientScoreB.ReliabilityWeight < clientScoreA.ReliabilityWeight:
						return -1
					case clientScoreA.ReliabilityWeight < clientScoreB.ReliabilityWeight:
						return 1
					}
					if d := clientScoreA.Tiers[RankModeSpeed] - clientScoreB.Tiers[RankModeSpeed]; d != 0 {
						return d
					}
					return clientScoreA.Scores[RankModeSpeed] - clientScoreB.Scores[RankModeSpeed]
				})
				clientIds = append(clientIds, facetClientIds[:min(remainingCount, len(facetClientIds))]...)
			}
			return clientIds
		}

		// The natives: the requested mode's own providers within its latency
		// and throughput cutoffs. A provider past a cutoff carries the top
		// tier (ClientScoreCutoffTier) and stays in the mode's pool, but it is
		// not what the mode promises, so it waits behind the other bucket in
		// the backfill below rather than being drawn beside the natives --
		// where its zero score would also have given it the largest weight. An
		// online provider is in the sample but native to no mode. With
		// force_minimum, the caller's blanket override, the whole pool is
		// native, as always.
		nativeClientScores := clientScores
		if !findProviders2.ForceMinimum {
			nativeClientScores = map[server.Id]*ClientScore{}
			for clientId, clientScore := range clientScores {
				if !clientScore.Online && clientScore.Tiers[rankMode] < ClientScoreCutoffTier {
					nativeClientScores[clientId] = clientScore
				}
			}
		}
		clientIds := selectProviders(nativeClientScores, rankMode, count)

		directory := locationDirectory()

		// output in order of `clientIds`
		for _, clientId := range clientIds {
			clientScore := clientScores[clientId]
			providers = append(providers, findProvidersProviderFromClientScore(clientScore, rankMode, directory))
		}

		// Backfill (connect/GEOMAP.md §10.3): a bucket short of the request's
		// count is filled from the others in a fixed order, so a location with
		// few quality providers still answers a quality request, one with few
		// fast providers a speed request, and a mass probe failure -- every
		// verdict gone, or every verdict wrong -- still answers from what real
		// traffic proves.
		//
		//  1. The other bucket's natives that are not natives here, in the
		//     other bucket's order: quality short borrows the providers speed
		//     holds and quality does not (over the one-in-ten line), speed
		//     short the quality providers its cutoffs excluded.
		//  2. The probed providers of either bucket past that bucket's cutoffs,
		//     the requested bucket's first: they are the buckets', only slow,
		//     and last in either bucket's order.
		//  3. The online bucket, last, in its own order.
		//
		// A borrowed provider keeps its tier in the mode it came from plus
		// BackfillTierOffset, so every native ranks ahead of every borrowed
		// one on the client and the borrowed keep their order. The online
		// bucket is no mode and its order is reliability first, which no tier
		// of a mode expresses without inverting it on the client, so every
		// online provider carries the one tier twice the offset: behind every
		// other borrowed provider, in the answer's order among themselves.
		//
		// Nothing crosses an exclusion. The other mode's pool is its
		// non-forced cache, which leaves out the hard-excluded and the
		// country-gated exactly as this one does, and it passes the same
		// request-time filters, the hard exclusions included. force_minimum
		// is never backfilled. The other mode's set is an addition, never a
		// requirement: a set that is not cached, or that cannot be read,
		// leaves the answer to this mode's own sample.
		chosenClientIds := slices.Clone(clientIds)
		if otherRankMode, ok := backfillRankMode(rankMode); ok && !findProviders2.ForceMinimum {
			borrowedClientIds := []server.Id{}
			remainingCount := func() int {
				return count - len(clientIds) - len(borrowedClientIds)
			}
			borrow := func(clientScore *ClientScore, mode RankMode, tier int) {
				provider := findProvidersProviderFromClientScore(clientScore, mode, directory)
				provider.Tier = tier
				providers = append(providers, provider)
				borrowedClientIds = append(borrowedClientIds, clientScore.ClientId)
			}
			if 0 < remainingCount() {
				// the settings as a request path reads them: at most their own
				// RequestSettingsMaxAge old, re-read by the first request past
				// that age (two racing past it both read the file, which is
				// harmless)
				settingsSnapshot := requestEgressIndexSettingsSnapshot.Load()
				if settingsSnapshot == nil || settingsSnapshot.settings.RequestSettingsMaxAge <= time.Since(settingsSnapshot.loadTime) {
					settingsSnapshot = &egressIndexSettingsSnapshot{
						settings: egressIndexSettings(),
						loadTime: time.Now(),
					}
					requestEgressIndexSettingsSnapshot.Store(settingsSnapshot)
				}
				backfillTierOffset := settingsSnapshot.settings.BackfillTierOffset

				otherClientScores, err := loadClientScores(
					false,
					otherRankMode,
					session.Ctx,
					locationIds,
					locationGroupIds,
					clientLocationId,
					max(loadMultiplier*count, minLoadCount),
					facets,
				)
				if err != nil {
					glog.Infof("[nclm]findproviders2 could not read the %s set to backfill %s; answering from the %s sample alone (%s)\n", otherRankMode, rankMode, rankMode, err)
					otherClientScores = map[server.Id]*ClientScore{}
				}
				// the exclusions of the providers this call has not read yet
				unreadClientIds := []server.Id{}
				for clientId := range otherClientScores {
					if !exclusionReadClientIds[clientId] {
						unreadClientIds = append(unreadClientIds, clientId)
					}
				}
				otherHardExcludedClientIds, err := getProviderHardExclusions(session.Ctx, unreadClientIds)
				if err != nil {
					return nil, err
				}
				for clientId := range hardExcludedClientIds {
					otherHardExcludedClientIds[clientId] = true
				}
				filterPool(otherClientScores, otherRankMode, otherHardExcludedClientIds)

				// 1. the other bucket's natives
				borrowableClientScores := map[server.Id]*ClientScore{}
				for clientId, clientScore := range otherClientScores {
					if clientScore.Online || ClientScoreCutoffTier <= clientScore.Tiers[otherRankMode] {
						continue
					}
					if _, native := nativeClientScores[clientId]; native {
						continue
					}
					borrowableClientScores[clientId] = clientScore
				}
				for _, clientId := range selectProviders(borrowableClientScores, otherRankMode, remainingCount()) {
					clientScore := otherClientScores[clientId]
					borrow(clientScore, otherRankMode, clientScore.Tiers[otherRankMode]+backfillTierOffset)
				}

				// 2. the probed past their bucket's cutoffs, this bucket's first
				ownSlowClientScores := map[server.Id]*ClientScore{}
				for clientId, clientScore := range clientScores {
					if clientScore.Online || clientScore.Tiers[rankMode] < ClientScoreCutoffTier {
						continue
					}
					if _, held := borrowableClientScores[clientId]; held {
						continue
					}
					ownSlowClientScores[clientId] = clientScore
				}
				for _, clientId := range selectProviders(ownSlowClientScores, rankMode, remainingCount()) {
					clientScore := clientScores[clientId]
					borrow(clientScore, rankMode, clientScore.Tiers[rankMode]+backfillTierOffset)
				}
				otherSlowClientScores := map[server.Id]*ClientScore{}
				for clientId, clientScore := range otherClientScores {
					if clientScore.Online || clientScore.Tiers[otherRankMode] < ClientScoreCutoffTier {
						continue
					}
					if _, native := nativeClientScores[clientId]; native {
						continue
					}
					if _, taken := ownSlowClientScores[clientId]; taken {
						continue
					}
					otherSlowClientScores[clientId] = clientScore
				}
				for _, clientId := range selectProviders(otherSlowClientScores, otherRankMode, remainingCount()) {
					clientScore := otherClientScores[clientId]
					borrow(clientScore, otherRankMode, clientScore.Tiers[otherRankMode]+backfillTierOffset)
				}

				// 3. the online bucket, which both modes' samples carry alike
				onlineClientScores := map[server.Id]*ClientScore{}
				for clientId, clientScore := range clientScores {
					if clientScore.Online {
						onlineClientScores[clientId] = clientScore
					}
				}
				for _, clientId := range selectOnline(onlineClientScores, remainingCount()) {
					borrow(clientScores[clientId], RankModeSpeed, 2*backfillTierOffset)
				}
			}
			chosenClientIds = append(chosenClientIds, borrowedClientIds...)
			findProviders2BackfillProviders.WithLabelValues(rankMode).Observe(float64(len(borrowedClientIds)))
			findProviders2AnsweredProviders.WithLabelValues(rankMode).Add(float64(len(clientIds) + len(borrowedClientIds)))
		}

		// export one anonymized stats sample tracing this call's pool and
		// selection. Best-effort and gated on stats being enabled, so it is
		// inert unless a process opts in (see recordFindProviders2Sample).
		if s := stats.Default(); s.Enabled() {
			recordFindProviders2Sample(
				s,
				findProviders2,
				rankMode,
				count,
				ipInfo.CountryCode,
				float64(loadDuration.Nanoseconds())/1e6,
				clientScores,
				chosenClientIds,
			)
		}
	} else {
		hardExcludedClientIds, err := getProviderHardExclusions(session.Ctx, specClientIds)
		if err != nil {
			return nil, err
		}
		appendSpecProviders(hardExcludedClientIds)
	}

	// record provider "search interest": each provider that appeared in this
	// result gets one match count, accumulated in redis (never pg on this hot
	// path) and rolled up by RollupSearchProviderStats. Best-effort.
	if 0 < len(providers) {
		providerClientIds := make([]server.Id, 0, len(providers))
		for _, provider := range providers {
			providerClientIds = append(providerClientIds, provider.ClientId)
		}
		RecordProviderSearchMatches(session.Ctx, providerClientIds, server.NowUtc())
	}

	result := &FindProviders2Result{
		Providers: providers,
	}
	recordFindProviders2Outcome(findProviders2, callerCountryCode, len(result.Providers))
	return result, nil
}

type CreateProviderSpecArgs struct {
	Query string `json:"query"`
}

type CreateProviderSpecResult struct {
	Specs []*ProviderSpec `json:"specs"`
}

func CreateProviderSpec(
	createProviderSpec *CreateProviderSpecArgs,
	session *session.ClientSession,
) (*CreateProviderSpecResult, error) {
	// TODO: parse the free-text query into location/group provider specs.
	// Until that resolver exists, return an empty (spec-conformant) result
	// rather than erroring, so the route behaves per the spec shape.
	return &CreateProviderSpecResult{
		Specs: []*ProviderSpec{},
	}, nil
}
