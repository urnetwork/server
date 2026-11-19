package nginx_test

// The parity harness inside its container (parity_test.go): it sets up the
// fixture's addresses, certificates and upstreams, starts nginx through the
// image's lb binary and the front through nginx.NewFront, runs the corpus
// against both, and reports every difference with its classification.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"maps"
	"math/big"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/nginx"
)

// nginx's internal ports, the WARP_PORTS the harness gives the lb binary,
// as warpctl gives an lb block its own: the front listens on the route
// file's, so the two never share a socket.
const (
	parityNginxHttpPort   = 8080
	parityNginxHttpsPort  = 8443
	parityNginxWhodisPort = 8453
)

// The version both report on the lb status route: nginx's static
// status.json and the front's own json.
const parityVersion = "0.0.0-parity"

// The bits of a generated certificate's random serial number, as RFC 5280
// allows at most 20 bytes.
const parityCertificateSerialBits = 128

// The image's paths the harness writes or runs.
const (
	parityLbBinary      = "/usr/local/sbin/lb"
	parityVaultDir      = "/srv/warp/vault"
	parityStatusJsonDir = "/srv/warp/status"
)

// Runs the corpus against nginx and the front, inside the parity container,
// and skips with the reason anywhere else.
func TestNginxParityContainer(t *testing.T) {
	if os.Getenv(parityContainerEnvName) != "1" {
		t.Skipf("runs only inside the parity harness's container; see TestNginxParity (%s=1)", parityHostEnvName)
	}
	if runtime.GOOS != "linux" {
		t.Skipf("the parity container is linux, not %s", runtime.GOOS)
	}
	harness := newParityHarness(t, defaultParityHarnessSettings())
	defer harness.close()
	harness.runCorpus(parityCorpus())
}

// Everything one run of the harness holds.
type parityHarness struct {
	t        *testing.T
	settings *parityHarnessSettings
	recorder *parityRecorder
	nginx    *parityTarget
	front    *parityTarget
	// the fixture's route file, which the front serves
	routeTable *nginx.RouteTable

	ctx    context.Context
	cancel context.CancelFunc
	// the upstreams' and the front's servers, closed last to first
	httpServers []*http.Server
	frontObject *nginx.Front
	nginxCmd    *exec.Cmd
	nginxExited chan struct{}
	nginxOutput *parityBoundedBuffer
}

