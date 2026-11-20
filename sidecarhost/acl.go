package sidecarhost

// The verified projection cache owned by a serving process. Metadata hosts read
// the log directly. Hosts without metadata credentials supply the internal API
// source. A failed watch never leaves a cache serving on its poll alone.

import (
	"context"
	"errors"

	"github.com/urnetwork/sidecar"
)

// Starts the cache and its version watch. The caller closes both after drain.
func WatchAclGate(ctx context.Context, host *sidecar.ServiceHost, source sidecar.AclGateSource, settings *sidecar.AclGateSettings) (*sidecar.AclGateCache, func(), error) {
	if host == nil || host.Redis == nil {
		return nil, nil, errors.New("the authorization cache needs Redis")
	}
	if settings == nil {
		settings = sidecar.DefaultAclGateSettings()
	}
	gateSettings := *settings
	if gateSettings.ConfigLog == nil {
		gateSettings.ConfigLog = sidecar.DefaultConfigLogSettings()
	}
	logSettings := *gateSettings.ConfigLog
	if logSettings.AclSpendCheck == nil {
		logSettings.AclSpendCheck = sidecar.NewSpendCheck(nil)
	}
	if logSettings.Log == nil {
		logSettings.Log = gateSettings.Log
	}
	gateSettings.ConfigLog = &logSettings
	if source == nil {
		if host.Blobs == nil {
			return nil, nil, errors.New("the authorization cache needs a projection source")
		}
		source = sidecar.NewAclGateDirectSource(host.Redis, host.Blobs, &gateSettings)
	}
	cache := sidecar.NewAclGateCache(ctx, source, host.Clock, &gateSettings)
	removeWatch, err := cache.Watch(host.Redis)
	if err != nil {
		cache.Close()
		return nil, nil, err
	}
	return cache, func() {
		removeWatch()
		cache.Close()
	}, nil
}
