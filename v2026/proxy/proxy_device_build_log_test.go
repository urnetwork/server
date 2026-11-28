// The build line (proxy_device_build_log.go): one line for each device
// construction that succeeds, logged by the opener that ran it, naming the
// construction's kind, the entry point that started it and the peppered hash
// of its caller's address bucket, never the address. The tests drive the
// production entry points on the synctest clock with devices that need no
// platform (proxy_device_lazy_open_test.go). Every address is a documentation
// address and every credential synthetic.
package proxy

import (
	"context"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	proxylib "github.com/urnetwork/proxy/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// Installs a synthetic pepper before the process first hashes an address. The
// pepper is read once per process, so the tests compute the hashes they expect
// with server.ClientIpHashForAddr rather than from this value.
func configureBuildLogTest(t *testing.T) {
	t.Helper()
	configureHttpBudgetTest(t)
	t.Cleanup(server.Vault.PushSimpleResource("client.yml", []byte("client_ip_hash_pepper: synthetic-proxy-build-log-test-only\n")))
	// the pepper loads outside the clock bubble
	server.ClientIpHashForAddr(netip.MustParseAddr("192.0.2.1"))
}

// The build lines a manager logs, in order. Safe for concurrent use.
type buildLogTestLines struct {
	stateLock sync.Mutex
	lines     []string
}

// Takes the manager's build lines from now on.
func captureBuildLogTestLines(manager *ProxyDeviceManager) *buildLogTestLines {
	lines := &buildLogTestLines{}
	manager.buildLogf = func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		lines.stateLock.Lock()
		defer lines.stateLock.Unlock()
		lines.lines = append(lines.lines, line)
	}
	return lines
}

func (self *buildLogTestLines) get() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.lines)
}

// The line the spec gives: the first 8 bytes of the caller's
// server.ClientIpHashForAddr in hex, or none for no address.
func buildLogTestLine(proxyId server.Id, openKind string, path string, callerAddr netip.Addr) string {
	caller := "none"
	if callerAddr.IsValid() {
		callerHash := server.ClientIpHashForAddr(callerAddr)
		caller = hex.EncodeToString(callerHash[:8])
	}
	return fmt.Sprintf("[pd][%s]build open=%s path=%s caller=%s\n", proxyId, openKind, path, caller)
}

func requireBuildLogTestLines(t *testing.T, lines *buildLogTestLines, want []string) {
	t.Helper()
	if got := lines.get(); !slices.Equal(got, want) {
		t.Fatalf("build lines:\n%q\nwant:\n%q", got, want)
	}
}

// An HTTP CONNECT through the production dial callback from remoteAddr.
func buildLogTestHttpDial(ctx context.Context, core *proxylib.HttpProxy, proxyId server.Id, remoteAddr string) lazyOpenTestDial {
	r := httptest.NewRequest(http.MethodConnect, "https://192.0.2.10:443", nil)
	r.RemoteAddr = remoteAddr
	r.Header.Set("Proxy-Authorization", httpConnectTestBasic(proxyId))
	conn, err := core.ConnectDialContextWithRequest(ctx, r, "tcp", "192.0.2.10:443")
	return lazyOpenTestDial{conn: conn, err: err}
}

// A SOCKS CONNECT through the production dial callback from remoteAddr.
func buildLogTestSocksDial(ctx context.Context, core *proxylib.SocksProxy, proxyId server.Id, remoteAddr string) lazyOpenTestDial {
	request := &proxylib.Request{
		AuthContext: &proxylib.AuthContext{Payload: map[string]string{"username": model.SignProxyId(proxyId)}},
		DestAddr:    &proxylib.AddrSpec{IP: net.ParseIP("192.0.2.10"), Port: 443},
		RemoteAddr:  net.TCPAddrFromAddrPort(netip.MustParseAddrPort(remoteAddr)),
	}
	conn, err := core.ConnectDialWithRequest(ctx, request, "tcp", "192.0.2.10:443")
	return lazyOpenTestDial{conn: conn, err: err}
}

func newBuildLogTestSocksProxy(manager *ProxyDeviceManager) *proxylib.SocksProxy {
	return (&socks5Server{
		ctx:                manager.ctx,
		settings:           DefaultProxySettings(),
		proxyDeviceManager: manager,
	}).newSocksProxy()
}

func closeBuildLogTestDial(t *testing.T, dial lazyOpenTestDial) {
	t.Helper()
	if dial.err != nil {
		t.Fatalf("the open failed: %v", dial.err)
	}
	if dial.conn != nil {
		_ = dial.conn.Close()
	}
}

