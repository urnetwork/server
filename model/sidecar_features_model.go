package model

// {scf_<network_id>} (DESIGN-COMPUTE §16.6; §2.8's api-7; CACL-M11-03): each sidecar's stated authorization features,
// the features its release implements as its last GET /sidecar/sync/core named them, which api keeps unsigned and for
// the turn-on prompt alone, never as authority. One Redis hash per network: field the sidecar's id as text, value its
// features' bits in decimal.
//
// An entry lives until the sidecar's next core sync replaces it or its removal drops it: each read for an Owner's or
// Admin's device drops the entries of sidecars the network's caught-up state holds revoked or does not hold at all.
// The hash also expires HashTtl past its last write, so a network no sidecar syncs for that long, a deleted one among
// them, keeps none. Soft state: a lost entry is stated again at the sidecar's next core sync, hourly at the latest
// while it is online, and meanwhile reads as stating none, the cautious direction for the prompt, which then lists it
// as one that would pause.
//
// What an Owner's or Admin's device is told (SidecarFeaturesLacking) comes from the network's standing sidecars in the
// caught-up verified state, the authority on who is admitted, each with what it last stated: those that lack one of
// the features every service serving the network implements. Whether the device asking is an Owner's or Admin's is
// read from this instance's verifier first, which costs no store read, and again from the caught-up state before
// anything is listed; any other device costs one in-memory check.

import (
	"context"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// What api's keeping and listing of the stated features is held to.
type SidecarFeaturesSettings struct {
	// The bound of one Redis call on a network's hash: 5 s, as api's other soft-state reads on a request.
	RedisTimeout time.Duration
	// The most sidecars one core answer lists as lacking a feature: 1,000, past which it says more were left out, so
	// an answer to a large network stays bounded. Non-positive lists none, saying more wherever one lacks a feature.
	ListedSidecarCount int
	// The bound of the whole listing on one core answer, the caught-up state's store reads included: 2 s, so a slow
	// store costs an Owner's or Admin's device the advisory list alone, never the core state it rides on. Non-positive
	// lists nothing.
	ListTimeout time.Duration
	// How long a network's hash lives past its last write: 30 days. Every online sidecar states its features at least
	// hourly, so only a network none of whose sidecars synced for that long loses it, a deleted one among them, and a
	// lost entry reads as stating none, the cautious direction. Non-positive keeps the hash until its entries are
	// dropped.
	HashTtl time.Duration
}

// The defaults, a fresh value each call.
func DefaultSidecarFeaturesSettings() *SidecarFeaturesSettings {
	return &SidecarFeaturesSettings{
		RedisTimeout:       5 * time.Second,
		ListedSidecarCount: 1000,
		ListTimeout:        2 * time.Second,
		HashTtl:            30 * 24 * time.Hour,
	}
}

// {scf_<network_id>}.
func sidecarFeaturesKey(networkId server.Id) string {
	return "{scf_" + networkId.String() + "}"
}

// Keeps features as the sidecar's stated set, replacing what its last core sync stated, and moves the hash's expiry
// to HashTtl from now.
func SetSidecarStatedAuthFeatures(ctx context.Context, settings *SidecarFeaturesSettings, networkId server.Id, sidecarId server.Id, features sidecar.AuthFeature) error {
	boundedCtx, cancel := context.WithTimeout(ctx, settings.RedisTimeout)
	defer cancel()
	key := sidecarFeaturesKey(networkId)
	return server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
		_, err := r.TxPipelined(boundedCtx, func(pipe redis.Pipeliner) error {
			pipe.HSet(boundedCtx, key, sidecarId.String(), strconv.FormatUint(uint64(features), 10))
			if 0 < settings.HashTtl {
				pipe.Expire(boundedCtx, key, settings.HashTtl)
			}
			return nil
		})
		return err
	})
}

// Every sidecar's stated set the network's hash holds, by sidecar; a field or value that does not parse is skipped.
func GetSidecarStatedAuthFeatures(ctx context.Context, settings *SidecarFeaturesSettings, networkId server.Id) (map[server.Id]sidecar.AuthFeature, error) {
	var fieldValues map[string]string
	boundedCtx, cancel := context.WithTimeout(ctx, settings.RedisTimeout)
	defer cancel()
	err := server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
		var err error
		fieldValues, err = r.HGetAll(boundedCtx, sidecarFeaturesKey(networkId)).Result()
		return err
	})
	if err != nil {
		return nil, err
	}
	stated := map[server.Id]sidecar.AuthFeature{}
	for field, value := range fieldValues {
		sidecarId, err := server.ParseId(field)
		if err != nil {
			continue
		}
		features, err := strconv.ParseUint(value, 10, 32)
		if err != nil {
			continue
		}
		stated[sidecarId] = sidecar.AuthFeature(features)
	}
	return stated, nil
}

