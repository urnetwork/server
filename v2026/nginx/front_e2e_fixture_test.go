package nginx

// The end-to-end fixture: the assembled lb_alt front, built through NewFront
// from a route file and a certificate vault the test writes, served on real
// loopback sockets by http.Servers as connect serves its public ports, with a
// recording upstream on every block port. Clients dial the sockets over TLS,
// as public clients do, and each test checks both what the client got and
// what the block, or connect's in-process handler, received.
//
// The route file is testdata/front_e2e_route.json, a variant of the shared
// example whose upstream bridge is loopback: each block's port is replaced by
// the port of the upstream the test bound for it, and the table goes through
// LoadRouteTable's strict parse and revision before the front sees it.
//
// Everything is synthetic: .example names, certificates from a generated
// authority, loopback sockets and documentation addresses. Waits are bounded
// only to turn a hang into a failure. Ordering comes from barriers (held
// upstream handlers, the servers' record of finished requests, connection
// state callbacks), never from sleeps.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// How long one wait for an event may take before a test fails. It bounds a
// hang only: no test waits it out to pass.
const e2eWaitTimeout = 30 * time.Second

// The version the front reports in its lb status json.
const e2eVersion = "1.2.3"

// The lb status json the front answers with (§2.6).
const e2eLbStatusBody = `{"version":"` + e2eVersion + `","status":"ok","front":"lbalt"}` + "\n"

// Front settings for the end-to-end tests: the defaults, with spill files in
// a directory of the test's own, so a test can see that none is left, and a
// long wait for a request write that reports after its response was read
// (ForwardSettings.KeepaliveWriteWait), so a loaded machine never costs a
// test its pooled upstream connection.
func e2eFrontSettings(t *testing.T) *FrontSettings {
	settings := DefaultFrontSettings()
	settings.Body.SpillDir = t.TempDir()
	settings.Forward.KeepaliveWriteWait = 30 * time.Second
	return settings
}

// Receives the next value from a channel within e2eWaitTimeout, failing the
// test otherwise.
func receiveE2e[T any](t *testing.T, values <-chan T, what string) T {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(e2eWaitTimeout):
		t.Fatalf("no %s", what)
		var zero T
		return zero
	}
}

// Waits for a channel to close within e2eWaitTimeout, failing the test
// otherwise.
func waitE2eClosed(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(e2eWaitTimeout):
		t.Fatalf("%s did not happen", what)
	}
}

// n bytes of a pattern that repeats every 251 bytes, a prime, so a byte
// replayed at the wrong offset never lines up with a power-of-two buffer.
func e2eBody(n int) []byte {
	body := make([]byte, n)
	for i := range body {
		body[i] = byte(i % 251)
	}
	return body
}

// A notification a front's upstreams share: closed and replaced on every
// change of any of their states, so a test can wait for a change at one
// block or across several. Safe for concurrent use.
type e2eMonitor struct {
	stateLock sync.Mutex
	update    chan struct{}
}

// A monitor with nothing to report yet.
func newE2eMonitor() *e2eMonitor {
	return &e2eMonitor{
		update: make(chan struct{}),
	}
}

// The channel the next change closes.
func (self *e2eMonitor) notifyChannel() chan struct{} {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.update
}

// Wakes every waiter, after a change. Called inside the changed state's
// locked scope.
func (self *e2eMonitor) notifyAll() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	close(self.update)
	self.update = make(chan struct{})
}

// Waits until check holds, subscribing right before each read of it.
func (self *e2eMonitor) await(t *testing.T, what string, check func() bool) {
	t.Helper()
	timeout := time.After(e2eWaitTimeout)
	for {
		update := self.notifyChannel()
		if check() {
			return
		}
		select {
		case <-update:
		case <-timeout:
			t.Fatalf("%s did not happen", what)
		}
	}
}

// One request as a block received it.
type e2eUpstreamRequest struct {
	service string
	block   string
	// the front's end of the upstream connection, which tells connections
	// apart
	remoteAddr       string
	method           string
	requestUri       string
	proto            string
	host             string
	header           http.Header
	contentLength    int64
	transferEncoding []string
	body             []byte
	bodyErr          error
}

