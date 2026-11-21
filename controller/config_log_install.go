package controller

// The issuer's metadata services share its scoped first cluster, one write
// budget, and one verified projection cache. Ordinary API routes keep serving
// while a missing mount is retried.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/sidecarhost"
)

var apiAclCache atomic.Pointer[sidecar.AclGateCache]

// The issuer's verified cache for features it hosts. A missing cache means
// this process has no verified authorization state.
func ApiAuthorizationCache() *sidecar.AclGateCache {
	return apiAclCache.Load()
}

// Runs an install through initial startup, configuration changes and shutdown.
func runAuthorizationInstall(ctx context.Context, install *server.SidecarSwitchInstall) func() {
	runCtx, cancel := context.WithCancel(ctx)
	install.Sync(runCtx)
	var lifetime sync.WaitGroup
	lifetime.Go(func() { install.Run(runCtx) })
	return func() {
		cancel()
		lifetime.Wait()
	}
}

// Starts the issuer's authorization services before routes can be served.
func InstallApiAuthorization(ctx context.Context, processHost *sidecar.ServiceHost) func() {
	install := server.NewSidecarSwitchInstall("api-authorization", server.SidecarMg2Linked, func(ctx context.Context) (func(), error) {
		clusters, err := server.LoadFileClusters(ctx, "api", nil)
		if err != nil {
			return nil, errors.New("the API metadata cluster configuration is unavailable")
		}
		host := *processHost
		host.Blobs = clusters
		budget, err := evsservice.NewWriteBudget(ctx, &host, nil, nil)
		if err != nil {
			clusters.Close()
			return nil, err
		}
		host.Budget = budget
		closeViews, err := installApiProjection(ctx, &host)
		if err != nil {
			budget.Close()
			clusters.Close()
			return nil, err
		}
		return func() {
			closeViews()
			budget.Close()
			clusters.Close()
		}, nil
	}, nil)
	return runAuthorizationInstall(ctx, install)
}

// Publishes model and OAuth dependencies only after the watcher is installed.
func installApiProjection(ctx context.Context, host *sidecar.ServiceHost) (func(), error) {
	first, err := host.Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	cache, closeGate, err := sidecarhost.WatchAclGate(ctx, host, nil, nil)
	if err != nil {
		return nil, err
	}
	settings := model.NewApiConfigLogSettings()
	settings.AclSpendCheck = sidecar.NewSpendCheck(nil)
	settings.Clock = host.Clock
	sequencer := model.NewConfigLogSequencer(first, settings, model.AdvanceNetworkAclHead)
	prices := evsservice.DefaultPriceTable4KiB()
	priceCreate := func(byteCount int64) int64 {
		price, _ := prices.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: byteCount, Count: 1})
		return price
	}
	sequencer.SetWriteBudget(host.Budget, priceCreate)
	revSettings := model.DefaultSidecarRevSettings()
	revSettings.Clock = host.Clock
	revocations := model.NewSidecarRevStore(first, oauth.SignRevocation, revSettings)
	revocations.SetWriteBudget(host.Budget, priceCreate)
	model.SetSidecarRevStore(revocations)
	model.SetConfigLogSequencer(sequencer)
	model.SetApprovalGateStates(model.ApprovalGateStatesOf(cache))
	apiAclCache.Store(cache)
	restoreScopes := oauth.SetExchangeScopeSource(oauth.NewProjectionExchangeScopeSource(cache, &oauth.ExchangeScopeSettings{Clock: host.Clock}))
	return func() {
		restoreScopes()
		apiAclCache.CompareAndSwap(cache, nil)
		model.SetApprovalGateStates(nil)
		model.SetConfigLogSequencer(nil)
		model.SetSidecarRevStore(nil)
		closeGate()
	}, nil
}
