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
// FS-122 here: the version's route probe reaching tlsedge and no listener, an lb restarted with an old map file loading
// the current version before it serves 443, and the dns API down at start leaving the split on no version until it
// answers. FS-122's other clauses are tlsedge_map_stale_t3_test.go's (two lb interfaces, an lbmap stopped, nginx -t
// refusing, map-stale, a false report, a dropped host) and tlsedge_custom_host_t3_test.go's (one window's changes in
// one reload), FS-42's custom-host step the latter's too, and 17.6's challenge-routing and eligibility rows and
// FS-122's TLS-ALPN-01 clause tlsedge_custom_route_t3_test.go's. The tests step on lbmap's own reports, never on a
// sleep; a real timer is only a guard. The dns API holds a long-poll 1 s, inside lbmap's 5 s poll timeout, so each
// poll ends with the hold's answer, not a timeout lbmap would back off from.

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
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"text/template"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/tlsedgeservice"

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

// The dns API's internal routes' check for the test's two bearers: lbmap's, and the dns edge stand-in's route probes.
type t3SplitAuth struct {
	bearer     string
	edgeBearer string
}

func (self *t3SplitAuth) Authenticate(request *http.Request) (dnsservice.DnsCallerClass, bool) {
	switch request.Header.Get("Authorization") {
	case "Bearer " + self.bearer:
		return dnsservice.DnsCallerLbmap, true
	case "Bearer " + self.edgeBearer:
		return dnsservice.DnsCallerEdge, true
	}
	return "", false
}

// 256 random bits in unpadded base64url, an internal caller's bearer.
func t3SplitBearer(t testing.TB) string {
	t.Helper()
	bearerBytes := make([]byte, 32)
	if _, err := rand.Read(bearerBytes); err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(bearerBytes)
}

// The dns API on Postgres for one or more lb interfaces, with each interface's applied reports.
type t3SplitApi struct {
	url        string
	bearer     string
	edgeBearer string
	// the block's own routes, behind none of the test's middleware
	handler http.Handler
	// by interface
	reports map[string]*t3SplitReports
	// while set, the API answers 503 to everything, as one that is down
	down atomic.Bool
	// the device's certificate for each custom host, one for every split of the API, and what trusts them
	custom     map[string]tls.Certificate
	customPool *x509.CertPool

	stateLock sync.Mutex
	// interfaces whose lbmap the API answers 503, as one its lbmap cannot reach, and each interface's polls answered so
	muted          map[string]bool
	refusedPolls   map[string]int
	refusedChanged chan struct{}
}

// The dns API over world's Postgres for the lb interfaces named, its hold on the wall clock, behind lbmap's bearer and
// the dns edge's.
func newT3SplitApi(t testing.TB, world *t3EdgeWorld, interfaces []string) *t3SplitApi {
	t.Helper()
	api := &t3SplitApi{
		bearer:     t3SplitBearer(t),
		edgeBearer: t3SplitBearer(t),
		reports:    map[string]*t3SplitReports{},
		custom:     map[string]tls.Certificate{},
		customPool: x509.NewCertPool(),
		muted:          map[string]bool{},
		refusedPolls:   map[string]int{},
		refusedChanged: make(chan struct{}),
	}
	inventory := []dnsservice.DnsLbInterface{}
	for _, name := range interfaces {
		api.reports[name] = &t3SplitReports{changed: make(chan struct{})}
		inventory = append(inventory, dnsservice.DnsLbInterface{Name: name, Ipv4: netip.MustParseAddr("127.0.0.1")})
	}
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
	// a long-poll held 1 s, inside lbmap's poll timeout below, so each poll ends with the hold's 204 rather than lbmap's
	// timeout, which lbmap counts as a failure and backs off from, to 30 s; lbmap then sees a new version within the
	// hold's second
	dnsSettings.Map.HoldSeconds = 1
	block, err := dnsservice.NewDnsApi(world.ctx, &apiHost, dnsSettings, sidecartest.NewFakeDnsStore(world.clock, nil), &dnsservice.DnsApiDeps{
		Auth:           &t3SplitAuth{bearer: api.bearer, edgeBearer: api.edgeBearer},
		Interfaces:     func() []dnsservice.DnsLbInterface { return inventory },
		CustomHostMaps: dnshost.NewCustomHostMaps(),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(block.Close)
	handler := block.Handler()
	api.handler = handler
	apiServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if api.down.Load() {
			http.Error(writer, "down", http.StatusServiceUnavailable)
			return
		}
		if request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/custom-host-map") && api.refusePoll(request.URL.Query().Get("interface")) {
			http.Error(writer, "unreachable", http.StatusServiceUnavailable)
			return
		}
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/custom-host-map/applied") {
			body, _ := io.ReadAll(request.Body)
			report := &dnsservice.DnsCustomHostMapReport{}
			if json.Unmarshal(body, report) == nil {
				if api.isMuted(report.Interface) {
					http.Error(writer, "unreachable", http.StatusServiceUnavailable)
					return
				}
				if reports, ok := api.reports[report.Interface]; ok {
					defer reports.add(report.Version, report.Master)
				}
			}
			request.Body = io.NopCloser(bytes.NewReader(body))
		}
		handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(apiServer.Close)
	api.url = apiServer.URL
	return api
}

