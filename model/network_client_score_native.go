// Native score pages are published separately from the legacy online union.
// Two guarded slots retain the last complete snapshot through a failed refresh.
package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

const clientScoreNativeBaseline = "b"

var errClientScoreNativeUnavailable = errors.New("native client score source unavailable")

// Native aliases become visible only after the baseline snapshot commits.
// They share the bounded SET stream and its TTL/error handling; staging and
// committing native snapshots retain their guarded publication boundary.
func newClientScoreNativeFanout(ctx context.Context, r server.RedisClient, countsKey func(server.Id) string, ttl time.Duration, census *ClientScoreNativeCensus, emit func(clientScoreRedisSet) error) *clientScoreNativeFanout {
	return &clientScoreNativeFanout{
		publish: func(callerId server.Id, facets map[ipFamilyFacet]clientScoreFacetPayload) error {
			return writeClientScoreNativeSnapshot(ctx, r, clientScoreNativeKey(countsKey(callerId)), ttl, facets, census)
		},
		alias: func(callerId server.Id) error {
			return emit(clientScoreRedisSet{
				key: clientScoreNativeKey(countsKey(callerId)), value: []byte(clientScoreNativeBaseline),
			})
		},
	}
}

// Called after every exporter has joined and its error channel is closed.
// Cancellation cannot discard worker errors or certify complete publication.
func finishClientScoreExport(ctx context.Context, workerErrs <-chan error) error {
	var result error
	for err := range workerErrs {
		result = errors.Join(result, err)
	}
	return errors.Join(result, ctx.Err())
}

// Evidence can expire after publication. Do not retain an entire expired
// native snapshot while refilling; online fallback reads its separate source.
func retainNativeClientScores(scores map[server.Id]*ClientScore, mode RankMode) {
	for clientId, score := range scores {
		if !score.PassesMinimums[mode] {
			delete(scores, clientId)
		}
	}
}

type clientScoreNativePageInfo struct {
	Count    int
	Checksum [sha256.Size]byte
}

type clientScoreNativeManifest struct {
	Generation        string
	PublicationId     string
	SourceStartedAt   time.Time
	SourceCompletedAt time.Time
	Facets            map[ipFamilyFacet][]clientScoreNativePageInfo
}

type clientScoreNativePointer struct {
	slot       int
	generation string
	checksum   [sha256.Size]byte
}

// Reuse the caller/target hash tag, not a global hot slot or per-export keys.
func clientScoreNativeKey(countsKey string) string {
	return countsKey + ":native_v1"
}

func clientScoreNativeSlotKey(pointerKey string, slot int) string {
	return fmt.Sprintf("%s:%d", pointerKey, slot)
}

func clientScoreNativePageField(facet ipFamilyFacet, index int) string {
	return fmt.Sprintf("%s:%d", facet, index)
}

func parseClientScoreNativePointer(value string) (clientScoreNativePointer, error) {
	parts := strings.Split(value, ":")
	if len(parts) != 3 || (parts[0] != "0" && parts[0] != "1") || len(parts[1]) == 0 || 64 < len(parts[1]) {
		return clientScoreNativePointer{}, fmt.Errorf("%w: invalid pointer", errClientScoreNativeUnavailable)
	}
	checksum, err := hex.DecodeString(parts[2])
	if err != nil || len(checksum) != sha256.Size {
		return clientScoreNativePointer{}, fmt.Errorf("%w: invalid manifest checksum", errClientScoreNativeUnavailable)
	}
	pointer := clientScoreNativePointer{slot: int(parts[0][0] - '0'), generation: parts[1]}
	copy(pointer.checksum[:], checksum)
	return pointer, nil
}

// All script keys retain one caller/target hash tag. A stale publisher may
// supersede another inactive staging attempt, but cannot write the active slot.
const clientScoreNativeBeginScript = `
local active = redis.call('GET', KEYS[1]) or ''
if active ~= ARGV[1] or string.sub(active, 1, 2) == ARGV[4] .. ':' then return 0 end
redis.call('DEL', KEYS[2])
redis.call('HSET', KEYS[2], 'g', ARGV[2])
redis.call('PEXPIRE', KEYS[2], ARGV[3])
return 1
`