// A build logs exactly one line, with its caller's hash: a proxy's first
// CONNECT builds its device and logs it, and a second CONNECT on the live
// device logs nothing. A proxy created yesterday reopens, from an IPv6 caller
// in its own /56.
func TestProxyDeviceBuildLogsOneLineWithTheCallerHash(t *testing.T) {
	configureBuildLogTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		lines := captureBuildLogTestLines(manager)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		core := newHttpConnectTestProxy(manager)

		coldId := setupTestProxyIdAt(time.Now().Add(-time.Minute))
		seedHttpConnectTestLock(manager, coldId, true)
		for range 2 {
			closeBuildLogTestDial(t, buildLogTestHttpDial(manager.ctx, core, coldId, "192.0.2.77:41000"))
		}
		want := []string{buildLogTestLine(coldId, proxyOpenCold, proxyDevicePathHttp, netip.MustParseAddr("192.0.2.77"))}
		requireBuildLogTestLines(t, lines, want)
		if devices.starts() != 1 {
			t.Fatalf("two CONNECTs started %d constructions, want 1", devices.starts())
		}

		reopenId := setupTestProxyIdAt(time.Now().Add(-24 * time.Hour))
		seedHttpConnectTestLock(manager, reopenId, true)
		closeBuildLogTestDial(t, buildLogTestHttpDial(manager.ctx, core, reopenId, "[2001:db8:0:12::77]:41000"))
		want = append(want, buildLogTestLine(reopenId, proxyOpenReopen, proxyDevicePathHttp, netip.MustParseAddr("2001:db8:0:12::77")))
		requireBuildLogTestLines(t, lines, want)
	})
}

// The textual and binary forms of an address that a line must not carry: the
// address, its other spellings, the remote address with its port, and the
// bucket network the hash is taken over.
func buildLogTestAddressForms(remoteAddr string, addr netip.Addr) []string {
	unmapped := addr.Unmap()
	bucketBits := 56
	if unmapped.Is4() {
		bucketBits = 29
	}
	bucket := netip.PrefixFrom(unmapped, bucketBits).Masked().Addr()
	forms := []string{
		remoteAddr,
		addr.String(),
		addr.StringExpanded(),
		unmapped.String(),
		unmapped.StringExpanded(),
		bucket.String(),
		bucket.StringExpanded(),
		hex.EncodeToString(addr.AsSlice()),
		hex.EncodeToString(unmapped.AsSlice()),
		hex.EncodeToString(bucket.AsSlice()),
	}
	if unmapped.Is4() {
		mapped := netip.AddrFrom16(unmapped.As16())
		forms = append(forms, mapped.String(), mapped.StringExpanded(), hex.EncodeToString(mapped.AsSlice()))
	}
	return forms
}

// The line never carries the caller's address in any form, through any path
// that has one: an IPv4 HTTP caller, a v4-mapped HTTP caller from a dual-stack
// listener, an IPv6 SOCKS caller and an IPv4 SOCKS sign-in. A v4-mapped caller
// logs its IPv4 bucket, as the api's sign-up buckets key it.
func TestProxyDeviceBuildLineCarriesNoAddress(t *testing.T) {
	configureBuildLogTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		lines := captureBuildLogTestLines(manager)
		window := newLazyOpenTestWindow()
		window.addExit()
		newLazyOpenTestDevices(t, manager, window)
		httpCore := newHttpConnectTestProxy(manager)
		socksCore := newBuildLogTestSocksProxy(manager)

		for _, c := range []struct {
			path       string
			remoteAddr string
			// the address the bucket is the hash of
			bucketAddr netip.Addr
		}{
			{path: proxyDevicePathHttp, remoteAddr: "192.0.2.77:41000", bucketAddr: netip.MustParseAddr("192.0.2.77")},
			{path: proxyDevicePathHttp, remoteAddr: "[::ffff:198.51.100.77]:41001", bucketAddr: netip.MustParseAddr("198.51.100.77")},
			{path: proxyDevicePathSocks, remoteAddr: "[2001:db8:0:12::77]:41002", bucketAddr: netip.MustParseAddr("2001:db8:0:12::77")},
			{path: proxyDevicePathSocksSignIn, remoteAddr: "203.0.113.77:41003", bucketAddr: netip.MustParseAddr("203.0.113.77")},
		} {
			proxyId := setupTestProxyIdAt(time.Now())
			seedHttpConnectTestLock(manager, proxyId, true)
			before := len(lines.get())
			switch c.path {
			case proxyDevicePathHttp:
				closeBuildLogTestDial(t, buildLogTestHttpDial(manager.ctx, httpCore, proxyId, c.remoteAddr))
			case proxyDevicePathSocks:
				closeBuildLogTestDial(t, buildLogTestSocksDial(manager.ctx, socksCore, proxyId, c.remoteAddr))
			case proxyDevicePathSocksSignIn:
				if !socksCore.ValidUser(model.SignProxyId(proxyId), "x", c.remoteAddr) {
					t.Fatalf("the sign-in from %s was refused", c.remoteAddr)
				}
				synctest.Wait()
			}
			got := lines.get()
			if len(got) != before+1 {
				t.Fatalf("%s from %s logged %d build lines, want 1", c.path, c.remoteAddr, len(got)-before)
			}
			line := got[before]
			if want := buildLogTestLine(proxyId, proxyOpenCold, c.path, c.bucketAddr); line != want {
				t.Errorf("%s from %s logged %q, want %q", c.path, c.remoteAddr, line, want)
			}
			addrPort := netip.MustParseAddrPort(c.remoteAddr)
			for _, form := range buildLogTestAddressForms(c.remoteAddr, addrPort.Addr()) {
				if strings.Contains(strings.ToLower(line), strings.ToLower(form)) {
					t.Errorf("%s from %s logged the address as %q: %q", c.path, c.remoteAddr, form, line)
				}
			}
		}
	})
}