// Whether the API answers name's lbmap 503.
func (self *t3SplitApi) isMuted(name string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.muted[name]
}

// Whether name's lbmap's poll is to be answered 503, counting it if so.
func (self *t3SplitApi) refusePoll(name string) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if !self.muted[name] {
		return false
	}
	self.refusedPolls[name] += 1
	close(self.refusedChanged)
	self.refusedChanged = make(chan struct{})
	return true
}

// Has the API answer name's lbmap 503 from now on, its polls and reports alike, as an API that lbmap cannot reach,
// while the interface's nginx serves on: lbmap keeps the map nginx runs and reports nothing. Returns once a poll begun
// after the mute was refused, so no poll the API held from before it can still hand lbmap a version.
func (self *t3SplitApi) mute(t testing.TB, name string) {
	t.Helper()
	refused := func() int {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.muted[name] = true
		return self.refusedPolls[name]
	}()
	guard := time.After(t3EdgeGuardTimeout)
	for {
		changed := func() chan struct{} {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if refused < self.refusedPolls[name] {
				return nil
			}
			return self.refusedChanged
		}()
		if changed == nil {
			return
		}
		select {
		case <-changed:
		case <-guard:
			t.Fatalf("%s's lbmap polled nothing after the mute", name)
		}
	}
}

// Lets name's lbmap reach the API again.
func (self *t3SplitApi) unmute(name string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.muted, name)
}

// Posts report to the block's applied route as an lbmap would, on lbmap's bearer and past the test's middleware, so a
// muted interface's lbmap's silence holds and the report is not counted as lbmap's own.
func (self *t3SplitApi) forgeReport(t testing.TB, report *dnsservice.DnsCustomHostMapReport) {
	t.Helper()
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, dnsservice.DnsCustomHostMapAppliedPath, bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+self.bearer)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	self.handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("the forged report answered %d: %s", recorder.Code, recorder.Body.String())
	}
}

// What lbmap reported, version by version, and the nginx master its last report named.
type t3SplitReports struct {
	stateLock sync.Mutex
	versions  []uint64
	master    *dnsservice.DnsMapMaster
	changed   chan struct{}
}

func (self *t3SplitReports) add(version uint64, master *dnsservice.DnsMapMaster) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.versions = append(self.versions, version)
	if master != nil {
		self.master = master
	}
	close(self.changed)
	self.changed = make(chan struct{})
}

// The nginx master lbmap's last report named, nil before one did.
func (self *t3SplitReports) lastMaster() *dnsservice.DnsMapMaster {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.master == nil {
		return nil
	}
	master := *self.master
	return &master
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
	// the dns API, shared with the world's other splits, and this interface's name in its inventory
	api           *t3SplitApi
	interfaceName string
	// the config the lb's nginx runs and lbmap's nginx -t reads, and the run directory lbmap writes the map into
	configPath string
	runDir     string
	// the lb binary and the environment it runs in
	lb  string
	env []string

	stateLock sync.Mutex
	// the running lb, nil while stopped
	command *exec.Cmd
	exited  chan struct{}
	output  *t3SplitOutput
}

// What the lb and its nginx wrote, kept for a failure's log.
type t3SplitOutput struct {
	stateLock sync.Mutex
	buffer    bytes.Buffer
}

func (self *t3SplitOutput) Write(b []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.buffer.Write(b)
}

func (self *t3SplitOutput) String() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.buffer.String()
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

// The split over world with a dns API of its own for its one interface.
func newT3Split(t testing.TB, world *t3EdgeWorld, customHosts []string) *t3Split {
	t.Helper()
	return newT3SplitOn(t, world, newT3SplitApi(t, world, []string{t3SplitInterface}), t3SplitInterface, customHosts)
}