const clientScoreNativePagesScript = `
if (redis.call('GET', KEYS[1]) or '') ~= ARGV[1] or redis.call('HGET', KEYS[2], 'g') ~= ARGV[2] then return 0 end
for i = 4, #ARGV, 2 do redis.call('HSET', KEYS[2], ARGV[i], ARGV[i + 1]) end
redis.call('PEXPIRE', KEYS[2], ARGV[3])
return 1
`

const clientScoreNativeCommitScript = `
if redis.call('GET', KEYS[1]) == ARGV[5] and redis.call('HGET', KEYS[2], 'g') == ARGV[2] and redis.call('HGET', KEYS[2], 'm') == ARGV[4] then return 1 end
if (redis.call('GET', KEYS[1]) or '') ~= ARGV[1] or redis.call('HGET', KEYS[2], 'g') ~= ARGV[2] then return 0 end
if redis.call('HLEN', KEYS[2]) ~= 1 + (#ARGV - 5) / 2 then return 0 end
for i = 6, #ARGV, 2 do
  local page = redis.call('HGET', KEYS[2], ARGV[i])
  if not page or string.sub(page, 1, 32) ~= ARGV[i + 1] then return 0 end
end
redis.call('HSET', KEYS[2], 'm', ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[3])
redis.call('SET', KEYS[1], ARGV[5], 'PX', ARGV[3])
return 1
`

// A snapshot owns one inactive slot. Expected page names are recorded while
// streaming, not reconstructed from possibly partial Redis contents at commit.
type clientScoreNativePublication struct {
	pointerKey string
	slotKey    string
	expected   string
	generation string
	slot       int
	ttl        time.Duration
	pageFields []string
}

func beginClientScoreNativePublication(ctx context.Context, r server.RedisClient, key string, ttl time.Duration) (*clientScoreNativePublication, error) {
	if ttl < time.Millisecond {
		return nil, fmt.Errorf("native client score publication requires positive ttl")
	}
	active, err := r.Get(ctx, key).Result()
	if err != nil && err != redis.Nil {
		return nil, err
	}
	slot := 0
	if active != "" && active != clientScoreNativeBaseline {
		pointer, err := parseClientScoreNativePointer(active)
		if err != nil {
			return nil, err
		}
		slot = 1 - pointer.slot
	}
	publication := &clientScoreNativePublication{
		pointerKey: key, slotKey: clientScoreNativeSlotKey(key, slot),
		expected: active, generation: server.NewId().String(), slot: slot, ttl: ttl,
	}
	result, err := r.Eval(ctx, clientScoreNativeBeginScript, []string{key, publication.slotKey}, active, publication.generation, ttl.Milliseconds(), slot).Int()
	if err != nil {
		return nil, err
	}
	if result != 1 {
		return nil, fmt.Errorf("native client score publisher lost its staging generation")
	}
	return publication, nil
}

func (self *clientScoreNativePublication) writePages(ctx context.Context, r server.RedisClient, pages []clientScoreRedisSet) error {
	args := []any{self.expected, self.generation, self.ttl.Milliseconds()}
	for _, page := range pages {
		args = append(args, page.key, page.value)
	}
	result, err := r.Eval(ctx, clientScoreNativePagesScript, []string{self.pointerKey, self.slotKey}, args...).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return fmt.Errorf("native client score publisher lost its staging generation")
	}
	for _, page := range pages {
		self.pageFields = append(self.pageFields, page.key)
	}
	return nil
}

