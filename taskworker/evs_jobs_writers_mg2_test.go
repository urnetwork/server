//go:build sidecar_pending

package taskworker

import (
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// With MG2 linked, the writers install taskworker's sequencer in the worker's slot, beside api's (§19.4's writers that
// share a process; owner decision 788): api's log routes and the issuer's tick handoffs keep api's, the worker's
// duties read the worker's, which hands its ticks to the issuer, and the writers' removal leaves api's in its slot and
// the worker's duties back on it. Before, the worker's sequencer replaced api's, the issuer's handoffs handed off
// again, and the writers' removal cleared api's.
func TestEvsJobWritersInstallTheWorkersSequencerBesideApis(t *testing.T) {
	if !server.SidecarMg2Linked() {
		t.Fatal("this binary does not link MG2")
	}
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := evsservice.NewBudgetedBlobs(sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings()))
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	model.SetSidecarRevStore(nil)
	defer model.SetSidecarRevStore(nil)
	apiSettings := model.DefaultConfigLogSettings()
	apiSettings.Clock = clock
	api := model.NewConfigLogSequencer(first, apiSettings, nil)
	model.SetConfigLogSequencer(api)
	defer model.SetConfigLogSequencer(nil)
	host := &sidecar.ServiceHost{Blobs: blobs, Budget: sidecartest.NewFakeWriteBudget(nil), Clock: clock}
	closeWriters := installEvsJobWriters(host, first, nil)
	defer closeWriters()

	if issuer, err := model.CurrentConfigLogSequencer(); err != nil || issuer != api {
		t.Fatalf("api's slot holds another sequencer beside the worker's writers: %v", err)
	}
	worker, err := model.CurrentWorkerConfigLogSequencer()
	if err != nil || worker == nil || worker == api {
		t.Fatalf("the worker's duties do not read the worker's sequencer: %v", err)
	}
	clientSession := session.NewLocalClientSession(ctx, "", nil)
	defer clientSession.Cancel()
	args := &model.AppendConfigLogTickArgs{NetworkId: server.NewId()}
	if tick, err := model.AppendConfigLogTickTask(args, clientSession); err != nil || !tick.PublishAtIssuer {
		t.Fatalf("the worker's tick task did not hand its run to the issuer: %+v, %v", tick, err)
	}
	if published, err := model.PublishConfigLogTickTask(args, clientSession); err != nil || published.PublishAtIssuer {
		t.Fatalf("the issuer's handoff did not append through api's sequencer: %+v, %v", published, err)
	}

	closeWriters()
	if issuer, err := model.CurrentConfigLogSequencer(); err != nil || issuer != api {
		t.Fatalf("the writers' removal took away api's sequencer: %v", err)
	}
	if forWorker, err := model.CurrentWorkerConfigLogSequencer(); err != nil || forWorker != api {
		t.Fatalf("after the writers' removal the worker's duties do not fall back to api's sequencer: %v", err)
	}
}