// The split for interfaceName over world and api: lbmap's credential, the config and the lb process's environment,
// the lb not started.
func newT3SplitOn(t testing.TB, world *t3EdgeWorld, api *t3SplitApi, interfaceName string, customHosts []string) *t3Split {
	t.Helper()
	nginx := t3SplitNginx(t)
	lb := t3SplitBuildLb(t)
	split := &t3Split{
		world:         world,
		port:          t3SplitPort(t),
		http80Port:    t3SplitPort(t),
		reports:       api.reports[interfaceName],
		platform:      x509.NewCertPool(),
		custom:        api.custom,
		customPool:    api.customPool,
		api:           api,
		interfaceName: interfaceName,
	}
	for _, host := range customHosts {
		if _, ok := api.custom[host]; ok {
			continue
		}
		certificate, _, _ := t3SplitCertificate(t, []string{host}, world.start)
		api.custom[host] = certificate
		api.customPool.AddCert(certificate.Leaf)
	}
	world.dial.serve = split.serveDevice
	bearer := api.bearer

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
	split.configPath = filepath.Join(prefixDir, interfaceName+".conf")
	split.runDir = runDir
	if err := os.WriteFile(split.configPath, config.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	split.lb = lb
	split.env = append(os.Environ(),
		"PATH="+filepath.Dir(nginx)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"WARP_BLOCK="+interfaceName,
		"WARP_LB_CONFIG_DIR="+prefixDir,
		"WARP_LB_RUN_DIR="+runDir,
		"WARP_LBMAP_CREDENTIAL="+credentialPath,
		"WARP_LBMAP_API_URL="+api.url,
		"WARP_LBMAP_LOAD_DELAY=0s",
		"WARP_LBMAP_RELOAD_MIN_INTERVAL=0s",
		"WARP_LBMAP_START_TIMEOUT=2s",
		"WARP_LBMAP_POLL_TIMEOUT=5s",
		"WARP_LBMAP_POLL_MIN_INTERVAL=0s",
	)
	t.Cleanup(func() {
		split.stopLb()
	})
	return split
}

// Sets one of the lb's environment values for its next start.
func (self *t3Split) setEnv(key string, value string) {
	prefix := key + "="
	for index, entry := range self.env {
		if strings.HasPrefix(entry, prefix) {
			self.env[index] = prefix + value
			return
		}
	}
	self.env = append(self.env, prefix+value)
}

// The versions lbmap reported, in order.
func (self *t3SplitReports) reported() []uint64 {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return append([]uint64{}, self.versions...)
}

// Starts the lb process and returns once its nginx listens, after lbmap reported version when it is above 0: lbmap
// loads the newest version before nginx serves 443, so a report of it means the first connection routes by it.
func (self *t3Split) startLb(version uint64) {
	t := self.world.t
	t.Helper()
	command := exec.Command(self.lb)
	command.Env = self.env
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := &t3SplitOutput{}
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() {
		command.Wait()
		close(exited)
	}()
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.command, self.exited, self.output = command, exited, output
	}()
	if 0 < version {
		self.reports.await(t, version)
	}
	guard := time.Now().Add(t3EdgeGuardTimeout)
	for {
		if conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", self.port)); err == nil {
			conn.Close()
			return
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
}

// The nginx master's children: its workers.
func t3SplitWorkers(t testing.TB, masterPid int) []int {
	t.Helper()
	output, err := exec.Command("ps", "-Ao", "pid=,ppid=").Output()
	if err != nil {
		t.Fatal(err)
	}
	workers := []int{}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr == nil && ppidErr == nil && ppid == masterPid {
			workers = append(workers, pid)
		}
	}
	return workers
}

// Waits until the nginx master lbmap last reported runs one worker, none of old: a reload's handover done and the old
// workers gone, which until then still take new connections by the map they loaded.
func (self *t3Split) awaitSettled(old []int) {
	t := self.world.t
	t.Helper()
	master := self.reports.lastMaster()
	if master == nil {
		t.Fatal("lbmap reported no nginx master")
	}
	guard := time.Now().Add(t3EdgeGuardTimeout)
	for {
		workers := t3SplitWorkers(t, master.Pid)
		if len(workers) == 1 && !slices.Contains(old, workers[0]) {
			return
		}
		if time.Now().After(guard) {
			t.Fatalf("nginx still runs workers %v, of which %v are old", workers, old)
		}
		runtime.Gosched()
	}
}

// Waits for lbmap's report of version, then for nginx's handover to the worker that loaded it, so the next connection
// routes by that version.
func (self *t3Split) awaitLoaded(version uint64) {
	self.world.t.Helper()
	self.reports.await(self.world.t, version)
	self.awaitSettled(nil)
}

// Ends the lb and its nginx with their process group, at once: a drain is past what a test needs.
func (self *t3Split) stopLb() {
	command, exited, output := func() (*exec.Cmd, chan struct{}, *t3SplitOutput) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		command, exited, output := self.command, self.exited, self.output
		self.command, self.exited, self.output = nil, nil, nil
		return command, exited, output
	}()
	if command == nil {
		return
	}
	syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	<-exited
	if self.world.t.Failed() {
		self.world.t.Logf("the lb's output:\n%s", server.ScrubAddrsText(output.String()))
	}
}