func (self *clientScoreNativePublication) commit(ctx context.Context, r server.RedisClient, manifest clientScoreNativeManifest) error {
	if manifest.Generation != self.generation || len(manifest.Facets) != len(ipFamilyFacets) {
		return fmt.Errorf("native client score manifest does not describe this publication")
	}
	data := encodeClientScoreGobValue(updateClientScoresPhaseMetrics, manifest)
	checksum := sha256.Sum256(data)
	pointer := fmt.Sprintf("%d:%s:%x", self.slot, self.generation, checksum)
	args := []any{self.expected, self.generation, self.ttl.Milliseconds(), data, pointer}
	written := map[string]bool{}
	for _, field := range self.pageFields {
		if written[field] {
			return fmt.Errorf("native client score publication repeated a page")
		}
		written[field] = true
	}
	for _, facet := range ipFamilyFacets {
		pages, ok := manifest.Facets[facet]
		if !ok {
			return fmt.Errorf("native client score manifest omitted a facet")
		}
		for index, page := range pages {
			field := clientScoreNativePageField(facet, index)
			if !written[field] || page.Count <= 0 || ClientScoreSampleCount < page.Count {
				return fmt.Errorf("native client score manifest has an unwritten page")
			}
			delete(written, field)
			args = append(args, field, page.Checksum[:])
		}
	}
	if len(written) != 0 {
		return fmt.Errorf("native client score manifest omitted written pages")
	}
	result, err := r.Eval(ctx, clientScoreNativeCommitScript, []string{self.pointerKey, self.slotKey}, args...).Int()
	if err != nil {
		return err
	}
	if result != 1 {
		return fmt.Errorf("native client score snapshot incomplete or superseded")
	}
	return nil
}

// The encoder already owns native-only, bounded pages. Retain only one
// bounded write batch and small per-page count/checksum metadata during export.
func writeClientScoreNativeSnapshot(ctx context.Context, r server.RedisClient, key string, ttl time.Duration, facets map[ipFamilyFacet]clientScoreFacetPayload, sources ...*ClientScoreNativeCensus) error {
	publication, err := beginClientScoreNativePublication(ctx, r, key, ttl)
	if err != nil {
		return err
	}
	manifest := clientScoreNativeManifest{Generation: publication.generation, Facets: map[ipFamilyFacet][]clientScoreNativePageInfo{}}
	if 0 < len(sources) && sources[0] != nil {
		manifest.PublicationId = sources[0].PublicationId
		manifest.SourceStartedAt = sources[0].SourceStartedAt
		manifest.SourceCompletedAt = sources[0].SourceCompletedAt
	}
	err = runClientScoreExportStream(ctx, clientScoreExportBatchSize, clientScoreExportBatchBytes, 1,
		func(emit func(clientScoreRedisSet) error) error {
			for _, facet := range ipFamilyFacets {
				payload, ok := facets[facet]
				if !ok {
					return fmt.Errorf("native client score publication omitted a facet")
				}
				manifest.Facets[facet] = []clientScoreNativePageInfo{}
				for index, count := range payload.counts {
					if count <= 0 || ClientScoreSampleCount < count {
						return fmt.Errorf("native client score publication has invalid page size")
					}
					data := payload.encodeSample(index)
					checksum := sha256.Sum256(data)
					manifest.Facets[facet] = append(manifest.Facets[facet], clientScoreNativePageInfo{Count: count, Checksum: checksum})
					value := append(checksum[:sha256.Size:sha256.Size], data...)
					if err := emit(clientScoreRedisSet{key: clientScoreNativePageField(facet, index), value: value}); err != nil {
						return err
					}
				}
			}
			return nil
		},
		func(pages []clientScoreRedisSet) error { return publication.writePages(ctx, r, pages) },
		func(context.Context, int) error { return nil },
	)
	if err != nil {
		return err
	}
	return publication.commit(ctx, r, manifest)
}