// A block of the route table: an http server on loopback that records each
// request, its whole body included, then answers with its handler, by
// default 200 with `<service>/<block>`. It follows its connections' states,
// so a test can wait for the front to close them. Every change notifies the
// monitor it shares with the front's other blocks. Safe for concurrent use.
type e2eUpstream struct {
	service  string
	block    string
	listener net.Listener
	server   *http.Server
	monitor  *e2eMonitor

	stateLock sync.Mutex
	handler   func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest)
	// when set, answers in place of handler, with the body left unread and
	// unrecorded, for a test that reads it as it arrives
	streamHandler func(w http.ResponseWriter, r *http.Request)
	requests      []*e2eUpstreamRequest
	// connections open and not hijacked
	openConnCount int
}

// A block's upstream on a new loopback port, closed when the test ends.
func newE2eUpstream(t *testing.T, service string, block string, monitor *e2eMonitor) *e2eUpstream {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstream := &e2eUpstream{
		service:  service,
		block:    block,
		listener: listener,
		monitor:  monitor,
	}
	upstream.server = &http.Server{
		Handler:   http.HandlerFunc(upstream.serveHttp),
		ConnState: upstream.connState,
		ErrorLog:  log.New(io.Discard, "", 0),
		// past nginx's max_headers, so a head the front forwards whole
		// reaches the handler
		MaxHeaderValueCount: 4 * 1000,
	}
	go upstream.server.Serve(listener)
	t.Cleanup(func() {
		upstream.server.Close()
	})
	return upstream
}

// The port the block listens on, which the route file names.
func (self *e2eUpstream) port() int {
	return self.listener.Addr().(*net.TCPAddr).Port
}

// Answers every request from now on with handler; nil answers the default.
func (self *e2eUpstream) setHandler(handler func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.handler = handler
}

// Answers every request from now on with streamHandler, which reads the
// body itself; nil goes back to the recorded body and handler.
func (self *e2eUpstream) setStreamHandler(streamHandler func(w http.ResponseWriter, r *http.Request)) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.streamHandler = streamHandler
}

// Records the request, its whole body included unless a stream handler
// reads it, then answers it.
func (self *e2eUpstream) serveHttp(w http.ResponseWriter, r *http.Request) {
	streamHandler := func() func(w http.ResponseWriter, r *http.Request) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.streamHandler
	}()
	var body []byte
	var bodyErr error
	if streamHandler == nil {
		body, bodyErr = io.ReadAll(r.Body)
	}
	request := &e2eUpstreamRequest{
		service:          self.service,
		block:            self.block,
		remoteAddr:       r.RemoteAddr,
		method:           r.Method,
		requestUri:       r.RequestURI,
		proto:            r.Proto,
		host:             r.Host,
		header:           r.Header.Clone(),
		contentLength:    r.ContentLength,
		transferEncoding: slices.Clone(r.TransferEncoding),
		body:             body,
		bodyErr:          bodyErr,
	}
	handler := func() func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, request)
		self.monitor.notifyAll()
		return self.handler
	}()
	if streamHandler != nil {
		streamHandler(w, r)
		return
	}
	if handler != nil {
		handler(w, r, request)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, self.service+"/"+self.block)
}

// Follows the server's connections: a hijacked one leaves its count.
func (self *e2eUpstream) connState(conn net.Conn, connState http.ConnState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch connState {
	case http.StateNew:
		self.openConnCount += 1
	case http.StateClosed, http.StateHijacked:
		self.openConnCount -= 1
	default:
		return
	}
	self.monitor.notifyAll()
}

// The requests recorded so far.
func (self *e2eUpstream) recorded() []*e2eUpstreamRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// Waits until check, read under the upstream's lock, holds.
func (self *e2eUpstream) await(t *testing.T, what string, check func() bool) {
	t.Helper()
	self.monitor.await(t, self.service+"/"+self.block+": "+what, func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return check()
	})
}

// Waits until the block has received count requests, and returns them.
func (self *e2eUpstream) awaitRequests(t *testing.T, count int) []*e2eUpstreamRequest {
	t.Helper()
	self.await(t, fmt.Sprintf("request %d", count), func() bool {
		return count <= len(self.requests)
	})
	return self.recorded()
}

// Waits until the block has count connections open, hijacked ones aside.
func (self *e2eUpstream) awaitOpenConns(t *testing.T, count int) {
	t.Helper()
	self.await(t, fmt.Sprintf("%d open connections", count), func() bool {
		return self.openConnCount == count
	})
}

