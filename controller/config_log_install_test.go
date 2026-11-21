package controller

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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
