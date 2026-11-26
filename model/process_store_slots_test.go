package model

// The rev/ store and the authorization log's sequencer of a process that runs api and taskworker together, as
// server/cli/all's does (§19.4's writers that share a process; owner decision 788): api's signing rev/ store stays
// current beside taskworker's sweep store, and api's sequencer and the worker's each keep a slot of their own,
// whichever is installed first and whichever is removed first. Before, each was one process-wide pointer, so the
// worker's install replaced api's: every rev/ write failed as unsigned, the issuer's tick handoffs handed off again,
// and the worker's removal cleared what api installed. No Postgres.

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// The store writes and sweeps go through now, nil for none.
func testCurrentRevStore() *SidecarRevStore {
	store, err := currentSidecarRevStore()
	if err != nil {
		return nil
	}
	return store
}

// api's signing store stays current beside the worker's sweep store in every order of install and removal, and each
// removal takes out its own store alone.
func TestRevStoreOfOneProcessKeepsTheIssuers(t *testing.T) {
	SetSidecarRevStore(nil)
	defer SetSidecarRevStore(nil)
	s := newTestRevStore(t)
	issuer := s.store
	sweep := NewSidecarRevStore(s.cluster, nil, nil)

	// api's first, as all's main installs api's parts before taskworker starts, and removed last, as all's one undo
	// runs once every service has drained
	removeIssuer := InstallSidecarRevStore(issuer)
	removeSweep := InstallSidecarRevStore(sweep)
	if testCurrentRevStore() != issuer {
		t.Fatal("the worker's sweep store stands in for api's signing store")
	}
	removeSweep()
	if testCurrentRevStore() != issuer {
		t.Fatal("removing the worker's store took away api's")
	}
	removeIssuer()
	if testCurrentRevStore() != nil {
		t.Fatal("a store stands with both removed")
	}

	// api's removed first: the worker's sweeps on
	removeIssuer = InstallSidecarRevStore(issuer)
	removeSweep = InstallSidecarRevStore(sweep)
	removeIssuer()
	if testCurrentRevStore() != sweep {
		t.Fatal("removing api's store took away the worker's")
	}
	removeSweep()
	if testCurrentRevStore() != nil {
		t.Fatal("a store stands with both removed")
	}

	// the worker's first, as when api's mount comes after taskworker's start
	removeSweep = InstallSidecarRevStore(sweep)
	if testCurrentRevStore() != sweep {
		t.Fatal("the worker's sweep store alone is not current")
	}
	removeIssuer = InstallSidecarRevStore(issuer)
	if testCurrentRevStore() != issuer {
		t.Fatal("api's signing store, installed later, is not current")
	}
	removeSweep()
	if testCurrentRevStore() != issuer {
		t.Fatal("removing the worker's store took away api's")
	}
	removeIssuer()
	if testCurrentRevStore() != nil {
		t.Fatal("a store stands with both removed")
	}

	// a removal of a store another install has since replaced takes out nothing
	other := NewSidecarRevStore(s.cluster, testRevSign, nil)
	removeIssuer = InstallSidecarRevStore(issuer)
	removeOther := InstallSidecarRevStore(other)
	removeIssuer()
	if testCurrentRevStore() != other {
		t.Fatal("a stale removal took away the store installed since")
	}
	removeOther()

	// SetSidecarRevStore installs one store alone, and nil removes both
	InstallSidecarRevStore(sweep)
	SetSidecarRevStore(issuer)
	if testCurrentRevStore() != issuer {
		t.Fatal("SetSidecarRevStore's store is not current")
	}
	SetSidecarRevStore(nil)
	if testCurrentRevStore() != nil {
		t.Fatal("SetSidecarRevStore(nil) left a store")
	}
}

