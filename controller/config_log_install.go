package controller

// The issuer's metadata services share its scoped first cluster, one write
// budget, and one verified projection cache. Ordinary API routes keep serving
// while a missing mount is retried.

import (
	"context"
	"errors"
	"os"
	"sync"
	"sync/atomic"
	"time"

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

// The first metadata cluster the issuer's services read and write, and what releases it: vault's
// file-clusters-api.yml, through the MinIO user api's block holds, which reaches proj/, projrows/, rev/ and hold/
// alone (§19.4's api row).
func loadApiFileClusters(ctx context.Context) (sidecar.BlobStore, func(), error) {
	clusters, err := server.LoadFileClusters(ctx, "api", nil)
	if err != nil {
		return nil, nil, errors.New("the API metadata cluster configuration is unavailable")
	}
	return clusters, clusters.Close, nil
}

// Starts the issuer's authorization services, and hold/'s writer the renew_hold staff kind writes through
// (installApiLegalHolds), before routes can be served, on the clusters loadClusters gives (loadApiFileClusters in
// each process that serves api's routes, through InstallApiRoutes).
func installApiAuthorization(ctx context.Context, processHost *sidecar.ServiceHost, loadClusters func(context.Context) (sidecar.BlobStore, func(), error)) func() {
	install := server.NewSidecarSwitchInstall("api-authorization", func() bool {
		return server.SidecarMg1Linked() || server.SidecarMg2Linked()
	}, func(ctx context.Context) (func(), error) {
		// The issuer drains already claimed writes before closing these resources.
		hostCtx := context.WithoutCancel(ctx)
		clusters, closeClusters, err := loadClusters(hostCtx)
		if err != nil {
			return nil, err
		}
		first, err := clusters.FirstCluster()
		if err != nil {
			closeClusters()
			return nil, err
		}
		host := *processHost
		host.Blobs = clusters
		budget, err := evsservice.NewWriteBudget(hostCtx, &host, nil, nil)
		if err != nil {
			closeClusters()
			return nil, err
		}
		host.Budget = budget
		closeRevocations := installApiRevocations(&host, first)
		closeLegalHolds := installApiLegalHolds(&host, first)
		worker := newAuthorizationIssuerWorker(hostCtx)
		claimCtx, stopClaims := context.WithCancel(ctx)
		var lifetime sync.WaitGroup
		lifetime.Go(func() {
			// The ordinary taskworker also supervises its claim loop. An
			// unavailable direct database resource can recover on a later poll.
			for claimCtx.Err() == nil {
				if server.HandleError(worker.Run) == nil {
					return
				}
				select {
				case <-claimCtx.Done():
					return
				case <-time.After(task.DefaultTaskWorkerSettings().RetryTimeoutAfterError):
				}
			}
		})
		// A missing projection mount or subscription must not interrupt revocations.
		projection := server.NewSidecarSwitchInstall("api-projection", server.SidecarMg2Linked, func(ctx context.Context) (func(), error) {
			return installApiProjection(ctx, &host)
		}, nil)
		closeViews := runAuthorizationInstall(hostCtx, projection)
		return func() {
			stopClaims()
			worker.Drain()
			worker.Close()
			lifetime.Wait()
			closeViews()
			closeLegalHolds()
			closeRevocations()
			budget.Close()
			closeClusters()
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

// rev/'s writer on the issuer's first cluster, spending host's budget: each create, raise and DELETE at its own
// one of §11.13's cost rows, by the table the budget, built with evsservice's default settings, prices with
// (model.SidecarRevStore.SetWriteBudget's nil prices).
func installApiRevocations(host *sidecar.ServiceHost, first sidecar.BlobCluster) func() {
	settings := model.DefaultSidecarRevSettings()
	settings.Clock = host.Clock
	store := model.NewSidecarRevStore(first, oauth.SignRevocation, settings)
	store.SetWriteBudget(host.Budget, nil)
	model.SetSidecarRevStore(store)
	return func() { model.SetSidecarRevStore(nil) }
}

// hold/'s writer on the issuer's first cluster (§19.4's api row; §21.9's "api mirrors each hold"), which the
// renew_hold staff kind writes through (RenewLegalHold, model.RenewLegalHold; QD-1399): a renewal reads nothing the
// worker alone can read, so it rewrites hold/<network_id>/<case_id>/_ here, while placement stays dispatched to the
// worker for its GroupForms read (InstallLegalHoldDispatch). api is so a second hold/ writer beside taskworker's,
// whose pass ends holds and puts objects back; each writes If-Match the object it read, or If-None-Match for none
// (model.AbuseHoldStore.RewriteUnless), so neither replaces what the other wrote unread. Each create, rewrite and
// DELETE spends host's budget, P0 of kind lifecycle, at its own one of §11.13's cost rows, by the table the budget,
// built with evsservice's default settings, prices with (model.AbuseHoldStore.SetWriteBudget's nil prices). Answers
// what removes the writer, which leaves one installed since in place, as all's process installs taskworker's too.
func installApiLegalHolds(host *sidecar.ServiceHost, first sidecar.BlobCluster) func() {
	store := model.NewAbuseHoldStore(first)
	store.SetWriteBudget(host.Budget, nil)
	return model.InstallAbuseHoldStore(store)
}

// The API's writer and the worker's retention and effects reader use the same
// verifier on their own scoped cluster. Settings select each process's duties. The sequencer spends host's budget
// at §11.13's create row for each object it creates and its delete row for each its retention deletes, by the table
// the budget, built with evsservice's default settings, prices with.
func NewHostedConfigLogSequencer(host *sidecar.ServiceHost, first sidecar.BlobCluster, settings *model.ConfigLogSettings) *model.ConfigLogSequencer {
	settings.AclSpendCheck = sidecar.NewSpendCheck(nil)
	settings.Clock = host.Clock
	sequencer := model.NewConfigLogSequencer(first, settings, model.AdvanceNetworkAclHead)
	sequencer.SetWriteBudget(host.Budget, nil)
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
	restoreApprovals := model.SetApprovalGateStates(model.ApprovalGateStatesOf(cache))
	restoreDnsScopes := model.SetDnsScopeLabelSource(model.DnsScopeLabelSourceOf(cache))
	// §14.14's mail_send and report read, without which POST /smtp/dkim-keys and GET /smtp/reports answer 503
	restoreSmtpChecks := model.SetSmtpApiGate(model.SmtpApiGateOf(cache))
	// §14.8's report seal key from the same cache and gkm, without which the internal report-key route answers 503
	restoreReportKeys := SetSmtpReportKeySource(newApiSmtpReportKeySource(cache, os.Getenv(sidecarhost.GkmUrlEnvName), host.Clock))
	apiAclCache.Store(cache)
	oauth.SetEdgeLoginSource(oauth.NewProjectionEdgeLoginSource(cache))
	closeContracts := installContractGateWhenEnabled(ctx, cache)
	restoreScopes := oauth.SetExchangeScopeSource(oauth.NewProjectionExchangeScopeSource(cache, &oauth.ExchangeScopeSettings{Clock: host.Clock}))
	return func() {
		restoreScopes()
		closeContracts()
		oauth.SetEdgeLoginSource(nil)
		apiAclCache.CompareAndSwap(cache, nil)
		restoreReportKeys()
		restoreSmtpChecks()
		restoreDnsScopes()
		restoreApprovals()
		model.SetConfigLogSequencer(nil)
		closeGate()
	}, nil
}