// A barrier upstream handlers wait at until the test lets them go. release
// is idempotent, and runs when the test ends at the latest, so a failed test
// never leaves a handler waiting.
type e2eHold struct {
	released    chan struct{}
	releaseOnce sync.Once
}

// A barrier not yet released. It is released when the test ends, at the
// latest.
func newE2eHold(t *testing.T) *e2eHold {
	hold := &e2eHold{
		released: make(chan struct{}),
	}
	t.Cleanup(hold.release)
	return hold
}

// Lets every waiter go.
func (self *e2eHold) release() {
	self.releaseOnce.Do(func() {
		close(self.released)
	})
}

// Waits for the release, or for the request to end, as when the front cuts
// its connection.
func (self *e2eHold) wait(r *http.Request) {
	select {
	case <-self.released:
	case <-r.Context().Done():
	}
}

// One request connect's in-process handler received.
type e2eConnectRequest struct {
	remoteAddr string
	method     string
	host       string
	requestUri string
	path       string
	header     http.Header
	overTls    bool
}

// The connect handler the front serves connect names with: it records each
// request and answers 200 "connect". Safe for concurrent use.
type e2eConnectHandler struct {
	stateLock sync.Mutex
	requests  []*e2eConnectRequest
}

// See http.Handler.
func (self *e2eConnectHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.requests = append(self.requests, &e2eConnectRequest{
			remoteAddr: r.RemoteAddr,
			method:     r.Method,
			host:       r.Host,
			requestUri: r.RequestURI,
			path:       r.URL.Path,
			header:     r.Header.Clone(),
			overTls:    r.TLS != nil,
		})
	}()
	w.Header().Set("Content-Type", "text/plain")
	io.WriteString(w, "connect")
}

// The requests so far.
func (self *e2eConnectHandler) recorded() []*e2eConnectRequest {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.requests)
}

// The request targets a server's handler has finished, in order: the
// front's handler has returned for them, so their limit slots are let go and
// whatever they held is released. Safe for concurrent use.
type e2eServedLog struct {
	stateLock   sync.Mutex
	requestUris []string
	// closed and replaced on every addition
	update chan struct{}
}

// An empty log.
func newE2eServedLog() *e2eServedLog {
	return &e2eServedLog{
		requestUris: []string{},
		update:      make(chan struct{}),
	}
}

// Records a finished request.
func (self *e2eServedLog) add(requestUri string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requestUris = append(self.requestUris, requestUri)
	close(self.update)
	self.update = make(chan struct{})
}

// Waits until a request for requestUri has finished.
func (self *e2eServedLog) await(t *testing.T, requestUri string) {
	t.Helper()
	timeout := time.After(e2eWaitTimeout)
	for {
		update, served := func() (chan struct{}, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.update, slices.Contains(self.requestUris, requestUri)
		}()
		if served {
			return
		}
		select {
		case <-update:
		case <-timeout:
			t.Fatalf("the request for %s never finished", requestUri)
		}
	}
}

// Serves through the front's handler, then records the request as finished,
// whether the handler returned or panicked: the forwarder aborts a response
// cut short with http.ErrAbortHandler. The target is read first, since the
// front rewrites it for connect's in-process handler.
func newE2eServedHandler(handler http.Handler, servedLog *e2eServedLog) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestUri := r.RequestURI
		defer servedLog.add(requestUri)
		handler.ServeHTTP(w, r)
	})
}

// A public server's error log, kept so the fixture can fail a test on a
// handler panic the server would otherwise only log. Safe for concurrent use.
type e2eErrorLog struct {
	stateLock sync.Mutex
	lines     []string
}

// See io.Writer.
func (self *e2eErrorLog) Write(p []byte) (int, error) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lines = append(self.lines, string(p))
	return len(p), nil
}

// The handler panics logged so far. http.ErrAbortHandler is never logged.
func (self *e2eErrorLog) panics() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	panicLines := []string{}
	for _, line := range self.lines {
		if strings.Contains(line, "panic serving") {
			panicLines = append(panicLines, line)
		}
	}
	return panicLines
}

