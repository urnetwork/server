package connect

// The provider rollout gauge: the bounded app release label, the per-label
// connection counts, and the count kept current by the announce through every
// transport carrier.

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/quic-go/quic-go"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Reads every app_version series of the gauge without creating any.
func gatherProviderConnections(t testing.TB, gauge *prometheus.GaugeVec) map[string]float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	registry.MustRegister(gauge)
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]float64{}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				if label.GetName() == "app_version" {
					values[label.GetValue()] = metric.GetGauge().GetValue()
				}
			}
		}
	}
	return values
}

// The label is the app's calendar release without the build code: the day
// while the release is recent, the month once it is older, and a closed set
// of fallbacks for everything else, so a client cannot mint labels.
func TestProviderAppVersionLabel(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		appVersion string
		label      string
	}{
		// android and windows send the build code, apple does not
		{appVersion: "2026.10.1-1060587890", label: "2026.10.1"},
		{appVersion: "2026.10.1", label: "2026.10.1"},
		{appVersion: " v2026.10.01 ", label: "2026.10.1"},
		{appVersion: "2026.10.6+1060587890", label: "2026.10.6"},
		// tomorrow's release in another time zone
		{appVersion: "2026.10.6", label: "2026.10.6"},
		// still inside the day window (62 days back from noon)
		{appVersion: "2026.8.5-1", label: "2026.8.5"},
		// older releases fold into their month
		{appVersion: "2026.8.4-1", label: "2026.8"},
		{appVersion: "2025.3.31-58605629", label: "2025.3"},
		{appVersion: "", label: "unknown"},
		{appVersion: "   ", label: "unknown"},
		{appVersion: "0.0.40", label: "other"},
		{appVersion: "0.0.1", label: "other"},
		{appVersion: "0.0.0-sim", label: "other"},
		{appVersion: "urmessage", label: "other"},
		{appVersion: "connectctl 2026.10.1", label: "other"},
		{appVersion: "2026.10", label: "other"},
		{appVersion: "2026.10.1.2", label: "other"},
		{appVersion: "2026.13.1", label: "other"},
		{appVersion: "2026.2.30", label: "other"},
		{appVersion: "2026.0.1", label: "other"},
		{appVersion: "2026.10.0", label: "other"},
		{appVersion: "2024.12.31", label: "other"},
		{appVersion: "2026.10.8", label: "other"},
		{appVersion: "2099.1.1", label: "other"},
		{appVersion: "20266.1.1", label: "other"},
		{appVersion: "2026.+1.1", label: "other"},
		{appVersion: "2026..1", label: "other"},
	} {
		if label := providerAppVersionLabel(c.appVersion, now); label != c.label {
			t.Errorf("providerAppVersionLabel(%q) = %q, want %q", c.appVersion, label, c.label)
		}
	}
}

// Each connection counts once under its label while its client provides
// publicly; a label's series is deleted when its last connection leaves, so
// only live versions are exported.
func TestProviderVersionConnections(t *testing.T) {
	gauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "urnetwork",
			Subsystem: "connect",
			Name:      "provider_connections_test",
		},
		[]string{"app_version"},
	)
	connections := newProviderVersionConnections(gauge)
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

	a := newProviderVersionConnection(connections, "2026.10.1-1060587890")
	b := newProviderVersionConnection(connections, "2026.10.1")
	c := newProviderVersionConnection(connections, "")
	consumer := newProviderVersionConnection(connections, "2026.9.30-1060363020")

	a.update(true, now)
	b.update(true, now)
	c.update(true, now)
	consumer.update(false, now)
	// repeated syncs do not double count
	a.update(true, now)
	values := gatherProviderConnections(t, gauge)
	if len(values) != 2 || values["2026.10.1"] != 2 || values["unknown"] != 1 {
		t.Fatalf("provider connections = %v, want 2026.10.1=2 unknown=1", values)
	}

	// a client that stops providing publicly leaves the gauge
	b.update(false, now)
	if values := gatherProviderConnections(t, gauge); values["2026.10.1"] != 1 {
		t.Fatalf("provider connections = %v, want 2026.10.1=1", values)
	}

	// a long connection moves from its release day to its month
	later := now.Add(providerAppVersionDayWindow)
	a.update(true, later)
	values = gatherProviderConnections(t, gauge)
	if _, ok := values["2026.10.1"]; ok || values["2026.10"] != 1 {
		t.Fatalf("provider connections = %v, want the day series gone and 2026.10=1", values)
	}

	a.release()
	c.release()
	b.release()
	consumer.release()
	if values := gatherProviderConnections(t, gauge); len(values) != 0 {
		t.Fatalf("provider connections after every release = %v, want no series", values)
	}
}

