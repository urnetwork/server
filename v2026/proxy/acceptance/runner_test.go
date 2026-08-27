package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunReportsEachProtocolAndAlwaysRemovesClient(t *testing.T) {
	const (
		networkJWT = "network-jwt-secret"
		clientID   = "client-id-secret"
		proxyToken = "proxy-token-secret"
	)
	var mu sync.Mutex
	paths := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		mu.Lock()
		paths = append(paths, request.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			var body map[string]any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode login: %v", err)
			}
			if body["user_auth"] != "person@example.invalid" || body["password"] != "password-secret" {
				t.Errorf("unexpected login body: %#v", body)
			}
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"` + networkJWT + `"}}`))
		case "/network/auth-client":
			if request.Header.Get("Authorization") != "Bearer "+networkJWT {
				t.Errorf("provision authorization was not the network JWT")
			}
			var body struct {
				ProxyConfig struct {
					LockCallerIP       bool `json:"lock_caller_ip"`
					EnableWG           bool `json:"enable_wg"`
					InitialDeviceState struct {
						CountryCode string `json:"country_code"`
					} `json:"initial_device_state"`
				} `json:"proxy_config"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode provision: %v", err)
			}
			if body.ProxyConfig.LockCallerIP || !body.ProxyConfig.EnableWG || body.ProxyConfig.InitialDeviceState.CountryCode != "us" {
				t.Errorf("unexpected proxy config: %#v", body.ProxyConfig)
			}
			_, _ = w.Write([]byte(`{
                    "client_id":"` + clientID + `",
                    "proxy_config_result":{
                        "socks_proxy_url":"socks5h://proxy.example:8080",
                        "http_proxy_url":"http://proxy.example:8081",
                        "api_base_url":"https://api.proxy.example:8083",
                        "auth_token":"` + proxyToken + `",
                        "proxy_host":"proxy.example",
                        "wg_config":{
                            "wg_proxy_port":8084,
                            "client_private_key":"private-key-secret",
                            "client_public_key":"public-key",
                            "proxy_public_key":"proxy-public-key",
                            "client_ipv4":"10.0.0.2",
                            "config":"wireguard-config-secret"
                        }
                    }
                }`))
		case "/network/remove-client":
			if request.Header.Get("Authorization") != "Bearer "+networkJWT {
				t.Errorf("cleanup authorization was not the network JWT")
			}
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode cleanup: %v", err)
			}
			if body["client_id"] != clientID {
				t.Errorf("cleanup client id = %q", body["client_id"])
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	results := runWithDependencies(context.Background(), Options{
		APIURL:          server.URL,
		TargetURL:       server.URL + "/target",
		CredentialsPath: "injected-for-test",
		Repeat:          1,
		ProbeTimeout:    time.Second,
	}, runDependencies{
		credentials: &credentials{user: "person@example.invalid", password: "password-secret"},
		httpClient:  server.Client(),
		probes: func(config *proxyConfigResult) map[string]protocolProbe {
			if config.AuthToken != proxyToken {
				t.Errorf("probe factory received auth token %q", config.AuthToken)
			}
			return map[string]protocolProbe{
				"socks":     func(context.Context) error { return errors.New("rejected " + proxyToken + " for " + clientID) },
				"http":      func(context.Context) error { return nil },
				"wireguard": func(context.Context) error { return nil },
			}
		},
	})

	assertResult(t, results, "socks", "FAIL")
	assertResult(t, results, "http", "PASS")
	assertResult(t, results, "wireguard", "PASS")
	for _, result := range results {
		if strings.Contains(result.Detail, proxyToken) || strings.Contains(result.Detail, clientID) || strings.Contains(result.Detail, networkJWT) {
			t.Fatalf("result leaked a secret: %q", result.Detail)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	wantPaths := []string{"/auth/login-with-password", "/network/auth-client", "/network/remove-client"}
	if strings.Join(paths, ",") != strings.Join(wantPaths, ",") {
		t.Fatalf("request paths = %v, want %v", paths, wantPaths)
	}
}

func TestRunRemovesPartiallyProvisionedClientAndFailsEveryProtocol(t *testing.T) {
	removed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			_, _ = w.Write([]byte(`{"client_id":"partial-client","error":{"message":"proxy allocation failed"}}`))
		case "/network/remove-client":
			removed = true
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	results := runWithDependencies(context.Background(), Options{
		APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
	}, runDependencies{
		credentials: &credentials{user: "user", password: "password"},
		httpClient:  server.Client(),
		probes: func(*proxyConfigResult) map[string]protocolProbe {
			t.Fatal("protocol probes ran after provisioning failed")
			return nil
		},
	})
	if !removed {
		t.Fatal("partially provisioned client was not removed")
	}
	for _, name := range protocolNames {
		assertResult(t, results, name, "FAIL")
	}
}

func TestCancellationDuringProvisionStillCapturesAndRemovesClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	removed := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			cancel()
			_, _ = w.Write([]byte(`{"client_id":"interrupted-client","proxy_config_result":{"auth_token":"token"}}`))
		case "/network/remove-client":
			removed = true
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	results := runWithDependencies(ctx, Options{
		APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
	}, runDependencies{
		credentials: &credentials{user: "user", password: "password"},
		httpClient:  server.Client(),
		probes: func(*proxyConfigResult) map[string]protocolProbe {
			t.Fatal("protocol probes ran after cancellation")
			return nil
		},
	})
	if !removed {
		t.Fatal("client created during cancellation was not removed")
	}
	for _, name := range protocolNames {
		result := assertResult(t, results, name, "FAIL")
		if !strings.Contains(result.Detail, "context canceled") {
			t.Fatalf("%s cancellation detail = %q", name, result.Detail)
		}
	}
}

func TestCleanupFailureFailsOtherwisePassingProtocols(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch request.URL.Path {
		case "/auth/login-with-password":
			_, _ = w.Write([]byte(`{"network":{"by_jwt":"jwt"}}`))
		case "/network/auth-client":
			_, _ = w.Write([]byte(`{"client_id":"client","proxy_config_result":{"auth_token":"token"}}`))
		case "/network/remove-client":
			_, _ = w.Write([]byte(`{"error":{"message":"cleanup failed"}}`))
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()
	passing := func(context.Context) error { return nil }
	results := runWithDependencies(context.Background(), Options{
		APIURL: server.URL, TargetURL: server.URL + "/target", CredentialsPath: "injected", Repeat: 1,
	}, runDependencies{
		credentials: &credentials{user: "user", password: "password"},
		httpClient:  server.Client(),
		probes: func(*proxyConfigResult) map[string]protocolProbe {
			return map[string]protocolProbe{"socks": passing, "http": passing, "wireguard": passing}
		},
	})
	for _, name := range protocolNames {
		result := assertResult(t, results, name, "FAIL")
		if !strings.Contains(result.Detail, "cleanup failed") {
			t.Fatalf("%s detail did not report cleanup: %q", name, result.Detail)
		}
	}
}

func TestReadCredentialsRequiresPrivateRegularFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "credentials")
	if err := os.WriteFile(path, []byte("user\npassword\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.user != "user" || got.password != "password" {
		t.Fatalf("credentials = %#v", got)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentials(path); err == nil || !strings.Contains(err.Error(), "0600") {
		t.Fatalf("public credentials error = %v", err)
	}
	link := filepath.Join(directory, "credentials-link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readCredentials(link); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink credentials error = %v", err)
	}
}

func TestWriteResultsIsPrivateAtomicTSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "results.tsv")
	results := []Result{
		{Case: "socks", Status: "PASS", Detail: "one\tline\nonly"},
		{Case: "http", Status: "FAIL", Detail: "failed safely"},
		{Case: "wireguard", Status: "PASS", Detail: "complete"},
	}
	if err := WriteResults(path, results); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("result mode = %o, want 600", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "server/proxy\tsocks\tPASS\tone line only\n" +
		"server/proxy\thttp\tFAIL\tfailed safely\n" +
		"server/proxy\twireguard\tPASS\tcomplete\n"
	if string(data) != want {
		t.Fatalf("result TSV = %q, want %q", data, want)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".proxy-results-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary result files remained: %v", matches)
	}
}

func assertResult(t *testing.T, results []Result, name, status string) Result {
	t.Helper()
	for _, result := range results {
		if result.Case == name {
			if result.Status != status {
				t.Fatalf("%s status = %s, want %s (%s)", name, result.Status, status, result.Detail)
			}
			return result
		}
	}
	t.Fatalf("no result for %s: %#v", name, results)
	return Result{}
}