// The newest custom-host map's version, 0 before any.
func (self *t3Split) mapVersion() uint64 {
	self.world.t.Helper()
	customHostMap, err := model.GetDnsCustomHostMap(self.world.ctx, self.world.root)
	if err != nil {
		self.world.t.Fatal(err)
	}
	if customHostMap == nil {
		return 0
	}
	return customHostMap.Version
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

// What the split answers bytes sent raw over TCP, read until it closes.
func (self *t3Split) raw(hello []byte) []byte {
	self.world.t.Helper()
	conn, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", self.port))
	if err != nil {
		self.world.t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(t3EdgeGuardTimeout))
	if _, err := conn.Write(hello); err != nil {
		self.world.t.Fatal(err)
	}
	answer, _ := io.ReadAll(conn)
	return answer
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
		split.startLb(version)

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
		// the version's route probe goes to tlsedge, which answers it unrecognized_name and reaches no listener (§13.4)
		customHostMap, err := model.GetDnsCustomHostMap(world.ctx, world.root)
		if err != nil {
			t.Fatal(err)
		}
		if answer := split.raw(t3EdgeHello(t, dnsservice.DnsCustomHostMapProbeName(customHostMap.Digest), []string{"ur-route-probe", "http/1.1"})); !bytes.Equal(answer, t3EdgeAlert(tlsedgeservice.TlsAlertUnrecognizedName)) {
			t.Fatalf("the route probe answered %x", answer)
		}
		if len(world.dial.Requests()) != dials {
			t.Fatal("the route probe reached Find")
		}

		// the removal withdraws the route at lbmap's next load
		removed := world.unmapCustomHost(host)
		world.reloadNames()
		split.awaitLoaded(removed)
		if !split.reachesPlatform(host) {
			t.Fatal("the removed host's hello did not reach the http branch")
		}
		if len(world.dial.Requests()) != dials {
			t.Fatal("the removed host reached Find")
		}
	})
}

// FS-122's restart clause: an lb restarted with an old map file loads the current version before its nginx serves 443,
// so a host added while it was down routes from the first connection on.
func TestT3Fs122AnLbRestartedOnAnOldMapLoadsTheCurrentVersionFirst(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newT3EdgeWorld(t, nil)
		suffix := make([]byte, 4)
		if _, err := rand.Read(suffix); err != nil {
			t.Fatal(err)
		}
		first := fmt.Sprintf("app.corp-%x.example", suffix)
		second := fmt.Sprintf("shop.corp-%x.example", suffix)
		version := world.mapCustomHost(first)
		world.reloadNames()
		split := newT3Split(t, world, []string{first, second})
		split.startLb(version)
		if !split.reachesDevice(first, []string{"h2"}, split.customPool) {
			t.Fatal("the first host did not route")
		}

		// the lb stops; a host is added; it starts again on the map file it left
		split.stopLb()
		version = world.mapCustomHost(second)
		world.reloadNames()
		split.startLb(version)
		if !split.reachesDevice(second, []string{"h2"}, split.customPool) {
			t.Fatal("a host added while the lb was down did not route from its restart's first connection")
		}
		if !split.reachesDevice(first, []string{"h2"}, split.customPool) {
			t.Fatal("the old map's host stopped routing")
		}
	})
}

// FS-122's outage clause: with the dns API down at start, nginx still starts on the map it holds, none at a first
// start, so the interface is current for no version and a custom host's hello reaches the http branch, platform names
// routing as ever; once the API answers, lbmap's next poll loads the version and the host routes.
func TestT3Fs122TheDnsApiDownAtStartLeavesTheSplitOnNoVersion(t *testing.T) {
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
		world.reloadNames()
		split := newT3Split(t, world, []string{host})
		split.api.down.Store(true)
		split.startLb(0)
		if !split.reachesPlatform(t3SplitPlatformHost + "." + world.root) {
			t.Fatal("a platform name did not route with the dns API down")
		}
		if !split.reachesPlatform(host) {
			t.Fatal("a custom host routed with no version loaded")
		}
		split.reports.stateLock.Lock()
		reported := len(split.reports.versions)
		split.reports.stateLock.Unlock()
		if reported != 0 {
			t.Fatalf("lbmap reported %d versions with the API down", reported)
		}
		split.api.down.Store(false)
		split.awaitLoaded(version)
		if !split.reachesDevice(host, []string{"h2"}, split.customPool) {
			t.Fatal("the host did not route once its version loaded")
		}
	})
}