// An http.Server for one of the front's public TCP ports, as connect's
// newHttpServer builds it (connect/front_http.go): the handler, the error log
// and requests that end with ctx are the caller's, and the front sets the
// rest from its HttpSettings (Front.ConfigureServer).
func newE2eHttpServer(ctx context.Context, front *Front, handler http.Handler, errorLog *e2eErrorLog) *http.Server {
	httpServer := &http.Server{
		Handler:  handler,
		ErrorLog: log.New(errorLog, "", 0),
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
	front.ConfigureServer(httpServer)
	return httpServer
}

// One interface's public sockets, as connect binds them: TCP 443 on loopback
// IPv4, and on loopback IPv6 when the host has it, served by one TLS server,
// and TCP 80, served by another.
type e2eInterface struct {
	name      string
	httpsAddr string
	// empty when the host has no loopback IPv6
	httpsIpv6Addr string
	httpAddr      string
	// what each server's handler has finished
	httpsServed *e2eServedLog
	httpServed  *e2eServedLog
}

// The front under test, its sockets, and what they reach.
type e2eFront struct {
	routeTable *RouteTable
	revision   string
	settings   *FrontSettings
	authority  *testTlsAuthority
	// each vault directory's leaf, by directory name
	dirLeafs       map[string]*x509.Certificate
	front          *Front
	connectHandler *e2eConnectHandler
	// each block's upstream, by `<service>/<block>`
	upstreams map[string]*e2eUpstream
	// what every upstream's changes notify
	monitor *e2eMonitor
	// by interface name
	interfaces map[string]*e2eInterface
	errorLog   *e2eErrorLog
}

// Builds the front through NewFront and serves it: an upstream for every
// block, the route file with their ports, loaded through LoadRouteTable, a
// vault loaded through LoadCertificateStore, and each interface's sockets.
// settings nil takes e2eFrontSettings; editRouteTable, when set, changes the
// table before it is written and loaded. Everything closes when the test
// ends, the front last, and a handler panic any server logged fails the test.
func newE2eFront(t *testing.T, settings *FrontSettings, editRouteTable func(routeTable *RouteTable)) *e2eFront {
	t.Helper()
	if settings == nil {
		settings = e2eFrontSettings(t)
	}
	raw, err := os.ReadFile(filepath.Join("testdata", "front_e2e_route.json"))
	if err != nil {
		t.Fatal(err)
	}
	routeTable, err := ParseRouteTable(raw)
	if err != nil {
		t.Fatal(err)
	}
	monitor := newE2eMonitor()
	upstreams := map[string]*e2eUpstream{}
	for _, service := range routeTable.Services {
		for _, block := range service.Blocks {
			upstream := newE2eUpstream(t, service.Name, block.Name, monitor)
			upstreams[service.Name+"/"+block.Name] = upstream
			block.Port = upstream.port()
		}
	}
	if editRouteTable != nil {
		editRouteTable(routeTable)
	}
	encoded, err := json.MarshalIndent(routeTable, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	routePath := filepath.Join(t.TempDir(), "route.json")
	if err := os.WriteFile(routePath, append(encoded, '\n'), 0o444); err != nil {
		t.Fatal(err)
	}
	routeTable, revision, err := LoadRouteTable(routePath)
	if err != nil {
		t.Fatal(err)
	}

	authority := newTestTlsAuthority(t)
	vaultDir := t.TempDir()
	// the certificates of the table's names, by vault directory, and the
	// names each leaf lists. Every served name finds its pair by
	// findLatestTls's own, star.<parent> or <parent> rule, and a few names
	// have a leaf of their own, so a handshake shows which name chose its
	// certificate.
	dirDnsNames := map[string][]string{
		"test-lb.service.example":       {"test-lb.service.example"},
		"api.service.example":           {"api.service.example"},
		"app.web.service.example":       {"app.web.service.example"},
		"star.service.example":          {"*.service.example"},
		"star.other.example":            {"*.other.example"},
		"star.connect.service.example":  {"*.connect.service.example"},
		"star.web.service.example":      {"*.web.service.example"},
		"star.deep.web.service.example": {"*.deep.web.service.example"},
	}
	dirLeafs := map[string]*x509.Certificate{}
	for dirName, dnsNames := range dirDnsNames {
		pair := authority.issueValid(t, 30*24*time.Hour, dnsNames...)
		writeTestTlsPair(t, filepath.Join(vaultDir, routeTable.Env), testTlsVersion, dirName, pair)
		dirLeafs[dirName] = pair.leaf
	}
	certificateStore, err := LoadCertificateStore(vaultDir, routeTable.Env, routeTable)
	if err != nil {
		t.Fatal(err)
	}

	connectHandler := &e2eConnectHandler{}
	front, err := NewFront(t.Context(), routeTable, revision, certificateStore, connectHandler, e2eVersion, settings)
	if err != nil {
		t.Fatal(err)
	}
	errorLog := &e2eErrorLog{}
	// the cleanups run last first: the servers close before the front, and
	// a panic is checked once both have
	t.Cleanup(func() {
		front.Close()
		for _, panicLine := range errorLog.panics() {
			t.Errorf("a front server logged a handler panic: %s", panicLine)
		}
	})

	e2e := &e2eFront{
		routeTable:     routeTable,
		revision:       revision,
		settings:       settings,
		authority:      authority,
		dirLeafs:       dirLeafs,
		front:          front,
		connectHandler: connectHandler,
		upstreams:      upstreams,
		monitor:        monitor,
		interfaces:     map[string]*e2eInterface{},
		errorLog:       errorLog,
	}
	for _, routeInterface := range routeTable.Interfaces {
		e2e.interfaces[routeInterface.Name] = e2e.serveInterface(t, routeInterface.Name)
	}
	return e2e
}

// Binds one interface's sockets and serves them, as connect does: TLS
// through the front's one long-lived config with NewTlsListener, never a
// clone of it, so the server leaves the config alone.
func (self *e2eFront) serveInterface(t *testing.T, interfaceName string) *e2eInterface {
	t.Helper()
	e2eInterface := &e2eInterface{
		name:        interfaceName,
		httpsServed: newE2eServedLog(),
		httpServed:  newE2eServedLog(),
	}
	httpsServer := newE2eHttpServer(t.Context(), self.front, newE2eServedHandler(self.front.HttpsHandler(interfaceName), e2eInterface.httpsServed), self.errorLog)
	httpServer := newE2eHttpServer(t.Context(), self.front, newE2eServedHandler(self.front.HttpHandler(interfaceName), e2eInterface.httpServed), self.errorLog)
	t.Cleanup(func() {
		httpsServer.Close()
		httpServer.Close()
	})

	httpsListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e2eInterface.httpsAddr = httpsListener.Addr().String()
	go httpsServer.Serve(NewTlsListener(httpsListener, self.front.TlsConfig(), httpsServer.ReadHeaderTimeout, nil))
	if httpsIpv6Listener, err := net.Listen("tcp6", "[::1]:0"); err == nil {
		e2eInterface.httpsIpv6Addr = httpsIpv6Listener.Addr().String()
		go httpsServer.Serve(NewTlsListener(httpsIpv6Listener, self.front.TlsConfig(), httpsServer.ReadHeaderTimeout, nil))
	}
	httpListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e2eInterface.httpAddr = httpListener.Addr().String()
	go httpServer.Serve(httpListener)
	return e2eInterface
}

// The upstream of a service's block.
func (self *e2eFront) upstream(t *testing.T, service string, block string) *e2eUpstream {
	t.Helper()
	upstream, ok := self.upstreams[service+"/"+block]
	if !ok {
		t.Fatalf("no upstream %s/%s", service, block)
	}
	return upstream
}

// Every block's request count, to compare with later.
func (self *e2eFront) requestCounts() map[string]int {
	requestCounts := map[string]int{}
	for key, upstream := range self.upstreams {
		requestCounts[key] = len(upstream.recorded())
	}
	return requestCounts
}

// The requests any block received after requestCounts was taken.
func (self *e2eFront) requestsSince(requestCounts map[string]int) []*e2eUpstreamRequest {
	requests := []*e2eUpstreamRequest{}
	for key, upstream := range self.upstreams {
		recorded := upstream.recorded()
		requests = append(requests, recorded[min(requestCounts[key], len(recorded)):]...)
	}
	return requests
}

// The requests every block of a service received, in block order.
func (self *e2eFront) serviceRequests(service string) []*e2eUpstreamRequest {
	requests := []*e2eUpstreamRequest{}
	for _, routeService := range self.routeTable.Services {
		if routeService.Name != service {
			continue
		}
		for _, block := range routeService.Blocks {
			requests = append(requests, self.upstreams[service+"/"+block.Name].recorded()...)
		}
	}
	return requests
}

// Waits until a service's blocks together have received count requests,
// and returns them, in block order.
func (self *e2eFront) awaitServiceRequests(t *testing.T, service string, count int) []*e2eUpstreamRequest {
	t.Helper()
	self.monitor.await(t, fmt.Sprintf("%s request %d", service, count), func() bool {
		return count <= len(self.serviceRequests(service))
	})
	return self.serviceRequests(service)
}

// Sets one handler on every block of a service.
func (self *e2eFront) setServiceHandler(t *testing.T, service string, handler func(w http.ResponseWriter, r *http.Request, request *e2eUpstreamRequest)) {
	t.Helper()
	found := false
	for key, upstream := range self.upstreams {
		if strings.HasPrefix(key, service+"/") {
			upstream.setHandler(handler)
			found = true
		}
	}
	if !found {
		t.Fatalf("no blocks for service %s", service)
	}
}

// The front's body store, the one NewFront built.
func (self *e2eFront) store(t *testing.T) *bodyStore {
	t.Helper()
	store, ok := self.front.bodyStore.(*bodyStore)
	if !ok {
		t.Fatalf("the front's body store is a %T, not NewFront's", self.front.bodyStore)
	}
	return store
}

// What the front's body store holds now: reserved memory, spilled bytes,
// spill files, and bodies not yet released.
func (self *e2eFront) bodyUsage(t *testing.T) (memoryBytes int64, spillBytes int64, spillFiles int, bodies int) {
	t.Helper()
	return self.store(t).usage()
}

// The names left in the spill directory.
func (self *e2eFront) spillDirNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(self.settings.Body.SpillDir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// The interface named name.
func (self *e2eFront) iface(t *testing.T, name string) *e2eInterface {
	t.Helper()
	e2eInterface, ok := self.interfaces[name]
	if !ok {
		t.Fatalf("no interface %s", name)
	}
	return e2eInterface
}

// A client TLS config, as a public client's: sni as the server name, or none
// when it is empty, offering nextProtos. The chain must verify against the
// test authority. The name isn't checked here, so a missing or unknown SNI
// can be sent: the tests check which leaf the front chose (checkCertificate),
// which a name check would only approximate.
func (self *e2eFront) clientTlsConfig(sni string, nextProtos []string) *tls.Config {
	roots := self.authority.pool()
	return &tls.Config{
		ServerName: sni,
		NextProtos: nextProtos,
		// VerifyConnection verifies the chain instead
		InsecureSkipVerify: true,
		VerifyConnection: func(connectionState tls.ConnectionState) error {
			if len(connectionState.PeerCertificates) == 0 {
				return fmt.Errorf("no peer certificate")
			}
			intermediates := x509.NewCertPool()
			for _, certificate := range connectionState.PeerCertificates[1:] {
				intermediates.AddCert(certificate)
			}
			_, err := connectionState.PeerCertificates[0].Verify(x509.VerifyOptions{
				Roots:         roots,
				Intermediates: intermediates,
			})
			return err
		},
	}
}

// Dials addr and completes the TLS handshake as a public client would.
func (self *e2eFront) dialTls(ctx context.Context, addr string, sni string, nextProtos []string) (*tls.Conn, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	tlsConn := tls.Client(conn, self.clientTlsConfig(sni, nextProtos))
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

// An HTTP/1.1 client that sends every request to addr over TLS with sni, or
// none when it is empty, whatever the request's url names, as a client that
// resolved a fronted name to the interface does. It follows no redirect and
// asks for no gzip. Its connections close when the test ends.
func (self *e2eFront) newH1Client(t *testing.T, addr string, sni string) *http.Client {
	transport := &http.Transport{
		DialTLSContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return self.dialTls(ctx, addr, sni, []string{"http/1.1"})
		},
		DisableCompression: true,
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// A plain HTTP client for TCP 80 that sends every request to addr, follows
// no redirect and asks for no gzip.
func (self *e2eFront) newHttpClient(t *testing.T, addr string) *http.Client {
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			dialer := &net.Dialer{}
			return dialer.DialContext(ctx, "tcp", addr)
		},
		DisableCompression: true,
	}
	t.Cleanup(transport.CloseIdleConnections)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// One h2 connection to addr with sni, through golang.org/x/net/http2's
// client, so every request is a stream of this one connection. It closes
// when the test ends. localAddr is the client's end, the peer the front
// accepted.
func (self *e2eFront) newH2ClientConn(t *testing.T, addr string, sni string) (clientConn *http2.ClientConn, localAddr string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eWaitTimeout)
	defer cancel()
	tlsConn, err := self.dialTls(ctx, addr, sni, []string{"h2"})
	if err != nil {
		t.Fatal(err)
	}
	if negotiatedProtocol := tlsConn.ConnectionState().NegotiatedProtocol; negotiatedProtocol != "h2" {
		tlsConn.Close()
		t.Fatalf("%s negotiated %q, want h2", sni, negotiatedProtocol)
	}
	transport := &http2.Transport{}
	clientConn, err = transport.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clientConn.Close()
	})
	return clientConn, tlsConn.LocalAddr().String()
}

// A request whose context ends with the test, or after e2eWaitTimeout, so a
// stuck exchange fails instead of hanging.
func newE2eRequest(t *testing.T, method string, url string, body io.Reader) *http.Request {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eWaitTimeout)
	t.Cleanup(cancel)
	request, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

// Sends a request and reads its whole response. A transport error fails the
// test.
func e2eDo(t *testing.T, client *http.Client, request *http.Request) (*http.Response, []byte) {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", request.Method, request.URL, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s %s: reading the body: %v", request.Method, request.URL, err)
	}
	return response, body
}