// Openers that join a construction already in flight log nothing, whatever
// their path: a SOCKS sign-in starts the construction and is the one line,
// and an HTTP CONNECT, a SOCKS CONNECT and an open with no caller that come
// while it holds join it.
func TestProxyDeviceBuildJoinersDoNotLog(t *testing.T) {
	configureBuildLogTest(t)
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		lines := captureBuildLogTestLines(manager)
		proxyId := setupTestProxyIdAt(time.Now())
		seedHttpConnectTestLock(manager, proxyId, true)
		window := newLazyOpenTestWindow()
		window.addExit()
		devices := newLazyOpenTestDevices(t, manager, window)
		devices.hold = make(chan struct{})
		httpCore := newHttpConnectTestProxy(manager)
		socksCore := newBuildLogTestSocksProxy(manager)

		if !socksCore.ValidUser(model.SignProxyId(proxyId), "x", "192.0.2.10:41000") {
			t.Fatal("the proxy's caller did not sign in")
		}
		<-devices.started
		results := make(chan lazyOpenTestDial, 3)
		go func() {
			results <- buildLogTestHttpDial(manager.ctx, httpCore, proxyId, "198.51.100.20:41001")
		}()
		go func() {
			results <- buildLogTestSocksDial(manager.ctx, socksCore, proxyId, "203.0.113.30:41002")
		}()
		go func() {
			_, err := manager.OpenProxyDevice(proxyId)
			results <- lazyOpenTestDial{err: err}
		}()
		synctest.Wait()
		if devices.starts() != 1 || len(results) != 0 {
			t.Fatalf("with the construction held: %d constructions, %d opens returned; want 1, 0", devices.starts(), len(results))
		}
		requireBuildLogTestLines(t, lines, nil)

		close(devices.hold)
		for range 3 {
			closeBuildLogTestDial(t, <-results)
		}
		synctest.Wait()
		if devices.starts() != 1 || len(devices.built()) != 1 {
			t.Fatalf("four opens built %d devices, want 1", len(devices.built()))
		}
		requireBuildLogTestLines(t, lines, []string{
			buildLogTestLine(proxyId, proxyOpenCold, proxyDevicePathSocksSignIn, netip.MustParseAddr("192.0.2.10")),
		})
	})
}

