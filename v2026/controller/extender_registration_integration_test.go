package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/extender"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Extender registration end to end (connect/EXTENDER.md C2, lag.dev/PLAN.md
// C4): connect's own activation loop, as every extender runs it, posts to the
// activation and certificate routes, which run this controller on the test
// database and probe the fixture extender back. The loop learns its id from
// the first answer and carries the measurement request on the next, and the
// request is stored for that id's name.
//
// The unit tests on either side each stand in for the other side. These
// don't, so a request that one side never sends or the other never stores
// fails here: lag.dev went empty on Main with every extender activating and
// none ever asking for a certificate (2026-10-22).

// Serves the two routes the activation loop uses, as the api does, for one
// client session.
func newTestExtenderRegistrationApi(t testing.TB, clientSession *session.ClientSession) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+connect.ExtenderActivatePath, func(w http.ResponseWriter, req *http.Request) {
		var args ExtenderActivateArgs
		if err := json.NewDecoder(req.Body).Decode(&args); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		result, err := ExtenderActivate(&args, clientSession)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})
	mux.HandleFunc("GET "+extender.ExtenderMeasurementCertPath, func(w http.ResponseWriter, req *http.Request) {
		result, err := ExtenderMeasurementCert(&ExtenderMeasurementCertArgs{
			PublicKeyHex: req.URL.Query().Get("public_key_hex"),
		}, clientSession)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(result)
	})
	apiServer := httptest.NewServer(mux)
	t.Cleanup(apiServer.Close)
	return apiServer.URL
}

// Runs connect's activation loop for the fixture against the api at apiUrl,
// asking for a measurement certificate for measurementCert's key when it is
// set, as an extender with a kept measurement key does.
func startTestExtenderRegistration(
	t testing.TB,
	ctx context.Context,
	fixture *testExtenderFixture,
	apiUrl string,
	measurementCert *extender.ExtenderMeasurementCert,
) *connect.ExtenderActivator {
	t.Helper()
	settings := connect.DefaultExtenderActivatorSettings()
	// the plain url, so one post a pass whose family the operator derives
	// from the caller address (C2)
	settings.ApiUrl = apiUrl
	settings.ByJwt = func() string { return "" }
	settings.ClientStrategy = connect.NewClientStrategyWithDefaults(ctx)
	settings.PublicKey = fixture.publicKey
	settings.TcpPort = fixture.tcpPort
	settings.UdpPort = fixture.quicPort
	settings.DnsPort = fixture.dnsPort
	settings.DnsTld = testExtenderDnsTld
	settings.Carriers = func() []string {
		return []string{connect.ExtenderCarrierTcp}
	}
	if measurementCert != nil {
		settings.MeasurementCsr = measurementCert.ActivationCsr
	}
	activator := connect.NewExtenderActivator(ctx, settings)
	t.Cleanup(activator.Close)
	return activator
}

// The id of the active extender stored for publicKey, if there is one yet.
func testFindExtenderIdForKey(ctx context.Context, publicKey ed25519.PublicKey) (server.Id, bool) {
	for _, entry := range model.GetRandomActiveNetworkExtenders(ctx, 64, server.Id{}) {
		if string(entry.Extender.PublicKey) == string(publicKey) {
			return entry.Extender.ExtenderId, true
		}
	}
	return server.Id{}, false
}

// Waits for the fixture's extender to be stored, and returns its id.
func waitTestExtenderRegistered(t testing.TB, ctx context.Context, fixture *testExtenderFixture) server.Id {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		if extenderId, ok := testFindExtenderIdForKey(ctx, fixture.publicKey); ok {
			return extenderId
		}
		if time.Now().After(deadline) {
			t.Fatal("the extender was never registered")
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// An extender with a measurement key registers, learns its id, and asks for a
// certificate for that id's measurement name, which the operator stores
// pending for the issuance task (C4, C5) and answers the extender's poll with.
func TestExtenderRegistrationCarriesTheMeasurementRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fixture := newTestExtenderFixture(t)
		installTestExtenderMeasurementConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		apiUrl := newTestExtenderRegistrationApi(t, clientSession)

		measurementCert, err := extender.NewExtenderMeasurementCert(
			filepath.Join(t.TempDir(), extender.ExtenderMeasurementKeyFileName),
			testMeasurementZone,
		)
		if err != nil {
			t.Fatal(err)
		}
		startTestExtenderRegistration(t, ctx, fixture, apiUrl, measurementCert)

		extenderId := waitTestExtenderRegistered(t, ctx, fixture)
		name := ExtenderMeasurementName(extenderId, testMeasurementZone)
		deadline := time.Now().Add(30 * time.Second)
		var measurement *model.NetworkExtenderMeasurement
		for {
			measurement = model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId)
			if measurement != nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("the activation loop never carried a measurement request")
			}
			time.Sleep(100 * time.Millisecond)
		}
		connect.AssertEqual(t, measurement.State, model.NetworkExtenderMeasurementStatePending)
		if !slices.Equal(measurement.Names, []string{name}) {
			t.Fatalf("names = %v, expected %v", measurement.Names, []string{name})
		}
		expectedPublicKey, err := x509.MarshalPKIXPublicKey(measurementCert.PublicKey())
		if err != nil {
			t.Fatal(err)
		}
		connect.AssertEqual(t, slices.Equal(measurement.PublicKey, expectedPublicKey), true)
	})
}

// An extender that asks for no certificate -- no kept measurement key, as the
// sn miner ran before 2026-10-22 -- still registers, and leaves nothing for
// lag.dev: no request is stored, so its name is never issued or offered (A4).
func TestExtenderRegistrationWithoutAMeasurementKeyStoresNoRequest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		fixture := newTestExtenderFixture(t)
		installTestExtenderMeasurementConfig(t, fixture.api)
		clientSession := newTestExtenderSession(t, ctx, fixture.clientAddress("127.0.0.1"))
		apiUrl := newTestExtenderRegistrationApi(t, clientSession)

		activator := startTestExtenderRegistration(t, ctx, fixture, apiUrl, nil)
		extenderId := waitTestExtenderRegistered(t, ctx, fixture)
		// a pass after the one that registered, which is where a request
		// would have ridden
		activator.Reactivate()
		time.Sleep(2 * time.Second)
		if measurement := model.Testing_GetNetworkExtenderMeasurement(ctx, extenderId); measurement != nil {
			t.Fatalf("a request was stored with no measurement key: %v", measurement.State)
		}
	})
}
