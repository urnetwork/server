// Synthetic private leaves and loopback HTTP prove the deployed auth shapes.
// No real credential, Grafana/Mimir service or external network is consulted.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A typed private Basic leaf is applied only to its exact validated origin and
// cannot be serialized into launch JSON by accident.
func TestLaunchHttpCredentialLoadsPrivateBasic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "basic.json")
	if err := os.WriteFile(path, []byte(`{"schema":1,"kind":"sim-latency-launch-basic-credential","username":"synthetic-reader","password":"synthetic-private-password"}`), 0600); err != nil {
		t.Fatal(err)
	}
	credential, err := readLaunchHttpCredential(path, defaultLaunchHttpCredentialSettings())
	if err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if !ok || username != "synthetic-reader" || password != "synthetic-private-password" {
			t.Error("wrong Basic credential at loopback origin")
		}
		response.Write([]byte(`{"ok":true}`))
	}))
	defer endpoint.Close()
	if _, err := getLaunchBytesWithCredential(t.Context(), newLaunchHttpClient(), endpoint.URL, "/read-only", credential); err != nil {
		t.Fatal(err)
	}
	serialized, err := json.Marshal(credential)
	if err != nil || string(serialized) != "{}" {
		t.Fatal("private credential entered JSON serialization")
	}
}

// Raw single-line bearer files preserve their existing CLI interpretation.
func TestLaunchHttpCredentialPreservesLegacyBearer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bearer.token")
	if err := os.WriteFile(path, []byte("synthetic-legacy-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	credential, err := readLaunchHttpCredential(path, defaultLaunchHttpCredentialSettings())
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://monitoring.example/read", nil)
	if err := credential.apply(request); err != nil || request.Header.Get("Authorization") != "Bearer synthetic-legacy-token" {
		t.Fatal("legacy bearer semantics changed")
	}
	if _, err := selectLaunchHttpCredential(&credential, "another-synthetic-token"); err == nil {
		t.Fatal("ambiguous credentials silently selected")
	}
}

// Bad private formats, links, public permissions and size limits fail without
// returning the raw secret or a credential-bearing JSON parse diagnostic.
func TestLaunchHttpCredentialRejectsUnsafeOrMalformedLeaves(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "credential.json")
	secret := "synthetic-private-password"
	for index, content := range []string{`{"schema":1,"kind":"wrong","username":"synthetic-reader","password":"` + secret + `"}`, `{"schema":1,"kind":"sim-latency-launch-basic-credential","Username":"synthetic-reader","password":"` + secret + `"}`, `{"schema":1,"kind":"sim-latency-launch-basic-credential","username":"synthetic-reader","password":"` + secret + `","unknown":"extra"}`, `{"schema":1,"kind":"sim-latency-launch-basic-credential","username":"synthetic:reader","password":"` + secret + `"}`, `{"schema":1,"kind":"sim-latency-launch-basic-credential","username":"synthetic-reader","password":null}`} {
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readLaunchHttpCredential(path, defaultLaunchHttpCredentialSettings())
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Errorf("malformed leaf %d accepted or leaked secret", index)
		}
	}
	if err := os.WriteFile(path, []byte("synthetic-token"), 0600); err != nil {
		t.Fatal(err)
	}
	settings := defaultLaunchHttpCredentialSettings()
	settings.MaximumBytes = 1
	if _, err := readLaunchHttpCredential(path, settings); err == nil {
		t.Fatal("oversized credential accepted")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchHttpCredential(path, defaultLaunchHttpCredentialSettings()); err == nil {
		t.Fatal("public credential accepted")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(root, "linked.json")
	if err := os.Symlink(path, linked); err != nil {
		t.Fatal(err)
	}
	if _, err := readLaunchHttpCredential(linked, defaultLaunchHttpCredentialSettings()); err == nil {
		t.Fatal("redirected credential accepted")
	}
}

// Real-shape Basic credentials are distinct for Grafana provisioning reads and
// Mimir's query-only /prometheus route. Existing CLI flag names do not change.
func TestLaunchHttpCredentialSupportsScopedGrafanaAndMimir(t *testing.T) {
	queries := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		if strings.HasPrefix(request.URL.Path, "/prometheus/") {
			if !ok || username != "synthetic-query" || password != "synthetic-query-password" {
				t.Error("Grafana authority crossed into metrics")
			}
			queries++
			value := "12"
			if queries == 2 {
				value = "42"
			}
			response.Write([]byte(`{"status":"success","data":{"result":[{"value":[1,"` + value + `"]}]}}`))
			return
		}
		if !ok || username != "synthetic-grafana-reader" || password != "synthetic-grafana-password" {
			t.Error("wrong Grafana read authority")
		}
		switch request.URL.Path {
		case "/api/health":
			response.Write([]byte(`{"database":"ok"}`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer endpoint.Close()
	config := defaultLaunchPreflightConfig()
	config.GrafanaUrl, config.MetricsUrl = endpoint.URL, endpoint.URL+"/prometheus"
	config.GrafanaCredential = &launchHttpCredential{username: "synthetic-grafana-reader", password: "synthetic-grafana-password"}
	config.MetricsCredential = &launchHttpCredential{username: "synthetic-query", password: "synthetic-query-password"}
	credential, err := selectLaunchHttpCredential(config.GrafanaCredential, config.GrafanaToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := getLaunchBytesWithCredential(t.Context(), newLaunchHttpClient(), config.GrafanaUrl, "/api/health", credential); err != nil {
		t.Fatal(err)
	}
	if _, err := checkCompetitionMetrics(t.Context(), config); err != nil || queries != 2 {
		t.Fatalf("query route/credential failed: %v", err)
	}
}

// Credentials never follow a redirect or an ambient proxy selected by process
// environment. A denied response exposes status only, not its echoed body.
func TestLaunchHttpCredentialCannotEscapeViaRedirectOrProxy(t *testing.T) {
	called := make(chan struct{}, 1)
	destination := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		called <- struct{}{}
		response.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Location", destination.URL)
		response.WriteHeader(http.StatusFound)
		response.Write([]byte("synthetic-private-password"))
	}))
	defer redirect.Close()
	client := newLaunchHttpClient()
	if transport, ok := client.Transport.(*http.Transport); !ok || transport.Proxy != nil {
		t.Fatal("ambient proxy can receive launch credentials")
	}
	_, err := getLaunchBytesWithCredential(context.Background(), client, redirect.URL, "/redirect", launchHttpCredential{username: "synthetic-reader", password: "synthetic-private-password"})
	if err == nil || strings.Contains(err.Error(), "synthetic-private-password") {
		t.Fatal("redirect allowed or echoed body leaked credential")
	}
	select {
	case <-called:
		t.Fatal("credential followed redirect")
	default:
	}
}
