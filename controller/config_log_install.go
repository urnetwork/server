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
	"github.com/urnetwork/server/task"
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
	install := server.NewSidecarSwitchInstall("api-authorization", func() bool {
		return server.SidecarMg1Linked() || server.SidecarMg2Linked()
	}, func(ctx context.Context) (func(), error) {
		clusters, err := server.LoadFileClusters(ctx, "api", nil)
		if err != nil {
			return nil, errors.New("the API metadata cluster configuration is unavailable")
		}
		first, err := clusters.FirstCluster()
		if err != nil {
			clusters.Close()
			return nil, err
		}
		host := *processHost
		host.Blobs = clusters
		budget, err := evsservice.NewWriteBudget(ctx, &host, nil, nil)
		if err != nil {
			clusters.Close()
			return nil, err
		}
		host.Budget = budget
		closeRevocations := installApiRevocations(&host, first)
		worker := newAuthorizationIssuerWorker(context.WithoutCancel(ctx))
		var lifetime sync.WaitGroup
		lifetime.Go(worker.Run)
		// A missing projection mount or subscription must not interrupt revocations.
		projection := server.NewSidecarSwitchInstall("api-projection", server.SidecarMg2Linked, func(ctx context.Context) (func(), error) {
			return installApiProjection(ctx, &host)
		}, nil)
		closeViews := runAuthorizationInstall(ctx, projection)
		return func() {
			closeViews()
			worker.Drain()
			worker.Close()
			lifetime.Wait()
			closeRevocations()
			budget.Close()
			clusters.Close()
		}, nil
	}, nil)
	return runAuthorizationInstall(ctx, install)
}

// The API issuer alone holds the service signing keys and may PUT rev/.
// Its worker claims revocations, due tick handoffs and their own post hooks.
func newAuthorizationIssuerWorker(ctx context.Context) *task.TaskWorker {
	settings := task.DefaultTaskWorkerSettings()
	settings.ClaimRegisteredTargetsOnly = true
	worker := task.NewTaskWorker(ctx, settings)
	worker.AddTargets(model.AuthorizationIssuerTargets()...)
	return worker
}

func installApiRevocations(host *sidecar.ServiceHost, first sidecar.BlobCluster) func() {
	settings := model.DefaultSidecarRevSettings()
	settings.Clock = host.Clock
	store := model.NewSidecarRevStore(first, oauth.SignRevocation, settings)
	store.SetWriteBudget(host.Budget, priceMetadataCreate)
	model.SetSidecarRevStore(store)
	return func() { model.SetSidecarRevStore(nil) }
}

var metadataPrices = evsservice.DefaultPriceTable4KiB()

func priceMetadataCreate(byteCount int64) int64 {
	price, _ := metadataPrices.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationCreate, ObjectByteCount: byteCount, Count: 1})
	return price
}

// The API's writer and the worker's retention and effects reader use the same
// verifier on their own scoped cluster. Settings select each process's duties.
func NewHostedConfigLogSequencer(host *sidecar.ServiceHost, first sidecar.BlobCluster, settings *model.ConfigLogSettings) *model.ConfigLogSequencer {
	settings.AclSpendCheck = sidecar.NewSpendCheck(nil)
	settings.Clock = host.Clock
	sequencer := model.NewConfigLogSequencer(first, settings, model.AdvanceNetworkAclHead)
	sequencer.SetWriteBudget(host.Budget, priceMetadataCreate)
	return sequencer
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
	model.SetConfigLogSequencer(NewHostedConfigLogSequencer(host, first, model.NewApiConfigLogSettings()))
	model.SetApprovalGateStates(model.ApprovalGateStatesOf(cache))
	apiAclCache.Store(cache)
	closeContracts := installContractGateWhenEnabled(ctx, cache)
	restoreScopes := oauth.SetExchangeScopeSource(oauth.NewProjectionExchangeScopeSource(cache, &oauth.ExchangeScopeSettings{Clock: host.Clock}))
	return func() {
		restoreScopes()
		closeContracts()
		apiAclCache.CompareAndSwap(cache, nil)
		model.SetApprovalGateStates(nil)
		model.SetConfigLogSequencer(nil)
		closeGate()
	}, nil
}
