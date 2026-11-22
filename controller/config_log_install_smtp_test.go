package controller

// api's projection installs §14.14's two §8 checks over the gate cache it publishes (installApiProjection): before it
// none is installed, which answered every DKIM publish and every reports read 503; while it runs the checks are
// installed beside the cache, answering a network that cache has not loaded unavailable for both, the closed
// direction; and its close takes them out again.

import (
	"errors"
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

func TestApiProjectionInstallsTheSmtpRoutesChecks(t *testing.T) {
	defer model.SetSmtpApiGate(nil)()
	host := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil).ServiceHost()
	closeViews, err := installApiProjection(t.Context(), host)
	if err != nil {
		t.Fatal(err)
	}
	gate := model.CurrentSmtpApiGate()
	if gate == nil || ApiAuthorizationCache() == nil {
		closeViews()
		t.Fatalf("the projection installed checks %v over the cache %v", gate, ApiAuthorizationCache())
	}
	networkId, sidecarId, now := server.NewId(), server.NewId(), server.NowUtc()
	if allowed, err := gate.MaySendMail(t.Context(), networkId, sidecarId, now); allowed || !errors.Is(err, model.ErrSmtpApiUnavailable) {
		t.Errorf("mail_send for a network the cache has not loaded: %t, %v", allowed, err)
	}
	if allowed, err := gate.MayReadReports(t.Context(), networkId, sidecarId, now); allowed || !errors.Is(err, model.ErrSmtpApiUnavailable) {
		t.Errorf("the reports read for a network the cache has not loaded: %t, %v", allowed, err)
	}
	closeViews()
	if model.CurrentSmtpApiGate() != nil {
		t.Fatal("the closed projection left its checks installed")
	}
}
