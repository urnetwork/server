//go:build sidecar_pending

package main

// FS-35 on T3 through the real splitter (TLS-M6-07; FS-35, FI-11, D-TLS-12, §13.4): tlsedge_t3_test.go's block on the
// private stack's Postgres and Redis, behind real nginx running the split (NGINX_SPLIT_BINARY, or an nginx on PATH with
// the stream ssl_preread module), started by warp's real lb process with lbmap, built from the sibling warp checkout and
// run as a process, since server does not import warp. lbmap polls the real dns API, dnsservice.DnsApi with
// dnshost.CustomHostMaps on Postgres, served on loopback with its lbmap bearer, and the lb takes its config and run
// directories and lbmap's §13.4 windows, short here, from its environment. Behind tlsedge the device ends TLS on its own
// key, with a certificate for the custom host beside its network's.
//
// FS-35: a registered custom host's ClientHello through the splitter reaches tlsedge, Find carrying the host as its
// server name, and completes TLS on the device's key, and its acme-tls/1 hello reaches the device the same way; beside
// it platform, unknown and absent SNI go to the http branch and user SNI to tlsedge; removing the host withdraws its
// route at lbmap's next load, its hello reaching the http branch again.
//
// What waits (TLS-M6-07's rest): FS-122's clauses, an lbmap stopped and nginx -t refusing past 15 and 20 min on a fake
// clock, map-stale, a restart with an old map, the dns API down at start, three adds and a remove in one window, a
// restore, a false report, a dropped host, route probes; FS-41's and FS-42's custom-host steps; 17.6's challenge
// routing and eligibility rows on T3; two lb interfaces. The test steps on lbmap's own reports, never on a sleep; a real
// timer is only a guard.

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/dnshost"
	"github.com/urnetwork/server/model"
)

// The lb interface the test runs: WARP_BLOCK with no domain, which lbmap names itself by.
const t3SplitInterface = "edge-1-eth0"

// The platform name left under the root, which keeps the http server.
const t3SplitPlatformHost = "api"

// What the http branch answers every request, so a visitor can tell which branch it reached.
const t3SplitPlatformPage = "the platform's own page"

// The split's upstream for the edge, as warpctl's render names tlsedge's 443.
const t3SplitEdgeUpstream = "stream-service-block-tlsedge-443"

// An nginx that runs the split: NGINX_SPLIT_BINARY, which must run it, else nginx on PATH, else the test skips, as
// warp's split tests do.
func t3SplitNginx(t testing.TB) string {
	t.Helper()
	binary := os.Getenv("NGINX_SPLIT_BINARY")
	set := binary != ""
	if !set {
		found, err := exec.LookPath("nginx")
		if err != nil {
			t.Skip("no NGINX_SPLIT_BINARY and no nginx on PATH")
		}
		binary = found
	}
	version, err := exec.Command(binary, "-V").CombinedOutput()
	if err == nil && bytes.Contains(version, []byte("--with-stream_ssl_preread_module")) && bytes.Contains(version, []byte("--with-http_ssl_module")) {
		return binary
	}
	if set {
		t.Fatalf("NGINX_SPLIT_BINARY=%s cannot run the split: %v", binary, err)
	}
	t.Skip("the nginx on PATH has no stream ssl_preread module")
	return ""
}

// warp's lb, built from the sibling checkout with the go on PATH, the run's own toolchain where lowgo runs the test.
func t3SplitBuildLb(t testing.TB) string {
	t.Helper()
	goBinary, err := exec.LookPath("go")
	if err != nil {
		t.Fatalf("no go to build warp's lb with: %v", err)
	}
	output := filepath.Join(t.TempDir(), "lb")
	command := exec.Command(goBinary, "build", "-o", output, ".")
	command.Dir = filepath.Join("..", "..", "..", "warp", "lb")
	if built, err := command.CombinedOutput(); err != nil {
		t.Fatalf("building warp's lb: %v\n%s", err, built)
	}
	return output
}

// A self-signed certificate for names on a P-256 key, and its PEM files' contents.
func t3SplitCertificate(t testing.TB, names []string, now time.Time) (tls.Certificate, []byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDer, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePem := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPem := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer})
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, certificatePem, keyPem
}