// Drops the sidecars' entries from the network's hash.
func dropSidecarStatedAuthFeatures(ctx context.Context, settings *SidecarFeaturesSettings, networkId server.Id, sidecarIds []server.Id) error {
	if len(sidecarIds) == 0 {
		return nil
	}
	fields := make([]string, 0, len(sidecarIds))
	for _, sidecarId := range sidecarIds {
		fields = append(fields, sidecarId.String())
	}
	boundedCtx, cancel := context.WithTimeout(ctx, settings.RedisTimeout)
	defer cancel()
	return server.RedisWithDeadline(boundedCtx, func(r server.RedisClient) error {
		return r.HDel(boundedCtx, sidecarFeaturesKey(networkId), fields...).Err()
	})
}

// One sidecar as its last core sync stated its release's features.
type SidecarStatedAuthFeatures struct {
	SidecarId    server.Id
	AuthFeatures sidecar.AuthFeature
}

// The network's sidecars that lack a feature the services implement, for an Owner's or Admin's device.
type SidecarFeaturesLacking struct {
	// In sidecar id order, at most ListedSidecarCount.
	Sidecars []*SidecarStatedAuthFeatures
	// More sidecars lack one than are listed.
	More bool
}

// What a core answer lists to requesterSidecarId (DESIGN-COMPUTE §16.6's turn on row): nil, with nothing read of the
// hash, unless it is an Owner's or Admin's standing, untagged device (sidecar.ConfigState.ServiceSettingsSigner);
// else the network's standing sidecars whose last stated features lack one of features, a sidecar with no entry
// stating none, and the entries of sidecars the state holds revoked or not at all dropped. features is what every
// service serving the network implements (server.SidecarConfig.AuthFeaturesFor), so none listed lacks a feature that
// could not be turned on. Who is asking is read first from this instance's verifier (heldState, dns_edge_grants_model.go),
// which costs no store read once the network is loaded. Advisory: never authority.
func (self *ConfigLogSequencer) SidecarFeaturesLacking(ctx context.Context, settings *SidecarFeaturesSettings, networkId server.Id, requesterSidecarId server.Id, features sidecar.AuthFeature) (*SidecarFeaturesLacking, error) {
	held, err := self.heldState(ctx, networkId)
	if err != nil || held == nil || !held.ServiceSettingsSigner(sidecar.Id(requesterSidecarId)) {
		return nil, err
	}
	state, err := self.CaughtUpState(ctx, networkId)
	if err != nil || state == nil || !state.ServiceSettingsSigner(sidecar.Id(requesterSidecarId)) {
		return nil, err
	}
	stated, err := GetSidecarStatedAuthFeatures(ctx, settings, networkId)
	if err != nil {
		return nil, err
	}
	lacking := &SidecarFeaturesLacking{Sidecars: []*SidecarStatedAuthFeatures{}}
	known := map[server.Id]bool{}
	for _, sidecarId := range state.SidecarIds() {
		row := state.SidecarRow(sidecarId)
		if row.GetRevokedAt() != 0 {
			continue
		}
		known[server.Id(sidecarId)] = true
		// a parked sidecar is kept, since an OwnerConfirm may lift its park, but it is not standing, so it is not listed
		if row.GetParkedAt() != 0 {
			continue
		}
		have := stated[server.Id(sidecarId)]
		if features&^have == 0 {
			continue
		}
		if settings.ListedSidecarCount <= len(lacking.Sidecars) {
			lacking.More = true
			continue
		}
		lacking.Sidecars = append(lacking.Sidecars, &SidecarStatedAuthFeatures{SidecarId: server.Id(sidecarId), AuthFeatures: have})
	}
	removed := []server.Id{}
	for sidecarId := range stated {
		if !known[sidecarId] {
			removed = append(removed, sidecarId)
		}
	}
	// the removal drop, best effort: an entry left behind is dropped at the next read, and lists nothing meanwhile
	if err := dropSidecarStatedAuthFeatures(ctx, settings, networkId, removed); err != nil {
		glog.Warningf("[scf]%s could not drop %d removed sidecars' stated features: %v\n", networkId, len(removed), err)
	}
	return lacking, nil
}
