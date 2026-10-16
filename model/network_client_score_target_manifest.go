// The score export's memory of its own targets. A location target exists only
// while a source row names it (newClientScoreTargetMaps), so an export never
// revisits a location whose last provider left: without this the previous
// export's union pages, native snapshot and aliases stay readable until the
// cache ttl, and FindProviders2 keeps lending the departed providers. Each
// export now compares its targets with the previous export's and publishes
// encoded-empty documents for every target that no row names any more.
package model

import (
	"context"
	"encoding/json"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// The targets the score export wrote from its source rows and known groups,
// each with the unix second it was last so written.
type clientScoreTargetManifest struct {
	locationIds      map[server.Id]int64
	locationGroupIds map[server.Id]int64
	// Explicit deadlines preserve earlier exports with a different ttl.
	locationExpireMillis      map[server.Id]int64
	locationGroupExpireMillis map[server.Id]int64
}

// The Redis document of a manifest, under clientScoreTargetManifestKey.
type clientScoreTargetManifestDocument struct {
	LocationIds               map[string]int64 `json:"location_ids"`
	LocationGroupIds          map[string]int64 `json:"location_group_ids"`
	LocationExpireMillis      map[string]int64 `json:"location_expire_ms,omitempty"`
	LocationGroupExpireMillis map[string]int64 `json:"location_group_expire_ms,omitempty"`
}

// Every discoverable payload in one export shares its write-ahead deadline.
// An absent value preserves the relative ttl of standalone writer callers.
type clientScoreExportExpiryContextKey struct{}

// Reads the immutable deadline chosen before this export's first payload.
func clientScoreExportExpiresAt(ctx context.Context) time.Time {
	expiresAt, _ := ctx.Value(clientScoreExportExpiryContextKey{}).(time.Time)
	return expiresAt
}

// Creates empty, independently owned target and expiry maps.
func newClientScoreTargetManifest() *clientScoreTargetManifest {
	return &clientScoreTargetManifest{
		locationIds:               map[server.Id]int64{},
		locationGroupIds:          map[server.Id]int64{},
		locationExpireMillis:      map[server.Id]int64{},
		locationGroupExpireMillis: map[server.Id]int64{},
	}
}

// Every target of the maps, written from rows at writtenAt. A complete export
// publishes this after its last document.
func (self *clientScoreTargetMaps) targetManifest(writtenAt time.Time) *clientScoreTargetManifest {
	manifest := newClientScoreTargetManifest()
	// Whole-second legacy timestamps must never claim expiry before the
	// last fractional second's payload can expire.
	writtenSecond := writtenAt.Unix()
	if writtenAt.Nanosecond() != 0 {
		writtenSecond += 1
	}
	for locationId := range self.locationClientScores {
		manifest.locationIds[locationId] = writtenSecond
	}
	for locationGroupId := range self.locationGroupClientScores {
		manifest.locationGroupIds[locationGroupId] = writtenSecond
	}
	return manifest
}

// The departed targets one export also writes: an empty target for each
// target of the previous manifest that the maps no longer hold and whose
// payload deadline has not passed. Legacy documents use their last-write time
// plus ttl. Expired entries are dropped so retention stays finite.
//
// The returned manifest is what this export may leave readable. Publication
// atomically merges it with other exports and keeps each target through its
// last payload deadline, even after an earlier export cleared that target.
func (self *clientScoreTargetMaps) departedTargets(
	previous *clientScoreTargetManifest,
	now time.Time,
	ttl time.Duration,
) (departed []clientScoreTarget, manifest *clientScoreTargetManifest) {
	manifest = self.targetManifest(now)
	if previous == nil {
		return
	}
	readable := func(writtenAt int64, expireMillis int64) bool {
		if expireMillis != 0 {
			return now.UnixMilli() < expireMillis
		}
		return now.Before(time.Unix(writtenAt, 0).Add(ttl))
	}
	for locationId, writtenAt := range previous.locationIds {
		if _, ok := self.locationClientScores[locationId]; ok || !readable(writtenAt, previous.locationExpireMillis[locationId]) {
			continue
		}
		departed = append(departed, clientScoreTarget{
			id:           locationId,
			clientScores: map[server.Id]*ClientScore{},
		})
		manifest.locationIds[locationId] = writtenAt
		manifest.locationExpireMillis[locationId] = previous.locationExpireMillis[locationId]
	}
	for locationGroupId, writtenAt := range previous.locationGroupIds {
		if _, ok := self.locationGroupClientScores[locationGroupId]; ok || !readable(writtenAt, previous.locationGroupExpireMillis[locationGroupId]) {
			continue
		}
		departed = append(departed, clientScoreTarget{
			id:            locationGroupId,
			locationGroup: true,
			clientScores:  map[server.Id]*ClientScore{},
		})
		manifest.locationGroupIds[locationGroupId] = writtenAt
		manifest.locationGroupExpireMillis[locationGroupId] = previous.locationGroupExpireMillis[locationGroupId]
	}
	return
}

func encodeClientScoreTargetManifest(manifest *clientScoreTargetManifest) ([]byte, error) {
	document := clientScoreTargetManifestDocument{
		LocationIds:               make(map[string]int64, len(manifest.locationIds)),
		LocationGroupIds:          make(map[string]int64, len(manifest.locationGroupIds)),
		LocationExpireMillis:      make(map[string]int64, len(manifest.locationExpireMillis)),
		LocationGroupExpireMillis: make(map[string]int64, len(manifest.locationGroupExpireMillis)),
	}
	for locationId, writtenAt := range manifest.locationIds {
		document.LocationIds[locationId.String()] = writtenAt
	}
	for locationGroupId, writtenAt := range manifest.locationGroupIds {
		document.LocationGroupIds[locationGroupId.String()] = writtenAt
	}
	for locationId, expireMillis := range manifest.locationExpireMillis {
		document.LocationExpireMillis[locationId.String()] = expireMillis
	}
	for locationGroupId, expireMillis := range manifest.locationGroupExpireMillis {
		document.LocationGroupExpireMillis[locationGroupId.String()] = expireMillis
	}
	return json.Marshal(document)
}

func decodeClientScoreTargetManifest(value []byte) (*clientScoreTargetManifest, error) {
	var document clientScoreTargetManifestDocument
	if err := json.Unmarshal(value, &document); err != nil {
		return nil, err
	}
	manifest := newClientScoreTargetManifest()
	for _, ids := range []struct {
		document map[string]int64
		manifest map[server.Id]int64
	}{
		{document: document.LocationIds, manifest: manifest.locationIds},
		{document: document.LocationGroupIds, manifest: manifest.locationGroupIds},
		{document: document.LocationExpireMillis, manifest: manifest.locationExpireMillis},
		{document: document.LocationGroupExpireMillis, manifest: manifest.locationGroupExpireMillis},
	} {
		for idStr, writtenAt := range ids.document {
			id, err := server.ParseId(idStr)
			if err != nil {
				return nil, err
			}
			ids.manifest[id] = writtenAt
		}
	}
	return manifest, nil
}

// The previous export's manifest, or nil when there is none: the first
// export, an expired manifest, or one that does not decode, which this export
// then replaces. A read error fails the export before it writes anything, as
// the migration marker reads do.
func readClientScoreTargetManifest(ctx context.Context) (manifest *clientScoreTargetManifest, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		value, err := r.Get(ctx, clientScoreTargetManifestKey).Bytes()
		if err == redis.Nil {
			return
		}
		if err != nil {
			returnErr = err
			return
		}
		manifest, err = decodeClientScoreTargetManifest(value)
		if err != nil {
			glog.Errorf("[nclm]client score target manifest does not decode, so departed targets are not cleared this export: %s\n", err)
			manifest = nil
		}
	})
	return
}