// The split as warpctl renders it, reduced to what FS-35 reads: the stream server's map with the platform name, the
// root's wildcard, lbmap's include and the http branch as its default, PROXY v1 to both branches; the http branch on
// the unix socket behind realip; port 80's redirect server holding lbmap's other include; and lbmap's marker.
var t3SplitConfigTemplate = template.Must(template.New("nginx.conf").Parse(`# warp-lbmap: {{.marker}}
worker_processes 1;
pid {{.prefix}}/nginx.pid;
error_log {{.prefix}}/error.log notice;
events {
    worker_connections 256;
}
stream {
    map $ssl_preread_server_name $tls_split_upstream {
        hostnames;
        {{.root}} lb-https;
        {{.platform}} lb-https;
        *.{{.root}} {{.edgeUpstream}};
        include {{.customHosts}};
        default lb-https;
    }
    upstream lb-https {
        server unix:{{.socket}};
    }
    upstream {{.edgeUpstream}} {
        server {{.edge}};
    }
    server {
        listen 127.0.0.1:{{.port}};
        ssl_preread on;
        preread_timeout 10s;
        proxy_protocol on;
        proxy_pass $tls_split_upstream;
    }
}
http {
    client_body_temp_path {{.prefix}}/client_body;
    proxy_temp_path {{.prefix}}/proxy;
    fastcgi_temp_path {{.prefix}}/fastcgi;
    uwsgi_temp_path {{.prefix}}/uwsgi;
    scgi_temp_path {{.prefix}}/scgi;
    access_log off;
    server_names_hash_bucket_size 512;
    server {
        listen unix:{{.socket}} ssl proxy_protocol;
        set_real_ip_from unix:;
        real_ip_header proxy_protocol;
        server_name _;
        ssl_certificate {{.prefix}}/platform.pem;
        ssl_certificate_key {{.prefix}}/platform.key;
        location / {
            return 200 "{{.page}}";
        }
    }
    server {
        listen 127.0.0.1:{{.http80Port}};
        server_name *.{{.root}};
        include {{.customHosts80}};
        return 301 https://$host$request_uri;
    }
}
`))

// The dns API's internal routes' check for the test's lbmap bearer alone.
type t3SplitAuth struct {
	bearer string
}

func (self *t3SplitAuth) Authenticate(request *http.Request) (dnsservice.DnsCallerClass, bool) {
	if request.Header.Get("Authorization") == "Bearer "+self.bearer {
		return dnsservice.DnsCallerLbmap, true
	}
	return "", false
}

// What lbmap reported, version by version.
type t3SplitReports struct {
	stateLock sync.Mutex
	versions  []uint64
	changed   chan struct{}
}

func (self *t3SplitReports) add(version uint64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.versions = append(self.versions, version)
	close(self.changed)
	self.changed = make(chan struct{})
}

// Waits for a report of version or a later one.
func (self *t3SplitReports) await(t testing.TB, version uint64) {
	t.Helper()
	guard := time.After(t3EdgeGuardTimeout)
	for {
		changed := func() chan struct{} {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			for _, reported := range self.versions {
				if version <= reported {
					return nil
				}
			}
			return self.changed
		}()
		if changed == nil {
			return
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("lbmap reported no version %d", version)
		}
	}
}

// One split in front of the T3 world's block.
type t3Split struct {
	world      *t3EdgeWorld
	port       int
	http80Port int
	reports    *t3SplitReports
	platform   *x509.CertPool
	custom     map[string]tls.Certificate
	customPool *x509.CertPool
}

// A free loopback port.
func t3SplitPort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// Maps host to owner web of the world's network as an active weak-mode row would, raising the custom-host map's version
// in the same transaction as §12.8's add does, and returns the version.
func (self *t3EdgeWorld) mapCustomHost(host string) uint64 {
	self.t.Helper()
	root := self.root + "."
	var version uint64
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			self.ctx,
			`
				INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, effective_caa_chain_ok, create_time)
				VALUES ($1, $2, $3, $4, $5, $6, false, $7)
			`,
			host, root, server.Id(self.networkId), "web."+self.label+"."+root, sidecar.DnsCustomHostStateActive, sidecar.DnsCustomHostModeWeak, server.NowUtc(),
		))
		var err error
		version, err = model.DnsRaiseCustomHostMapInTx(self.ctx, tx, root)
		server.Raise(err)
	}, server.TxReadCommitted)
	return version
}

