//go:build sidecar_pending

package main

// all's process runs api and taskworker together, and each installs its own first metadata cluster's writers (§19.4's
// writers that share a process; owner decisions 785, 788): api's parts, as all's main installs them
// (operatorInstallParts), put in api's signing rev/ store and api's sequencer under api's log settings, and
// taskworker's runtime, beside evs's jobs, its evs-job writers (taskworker.Testing_InstallEvsJobWriters): a rev/ sweep
// store and the worker's sequencer for retention, effects and the tick tasks. Here both run in one process over one
// fake first metadata cluster, api's on all's process host and api's budget, taskworker's on a fake budget of its own,
// against the database and Redis with MG1 to MG9, each statement signed on the clock both sequencers read:
//
//	rev/       a removal's revocation is published, signed, as its transaction commits, through api's store, and
//	           the sweep, 20 minutes on, deletes it through the store current, api's, spending none of the worker's
//	           budget, since a signing store sweeps as well (owner decision 788)
//	appends    a network's log, three bases with statements between them, appends through api's sequencer, whose
//	           entry cache INCRs {acl_<networkId>}v once an entry: the worker's settings write no cache
//	retention  a pass past the newest base's 30 days runs through the worker's sequencer: the entries before the
//	           base it retires to are gone, entry 0 and the rest stay, and every DELETE spends the worker's budget,
//	           P0 of kind log at §11.13's delete row, which api's MinIO user could not have sent
//	ticks      the worker's tick and activation tasks hand their runs to the issuer, and the issuer's handoffs append
//	           through api's sequencer rather than handing off again
//	removal    the worker's removal leaves api's store and sequencer in place
//
// Before, the worker's install replaced api's in each one-pointer slot: the revocation stayed owed and unpublished,
// api's appends ran on the worker's sequencer without the entry cache, and each handoff handed off again.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/taskworker"
)

// api's sequencer as the admission world's api: its three appends and its read, each refusal as the world reads one,
// and no unverified append.
type allWritersAdmissionApi struct {
	sequencer *model.ConfigLogSequencer
}

func allWritersRefusal(err error) error {
	configLogError := &model.ConfigLogError{}
	if errors.As(err, &configLogError) {
		return &sidecartest.ConfigLogRefusal{Status: configLogError.Status, Tip: configLogError.Tip, Err: configLogError.Err}
	}
	return err
}

func (self allWritersAdmissionApi) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
	return pin, allWritersRefusal(err)
}

func (self allWritersAdmissionApi) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendBase(ctx, server.Id(networkId), &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	return pin, allWritersRefusal(err)
}

func (self allWritersAdmissionApi) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
	return pin, allWritersRefusal(err)
}

func (self allWritersAdmissionApi) ReadConfigProjection(ctx context.Context, networkId sidecar.Id, after uint64) (*sidecar.ConfigProjection, error) {
	read, err := self.sequencer.Read(ctx, server.Id(networkId), after)
	if err != nil {
		return nil, allWritersRefusal(err)
	}
	return &sidecar.ConfigProjection{SignedGenesis: read.SignedGenesis, Entries: read.Entries, Rows: read.Rows, Tip: read.Tip, More: read.More}, nil
}

func (self allWritersAdmissionApi) SequenceAnyway(ctx context.Context, networkId sidecar.Id, kind uint32, body []byte) (*bridge.AuthPin, error) {
	return nil, errors.New("api's sequencer appends nothing its check refuses")
}