// The announce keeps the gauge current from the provide modes its sync reads:
// a connection counts under its client's app version while the client
// provides publicly, leaves when it stops, and leaves when it closes. A
// connection of a client that does not provide is never counted.
//
// Requires the test DB env (WARP_ENV=local + postgres/redis/vault), like the
// rest of this package; skipped under -short.
func TestConnectionAnnounceProviderVersionGauge(t *testing.T) {
	if testing.Short() {
		return
	}
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		networkId := server.NewId()
		handlerId := server.NewId()

		announceSettings := DefaultConnectionAnnounceSettings()
		announceSettings.SyncConnectionTimeout = 50 * time.Millisecond
		announceSettings.LocationRetryTimeout = 0

		// yesterday's release: no other test connection uses this label
		release := server.NowUtc().AddDate(0, 0, -1)
		appVersion := fmt.Sprintf("%d.%d.%d-1060587890", release.Year(), release.Month(), release.Day())
		label := fmt.Sprintf("%d.%d.%d", release.Year(), release.Month(), release.Day())

		newClient := func(provide bool) server.Id {
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
			if provide {
				model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
					model.ProvideModePublic: make([]byte, 32),
				})
			}
			return clientId
		}
		startAnnounce := func(clientId server.Id) *ConnectionAnnounce {
			announceCtx, announceCancel := context.WithCancel(ctx)
			return NewConnectionAnnounceWithIpFamily(
				announceCtx,
				announceCancel,
				networkId,
				clientId,
				"127.0.0.1:20000",
				0,
				appVersion,
				handlerId,
				0,
				&TestConfig{},
				announceSettings,
			)
		}
		waitFor := func(want float64) {
			t.Helper()
			waitForProviderConnections(ctx, t, label, want)
		}

		providerId := newClient(true)
		consumer := startAnnounce(newClient(false))
		defer consumer.CloseAndWait()
		first := startAnnounce(providerId)
		waitFor(1)
		second := startAnnounce(providerId)
		waitFor(2)

		// the client stops providing publicly, then provides again
		model.SetProvide(ctx, providerId, map[model.ProvideMode][]byte{})
		waitFor(0)
		model.SetProvide(ctx, providerId, map[model.ProvideMode][]byte{
			model.ProvideModePublic: make([]byte, 32),
		})
		waitFor(2)

		first.CloseAndWait()
		waitFor(1)
		second.CloseAndWait()
		waitFor(0)
	})
}