// With the worker's sweep store installed beside api's, a removal's write task lands its signed object, and the sweep,
// which reads whichever store is current, deletes it through api's 20 minutes on, spending api's budget and none of
// the worker's. Before, the worker's store replaced api's, and the task failed as unsigned.
func TestRevWritesAndTheSweepGoThroughApisBesideTheWorkers(t *testing.T) {
	SetSidecarRevStore(nil)
	defer SetSidecarRevStore(nil)
	ctx := context.Background()
	s := newTestRevStore(t)
	issuerBudget := sidecartest.NewFakeWriteBudget(nil)
	s.store.SetWriteBudget(issuerBudget, nil)
	removeIssuer := InstallSidecarRevStore(s.store)
	defer removeIssuer()
	sweepSettings := DefaultSidecarRevSettings()
	sweepSettings.Clock = s.clock
	sweep := NewSidecarRevStore(s.cluster, nil, sweepSettings)
	sweepBudget := sidecartest.NewFakeWriteBudget(nil)
	sweep.SetWriteBudget(sweepBudget, nil)
	removeSweep := InstallSidecarRevStore(sweep)
	defer removeSweep()

	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	revocation := newTestRevocation(s.clock.Now())
	if _, err := WriteSidecarRevTask(&WriteSidecarRevArgs{Revocations: []*SidecarRevocation{revocation}}, clientSession); err != nil {
		t.Fatalf("the write task beside the worker's sweep store: %v", err)
	}
	if s.held(t, revocation) == nil {
		t.Fatal("the write task did not publish the signed object")
	}
	created := issuerBudget.P0ByteCount(s.cluster.ClusterId(), sidecar.WriteP0KindRevocation)
	if created <= 0 {
		t.Fatalf("api's create spent %d", created)
	}

	s.clock.Advance(DefaultSidecarRevSettings().DeleteAfter + time.Minute)
	deletedCount, err := SweepSidecarRevocations(ctx)
	if err != nil || deletedCount != 1 {
		t.Fatalf("the sweep deleted %d: %v", deletedCount, err)
	}
	if s.held(t, revocation) != nil {
		t.Fatal("the sweep left the expired object")
	}
	if spent := issuerBudget.P0ByteCount(s.cluster.ClusterId(), sidecar.WriteP0KindRevocation); spent <= created {
		t.Errorf("api's budget spent %d after the sweep, %d before it: the sweep did not go through api's store", spent, created)
	}
	if spent := sweepBudget.P0ByteCount(s.cluster.ClusterId(), sidecar.WriteP0KindRevocation); spent != 0 {
		t.Errorf("the worker's budget spent %d while api's store was current", spent)
	}
	for _, budget := range []*sidecartest.FakeWriteBudget{issuerBudget, sweepBudget} {
		if violations := budget.Violations(); len(violations) != 0 {
			t.Errorf("the budget's violations: %v", violations)
		}
	}
}

// Whether err is the 503 a process with no sequencer answers.
func testConfigLogUnavailable(err error) bool {
	configLogError := &ConfigLogError{}
	return errors.As(err, &configLogError) && configLogError.Status == http.StatusServiceUnavailable
}