func TestAllRunsApisAndTheWorkersWritersSideBySide(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// the issuer's service key, which signs rev/ objects, and the verifier evs, gkm and sockets read them with
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			tb.Fatal(err)
		}
		thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
		if err != nil {
			tb.Fatal(err)
		}
		defer oauth.Testing_SetSignerKeys(&oauth.SignerKey{
			Kid: oauth.SignerKeySetService.KidPrefix() + thumbprint, Alg: "ES256", Set: oauth.SignerKeySetService, PrivateKey: privateKey,
		})()
		verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
			Issuer: "https://issuer.allwriters.example", Keys: oauth.PublicJwks(oauth.SignerKeySetService).Keys,
		})
		if err != nil {
			tb.Fatal(err)
		}
		// the world's three bases come within a minute of each other, past pro.yml's one a minute
		for _, tier := range []model.NetworkTier{model.NetworkTierFree, model.NetworkTierPro, model.NetworkTierTeam} {
			defer model.Testing_SetSidecarLimits(tier, map[string]int64{
				"config_statements_per_min": -1,
				"config_statements_per_day": -1,
				"config_bases_per_min":      -1,
				"config_bases_per_day":      -1,
			})()
		}

		// one first metadata cluster in place of vault's file-clusters-api.yml and file-clusters-taskworker.yml, on the
		// clock both sequencers and every statement read
		clock := sidecartest.NewFakeClock(server.NowUtc())
		blobs := sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
		first, err := blobs.FirstCluster()
		if err != nil {
			tb.Fatal(err)
		}
		model.SetSidecarRevStore(nil)
		model.SetConfigLogSequencer(nil)

		// api's parts, as all's main installs them, over the host it builds
		processHost := newProcessHost(ctx)
		processHost.Clock = clock
		undoParts := controller.InstallProcessParts(ctx, operatorInstallParts(processHost, commandOptions{}, &controller.ApiRoutesInstallDeps{
			Clusters: func(ctx context.Context) (sidecar.BlobStore, func(), error) {
				return blobs, func() {}, nil
			},
		}))
		defer undoParts()
		apiSequencer, err := model.CurrentConfigLogSequencer()
		if err != nil {
			tb.Fatalf("all's parts installed no sequencer of api's: %v", err)
		}
		// taskworker's writers, as its runtime installs them beside evs's jobs, on a budget of their own
		workerBudget := sidecartest.NewFakeWriteBudget(nil)
		removeWriters := taskworker.Testing_InstallEvsJobWriters(&sidecar.ServiceHost{Blobs: blobs, Budget: workerBudget, Clock: clock}, first)
		removed := false
		defer func() {
			if !removed {
				removeWriters()
			}
		}()
		workerSpent := func(kind sidecar.WriteP0Kind) int64 {
			if violations := workerBudget.Violations(); len(violations) != 0 {
				tb.Errorf("the worker's budget's violations: %v", violations)
			}
			return workerBudget.P0ByteCount(first.ClusterId(), kind)
		}

		// rev/: a removal's revocation, published as its transaction commits
		owe := func() *model.SidecarRevocation {
			revocation := &model.SidecarRevocation{
				NetworkId: server.NewId(),
				Kind:      sidecar.RevocationKindClient,
				SubjectId: server.NewId(),
				NotBefore: sidecar.RevocationNotBefore(clock.Now()),
				Reason:    sidecar.RevocationReasonRemoved,
			}
			server.Tx(ctx, func(tx server.PgTx) {
				model.WriteRevocation(ctx, tx, revocation)
			})
			return revocation
		}
		published := func(revocation *model.SidecarRevocation) bool {
			record, err := sidecar.ReadRevocation(ctx, first, verifier.VerifyRevocation, sidecar.Id(revocation.NetworkId), revocation.Kind, sidecar.Id(revocation.SubjectId))
			if err != nil {
				tb.Fatal(err)
			}
			return record != nil && record.NotBefore.Equal(revocation.NotBefore)
		}
		if revocation := owe(); !published(revocation) {
			tb.Fatal("beside the worker's writers the committed removal published no signed revocation")
		}
		// the sweep, 20 minutes on, through api's store, which is current
		clock.Advance(model.DefaultSidecarRevSettings().DeleteAfter + time.Minute)
		if deletedCount, err := model.SweepSidecarRevocations(ctx); err != nil || deletedCount != 1 {
			tb.Fatalf("the sweep deleted %d: %v", deletedCount, err)
		}
		if spent := workerSpent(sidecar.WriteP0KindRevocation); spent != 0 {
			tb.Errorf("the worker's budget spent %d of rev/ while api's store was current", spent)
		}

		// appends: a network's log through api's sequencer, three bases with statements between them
		suiteSettings := sidecartest.DefaultAdmissionSuiteSettings()
		suiteSettings.ForgedAppends = false
		net := sidecartest.NewAdmissionNet(tb, &sidecartest.AdmissionTier{
			Cluster:  first,
			Settings: suiteSettings,
			Clock:    clock,
			NewApi: func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
				return allWritersAdmissionApi{sequencer: apiSequencer}
			},
		})
		defer net.Close()
		net.AdmitMember(net.Owner(), "first", bridge.MemberRole_MEMBER_ROLE_MEMBER)
		net.AdmitMember(net.Owner(), "second", bridge.MemberRole_MEMBER_ROLE_MEMBER)
		retiredTo := net.Fold(true).GetVersion()
		net.AdmitMember(net.Owner(), "third", bridge.MemberRole_MEMBER_ROLE_MEMBER)
		net.Fold(true)
		tip := net.Tip().GetVersion()
		networkId := net.NetworkId()
		var cacheVersion int64
		server.Redis(ctx, func(r server.RedisClient) {
			cacheVersion, err = r.Get(ctx, sidecar.AclVersionKey(networkId)).Int64()
		})
		if err != nil || cacheVersion != int64(tip) {
			tb.Fatalf("api's entry cache counted %d of the %d entries: %v", cacheVersion, tip, err)
		}
		if spent := workerSpent(sidecar.WriteP0KindLog); spent != 0 {
			tb.Fatalf("the worker's budget spent %d of the log before retention", spent)
		}

		// retention, past the newest base's 30 days, through the worker's sequencer
		if retiredTo <= 2 {
			tb.Fatalf("the second base at %d leaves retention nothing to delete", retiredTo)
		}
		retentionSettings := model.DefaultConfigLogRetentionSettings()
		retention, err := model.RetainConfigLogs(ctx, clock.Now().Add(retentionSettings.KeepAfterNewerBase+24*time.Hour), nil, retentionSettings)
		if err != nil || retention.NetworkCount != 1 || retention.DeletedCount < int(retiredTo)-1 {
			tb.Fatalf("retention: %+v, %v; it should delete entries 1 to %d", retention, err, retiredTo-1)
		}
		entryHeld := func(n uint64) bool {
			_, err := first.Head(ctx, sidecar.ConfigEntryKey(networkId, n))
			return err == nil
		}
		for n := uint64(0); n <= tip; n += 1 {
			if want := n == 0 || retiredTo <= n; entryHeld(n) != want {
				tb.Errorf("entry %d of %d held %t after retention to %d", n, tip, !want, retiredTo)
			}
		}
		deletePrice, err := evsservice.DefaultWriteBudgetSettings().PriceTable.Price(evsservice.WriteOperation{Kind: evsservice.WriteOperationDelete, Count: 1})
		if err != nil {
			tb.Fatal(err)
		}
		if spent, want := workerSpent(sidecar.WriteP0KindLog), int64(retention.DeletedCount)*deletePrice; spent != want {
			tb.Errorf("retention's %d DELETEs spent %d of the worker's budget, and §11.13's delete row prices them at %d", retention.DeletedCount, spent, want)
		}

		// ticks: the worker's tasks hand their runs to the issuer, whose handoffs append through api's sequencer
		clientSession := session.NewLocalClientSession(ctx, "", nil)
		defer clientSession.Cancel()
		tickArgs := &model.AppendConfigLogTickArgs{NetworkId: server.Id(networkId)}
		if tick, err := model.AppendConfigLogTickTask(tickArgs, clientSession); err != nil || !tick.PublishAtIssuer {
			tb.Fatalf("the worker's tick task: %+v, %v", tick, err)
		}
		if tick, err := model.PublishConfigLogTickTask(tickArgs, clientSession); err != nil || tick.PublishAtIssuer {
			tb.Fatalf("the issuer's tick handoff: %+v, %v", tick, err)
		}
		activationArgs := &model.AppendConfigLogActivationArgs{NetworkId: server.Id(networkId), ProposalId: make([]byte, 32)}
		if activation, err := model.AppendConfigLogActivationTask(activationArgs, clientSession); err != nil || !activation.PublishAtIssuer {
			tb.Fatalf("the worker's activation task: %+v, %v", activation, err)
		}
		if activation, err := model.PublishConfigLogActivationTask(activationArgs, clientSession); err != nil || activation.PublishAtIssuer {
			tb.Fatalf("the issuer's activation handoff: %+v, %v", activation, err)
		}

		// removal: the worker's writers' removal leaves api's store and sequencer
		removeWriters()
		removed = true
		if sequencer, err := model.CurrentConfigLogSequencer(); err != nil || sequencer != apiSequencer {
			tb.Fatalf("the worker's removal took away api's sequencer: %v", err)
		}
		if revocation := owe(); !published(revocation) {
			tb.Fatal("after the worker's removal the committed removal published no signed revocation")
		}
	})
}