// Waits for the gauge's label to reach want. Zero is no series at all: an
// empty label is deleted, not left at 0.
func waitForProviderConnections(ctx context.Context, t testing.TB, label string, want float64) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		value, present := gatherProviderConnections(t, providerConnectionsGauge)[label]
		if (want == 0 && !present) || (want != 0 && value == want) {
			return
		}
		if deadline.Before(time.Now()) {
			t.Fatalf("provider connections %s = %v (series present %t), want %v", label, value, present, want)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// Every carrier hands the client's app version to the announce, so a
// providing client is counted under its release whichever transport it uses.
func TestConnectProviderVersionGaugeCarriers(t *testing.T) {
	if testing.Short() {
		return
	}
	for _, carrier := range []string{"h1_header", "h1_frame", "h3"} {
		t.Logf("carrier %s", carrier)
		server.DefaultTestEnv().Run(t, func(t testing.TB) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			env := testing_newPeerDiscoveryEnvWithAllSettings(ctx, t, func(settings *ExchangeSettings) {
				settings.EnableNetworkPeers = false
			}, func(settings *ConnectHandlerSettings) {
				settings.ConnectionAnnounceSettings.EnableNetworkPeers = false
				settings.ConnectionAnnounceSettings.SyncConnectionTimeout = 50 * time.Millisecond
				settings.ConnectionAnnounceSettings.LocationRetryTimeout = 0
			})
			defer env.Close()

			clientId, token := env.authClient(&model.AuthNetworkClientArgs{
				Description: "provider version fixture",
				DeviceSpec:  "synthetic",
			})
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{
				model.ProvideModePublic: make([]byte, 32),
			})

			release := server.NowUtc()
			appVersion := fmt.Sprintf("%d.%d.%d-1060587890", release.Year(), release.Month(), release.Day())
			label := fmt.Sprintf("%d.%d.%d", release.Year(), release.Month(), release.Day())

			// connects through the carrier's production entry point with the app
			// version where that carrier sends it: the X-UR-AppVersion header on
			// H1, the auth message on H1 frame auth and H3. Returns the close.
			dialClient := func() func() {
				t.Helper()
				instanceId := server.NewId()
				authBytes, err := connect.EncodeFrame(&protocol.Auth{
					ByJwt:      token,
					InstanceId: instanceId.Bytes(),
					AppVersion: appVersion,
				}, connect.DefaultProtocolVersion)
				if err != nil {
					t.Fatal(err)
				}
				defer connect.MessagePoolReturn(authBytes)
				switch carrier {
				case "h3":
					conn, err := quic.DialAddr(ctx, fmt.Sprintf("127.0.0.1:%d", env.h3Port), &tls.Config{
						InsecureSkipVerify: true, ServerName: "127.0.0.1", // Private fixture certificate only.
					}, &quic.Config{MaxIdleTimeout: 10 * time.Second})
					if err != nil {
						t.Fatal(err)
					}
					stream, err := conn.OpenStreamSync(ctx)
					if err != nil {
						conn.CloseWithError(0, "")
						t.Fatal(err)
					}
					if err := connect.NewFramer(env.handler.settings.FramerSettings).Write(stream, authBytes); err != nil {
						conn.CloseWithError(0, "")
						t.Fatal(err)
					}
					return func() { conn.CloseWithError(0, "") }
				case "h1_header":
					header := http.Header{}
					header.Set("Authorization", "Bearer "+token)
					header.Set("X-UR-AppVersion", appVersion)
					header.Set("X-UR-InstanceId", instanceId.String())
					header.Set("X-UR-TransportVersion", "2")
					ws, _, err := websocket.DefaultDialer.DialContext(ctx, fmt.Sprintf("ws://127.0.0.1:%d/", env.port), header)
					if err != nil {
						t.Fatal(err)
					}
					return func() { ws.Close() }
				case "h1_frame":
					ws, _, err := websocket.DefaultDialer.DialContext(ctx, fmt.Sprintf("ws://127.0.0.1:%d/", env.port), nil)
					if err != nil {
						t.Fatal(err)
					}
					if err := ws.WriteMessage(websocket.BinaryMessage, authBytes); err != nil {
						ws.Close()
						t.Fatal(err)
					}
					return func() { ws.Close() }
				default:
					t.Fatalf("unknown carrier %s", carrier)
					return nil
				}
			}

			closeClient := dialClient()
			waitForProviderConnections(ctx, t, label, 1)
			closeClient()
			waitForProviderConnections(ctx, t, label, 0)
		})
	}
}