// Sets up the container and starts every server, failing the test on any
// error.
func newParityHarness(t *testing.T, settings *parityHarnessSettings) *parityHarness {
	ctx, cancel := context.WithCancel(context.Background())
	harness := &parityHarness{
		t:           t,
		settings:    settings,
		recorder:    newParityRecorder(settings),
		ctx:         ctx,
		cancel:      cancel,
		nginxOutput: newParityBoundedBuffer(settings.NginxOutputBytes),
	}
	success := false
	defer func() {
		if !success {
			harness.close()
		}
	}()

	routeTable, revision, err := nginx.LoadRouteTable(filepath.Join(parityContainerFixtureDir, parityRouteFileName))
	if err != nil {
		t.Fatalf("the fixture's route file: %v", err)
	}
	harness.routeTable = routeTable
	routeInterface := routeTable.Interfaces[0]
	interfaceIpv4 := netip.MustParseAddr(routeInterface.Ipv4)
	interfaceIpv6 := netip.MustParseAddr(routeInterface.Ipv6)
	upstreamIpv4 := netip.MustParseAddr(routeTable.Upstream.Ipv4)
	harness.nginx = &parityTarget{
		name:      "nginx",
		ipv4:      interfaceIpv4,
		ipv6:      interfaceIpv6,
		httpPort:  parityNginxHttpPort,
		httpsPort: parityNginxHttpsPort,
	}
	harness.front = &parityTarget{
		name:      "front",
		ipv4:      interfaceIpv4,
		ipv6:      interfaceIpv6,
		httpPort:  routeTable.Listen.HttpPort,
		httpsPort: routeTable.Listen.HttpsPort,
	}

	// the interface's and the upstream's addresses, and every client address
	// the corpus uses
	addrs := []netip.Addr{interfaceIpv4, interfaceIpv6, upstreamIpv4, parityClientIpv4, parityClientIpv6, parityLimitClientIpv6}
	addrs = append(addrs, parityRingClients()...)
	for i := range parityLimitClientCount {
		addrs = append(addrs, parityLimitClient(i))
	}
	for _, addr := range addrs {
		if err := addParityLoopbackAddr(addr); err != nil {
			t.Fatal(err)
		}
	}

	nginxConfig, err := os.ReadFile(filepath.Join(parityContainerFixtureDir, parityNginxConfigName))
	if err != nil {
		t.Fatal(err)
	}
	// a certificate and key at every path the nginx config names, for the
	// name its directory is for: the name itself, or `*.<base>` for a
	// `star.<base>` directory. Each is self-signed with that name as its
	// common name, so a client can tell which one a server presented.
	writeCertificates := func() error {
		certificateRegexp := regexp.MustCompile(`(?m)^\s*ssl_certificate\s+(\S+);`)
		keyRegexp := regexp.MustCompile(`(?m)^\s*ssl_certificate_key\s+(\S+);`)
		dirKeyPaths := map[string]string{}
		for _, match := range keyRegexp.FindAllStringSubmatch(string(nginxConfig), -1) {
			dirKeyPaths[filepath.Dir(match[1])] = match[1]
		}
		writtenDirs := map[string]bool{}
		for _, match := range certificateRegexp.FindAllStringSubmatch(string(nginxConfig), -1) {
			certificatePath := match[1]
			dir := filepath.Dir(certificatePath)
			if writtenDirs[dir] {
				continue
			}
			keyPath, ok := dirKeyPaths[dir]
			if !ok {
				return fmt.Errorf("the nginx config names no key beside %s", certificatePath)
			}
			name := filepath.Base(dir)
			dnsName := name
			if base, ok := strings.CutPrefix(name, "star."); ok {
				dnsName = "*." + base
			}
			key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				return err
			}
			serialNumber, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), parityCertificateSerialBits))
			if err != nil {
				return err
			}
			now := time.Now()
			template := &x509.Certificate{
				SerialNumber: serialNumber,
				Subject:      pkix.Name{CommonName: name},
				DNSNames:     []string{dnsName},
				NotBefore:    now.Add(-settings.CertificateBackdate),
				NotAfter:     now.Add(settings.CertificateLifetime),
				KeyUsage:     x509.KeyUsageDigitalSignature,
				ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			}
			certificateDer, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
			if err != nil {
				return err
			}
			keyDer, err := x509.MarshalECPrivateKey(key)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(dir, 0700); err != nil {
				return err
			}
			if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDer}), 0644); err != nil {
				return err
			}
			if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDer}), 0600); err != nil {
				return err
			}
			writtenDirs[dir] = true
		}
		if len(writtenDirs) == 0 {
			return fmt.Errorf("the nginx config names no certificate")
		}
		return nil
	}
	if err := writeCertificates(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(parityStatusJsonDir, 0755); err != nil {
		t.Fatal(err)
	}
	statusJson := fmt.Sprintf("{\"version\":%q,\"status\":\"ok\"}\n", parityVersion)
	if err := os.WriteFile(filepath.Join(parityStatusJsonDir, "status.json"), []byte(statusJson), 0644); err != nil {
		t.Fatal(err)
	}

	// the upstreams, then nginx and the front, which dial them
	discardLog := log.New(io.Discard, "", 0)
	for _, service := range routeTable.Services {
		for _, block := range service.Blocks {
			listener, err := net.Listen("tcp", netip.AddrPortFrom(upstreamIpv4, uint16(block.Port)).String())
			if err != nil {
				t.Fatalf("upstream %s %s: %v", service.Name, block.Name, err)
			}
			httpServer := &http.Server{
				Handler:  harness.recorder.handler(service.Name, block.Name),
				ErrorLog: discardLog,
			}
			harness.httpServers = append(harness.httpServers, httpServer)
			go httpServer.Serve(listener)
		}
	}

	harness.startNginx(routeInterface.Block, interfaceIpv4, interfaceIpv6)

	frontSettings := nginx.DefaultFrontSettings()
	certificateStore, err := nginx.LoadCertificateStore(parityVaultDir, routeTable.Env, routeTable)
	if err != nil {
		t.Fatalf("the front's certificates: %v", err)
	}
	front, err := nginx.NewFront(ctx, routeTable, revision, certificateStore, harness.recorder.handler(routeTable.Front.ConnectService, parityInProcessBlock), parityVersion, frontSettings)
	if err != nil {
		t.Fatalf("the front: %v", err)
	}
	harness.frontObject = front
	for _, addr := range []netip.Addr{interfaceIpv4, interfaceIpv6} {
		httpsListener, err := net.Listen("tcp", netip.AddrPortFrom(addr, uint16(routeTable.Listen.HttpsPort)).String())
		if err != nil {
			t.Fatal(err)
		}
		httpsServer := newParityFrontHttpServer(ctx, front.HttpsHandler(routeInterface.Name), frontSettings.Http, discardLog)
		harness.httpServers = append(harness.httpServers, httpsServer)
		go httpsServer.Serve(tls.NewListener(httpsListener, front.TlsConfig()))

		httpListener, err := net.Listen("tcp", netip.AddrPortFrom(addr, uint16(routeTable.Listen.HttpPort)).String())
		if err != nil {
			t.Fatal(err)
		}
		httpServer := newParityFrontHttpServer(ctx, front.HttpHandler(routeInterface.Name), frontSettings.Http, discardLog)
		harness.httpServers = append(harness.httpServers, httpServer)
		go httpServer.Serve(httpListener)
	}

	for _, target := range []*parityTarget{harness.nginx, harness.front} {
		if err := harness.waitForTarget(target); err != nil {
			t.Fatalf("%s: %v\nnginx's output:\n%s", target.name, err, harness.nginxOutput.String())
		}
	}
	success = true
	return harness
}