// Sends a request as one stream of an h2 connection and reads its whole
// response.
func e2eRoundTripH2(t *testing.T, clientConn *http2.ClientConn, request *http.Request) (*http.Response, []byte) {
	t.Helper()
	response, err := clientConn.RoundTrip(request)
	if err != nil {
		t.Fatalf("h2 %s %s (authority %s): %v", request.Method, request.URL, request.Host, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("h2 %s %s: reading the body: %v", request.Method, request.URL, err)
	}
	return response, body
}

// Fails the test unless the connection was served the leaf of the vault
// directory dirName.
func (self *e2eFront) checkCertificate(t *testing.T, what string, connectionState *tls.ConnectionState, dirName string) {
	t.Helper()
	if connectionState == nil || len(connectionState.PeerCertificates) == 0 {
		t.Errorf("%s: no TLS connection state", what)
		return
	}
	leaf := connectionState.PeerCertificates[0]
	if !bytes.Equal(leaf.Raw, self.dirLeafs[dirName].Raw) {
		t.Errorf("%s: served the certificate for %v, want %s's (%v)", what, leaf.DNSNames, dirName, self.dirLeafs[dirName].DNSNames)
	}
}

// A raw HTTP/1.1 connection over TLS, for requests net/http's client would
// rewrite: odd request targets, and headers exactly as written.
type e2eRawConn struct {
	conn   *tls.Conn
	reader *bufio.Reader
}

// A raw connection to addr with sni, closed when the test ends.
func (self *e2eFront) dialRaw(t *testing.T, addr string, sni string) *e2eRawConn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eWaitTimeout)
	defer cancel()
	conn, err := self.dialTls(ctx, addr, sni, []string{"http/1.1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
	})
	return &e2eRawConn{
		conn:   conn,
		reader: bufio.NewReader(conn),
	}
}

