package controller

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/oauth"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// An empty network can distinguish its first bootstrap from an unavailable
// sequencer. Closing the host withdraws the log again.
func TestApiProjectionOwnsAnEmptyNetworksLog(t *testing.T) {
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	closeViews, err := installApiProjection(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	client := session.NewLocalClientSession(t.Context(), "", nil)
	defer client.Cancel()
	client.ByJwt = session.NewByJwt(server.NewId(), server.NewId(), "test", false, false)
	projection, err := NetworkConfigProjection(&NetworkConfigProjectionArgs{}, client)
	closeViews()
	if err != nil || projection == nil || len(projection.Genesis) != 0 || len(projection.Entries) != 0 || projection.More || projection.Tip != nil {
		t.Fatalf("empty network projection: %+v, %v", projection, err)
	}
	if _, err := model.CurrentConfigLogSequencer(); err == nil {
		t.Fatal("the closed host still serves a sequencer")
	}
}

// The queue advances by whole-second blocks. Age only the synthetic issuer
// tasks so a finite EvalTasks sees the same work as the production poller.
func makeAuthorizationIssuerTasksDue(t testing.TB) {
	t.Helper()
	names := []string{}
	for _, target := range model.AuthorizationIssuerTargets() {
		names = append(names, target.TargetFunctionName())
	}
	server.Tx(t.Context(), func(tx server.PgTx) {
		_, err := tx.Exec(t.Context(), `UPDATE pending_task SET run_at=$1, claim_time=$1, release_time=$1 WHERE function_name=ANY($2)`, server.NowUtc().Add(-time.Minute), names)
		server.Raise(err)
	})
}

// A removal committed on the issuer publishes immediately. A removal
// committed in a worker leaves the same durable target for the API issuer.
func TestIssuerPublishesNormalAndRetriedRevocation(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		clock := sidecartest.NewFakeClock(time.Unix(1767225600, 0))
		host := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil).ServiceHost()
		first, err := host.Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		thumbprint, err := oauth.SignerKid(&privateKey.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(oauth.Testing_SetSignerKeys(&oauth.SignerKey{
			Kid: oauth.SignerKeySetService.KidPrefix() + thumbprint, Alg: "ES256", Set: oauth.SignerKeySetService, PrivateKey: privateKey,
		}))
		verifier, err := oauth.NewServiceTokenVerifier(&oauth.PublicJwksDocument{
			Issuer: "https://issuer.example", Keys: oauth.PublicJwks(oauth.SignerKeySetService).Keys,
		})
		if err != nil {
			t.Fatal(err)
		}
		read := func(rev *model.SidecarRevocation) *sidecar.RevocationRecord {
			t.Helper()
			record, err := sidecar.ReadRevocation(ctx, first, verifier.VerifyRevocation, sidecar.Id(rev.NetworkId), rev.Kind, sidecar.Id(rev.SubjectId))
			if err != nil {
				t.Fatal(err)
			}
			return record
		}
		owe := func() *model.SidecarRevocation {
			rev := &model.SidecarRevocation{
				NetworkId: server.NewId(), Kind: sidecar.RevocationKindClient, SubjectId: server.NewId(),
				NotBefore: sidecar.RevocationNotBefore(clock.Now()), Reason: sidecar.RevocationReasonRemoved,
			}
			server.Tx(ctx, func(tx server.PgTx) { model.WriteRevocation(ctx, tx, rev) })
			return rev
		}
		closeIssuer := installApiRevocations(host, first)
		t.Cleanup(closeIssuer)
		direct := owe()
		if read(direct) == nil {
			t.Fatal("the committed removal did not publish a signed record")
		}
		closeIssuer()
		t.Cleanup(model.InstallSidecarRevStore(model.NewSidecarRevStore(first, nil, nil)))
		queued := owe()
		if read(queued) != nil {
			t.Fatal("a sweep-only worker published a revocation")
		}

		t.Cleanup(installApiRevocations(host, first))
		makeAuthorizationIssuerTasksDue(t)
		worker := newAuthorizationIssuerWorker(ctx)
		defer worker.Close()
		// WriteRevocations owes each write at TaskPriorityFastest, and a claim takes a task above the default
		// priority alone (the task worker's isolation of higher-priority work), so each claim finishes one owed
		// write, as each of the production poller's claims does: the direct removal's, which writes again what
		// its post-commit published, and the queued one's. A third claim finds nothing owed.
		finishedTaskIds := map[server.Id]bool{}
		for _, want := range []int{1, 1, 0} {
			finished, retry, postRetry, err := worker.EvalTasks(4)
			if err != nil || len(finished) != want || len(retry)+len(postRetry) != 0 {
				t.Fatalf("issuer claim, wanted %d: %v finished, %v/%v retries, %v", want, finished, retry, postRetry, err)
			}
			for _, taskId := range finished {
				finishedTaskIds[taskId] = true
			}
		}
		if len(finishedTaskIds) != 2 {
			t.Fatalf("the issuer finished %d distinct writes, not 2", len(finishedTaskIds))
		}
		if read(queued) == nil || read(direct) == nil {
			t.Fatal("the issuer did not publish the queued revocation and preserve the direct one")
		}
		checkSettings := sidecar.DefaultRevocationSettings()
		checkSettings.Clock = clock
		checker := sidecar.NewRevocationChecker(host.Blobs, verifier.VerifyRevocation, checkSettings)
		for _, revocation := range []*model.SidecarRevocation{direct, queued} {
			for _, audience := range []string{"evs", "gkm", "sockets"} {
				issued := &sidecar.Principal{
					NetworkId: [16]byte(revocation.NetworkId), ClientId: [16]byte(revocation.SubjectId),
					Audience: audience, IssuedAt: revocation.NotBefore.Add(-time.Second),
				}
				if err := checker.Check(ctx, issued); !errors.Is(err, sidecar.ErrRevocationVoidsToken) {
					t.Fatalf("%s accepted a token issued before removal: %v", audience, err)
				}
			}
		}
	})
}