// An http.Server for one of the front's public tcp ports, set up as
// connect's newHttpServer sets one up (connect/front_http.go): nginx's client
// header timeout, keepalive timeout, header bytes and h2 stream limit, and no
// general OPTIONS handler. It stands in for Front.ConfigureServer, which
// LBALT-IMPL.md §7 decides and lbalt/integrate doesn't have yet; once it
// exists, this should call it instead. Its MaxHeaderBytes is connect's,
// MaxRequestHeadersTotalBytes, where §7 decides twice that, so a head inside
// the band between them gets Go's 431 where the front should answer 400, and
// the run reports it.
func newParityFrontHttpServer(ctx context.Context, handler http.Handler, httpSettings *nginx.HttpSettings, errorLog *log.Logger) *http.Server {
	httpServer := &http.Server{
		Handler:                      handler,
		ReadHeaderTimeout:            httpSettings.ClientHeaderTimeout,
		IdleTimeout:                  httpSettings.KeepaliveTimeout,
		MaxHeaderBytes:               httpSettings.MaxRequestHeadersTotalBytes,
		DisableGeneralOptionsHandler: true,
		ErrorLog:                     errorLog,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	if 0 < httpSettings.Http2MaxConcurrentStreams {
		httpServer.HTTP2 = &http.HTTP2Config{
			MaxConcurrentStreams: httpSettings.Http2MaxConcurrentStreams,
		}
	}
	if httpSettings.KeepaliveTimeout <= 0 {
		httpServer.SetKeepAlivesEnabled(false)
	}
	return httpServer
}

// Starts nginx through the image's lb binary, which rewrites the block's
// listens to the interface addresses and WARP_PORTS, as in production.
func (self *parityHarness) startNginx(block string, interfaceIpv4 netip.Addr, interfaceIpv6 netip.Addr) {
	cmd := exec.Command(parityLbBinary)
	cmd.Env = append(
		os.Environ(),
		"WARP_BLOCK="+block,
		"WARP_HOST_IPV4="+interfaceIpv4.String(),
		"WARP_HOST_IPV6="+interfaceIpv6.String(),
		fmt.Sprintf("WARP_PORTS=80:%d,443:%d,4053:%d", parityNginxHttpPort, parityNginxHttpsPort, parityNginxWhodisPort),
	)
	cmd.Stdout = self.nginxOutput
	cmd.Stderr = self.nginxOutput
	if err := cmd.Start(); err != nil {
		self.t.Fatalf("starting the lb binary: %v", err)
	}
	self.nginxCmd = cmd
	self.nginxExited = make(chan struct{})
	go func() {
		defer close(self.nginxExited)
		cmd.Wait()
	}()
}

// Waits until the target accepts on its addresses and ports, or nginx exits.
func (self *parityHarness) waitForTarget(target *parityTarget) error {
	deadline := time.Now().Add(self.settings.StartTimeout)
	for _, addr := range []netip.Addr{target.ipv4, target.ipv6} {
		for _, port := range []int{target.httpPort, target.httpsPort} {
			addrPort := netip.AddrPortFrom(addr, uint16(port)).String()
			for {
				conn, err := net.DialTimeout("tcp", addrPort, self.settings.StartPollInterval)
				if err == nil {
					conn.Close()
					break
				}
				if deadline.Before(time.Now()) {
					return fmt.Errorf("%s does not accept: %w", addrPort, err)
				}
				select {
				case <-self.nginxExited:
					return fmt.Errorf("nginx exited before %s accepted", addrPort)
				case <-time.After(self.settings.StartPollInterval):
				}
			}
		}
	}
	return nil
}

// Stops everything the harness started.
func (self *parityHarness) close() {
	for _, httpServer := range slices.Backward(self.httpServers) {
		httpServer.Close()
	}
	if self.frontObject != nil {
		self.frontObject.Close()
	}
	if self.nginxCmd != nil {
		self.nginxCmd.Process.Signal(syscall.SIGQUIT)
		select {
		case <-self.nginxExited:
		case <-time.After(self.settings.StopTimeout):
			self.nginxCmd.Process.Kill()
			<-self.nginxExited
		}
	}
	self.cancel()
}

// A writer that keeps only its last bytes. Safe for concurrent use.
type parityBoundedBuffer struct {
	maxBytes  int
	stateLock sync.Mutex
	buffer    []byte
}

// A buffer that keeps at most maxBytes.
func newParityBoundedBuffer(maxBytes int) *parityBoundedBuffer {
	return &parityBoundedBuffer{
		maxBytes: maxBytes,
	}
}

// Appends, dropping the oldest bytes past the bound.
func (self *parityBoundedBuffer) Write(b []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.buffer = append(self.buffer, b...)
	if self.maxBytes < len(self.buffer) {
		self.buffer = slices.Clone(self.buffer[len(self.buffer)-self.maxBytes:])
	}
	return len(b), nil
}

// What is kept.
func (self *parityBoundedBuffer) String() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return string(self.buffer)
}