// Writes a request head as given, lines joined with CRLF and ended with an
// empty line, then reads one response and its body.
func (self *e2eRawConn) exchange(t *testing.T, headLines ...string) (*http.Response, []byte) {
	t.Helper()
	self.conn.SetDeadline(time.Now().Add(e2eWaitTimeout))
	defer self.conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(self.conn, strings.Join(headLines, "\r\n")+"\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(self.reader, nil)
	if err != nil {
		t.Fatalf("%s: %v", headLines[0], err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s: reading the body: %v", headLines[0], err)
	}
	return response, body
}

// An h2 connection driven frame by frame, so a test sees each frame the
// front's server sends, a GOAWAY included, rather than what a client library
// makes of it. Used by one goroutine.
type e2eH2Framer struct {
	conn          *tls.Conn
	framer        *http2.Framer
	encodeBuffer  *bytes.Buffer
	encoder       *hpack.Encoder
	nextStreamId  uint32
	goAwayFrame   *http2.GoAwayFrame
	streamHeaders map[uint32]*http2.MetaHeadersFrame
	streamBodies  map[uint32]*bytes.Buffer
	endedStreams  map[uint32]bool
}

// An h2 connection to addr with sni, past the preface and settings, closed
// when the test ends.
func (self *e2eFront) dialH2Framer(t *testing.T, addr string, sni string) *e2eH2Framer {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), e2eWaitTimeout)
	defer cancel()
	conn, err := self.dialTls(ctx, addr, sni, []string{"h2"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		conn.Close()
	})
	if negotiatedProtocol := conn.ConnectionState().NegotiatedProtocol; negotiatedProtocol != "h2" {
		t.Fatalf("%s negotiated %q, want h2", sni, negotiatedProtocol)
	}
	encodeBuffer := &bytes.Buffer{}
	h2Framer := &e2eH2Framer{
		conn:          conn,
		framer:        http2.NewFramer(conn, conn),
		encodeBuffer:  encodeBuffer,
		encoder:       hpack.NewEncoder(encodeBuffer),
		nextStreamId:  1,
		streamHeaders: map[uint32]*http2.MetaHeadersFrame{},
		streamBodies:  map[uint32]*bytes.Buffer{},
		endedStreams:  map[uint32]bool{},
	}
	// h2's initial SETTINGS_HEADER_TABLE_SIZE, which the framer's settings
	// keep
	h2Framer.framer.ReadMetaHeaders = hpack.NewDecoder(4*1024, nil)
	conn.SetDeadline(time.Now().Add(e2eWaitTimeout))
	if _, err := io.WriteString(conn, http2.ClientPreface); err != nil {
		t.Fatal(err)
	}
	if err := h2Framer.framer.WriteSettings(); err != nil {
		t.Fatal(err)
	}
	return h2Framer
}