// Merge overlapping exports atomically and retain each target through its
// latest payload deadline. A completed export cannot forget a slower writer's
// targets, and a shorter ttl cannot shorten an earlier generation's lifetime.
// Pruning bounds the single document to targets with unexpired payloads.
const clientScoreTargetManifestWriteScript = `
local incoming = cjson.decode(ARGV[1])
local clock = redis.call('TIME')
local now = math.max(tonumber(ARGV[2]), tonumber(clock[1]) * 1000 + math.floor(tonumber(clock[2]) / 1000))
local ttl = tonumber(ARGV[3])
local exportExpiry = tonumber(ARGV[4])
if ttl <= 0 or (exportExpiry > 0 and exportExpiry <= now) then
  return redis.error_reply('client score export expired')
end
local previous = {}
local value = redis.call('GET', KEYS[1])
if value then
  local ok, decoded = pcall(cjson.decode, value)
  if ok and type(decoded) == 'table' then previous = decoded end
end
local result = {}
local latestExpiry = 0
for _, names in ipairs({{'location_ids', 'location_expire_ms'}, {'location_group_ids', 'location_group_expire_ms'}}) do
  local targets, expirations = {}, {}
  for _, source in ipairs({previous, incoming}) do
    local sourceExpirations = source[names[2]] or {}
    if type(sourceExpirations) ~= 'table' then sourceExpirations = {} end
    local sourceTargets = source[names[1]] or {}
    if type(sourceTargets) == 'table' then
      for id, writtenAt in pairs(sourceTargets) do
        local validId = type(id) == 'string' and #id == 36 and
          string.sub(id, 9, 9) == '-' and string.sub(id, 14, 14) == '-' and
          string.sub(id, 19, 19) == '-' and string.sub(id, 24, 24) == '-' and
          #string.gsub(id, '-', '') == 32 and
          string.match(string.gsub(id, '-', ''), '^%x+$')
        if validId and type(writtenAt) == 'number' then
          local expiresAt = tonumber(sourceExpirations[id]) or 0
          if expiresAt <= 0 then
            expiresAt = writtenAt * 1000 + ttl
            if source == incoming and exportExpiry > 0 then expiresAt = exportExpiry end
          end
          if now < expiresAt and (not expirations[id] or expirations[id] < expiresAt) then
            targets[id], expirations[id] = writtenAt, expiresAt
            latestExpiry = math.max(latestExpiry, expiresAt)
          end
        end
      end
    end
  end
  result[names[1]], result[names[2]] = targets, expirations
end
if latestExpiry <= now then return redis.call('DEL', KEYS[1]) end
redis.call('SET', KEYS[1], cjson.encode(result), 'PXAT', latestExpiry)
return 1
`

// Publication and expiry are one Redis operation, including the write-ahead
// record left by an export that fails before its final manifest write.
func writeClientScoreTargetManifest(ctx context.Context, manifest *clientScoreTargetManifest, ttl time.Duration) (returnErr error) {
	value, err := encodeClientScoreTargetManifest(manifest)
	if err != nil {
		return err
	}
	expireMillis := int64(0)
	if expiresAt := clientScoreExportExpiresAt(ctx); !expiresAt.IsZero() {
		expireMillis = expiresAt.UnixMilli()
	}
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = r.Eval(ctx, clientScoreTargetManifestWriteScript, []string{clientScoreTargetManifestKey}, value, server.NowUtc().UnixMilli(), ttl.Milliseconds(), expireMillis).Err()
	})
	return
}