// An immutable pointer identifies one complete target snapshot. Aliases are
// resolved before this read; generation and checksum make a reused old slot
// unavailable instead of letting its pages become false native exhaustion.
func readClientScoreNativeManifest(ctx context.Context, r server.RedisClient, key, rawPointer string) (*clientScoreNativeManifest, string, error) {
	pointer, err := parseClientScoreNativePointer(rawPointer)
	if err != nil {
		return nil, "", err
	}
	slotKey := clientScoreNativeSlotKey(key, pointer.slot)
	values, err := r.HMGet(ctx, slotKey, "g", "m").Result()
	if err != nil {
		return nil, "", err
	}
	if len(values) != 2 {
		return nil, "", fmt.Errorf("%w: incomplete manifest response", errClientScoreNativeUnavailable)
	}
	generation, generationOk := values[0].(string)
	data, dataOk := values[1].(string)
	if !generationOk || !dataOk || generation != pointer.generation || sha256.Sum256([]byte(data)) != pointer.checksum {
		return nil, "", fmt.Errorf("%w: missing or changed manifest", errClientScoreNativeUnavailable)
	}
	manifest := &clientScoreNativeManifest{}
	if err := gob.NewDecoder(bytes.NewBufferString(data)).Decode(manifest); err != nil {
		return nil, "", fmt.Errorf("%w: invalid manifest", errClientScoreNativeUnavailable)
	}
	if manifest.Generation != generation || len(manifest.Facets) != len(ipFamilyFacets) {
		return nil, "", fmt.Errorf("%w: incomplete manifest", errClientScoreNativeUnavailable)
	}
	for _, facet := range ipFamilyFacets {
		pages, ok := manifest.Facets[facet]
		if !ok {
			return nil, "", fmt.Errorf("%w: missing facet", errClientScoreNativeUnavailable)
		}
		for _, page := range pages {
			if page.Count <= 0 || ClientScoreSampleCount < page.Count {
				return nil, "", fmt.Errorf("%w: invalid page size", errClientScoreNativeUnavailable)
			}
		}
	}
	return manifest, slotKey, nil
}

// Complete target metadata proves the native source's extent. Keep validated
// pages from other targets when one target is unavailable, but never turn that
// partial result into proof of exhaustion. No native metadata at all is the
// pre-schema compatibility case.
func loadNativeClientScoresWithCursor(ctx context.Context, mode RankMode, locations, groups map[server.Id]bool, caller server.Id, n int, facets []ipFamilyFacet, observation *findProviders2LoadObservation) (scores map[server.Id]*ClientScore, cursor *clientScoreCursor, available bool, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		type target struct {
			callerKey   string
			baselineKey string
			callerRead  *redis.StringCmd
			baseRead    *redis.StringCmd
		}
		targets := []target{}
		pipe := r.Pipeline()
		for _, set := range []struct {
			ids map[server.Id]bool
			key func(server.Id, server.Id) string
		}{
			{ids: locations, key: func(id, caller server.Id) string { return clientScoreLocationCountsKey(false, mode, id, caller) }},
			{ids: groups, key: func(id, caller server.Id) string { return clientScoreLocationGroupCountsKey(false, mode, id, caller) }},
		} {
			for id := range set.ids {
				key, baseline := clientScoreNativeKey(set.key(id, caller)), clientScoreNativeKey(set.key(id, server.Id{}))
				read := target{callerKey: key, baselineKey: baseline, callerRead: pipe.Get(ctx, key)}
				if key != baseline {
					read.baseRead = pipe.Get(ctx, baseline)
				}
				targets = append(targets, read)
			}
		}
		if err := execClientScoreReadPipeline(ctx, pipe); err != nil {
			clientScoreReadMetrics.counts.Inc()
			returnErr = err
			// Failed metadata is an unavailable native source, not evidence of
			// a legacy-only writer. Independently successful targets can still
			// prove their own manifests and pages. Cancellation remains fatal.
			available = true
			if ctx.Err() != nil {
				return
			}
		}
		pagesByFacet := make([]map[string]clientScorePage, len(facets))
		for i := range pagesByFacet {
			pagesByFacet[i] = map[string]clientScorePage{}
		}
		for _, target := range targets {
			if err := target.callerRead.Err(); err != nil && !errors.Is(err, redis.Nil) {
				continue
			}
			key, rawPointer := target.callerKey, string(clientScoreCommandBytes(target.callerRead))
			if rawPointer != "" {
				available = true
			}
			if rawPointer == clientScoreNativeBaseline {
				if target.baseRead == nil {
					returnErr = errors.Join(returnErr, fmt.Errorf("%w: baseline cannot alias itself", errClientScoreNativeUnavailable))
					continue
				}
				if err := target.baseRead.Err(); err != nil && !errors.Is(err, redis.Nil) {
					continue
				}
				key, rawPointer = target.baselineKey, string(clientScoreCommandBytes(target.baseRead))
				if rawPointer == "" {
					returnErr = errors.Join(returnErr, fmt.Errorf("%w: missing aliased baseline", errClientScoreNativeUnavailable))
					continue
				}
			}
			if rawPointer == "" {
				returnErr = errors.Join(returnErr, fmt.Errorf("%w: missing target metadata", errClientScoreNativeUnavailable))
				continue
			}
			manifest, slotKey, err := readClientScoreNativeManifest(ctx, r, key, rawPointer)
			if err != nil {
				returnErr = errors.Join(returnErr, err)
				continue
			}
			for i, facet := range facets {
				for index, info := range manifest.Facets[facet] {
					field := clientScoreNativePageField(facet, index)
					pagesByFacet[i][slotKey+":"+field] = clientScorePage{
						key: field, expectedCount: info.Count, nativeKey: slotKey,
						nativeGeneration: manifest.Generation, nativeChecksum: info.Checksum,
					}
				}
			}
		}
		if !available {
			// A legacy-only writer has not published this schema yet. Its
			// bounded cursor, not absent native metadata, establishes extent.
			returnErr = nil
			return
		}
		cursor = &clientScoreCursor{observation: observation, nativeOnly: true, sourceIncomplete: returnErr != nil}
		for _, pages := range pagesByFacet {
			group := []clientScorePage{}
			for _, page := range pages {
				group = append(group, page)
			}
			mathrand.Shuffle(len(group), func(i, j int) { group[i], group[j] = group[j], group[i] })
			cursor.pages = append(cursor.pages, group...)
		}
		var readErr error
		scores, readErr = cursor.readWithClient(ctx, r, n)
		returnErr = errors.Join(returnErr, readErr)
	})
	return
}