// The timer and its handoff can each restart. The issuer independently reads the tip,
// including a network where no proposal or ephemeral admission remains to append.
func TestWorkerHandsBothConfigTimersToTheIssuer(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		host := sidecartest.NewFakeHost(ctx, nil, nil, nil, nil).ServiceHost()
		first, err := host.Blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		settings := model.DefaultConfigLogSettings()
		settings.QueueTicksAtIssuer = true
		model.SetConfigLogSequencer(NewHostedConfigLogSequencer(host, first, settings))
		t.Cleanup(func() { model.SetConfigLogSequencer(nil) })
		client := session.NewLocalClientSession(ctx, "", nil)
		defer client.Cancel()
		past := task.RunAt(server.NowUtc().Add(-time.Minute))
		networkId := server.NewId()
		task.ScheduleTask(model.AppendConfigLogTickTask, &model.AppendConfigLogTickArgs{NetworkId: networkId}, client, past)
		task.ScheduleTask(model.AppendConfigLogActivationTask, &model.AppendConfigLogActivationArgs{NetworkId: networkId, ProposalId: make([]byte, 32)}, client, past)
		due := task.DefaultTaskWorkerSettings()
		due.ClaimRegisteredTargetsOnly = true
		worker := task.NewTaskWorker(ctx, due)
		defer worker.Close()
		worker.AddTargets(
			task.NewTaskTargetWithPost(model.AppendConfigLogTickTask, model.AppendConfigLogTickTaskPost),
			task.NewTaskTargetWithPost(model.AppendConfigLogActivationTask, model.AppendConfigLogActivationTaskPost),
		)
		eval := func(worker *task.TaskWorker, want int) {
			t.Helper()
			finished, retry, postRetry, err := worker.EvalTasks(4)
			if err != nil || len(finished) != want || len(retry)+len(postRetry) != 0 {
				t.Fatalf("wanted %d completions: %v finished, %v/%v retries, %v", want, finished, retry, postRetry, err)
			}
		}
		eval(worker, 2)
		eval(worker, 0)
		model.SetConfigLogSequencer(NewHostedConfigLogSequencer(host, first, model.NewApiConfigLogSettings()))
		makeAuthorizationIssuerTasksDue(t)
		issuer := newAuthorizationIssuerWorker(ctx)
		defer issuer.Close()
		eval(issuer, 2)
		eval(issuer, 0)
	})
}