// Runs one case against one target and returns what it saw.
func (self *parityHarness) runCase(parityCase *parityCase, target *parityTarget) *parityObservation {
	run := &parityCaseRun{
		harness:     self,
		target:      target,
		caseName:    parityCase.name,
		caseKey:     target.name + "/" + parityCase.name,
		observation: newParityObservation(),
	}
	if parityCase.run != nil {
		parityCase.run(run)
	} else {
		run.send(parityCase.request)
	}
	run.finish(parityCase.compareBlocks)
	return run.observation
}

// One case's result: its two observations.
type parityResult struct {
	parityCase *parityCase
	nginx      *parityObservation
	front      *parityObservation
}

// Runs the corpus, the concurrent cases in the background and the rest one
// after another, each against nginx and then the front, and reports.
func (self *parityHarness) runCorpus(parityCases []*parityCase) {
	caseNames := map[string]bool{}
	for _, parityCase := range parityCases {
		if caseNames[parityCase.name] {
			self.t.Fatalf("two cases are named %s", parityCase.name)
		}
		caseNames[parityCase.name] = true
		self.recorder.setScenario(parityCase.name, parityCase.scenario)
	}

	results := make([]*parityResult, len(parityCases))
	var concurrentCases sync.WaitGroup
	for i, parityCase := range parityCases {
		if !parityCase.concurrent {
			continue
		}
		concurrentCases.Add(1)
		go func() {
			defer concurrentCases.Done()
			result := &parityResult{
				parityCase: parityCase,
			}
			var targets sync.WaitGroup
			targets.Add(1)
			go func() {
				defer targets.Done()
				result.front = self.runCase(parityCase, self.front)
			}()
			result.nginx = self.runCase(parityCase, self.nginx)
			targets.Wait()
			results[i] = result
		}()
	}
	for i, parityCase := range parityCases {
		if parityCase.concurrent {
			continue
		}
		results[i] = &parityResult{
			parityCase: parityCase,
			nginx:      self.runCase(parityCase, self.nginx),
			front:      self.runCase(parityCase, self.front),
		}
	}
	concurrentCases.Wait()
	self.report(results)
}