// Old readers keep their union keys. An explicitly enabled reader uses native pages only when
// every required target has the schema. A damaged generation can also use a
// bounded legacy read, retaining independently validated native rows. Such a
// source remains explicitly incomplete even when fallback restores availability.
func loadPreferredClientScoresWithCursor(nativeReaderEnabled, forceMinimum bool, mode RankMode, ctx context.Context, locations, groups map[server.Id]bool, caller server.Id, n int, facets []ipFamilyFacet, observations ...*findProviders2LoadObservation) (scores map[server.Id]*ClientScore, cursor *clientScoreCursor, returnErr error) {
	var observation *findProviders2LoadObservation
	if 0 < len(observations) {
		observation = observations[0]
	}
	if nativeReaderEnabled && !forceMinimum {
		var available bool
		scores, cursor, available, returnErr = loadNativeClientScoresWithCursor(ctx, mode, locations, groups, caller, n, facets, observation)
		if ctx.Err() != nil {
			returnErr = ctx.Err()
			return
		}
		if returnErr == nil && available {
			return
		}
		if returnErr != nil {
			nativeScores, nativeErr := scores, returnErr
			scores, cursor, returnErr = loadClientScoresWithCursor(false, mode, ctx, locations, groups, caller, n, facets, observations...)
			if scores == nil {
				scores = map[server.Id]*ClientScore{}
			}
			for clientId, score := range nativeScores {
				if scores[clientId] == nil {
					scores[clientId] = score
				}
			}
			if cursor != nil {
				cursor.sourceIncomplete = true
			}
			// Return the source diagnostic with the usable rows. Only the
			// selector decides whether to degrade to another bucket; callers
			// must not mistake this partial result for healthy exhaustion.
			returnErr = errors.Join(nativeErr, returnErr)
			return
		}
	}
	return loadClientScoresWithCursor(forceMinimum, mode, ctx, locations, groups, caller, n, facets, observations...)
}
