package gkm

// gkm's newest verified authorization state for a block (QD-718, question keys-m2-17-gkm-auth-state-owner): §8.9's
// gate cache, the one this process holds (C-09), and gkmservice.AclGkmAuthState over it.
//
//   - the cache reads each network's log itself, as §8.9 gives evs and gkm (sidecar.AclGateDirectSource): {acl_<networkId>}e
//     on the block's Redis while it records {acl_}v's value, else proj/ and projrows/ on the first metadata cluster. It
//     writes neither;
//   - api's INCR of {acl_<networkId>}v reaches it through the block's shared key-event subscriber
//     (AclGateCache.Watch), a resubscription loads every held network again, and its poll covers a lost event, so a
//     revocation reaches the gate within §8.9's 5 s and never later than the poll;
//   - a network is loaded when a token first names it: until its first verified state installs every route answers
//     503, never an allow;
//   - rev/ is read by sidecar.RevocationChecker on the first metadata cluster, each object's signature checked under
//     the svc- key set this block mounts (oauth.VerifyRevocation), each answer kept at most 5 s (§7.8's check row).
//
// The block holds no Postgres pool, and nothing here reaches for one.

import (
	"context"
	"errors"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"

	"github.com/urnetwork/server/oauth"
)

// The state over a gate cache of its own on host, which needs Redis and Blobs, and what ends the cache and its watch.
// ctx bounds the cache's loads and poll and should outlive the block's drain. settings.Gate and settings.Revocation
// tune the two; nil takes each one's defaults, logging through glog.
func NewAuthState(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings) (state gkmservice.GkmAuthState, close func(), err error) {
	if host == nil || host.Redis == nil || host.Blobs == nil {
		return nil, nil, errors.New("gkm's authorization state needs a host with Redis and Blobs")
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	gateSettings := sidecar.DefaultAclGateSettings()
	if settings.Gate != nil {
		gateSettingsCopy := *settings.Gate
		gateSettings = &gateSettingsCopy
	}
	if gateSettings.Log == nil {
		gateSettings.Log = hostLogger{}
	}
	revocationSettings := sidecar.DefaultRevocationSettings()
	if settings.Revocation != nil {
		revocationSettingsCopy := *settings.Revocation
		revocationSettings = &revocationSettingsCopy
	}
	if revocationSettings.Clock == nil {
		revocationSettings.Clock = host.Clock
	}
	cache := sidecar.NewAclGateCache(ctx, sidecar.NewAclGateDirectSource(host.Redis, host.Blobs, gateSettings), host.Clock, gateSettings)
	removeWatch, err := cache.Watch(host.Redis)
	if err != nil {
		// with no events a revocation would wait for the poll: the block does not start on that
		cache.Close()
		return nil, nil, err
	}
	revocations := sidecar.NewRevocationChecker(host.Blobs, oauth.VerifyRevocation, revocationSettings)
	return gkmservice.NewAclGkmAuthState(cache, revocations, host.Clock), func() {
		removeWatch()
		cache.Close()
	}, nil
}