// Logs what each case saw and every difference with its classification:
// (a) accepted, (b) a difference no accepted entry names, which fails the
// test, its message naming the harness's proposal for it when there is one,
// and (c) a harness artifact, here a case that could not reach a target,
// which fails it too.
func (self *parityHarness) report(results []*parityResult) {
	acceptanceIdCounts := map[string]int{}
	proposedAcceptanceIdCounts := map[string]int{}
	unacceptedCount := 0
	for _, result := range results {
		check := &parityCheck{
			parityCase: result.parityCase,
			nginx:      result.nginx,
			front:      result.front,
		}
		for _, observation := range []*parityObservation{result.nginx, result.front} {
			for _, key := range observation.keys {
				if _, field, ok := parityConnKey(key); ok && field == "error" {
					self.t.Errorf("parity (c) %s: a connection failed: %s=%s", result.parityCase.name, key, observation.keyValues[key])
				}
			}
		}
		differences := check.differences()
		self.t.Logf("parity case %s: nginx %s; front %s; %d differences", result.parityCase.name, parityCaseSummary(result.nginx), parityCaseSummary(result.front), len(differences))
		for _, difference := range differences {
			acceptance := check.acceptance(difference)
			switch {
			case acceptance == nil:
				unacceptedCount += 1
				self.t.Errorf("parity (b) %s: %s: nginx=%s front=%s", result.parityCase.name, difference.key, difference.nginx, difference.front)
			case acceptance.proposed:
				proposedAcceptanceIdCounts[acceptance.id] += 1
				self.t.Errorf("parity (b) %s: %s: nginx=%s front=%s [%s: %s]", result.parityCase.name, difference.key, difference.nginx, difference.front, acceptance.id, acceptance.source)
			default:
				acceptanceIdCounts[acceptance.id] += 1
				self.t.Logf("parity (a) %s: %s: nginx=%s front=%s [%s: %s]", result.parityCase.name, difference.key, difference.nginx, difference.front, acceptance.id, acceptance.source)
			}
		}
	}
	summary := &bytes.Buffer{}
	fmt.Fprintf(summary, "parity: %d cases; %d differences no entry names", len(results), unacceptedCount)
	for _, id := range slices.Sorted(maps.Keys(proposedAcceptanceIdCounts)) {
		fmt.Fprintf(summary, "; proposed %s %d", id, proposedAcceptanceIdCounts[id])
	}
	for _, id := range slices.Sorted(maps.Keys(acceptanceIdCounts)) {
		fmt.Fprintf(summary, "; accepted %s %d", id, acceptanceIdCounts[id])
	}
	self.t.Log(summary.String())
	if self.t.Failed() {
		self.t.Logf("nginx's last output:\n%s", self.nginxOutput.String())
	}
}

// One side of a case in a line: each response's status or error, the
// upstream attempts, and the facts.
func parityCaseSummary(observation *parityObservation) string {
	responses := []string{}
	facts := []string{}
	for _, key := range observation.keys {
		if _, field, ok := parityResponseKey(key); ok && (field == "status" || field == "error") {
			responses = append(responses, observation.keyValues[key])
		}
		if name, ok := strings.CutPrefix(key, "fact."); ok {
			facts = append(facts, name+"="+observation.keyValues[key])
		}
		if _, field, ok := parityConnKey(key); ok {
			switch field {
			case "tls.error", "error":
				responses = append(responses, "connection "+observation.keyValues[key])
			case "tls.certificate", "tls.alpn":
				facts = append(facts, field+"="+observation.keyValues[key])
			}
		}
	}
	summary := fmt.Sprintf("responses [%s] upstream attempts %s", strings.Join(responses, " "), observation.keyValues["upstream.count"])
	if 0 < len(facts) {
		summary += " facts [" + strings.Join(facts, "; ") + "]"
	}
	return summary
}