// Each entry point of the service names itself, and only the ones that know
// their caller carry a hash: SOCKS, WireGuard's first packet, and the control
// api's /warmup, device rpc and flow trace, the latter three through the api's
// own routes and ingress. HTTP and the SOCKS sign-in are above. The pre-warm,
// which reads its proxies from the database, is held to naming its path by
// TestProxyDeviceOpensOfTheServiceNameTheirPath.
func TestProxyDeviceBuildLineNamesItsPath(t *testing.T) {
	configureBuildLogTest(t)
	transportTls := server.NewTransportTls(map[string]bool{}, &server.TransportTlsSettings{EnableSelfSign: true, DefaultHostName: "fixture.example"})
	synctest.Test(t, func(t *testing.T) {
		manager := newHttpConnectTestManager(t)
		lines := captureBuildLogTestLines(manager)
		window := newLazyOpenTestWindow()
		window.addExit()
		newLazyOpenTestDevices(t, manager, window)
		socksCore := newBuildLogTestSocksProxy(manager)
		api := &apiServer{
			ctx:                manager.ctx,
			proxyDeviceManager: manager,
			transportTls:       transportTls,
			settings:           DefaultProxySettings(),
		}
		apiHandler, _, _ := api.httpConfiguration()
		serveApi := func(method string, target string, body string, remoteAddr string, proxyId server.Id) {
			r := httptest.NewRequest(method, target, strings.NewReader(body))
			r.RemoteAddr = remoteAddr
			r.Header.Set("Authorization", "Bearer "+model.SignProxyId(proxyId))
			apiHandler.ServeHTTP(httptest.NewRecorder(), r)
		}

		for _, c := range []struct {
			path   string
			open   func(proxyId server.Id)
			caller netip.Addr
		}{
			{
				path: proxyDevicePathSocks,
				open: func(proxyId server.Id) {
					closeBuildLogTestDial(t, buildLogTestSocksDial(manager.ctx, socksCore, proxyId, "203.0.113.40:41000"))
				},
				caller: netip.MustParseAddr("203.0.113.40"),
			},
			{
				path: proxyDevicePathWg,
				open: func(proxyId server.Id) {
					if _, err := wgTunFactory(manager, proxyId)(); err != nil {
						t.Fatalf("the first WireGuard packet's open failed: %v", err)
					}
				},
			},
			{
				path: proxyDevicePathWarmup,
				open: func(proxyId server.Id) {
					serveApi(http.MethodPost, "/warmup", "", "198.51.100.40:41001", proxyId)
				},
				caller: netip.MustParseAddr("198.51.100.40"),
			},
			{
				path: proxyDevicePathRpc,
				open: func(proxyId server.Id) {
					serveApi(http.MethodGet, deviceRpcPath, "", "198.51.100.41:41002", proxyId)
				},
			},
			{
				path: proxyDevicePathFlowTrace,
				open: func(proxyId server.Id) {
					serveApi(http.MethodPost, flowTracePath, `{"port":443}`, "198.51.100.42:41003", proxyId)
				},
			},
		} {
			proxyId := setupTestProxyIdAt(time.Now())
			seedHttpConnectTestLock(manager, proxyId, true)
			before := len(lines.get())
			c.open(proxyId)
			synctest.Wait()
			got := lines.get()
			if len(got) != before+1 {
				t.Fatalf("the %s open logged %d build lines, want 1", c.path, len(got)-before)
			}
			if want := buildLogTestLine(proxyId, proxyOpenCold, c.path, c.caller); got[before] != want {
				t.Errorf("the %s open logged %q, want %q", c.path, got[before], want)
			}
		}
	})
}

// The non-test files of this package.
func buildLogTestSourceFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]*ast.File{}
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files[path] = file
	}
	if len(files) == 0 {
		t.Fatal("no source file was read, so this pins nothing")
	}
	return files
}

// The build line took the place of the line each construction logged with the
// api and platform urls, so the log carries as many lines as before: no line
// of the package logs that one any more.
func TestProxyDeviceBuildLineReplacesTheUrlLine(t *testing.T) {
	literalCount := 0
	for path, file := range buildLogTestSourceFiles(t) {
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			literalCount += 1
			if strings.Contains(literal.Value, "using api=") {
				t.Errorf("%s still logs the url line: %s", path, literal.Value)
			}
			return true
		})
	}
	if literalCount == 0 {
		t.Fatal("no string was read, so this pins nothing")
	}
}

// Every open the service makes names its entry point: no source of the
// package opens through OpenProxyDevice, whose line names no caller, and every
// OpenProxyDeviceWithContext passes a context that carries its caller. A
// path added later is held to the same rule.
func TestProxyDeviceOpensOfTheServiceNameTheirPath(t *testing.T) {
	openCount := 0
	for path, file := range buildLogTestSourceFiles(t) {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch selector.Sel.Name {
			case "OpenProxyDevice":
				t.Errorf("%s opens a device through OpenProxyDevice, which names no caller", path)
			case "OpenProxyDeviceWithContext":
				openCount += 1
				namesCaller := false
				if 0 < len(call.Args) {
					if callerCall, ok := call.Args[0].(*ast.CallExpr); ok {
						function, ok := callerCall.Fun.(*ast.Ident)
						namesCaller = ok && function.Name == "withProxyDeviceCaller"
					}
				}
				if !namesCaller {
					t.Errorf("%s opens a device with a context that names no caller", path)
				}
			case "openProxyDeviceFor", "startOpenProxyDevice":
				openCount += 1
			}
			return true
		})
	}
	if openCount == 0 {
		t.Fatal("no open was read, so this pins nothing")
	}
}