// Removes host's row and raises the map, as §12.8's remove does, and returns the version.
func (self *t3EdgeWorld) unmapCustomHost(host string) uint64 {
	self.t.Helper()
	root := self.root + "."
	var version uint64
	server.Tx(self.ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(self.ctx, `DELETE FROM network_dns_custom_host WHERE host = $1`, host))
		var err error
		version, err = model.DnsRaiseCustomHostMapInTx(self.ctx, tx, root)
		server.Raise(err)
	}, server.TxReadCommitted)
	return version
}

// Reads the block's names again on its clock and waits for the pass, so its custom-host map is the rows'.
func (self *t3EdgeWorld) reloadNames() {
	self.t.Helper()
	count, _ := self.host.engine.NameLoads()
	self.clock.Advance(self.settings.TlsEdge.NameReloadInterval)
	hostTestAwait(self.t, "the block's names", count+1, self.host.engine.NameLoads)
}

// The split over world: the dns API on Postgres, lbmap's credential, the config and the lb process, returned once
// lbmap has reported the map's version at start.
func newT3Split(t testing.TB, world *t3EdgeWorld, customHosts []string) *t3Split {
	t.Helper()
	nginx := t3SplitNginx(t)
	lb := t3SplitBuildLb(t)
	split := &t3Split{
		world:      world,
		port:       t3SplitPort(t),
		http80Port: t3SplitPort(t),
		reports:    &t3SplitReports{changed: make(chan struct{})},
		platform:   x509.NewCertPool(),
		custom:     map[string]tls.Certificate{},
		customPool: x509.NewCertPool(),
	}
	for _, host := range customHosts {
		certificate, _, _ := t3SplitCertificate(t, []string{host}, world.start)
		split.custom[host] = certificate
		split.customPool.AddCert(certificate.Leaf)
	}
	world.dial.serve = split.serveDevice

	// the dns API on Postgres, its hold on the wall clock, behind the lbmap bearer
	bearerBytes := make([]byte, 32)
	if _, err := rand.Read(bearerBytes); err != nil {
		t.Fatal(err)
	}
	bearer := base64.RawURLEncoding.EncodeToString(bearerBytes)
	apiHost := *world.fakes.ServiceHost()
	apiHost.Clock = sidecar.RealClock{}
	apiGates := sidecartest.NewFakeServiceGates(nil)
	if err := apiGates.Set("sidecar:\n  dns: {enabled: true, percent: 100}\n"); err != nil {
		t.Fatal(err)
	}
	apiHost.Gates = apiGates
	dnsSettings := dnsservice.DefaultDnsSettings()
	dnsSettings.Roots = []string{world.root + "."}
	dnsSettings.SignaturesPerSecond = 1000
	dnsSettings.Tcp.ConnectionsPerSource = 2
	dnsSettings.Rrl.ResponsesPerSecond = 2
	api, err := dnsservice.NewDnsApi(world.ctx, &apiHost, dnsSettings, sidecartest.NewFakeDnsStore(world.clock, nil), &dnsservice.DnsApiDeps{
		Auth: &t3SplitAuth{bearer: bearer},
		Interfaces: func() []dnsservice.DnsLbInterface {
			return []dnsservice.DnsLbInterface{{Name: t3SplitInterface, Ipv4: netip.MustParseAddr("127.0.0.1")}}
		},
		CustomHostMaps: dnshost.NewCustomHostMaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(api.Close)
	handler := api.Handler()
	apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/custom-host-map/applied") {
			body, _ := io.ReadAll(request.Body)
			report := &struct {
				Version uint64 `json:"version"`
			}{}
			if json.Unmarshal(body, report) == nil {
				defer split.reports.add(report.Version)
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(apiServer.Close)

	// the lb's directories: short, since a unix socket's path has a small bound
	runDir, err := os.MkdirTemp("", "tlslb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })
	prefixDir, err := os.MkdirTemp("", "tlslbp")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(prefixDir) })
	_, platformPem, platformKeyPem := t3SplitCertificate(t, []string{t3SplitPlatformHost + "." + world.root, world.root, "*.platform.example"}, world.start)
	platformLeaf, _ := pem.Decode(platformPem)
	platformCertificate, err := x509.ParseCertificate(platformLeaf.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	split.platform.AddCert(platformCertificate)
	credentialPath := filepath.Join(prefixDir, "internal-lbmap.yml")
	for path, data := range map[string][]byte{
		filepath.Join(prefixDir, "platform.pem"): platformPem,
		filepath.Join(prefixDir, "platform.key"): platformKeyPem,
		credentialPath:                           []byte("current: " + bearer + "\n"),
	} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	marker, err := json.Marshal(map[string]any{
		"format":           1,
		"roots":            []string{world.root},
		"platform_domains": []string{"platform.example", t3SplitPlatformHost + "." + world.root},
		"edge_upstream":    t3SplitEdgeUpstream,
	})
	if err != nil {
		t.Fatal(err)
	}
	var config bytes.Buffer
	if err := t3SplitConfigTemplate.Execute(&config, map[string]any{
		"marker":        string(marker),
		"prefix":        prefixDir,
		"root":          world.root,
		"platform":      t3SplitPlatformHost + "." + world.root,
		"edgeUpstream":  t3SplitEdgeUpstream,
		"customHosts":   filepath.Join(runDir, "tls-custom-hosts.map"),
		"customHosts80": filepath.Join(runDir, "tls-custom-hosts-80.conf"),
		"socket":        filepath.Join(runDir, "https.sock"),
		"edge":          world.address,
		"port":          split.port,
		"http80Port":    split.http80Port,
		"page":          t3SplitPlatformPage,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefixDir, t3SplitInterface+".conf"), config.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	command := exec.Command(lb)
	command.Env = append(os.Environ(),
		"PATH="+filepath.Dir(nginx)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WARP_BLOCK="+t3SplitInterface,
		"WARP_LB_CONFIG_DIR="+prefixDir,
		"WARP_LB_RUN_DIR="+runDir,
		"WARP_LBMAP_CREDENTIAL="+credentialPath,
		"WARP_LBMAP_API_URL="+apiServer.URL,
		"WARP_LBMAP_LOAD_DELAY=0s",
		"WARP_LBMAP_RELOAD_MIN_INTERVAL=0s",
		"WARP_LBMAP_START_TIMEOUT=10s",
		"WARP_LBMAP_POLL_TIMEOUT=1s",
		"WARP_LBMAP_POLL_MIN_INTERVAL=0s",
	)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		command.Wait()
		close(exited)
	}()
	t.Cleanup(func() {
		// the lb and its nginx, a drain past the test's need, end with their group
		syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		<-exited
		if t.Failed() {
			t.Logf("the lb's output:\n%s", server.ScrubAddrsText(output.String()))
		}
	})
	version, err := model.GetDnsCustomHostMap(world.ctx, world.root)
	if err != nil {
		t.Fatal(err)
	}
	if version != nil {
		split.reports.await(t, version.Version)
	}
	// nginx serves once its listener answers; lbmap's report came after its load, before nginx started
	guard := time.Now().Add(t3EdgeGuardTimeout)
	for {
		if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", split.port)); err == nil {
			conn.Close()
			break
		}
		select {
		case <-exited:
			t.Fatalf("the lb exited:\n%s", server.ScrubAddrsText(output.String()))
		default:
		}
		if time.Now().After(guard) {
			t.Fatal("the split never listened")
		}
		runtime.Gosched()
	}
	return split
}

// The device's end: TLS on the custom host's own certificate for its name and the network's otherwise, acme-tls/1
// negotiated where offered, then the greeting and an echo.
func (self *t3Split) serveDevice(conn *t3EdgeDeviceConn, opened *t3EdgeOpened) {
	defer conn.Close()
	server := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS13,
		NextProtos: []string{"acme-tls/1", "h2", "http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if certificate, ok := self.custom[hello.ServerName]; ok {
				return &certificate, nil
			}
			return &self.world.device.certificate, nil
		},
	})
	if err := server.Handshake(); err != nil {
		return
	}
	if _, err := server.Write([]byte(t3EdgeDeviceGreeting)); err != nil {
		return
	}
	io.Copy(server, server)
	server.CloseWrite()
}