// Opens a stream for a GET of path on authority, with no body, and returns
// its id.
func (self *e2eH2Framer) get(t *testing.T, authority string, path string) uint32 {
	t.Helper()
	self.encodeBuffer.Reset()
	for _, headerField := range []hpack.HeaderField{
		{Name: ":method", Value: "GET"},
		{Name: ":scheme", Value: "https"},
		{Name: ":authority", Value: authority},
		{Name: ":path", Value: path},
	} {
		if err := self.encoder.WriteField(headerField); err != nil {
			t.Fatal(err)
		}
	}
	streamId := self.nextStreamId
	self.nextStreamId += 2
	self.conn.SetDeadline(time.Now().Add(e2eWaitTimeout))
	err := self.framer.WriteHeaders(http2.HeadersFrameParam{
		StreamID:      streamId,
		BlockFragment: self.encodeBuffer.Bytes(),
		EndStream:     true,
		EndHeaders:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return streamId
}

// Reads frames until the stream has ended and, when untilGoAway, until a
// GOAWAY has arrived, answering the server's settings and pings. The
// stream's status and body, and the GOAWAY when one arrived.
func (self *e2eH2Framer) await(t *testing.T, streamId uint32, untilGoAway bool) (status string, body []byte, goAwayFrame *http2.GoAwayFrame) {
	t.Helper()
	self.conn.SetDeadline(time.Now().Add(e2eWaitTimeout))
	for !self.endedStreams[streamId] || (untilGoAway && self.goAwayFrame == nil) {
		frame, err := self.framer.ReadFrame()
		if err != nil {
			t.Fatalf("stream %d: reading frames: %v (stream ended %t, goaway %t)", streamId, err, self.endedStreams[streamId], self.goAwayFrame != nil)
		}
		switch frame := frame.(type) {
		case *http2.SettingsFrame:
			if !frame.IsAck() {
				if err := self.framer.WriteSettingsAck(); err != nil {
					t.Fatal(err)
				}
			}
		case *http2.PingFrame:
			if !frame.IsAck() {
				if err := self.framer.WritePing(true, frame.Data); err != nil {
					t.Fatal(err)
				}
			}
		case *http2.MetaHeadersFrame:
			self.streamHeaders[frame.StreamID] = frame
			if frame.StreamEnded() {
				self.endedStreams[frame.StreamID] = true
			}
		case *http2.DataFrame:
			streamBody, ok := self.streamBodies[frame.StreamID]
			if !ok {
				streamBody = &bytes.Buffer{}
				self.streamBodies[frame.StreamID] = streamBody
			}
			streamBody.Write(frame.Data())
			if frame.StreamEnded() {
				self.endedStreams[frame.StreamID] = true
			}
		case *http2.RSTStreamFrame:
			t.Fatalf("stream %d was reset: %v", frame.StreamID, frame.ErrCode)
		case *http2.GoAwayFrame:
			self.goAwayFrame = frame
		}
	}
	if headers, ok := self.streamHeaders[streamId]; ok {
		status = headers.PseudoValue("status")
	}
	if streamBody, ok := self.streamBodies[streamId]; ok {
		body = streamBody.Bytes()
	}
	return status, body, self.goAwayFrame
}