// The issuer slot holds api's sequencer alone and the worker slot taskworker's, which falls back to api's where the
// process holds none; each install's removal takes out its own alone.
func TestWorkerSequencerTakesASlotBesideApis(t *testing.T) {
	SetConfigLogSequencer(nil)
	defer SetConfigLogSequencer(nil)
	cluster := testConfigLogFakeCluster(t)
	api := NewConfigLogSequencer(cluster, DefaultConfigLogSettings(), nil)
	workerSettings := DefaultConfigLogSettings()
	workerSettings.QueueTicksAtIssuer = true
	worker := NewConfigLogSequencer(cluster, workerSettings, nil)
	current := func() (*ConfigLogSequencer, *ConfigLogSequencer) {
		t.Helper()
		issuer, issuerErr := CurrentConfigLogSequencer()
		if issuerErr != nil && !testConfigLogUnavailable(issuerErr) {
			t.Fatalf("api's slot answered %v", issuerErr)
		}
		forWorker, workerErr := CurrentWorkerConfigLogSequencer()
		if workerErr != nil && !testConfigLogUnavailable(workerErr) {
			t.Fatalf("the worker's slot answered %v", workerErr)
		}
		return issuer, forWorker
	}

	if issuer, forWorker := current(); issuer != nil || forWorker != nil {
		t.Fatal("a sequencer stands before any install")
	}
	// api's alone, as in api's process or all's whose vault holds api's clusters alone
	SetConfigLogSequencer(api)
	if issuer, forWorker := current(); issuer != api || forWorker != api {
		t.Fatal("the worker's duties do not run on api's sequencer where the process holds no worker's")
	}
	// the worker's beside it, as all's process installs taskworker's after api's parts
	removeWorker := InstallWorkerConfigLogSequencer(worker)
	if issuer, forWorker := current(); issuer != api || forWorker != worker {
		t.Fatal("the worker's sequencer took api's slot, or api's the worker's")
	}
	// api's removed: the worker's keeps its slot, and api's routes answer 503
	SetConfigLogSequencer(nil)
	if issuer, forWorker := current(); issuer != nil || forWorker != worker {
		t.Fatal("removing api's sequencer took away the worker's")
	}
	SetConfigLogSequencer(api)
	// a removal of a worker's sequencer another install has since replaced takes out nothing
	again := NewConfigLogSequencer(cluster, workerSettings, nil)
	removeAgain := InstallWorkerConfigLogSequencer(again)
	removeWorker()
	if issuer, forWorker := current(); issuer != api || forWorker != again {
		t.Fatal("a stale removal took away the worker's sequencer installed since")
	}
	removeAgain()
	if issuer, forWorker := current(); issuer != api || forWorker != api {
		t.Fatal("the worker's removal took away api's sequencer")
	}
}

// With taskworker's sequencer, which queues ticks at the issuer, in the worker's slot and api's in its own, the tick
// and activation tasks hand their runs to the issuer and the issuer's handoffs append through api's, never handing
// off again; here at a network whose log is empty, so the handoffs append nothing. Before, the worker's sequencer
// replaced api's, and each handoff handed off again.
func TestTickHandoffsReadTheirOwnSlots(t *testing.T) {
	SetConfigLogSequencer(nil)
	defer SetConfigLogSequencer(nil)
	cluster := testConfigLogFakeCluster(t)
	SetConfigLogSequencer(NewConfigLogSequencer(cluster, DefaultConfigLogSettings(), nil))
	workerSettings := DefaultConfigLogSettings()
	workerSettings.QueueTicksAtIssuer = true
	defer InstallWorkerConfigLogSequencer(NewConfigLogSequencer(cluster, workerSettings, nil))()
	clientSession := session.NewLocalClientSession(context.Background(), "", nil)
	defer clientSession.Cancel()
	networkId := server.NewId()

	tickArgs := &AppendConfigLogTickArgs{NetworkId: networkId}
	tick, err := AppendConfigLogTickTask(tickArgs, clientSession)
	if err != nil || !tick.PublishAtIssuer {
		t.Fatalf("the worker's tick task: %+v, %v", tick, err)
	}
	published, err := PublishConfigLogTickTask(tickArgs, clientSession)
	if err != nil || published.PublishAtIssuer || published.N != nil || published.More {
		t.Fatalf("the issuer's tick handoff: %+v, %v", published, err)
	}

	activationArgs := &AppendConfigLogActivationArgs{NetworkId: networkId, ProposalId: make([]byte, 32)}
	activation, err := AppendConfigLogActivationTask(activationArgs, clientSession)
	if err != nil || !activation.PublishAtIssuer {
		t.Fatalf("the worker's activation task: %+v, %v", activation, err)
	}
	publishedActivation, err := PublishConfigLogActivationTask(activationArgs, clientSession)
	if err != nil || publishedActivation.PublishAtIssuer || publishedActivation.N != nil || publishedActivation.RunAt != nil {
		t.Fatalf("the issuer's activation handoff: %+v, %v", publishedActivation, err)
	}

	// with no sequencer of api's, the handoffs wait rather than run on the worker's
	SetConfigLogSequencer(nil)
	if result, err := PublishConfigLogTickTask(tickArgs, clientSession); err == nil {
		t.Fatalf("a tick handoff with no api sequencer: %+v", result)
	}
	if result, err := PublishConfigLogActivationTask(activationArgs, clientSession); err == nil {
		t.Fatalf("an activation handoff with no api sequencer: %+v", result)
	}
}