// A visitor's TLS connection to the split for serverName, offering alpnIds, trusting roots.
func (self *t3Split) dial(serverName string, alpnIds []string, roots *x509.CertPool) (*tls.Conn, error) {
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", self.port))
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(t3EdgeGuardTimeout))
	client := tls.Client(conn, &tls.Config{ServerName: serverName, RootCAs: roots, NextProtos: alpnIds, MinVersion: tls.VersionTLS12})
	if err := client.Handshake(); err != nil {
		conn.Close()
		return nil, err
	}
	return client, nil
}

// Whether a visit for serverName reaches the device through tlsedge: its greeting read after a handshake on roots.
func (self *t3Split) reachesDevice(serverName string, alpnIds []string, roots *x509.CertPool) bool {
	self.world.t.Helper()
	client, err := self.dial(serverName, alpnIds, roots)
	if err != nil {
		return false
	}
	defer client.Close()
	greeting := make([]byte, len(t3EdgeDeviceGreeting))
	_, err = io.ReadFull(client, greeting)
	return err == nil && string(greeting) == t3EdgeDeviceGreeting
}

// Whether a visit for serverName reaches the http branch: a handshake on a certificate that chains to the platform's,
// with no name asked of it, since a hello without SNI names none, and the platform's page.
func (self *t3Split) reachesPlatform(serverName string) bool {
	self.world.t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", self.port))
	if err != nil {
		return false
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(t3EdgeGuardTimeout))
	client := tls.Client(conn, &tls.Config{
		ServerName:         serverName,
		NextProtos:         []string{"http/1.1"},
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) == 0 {
				return fmt.Errorf("no certificate")
			}
			_, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{Roots: self.platform})
			return err
		},
	})
	if err := client.Handshake(); err != nil {
		return false
	}
	if _, err := client.Write([]byte("GET / HTTP/1.1\r\nHost: platform\r\nConnection: close\r\n\r\n")); err != nil {
		return false
	}
	response, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return string(body) == t3SplitPlatformPage
}

