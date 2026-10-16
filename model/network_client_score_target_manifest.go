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
}

// The Redis document of a manifest, under clientScoreTargetManifestKey.
type clientScoreTargetManifestDocument struct {
	LocationIds      map[string]int64 `json:"location_ids"`
	LocationGroupIds map[string]int64 `json:"location_group_ids"`
}

func newClientScoreTargetManifest() *clientScoreTargetManifest {
	return &clientScoreTargetManifest{
		locationIds:      map[server.Id]int64{},
		locationGroupIds: map[server.Id]int64{},
	}
}

// Every target of the maps, written from rows at writtenAt. A complete export
// publishes this after its last document.
func (self *clientScoreTargetMaps) targetManifest(writtenAt time.Time) *clientScoreTargetManifest {
	manifest := newClientScoreTargetManifest()
	for locationId := range self.locationClientScores {
		manifest.locationIds[locationId] = writtenAt.Unix()
	}
	for locationGroupId := range self.locationGroupClientScores {
		manifest.locationGroupIds[locationGroupId] = writtenAt.Unix()
	}
	return manifest
}

// The departed targets one export also writes: an empty target for each
// target of the previous manifest that the maps no longer hold and that was
// written from rows less than ttl before now, so its documents may still be
// read. Older entries are dropped: their documents have expired, which bounds
// the memory to the targets of one cache ttl.
//
// The returned manifest is what this export may leave readable: the maps'
// targets at now, and each departed target at the time it was last written
// from rows. The export publishes it before writing any document, so an
// export that fails or dies part way still leaves every target it may have
// written, and every target it has not yet cleared, for the next export.
func (self *clientScoreTargetMaps) departedTargets(
	previous *clientScoreTargetManifest,
	now time.Time,
	ttl time.Duration,
) (departed []clientScoreTarget, manifest *clientScoreTargetManifest) {
	manifest = self.targetManifest(now)
	if previous == nil {
		return
	}
	readable := func(writtenAt int64) bool {
		return now.Before(time.Unix(writtenAt, 0).Add(ttl))
	}
	for locationId, writtenAt := range previous.locationIds {
		if _, ok := self.locationClientScores[locationId]; ok || !readable(writtenAt) {
			continue
		}
		departed = append(departed, clientScoreTarget{
			id:           locationId,
			clientScores: map[server.Id]*ClientScore{},
		})
		manifest.locationIds[locationId] = writtenAt
	}
	for locationGroupId, writtenAt := range previous.locationGroupIds {
		if _, ok := self.locationGroupClientScores[locationGroupId]; ok || !readable(writtenAt) {
			continue
		}
		departed = append(departed, clientScoreTarget{
			id:            locationGroupId,
			locationGroup: true,
			clientScores:  map[server.Id]*ClientScore{},
		})
		manifest.locationGroupIds[locationGroupId] = writtenAt
	}
	return
}

func encodeClientScoreTargetManifest(manifest *clientScoreTargetManifest) ([]byte, error) {
	document := clientScoreTargetManifestDocument{
		LocationIds:      make(map[string]int64, len(manifest.locationIds)),
		LocationGroupIds: make(map[string]int64, len(manifest.locationGroupIds)),
	}
	for locationId, writtenAt := range manifest.locationIds {
		document.LocationIds[locationId.String()] = writtenAt
	}
	for locationGroupId, writtenAt := range manifest.locationGroupIds {
		document.LocationGroupIds[locationGroupId.String()] = writtenAt
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

// The manifest expires with the documents it describes.
func writeClientScoreTargetManifest(ctx context.Context, manifest *clientScoreTargetManifest, ttl time.Duration) (returnErr error) {
	value, err := encodeClientScoreTargetManifest(manifest)
	if err != nil {
		return err
	}
	server.Redis(ctx, func(r server.RedisClient) {
		returnErr = r.Set(ctx, clientScoreTargetManifestKey, value, ttl).Err()
	})
	return
}
