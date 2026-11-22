package connect

// Loading the lb_alt front from the environment, and connect's start with and
// without it.

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/nginx"
	"github.com/urnetwork/server/router"
)

// Sets the environment runWithDependencies reads, without a route file.
func setTestFrontRunEnv(t *testing.T) {
	t.Setenv("WARP_HOST_IPV4", "127.0.0.1")
	t.Setenv("WARP_HOST_IPV6", "")
	t.Setenv("WARP_PORTS", "8081:8081")
	t.Setenv("WARP_ENV", "test")
	t.Setenv("WARP_VERSION", "0.0.0-test")
	t.Setenv("WARP_SERVICE", "connect")
	t.Setenv("WARP_BLOCK", "g1")
	t.Setenv("WARP_HOST", "edge-a.example")
	t.Setenv(frontRouteFileEnvName, "")
	t.Setenv(frontGenerationEnvName, "")
}

// Without WARP_LBALT_ROUTE_FILE nothing changes: no front is built, and the
// private /status carries none of the front's headers.
func TestLbAltWithoutTheRouteFileChangesNothing(t *testing.T) {
	setTestFrontRunEnv(t)
	publicFront, err := newFrontServerFromEnv()
	if err != nil || publicFront != nil {
		t.Fatalf("front %v, err %v without a route file", publicFront, err)
	}

	var statusHeader http.Header
	err = runWithDependencies(
		t.Context(),
		RunOptions{Port: 8081, WarpStatus: &router.WarpStatusState{}, PrivateHeapProfileTarget: "disabled"},
		func(context.Context) error {
			// no database: the start serves /status only
			return errors.New("the test's readiness failure")
		},
		func(context.Context) func() {
			t.Error("a start that is not ready published a metrics cohort")
			return func() {}
		},
		func(_ context.Context, _ string, handler http.Handler, _ bool, _ server.HttpServerOptions) error {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/status", nil))
			statusHeader = recorder.Header()
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if statusHeader == nil {
		t.Fatal("the private listener never served")
	}
	for name := range statusHeader {
		if strings.HasPrefix(name, "X-Ur-Front-") {
			t.Fatalf("/status without a route file carries %s", name)
		}
	}
}

// A route file that can't be read fails connect's start before readiness or
// any listener: connect never comes up half-configured.
func TestLbAltRouteFileFailureFailsTheStart(t *testing.T) {
	setTestFrontRunEnv(t)
	t.Setenv(frontRouteFileEnvName, filepath.Join(t.TempDir(), "absent", "route.json"))
	err := runWithDependencies(
		t.Context(),
		RunOptions{Port: 8081, WarpStatus: &router.WarpStatusState{}, PrivateHeapProfileTarget: "disabled"},
		func(context.Context) error {
			t.Error("readiness ran with an unreadable route file")
			return nil
		},
		func(context.Context) func() {
			t.Error("a failed start published a metrics cohort")
			return func() {}
		},
		func(context.Context, string, http.Handler, bool, server.HttpServerOptions) error {
			t.Error("the private listener served with an unreadable route file")
			return nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "lb_alt") {
		t.Fatalf("start with an unreadable route file = %v", err)
	}
}

// The route file must be this generation's and this env's: another one
// means warpctl mounted another block's file, and the load fails before it
// reads a certificate.
func TestLbAltRouteFileOfAnotherBlockFailsTheLoad(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "nginx", "testdata", "route_v1_example.json"))
	if err != nil {
		t.Fatal(err)
	}
	routeFilePath := filepath.Join(t.TempDir(), "route.json")
	if err := os.WriteFile(routeFilePath, raw, 0o444); err != nil {
		t.Fatal(err)
	}
	vaultDir := t.TempDir()
	if _, err := loadFront(routeFilePath, "test-connect-g2-fedcba9876543210", "test", vaultDir); err == nil || !strings.Contains(err.Error(), "generation") {
		t.Fatalf("a route file of another generation loaded: %v", err)
	}
	if _, err := loadFront(routeFilePath, "", "other", vaultDir); err == nil || !strings.Contains(err.Error(), "env") {
		t.Fatalf("a route file of another env loaded: %v", err)
	}
	if _, err := loadFront(filepath.Join(t.TempDir(), "absent.json"), "", "test", vaultDir); err == nil {
		t.Fatal("an absent route file loaded")
	}
}

// connect's lb_alt start from the environment warpctl gives a block, to the
// real nginx front, with no Postgres or Redis: the route file loads from
// WARP_LBALT_ROUTE_FILE and matches WARP_GENERATION and WARP_ENV; the
// certificates load from the vault home, here in a host's flat layout, the
// one a block's container sees at its vault mount; the front starts on
// every public socket and answers the lb status route with WARP_VERSION;
// and the private /status reports the file's generation and revision and
// the earliest certificate expiry, ready with every socket up.
//
// warpctl gives a block's container neither WARP_HOME nor WARP_VAULT_HOME
// and mounts the host's vault at /srv/warp/vault, so the vault home is the
// one under the default WARP_HOME. Here WARP_VAULT_HOME is cleared, so the
// vault is found only that way, under a temporary WARP_HOME.
func TestLbAltStartServesTheRealFrontFromTheEnv(t *testing.T) {
	routeFile := newTestFrontRouteFile(t)
	routeTable, err := nginx.ParseRouteTable(routeFile)
	if err != nil {
		t.Fatal(err)
	}
	routeFilePath := filepath.Join(t.TempDir(), "route.json")
	if err := os.WriteFile(routeFilePath, routeFile, 0o444); err != nil {
		t.Fatal(err)
	}
	identity := newTestFrontIdentity(t, routeTable.ServedNames())
	// warpctl's vault mount (warpctl/run.go) is the vault home under the
	// default WARP_HOME
	if vaultMount := filepath.Join(server.DefaultWarpHome, "vault"); vaultMount != "/srv/warp/vault" {
		t.Fatalf("the default vault home %s is not warpctl's vault mount", vaultMount)
	}
	warpHome := t.TempDir()
	writeTestFrontVault(t, filepath.Join(warpHome, "vault"), routeTable, identity)
	t.Setenv(frontRouteFileEnvName, routeFilePath)
	t.Setenv(frontGenerationEnvName, routeTable.Generation)
	t.Setenv("WARP_ENV", routeTable.Env)
	t.Setenv("WARP_HOME", warpHome)
	t.Setenv("WARP_VAULT_HOME", "")
	t.Setenv("WARP_VERSION", "0.0.0-test")

	publicFront, err := newFrontServerFromEnv()
	if err != nil || publicFront == nil {
		t.Fatalf("the front from the env = %v, %v", publicFront, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := publicFront.startWithConnect(ctx, http.NotFoundHandler(), newTestFrontConnectQuic()); err != nil {
		t.Fatal(err)
	}
	defer publicFront.close()

	lbName := routeTable.Lb.Names[0]
	httpsAddress := netip.AddrPortFrom(netip.MustParseAddr(routeTable.Interfaces[0].Ipv4), uint16(routeTable.Listen.HttpsPort)).String()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs: identity.roots,
		},
		DialContext: func(ctx context.Context, network string, _ string) (net.Conn, error) {
			dialer := &net.Dialer{}
			return dialer.DialContext(ctx, "tcp", httpsAddress)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   testFrontProgressTimeout,
	}
	response, err := client.Get("https://" + lbName + routeTable.Lb.RoutePrefixes[0] + "/status")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if want := `{"version":"0.0.0-test","status":"ok","front":"lbalt"}` + "\n"; err != nil || response.StatusCode != http.StatusOK || string(body) != want {
		t.Fatalf("the lb status = %d %q, %v; want the real front's %q", response.StatusCode, body, err, want)
	}

	leaf, err := x509.ParseCertificate(identity.certificate.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(routeFile)
	header := http.Header{}
	publicFront.writeStatusHeaders(header, frontReadyLatchStatus, true)
	for name, want := range map[string]string{
		frontGenerationHeader:    routeTable.Generation,
		frontRouteRevisionHeader: hex.EncodeToString(digest[:]),
		frontReadyHeader:         "1",
		frontCertExpiryHeader:    leaf.NotAfter.UTC().Format(time.RFC3339),
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
}

// The vault's lbalt_session_tickets.yml turns the shared session ticket keys
// on only with enabled and usable secrets, which it gives in its order;
// without the file, or with enabled false, the front keeps Go's per-process
// keys; and a file that sets enabled with secrets the front can't use, or
// that names another field, fails the start without quoting a secret
// (LBALTOPT2 R22 (a)).
func TestLoadFrontSessionTicketSecrets(t *testing.T) {
	secret := func(fill byte, length int) (string, []byte) {
		secretBytes := []byte(strings.Repeat(string([]byte{fill}), length))
		return base64.StdEncoding.EncodeToString(secretBytes), secretBytes
	}
	firstEncoded, first := secret('a', 32)
	secondEncoded, second := secret('b', 48)
	shortEncoded, _ := secret('c', 31)

	secrets, err := loadFrontSessionTicketSecrets()
	if err != nil || secrets != nil {
		t.Fatalf("without the file: %d secrets, %v; want none", len(secrets), err)
	}

	for _, c := range []struct {
		what    string
		content string
		want    [][]byte
		refused bool
	}{
		{what: "enabled with two secrets", content: "enabled: true\nsecrets:\n  - " + firstEncoded + "\n  - " + secondEncoded + "\n", want: [][]byte{first, second}},
		{what: "enabled false", content: "enabled: false\nsecrets:\n  - " + firstEncoded + "\n"},
		{what: "secrets without enabled", content: "secrets:\n  - " + firstEncoded + "\n"},
		{what: "empty", content: ""},
		{what: "enabled without secrets", content: "enabled: true\n", refused: true},
		{what: "a secret shorter than 32 bytes", content: "enabled: true\nsecrets:\n  - " + firstEncoded + "\n  - " + shortEncoded + "\n", refused: true},
		{what: "a secret that isn't base64", content: "enabled: true\nsecrets:\n  - " + firstEncoded + "!\n", refused: true},
		{what: "more secrets than the front takes", content: "enabled: true\nsecrets: [" + strings.Repeat(firstEncoded+", ", 4) + firstEncoded + "]\n", refused: true},
		{what: "another field", content: "enabled: true\nsecret: " + firstEncoded + "\n", refused: true},
		{what: "a second document", content: "enabled: false\n---\nenabled: true\n", refused: true},
	} {
		pop := server.Vault.PushSimpleResource(frontSessionTicketsResource, []byte(c.content))
		secrets, err := loadFrontSessionTicketSecrets()
		pop()
		if refused := err != nil; refused != c.refused {
			t.Errorf("%s: refused %t (%v), want %t", c.what, refused, err, c.refused)
			continue
		}
		if err != nil {
			for _, encoded := range []string{firstEncoded, secondEncoded, shortEncoded} {
				if strings.Contains(err.Error(), encoded) || strings.Contains(err.Error(), encoded[:16]) {
					t.Errorf("%s: the error quotes a secret: %v", c.what, err)
				}
			}
			continue
		}
		if len(secrets) != len(c.want) {
			t.Errorf("%s: %d secrets, want %d", c.what, len(secrets), len(c.want))
			continue
		}
		for i := range secrets {
			if string(secrets[i]) != string(c.want[i]) {
				t.Errorf("%s: secret %d differs", c.what, i)
			}
		}
	}
}