// FS-35 through the real splitter, lbmap and tlsedge: a registered custom host's hello reaches the device and completes
// TLS on its key, its acme-tls/1 hello too, with Find carrying the host; platform, unknown and absent SNI stay with the
// http branch and user SNI goes to tlsedge; removing the host withdraws its route at lbmap's next load.
func TestT3Fs35CustomHostThroughTheRealSplitter(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			t.Fatal(err)
		}
		host := fmt.Sprintf("app.corp-%x.example", suffix)
		version := world.mapCustomHost(host)
		if version == 0 {
			t.Fatal("the add raised no map version")
		}
		world.reloadNames()
		split := newT3Split(t, world, []string{host})

		// the custom host, to the device on its own certificate, with Find's server name
		if !split.reachesDevice(host, []string{"h2", "http/1.1"}, split.customPool) {
			t.Fatal("the custom host's visitor did not reach the device")
		}
		requests := world.dial.Requests()
		if len(requests) == 0 || requests[len(requests)-1].Name != "tls/443/web" || requests[len(requests)-1].ServerName != host {
			t.Fatalf("the custom host's dial: %+v", requests)
		}
		// its TLS-ALPN-01 hello reaches the device the same way, the id beside the name
		client, err := split.dial(host, []string{"acme-tls/1"}, split.customPool)
		if err != nil {
			t.Fatalf("the custom host's acme-tls/1 hello: %v", err)
		}
		if protocol := client.ConnectionState().NegotiatedProtocol; protocol != "acme-tls/1" {
			t.Fatalf("the acme-tls/1 hello negotiated %q", protocol)
		}
		client.Close()
		requests = world.dial.Requests()
		if last := requests[len(requests)-1]; last.ServerName != host || len(last.Alpns) != 1 || last.Alpns[0] != "acme-tls/1" {
			t.Fatalf("the acme-tls/1 dial: %+v", last)
		}
		// user SNI to tlsedge, with no server name
		if !split.reachesDevice(world.serverName("blog"), []string{"h2"}, world.device.roots) {
			t.Fatal("a user name's visitor did not reach the device")
		}
		if last := world.dial.Requests()[len(world.dial.Requests())-1]; last.Name != "tls/443/blog" || last.ServerName != "" {
			t.Fatalf("the user name's dial: %+v", last)
		}
		// platform, unknown and absent SNI to the http branch, reaching no Find
		dials := len(world.dial.Requests())
		for _, serverName := range []string{t3SplitPlatformHost + "." + world.root, "unknown.platform.example", ""} {
			if !split.reachesPlatform(serverName) {
				t.Errorf("%q did not reach the http branch", serverName)
			}
		}
		if len(world.dial.Requests()) != dials {
			t.Fatal("a platform, unknown or absent SNI reached Find")
		}

		// the removal withdraws the route at lbmap's next load
		removed := world.unmapCustomHost(host)
		world.reloadNames()
		split.reports.await(t, removed)
		if !split.reachesPlatform(host) {
			t.Fatal("the removed host's hello did not reach the http branch")
		}
		if len(world.dial.Requests()) != dials {
			t.Fatal("the removed host reached Find")
		}
	})
}
