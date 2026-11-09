// Package mxedgehost is server/cli/mxedge's host (MAIL-M5-04; §15.4, §15.15, §19.4, D-MAIL-1, D-MAIL-10,
// I-12, I-13): sidecar's mxedgeservice engine on TCP 25, run by sidecarhost.Run as one warp block, and what a
// T3 test starts it through. The engine's two roles are two warp services on different lb interfaces (W4),
// since an lb stream port maps to one service and one upstream holds every block of it
// (plan-OPS-small-readings #6): one binary, server/cli/mxedge, whose --role picks the role, and an image
// directory for each service that builds it and picks its role.
//
//   - platform, the mxedge-platform service on rpt.bringyour.com's two interfaces: §15.4's platform mail, the
//     only place a URnetwork server ends inbound TLS (mx-4). This block alone holds the platform-mail key: its
//     bringyour.com certificate and key come from the block's secret_files (HostCertificateFileName,
//     HostKeyFileName), and the block is ready only with a pair that loads, names the report host and is
//     inside its validity. It holds a Postgres pool for §14.8's intake, which runs in-process here
//     (plan-SMTP-report-intake-home) and which SMTP-M5-06 builds and hands in as HostDeps.Intake; until then
//     the listener has no intake, and every recipient it answers for gets 451 4.3.0, so a sender retries and
//     nothing is read.
//   - router, the mxedge service on every other lb interface, is MAIL-M7-19's: RunHost refuses it until then.
//
// What the roles share is here: mxedge.yml's settings, the engine's MxEdgeDeps with tlsedgeservice's hello
// functions (TLS reading 15: §4.4 keeps one engine from importing another, and server may import both),
// §19.11's mxedge gate from sidecar.yml, which the host rereads every 60 s, and the TCP 25 listener behind the
// lb's PROXY header. That listener is tlsedgeservice.NewProxyHeaderListener as RunOptions' Wrap, so the
// engine's RemoteAddr is the sender, and a connection that does not open with a header is dropped unanswered:
// only the lb reaches this port.
//
// Nothing here logs an address, an EHLO name, an SNI, a recipient or a byte of a message (I-13): a log line
// names a role, a file or a reason, and the engine's own lines follow the same rule.
package mxedgehost

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/tlsedgeservice"

	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The roles, as --role names them.
const (
	HostRolePlatform = "platform"
	HostRoleRouter   = "router"
)

// What the platform role reads from its block's secret_files, in warp's .pem and .key pair form. The design
// names no file; these are the plan's (proposed), and OPS-M5-03's fragment mounts them on mxedge-platform alone.
const (
	// The bringyour.com certificate chain, leaf first, PEM.
	HostCertificateFileName = "platform-mail.pem"
	// Its private key, PEM: §19.4's platform-mail key.
	HostKeyFileName = "platform-mail.key"
)

// The platform block as urnetwork_<svc>_* and its log name it: the service's name in lowercase letters and
// digits, as sidecarhost.Run asks. The engine's own families stay urnetwork_mxedge_*.
const hostPlatformService = "mxedgeplatform"

// What a role is given.
type HostSettings struct {
	// mxedge.yml. No default: its names are production names, which the file sets.
	MxEdge *mxedgeservice.MxEdgeSettings
	// Where the block's secret_files mount: sidecarhost.ScopedVaultHome in a block.
	SecretDirectory string
	// The lb's PROXY header on the TCP 25 port: §13.5's bounds, tlsedge's. Its Clock is replaced by the host's.
	ProxyHeader *tlsedgeservice.ProxyHeaderSettings
}

// The defaults, with no mxedge.yml.
func DefaultHostSettings() *HostSettings {
	return &HostSettings{
		SecretDirectory: sidecarhost.ScopedVaultHome,
		ProxyHeader:     tlsedgeservice.DefaultProxyHeaderSettings(),
	}
}

// The seams a test, or the change that lands a missing part, fills. Each nil takes the block's own, or, where
// server has none yet, leaves that part closed.
type HostDeps struct {
	// platform: SMTP-M5-05's SmtpReportIntake as the listener reaches it, which SMTP-M5-06 builds over this
	// block's Postgres pool. nil: none yet, so every recipient the listener answers for gets 451 4.3.0.
	Intake mxedgeservice.PlatformIntake
	// platform: the pending groups whose tables Intake reads (§19.7), so the block is ready only with them.
	IntakeGroups []*sidecarhost.RunGroup
	// platform: the certificate and key. nil: the block's own, HostCertificateFileName and HostKeyFileName in
	// HostSettings.SecretDirectory.
	Certificate *tls.Certificate
	// §19.11's gates. nil: server's reader of config/<env>/sidecar.yml.
	Gates sidecar.ServiceGates
	// The engine's and the PROXY listener's clock. nil: real time.
	Clock sidecar.Clock
	// The engine's log. nil: glog.
	Log sidecar.Logger
	// The block's /status latch, so a test reads it. nil: sidecarhost.Run's own.
	WarpStatus *router.WarpStatusState
}

// The ports a block serves on, as warp maps them to the host's (WARP_PORTS).
type HostPorts struct {
	// GET /status's: 80 in a block.
	Status int
	// TCP 25's, behind the lb's PROXY header: 25 in a block.
	Smtp int
}

// The engine's MxEdgeDeps on host: tlsedgeservice's hello functions, §19.11's mxedge gate as host's gates
// answer it at each call, host's clock, and log, nil being glog. A platform session names no network, so the
// gate it reads is the service's alone.
func NewEngineDeps(host *sidecar.ServiceHost, log sidecar.Logger) *mxedgeservice.MxEdgeDeps {
	if log == nil {
		log = hostLogger{}
	}
	gates := host.GatesOrClosed()
	return &mxedgeservice.MxEdgeDeps{
		ReadClientHello: func(conn net.Conn, maxByteCount int) (*mxedgeservice.MxClientHello, error) {
			hello, err := tlsedgeservice.ReadClientHello(conn, maxByteCount)
			if err != nil {
				return nil, err
			}
			return &mxedgeservice.MxClientHello{ServerName: hello.ServerName, RawBytes: hello.RawBytes}, nil
		},
		NormalizeSni: tlsedgeservice.NormalizeSni,
		// the engine's three alerts are RFC 8446's codes, as tlsedgeservice's are
		WriteTlsAlert: func(conn net.Conn, alert mxedgeservice.MxTlsAlert) error {
			return tlsedgeservice.WriteTlsAlert(conn, tlsedgeservice.TlsAlertCode(alert))
		},
		GateOpen: func() bool {
			return gates.Service(sidecar.ServiceGateMxedge).Enabled()
		},
		Clock: host.ClockOrReal(),
		Log:   log,
	}
}

// The platform certificate from directory: HostCertificateFileName's chain with HostKeyFileName's key. Refuses
// a pair that does not load or match, and a leaf that does not name reportHostName, so a block mounting the
// wrong certificate is never ready rather than presenting it. Validity is the caller's to check against its
// clock. No error holds a byte of either file.
func LoadPlatformCertificate(directory string, reportHostName string) (*tls.Certificate, error) {
	read := func(name string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		return data, nil
	}
	chainPem, err := read(HostCertificateFileName)
	if err != nil {
		return nil, err
	}
	keyPem, err := read(HostKeyFileName)
	if err != nil {
		return nil, err
	}
	certificate, err := tls.X509KeyPair(chainPem, keyPem)
	if err != nil {
		return nil, fmt.Errorf("%s and %s are not one certificate and its key: %w", HostCertificateFileName, HostKeyFileName, err)
	}
	if certificate.Leaf == nil {
		leaf, err := x509.ParseCertificate(certificate.Certificate[0])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", HostCertificateFileName, err)
		}
		certificate.Leaf = leaf
	}
	if err := certificate.Leaf.VerifyHostname(reportHostName); err != nil {
		return nil, fmt.Errorf("%s does not name the report host", HostCertificateFileName)
	}
	return &certificate, nil
}

// The platform role: mxedgeservice's platform listener, ready only while its certificate is inside its
// validity. Safe for concurrent use.
type PlatformHost struct {
	engine    *mxedgeservice.MxEdgeService
	clock     sidecar.Clock
	notBefore time.Time
	notAfter  time.Time
	// what the block built for the engine, ended after it
	closeAfter func()
	closeOnce  sync.Once
}

var _ sidecar.StreamEngine = (*PlatformHost)(nil)

// The platform role on host, which needs Redis and Metrics, with settings' mxedge.yml and deps' intake and
// certificate, or the block's own certificate. A certificate that does not load, a host without what the
// engine needs or settings ValidatePlatform refuses is an error, and the block then serves /status not ready.
// ctx bounds the engine and should outlive the drain: RunHost gives one that ends only at Close.
func NewPlatformHost(ctx context.Context, host *sidecar.ServiceHost, settings *HostSettings, deps *HostDeps) (*PlatformHost, error) {
	if settings == nil || settings.MxEdge == nil {
		return nil, errors.New("mxedge platform: no mxedge.yml")
	}
	if deps == nil {
		deps = &HostDeps{}
	}
	if err := settings.MxEdge.ValidatePlatform(); err != nil {
		return nil, err
	}
	certificate := deps.Certificate
	if certificate == nil {
		var err error
		if certificate, err = LoadPlatformCertificate(settings.SecretDirectory, settings.MxEdge.PlatformReportHostName); err != nil {
			return nil, err
		}
	}
	leaf := certificate.Leaf
	if leaf == nil {
		var err error
		if leaf, err = x509.ParseCertificate(certificate.Certificate[0]); err != nil {
			return nil, fmt.Errorf("mxedge platform: the certificate: %w", err)
		}
	}
	engine, err := mxedgeservice.NewMxEdgePlatformService(ctx, host, NewEngineDeps(host, deps.Log), &mxedgeservice.MxEdgePlatformDeps{
		Certificate: certificate,
		Intake:      deps.Intake,
	}, settings.MxEdge)
	if err != nil {
		return nil, err
	}
	self := &PlatformHost{
		engine:    engine,
		clock:     host.ClockOrReal(),
		notBefore: leaf.NotBefore,
		notAfter:  leaf.NotAfter,
	}
	if !self.certificateCurrent() {
		glog.Errorf("[%s]the platform certificate is outside its validity, so the block is not ready until a current one is mounted\n", hostPlatformService)
	}
	if deps.Intake == nil {
		glog.Errorf("[%s]no report intake yet (SMTP-M5-06): every recipient the listener answers for gets 451 4.3.0\n", hostPlatformService)
	}
	return self, nil
}

// The engine, for a test that drives it.
func (self *PlatformHost) Engine() *mxedgeservice.MxEdgeService {
	return self.engine
}

// Whether now is inside the certificate's validity.
func (self *PlatformHost) certificateCurrent() bool {
	now := self.clock.Now()
	return !now.Before(self.notBefore) && !now.After(self.notAfter)
}

// The certificate current and the engine ready: a probe has read today's closed-handshake key, and the block
// is not draining.
func (self *PlatformHost) Ready() bool {
	return self.certificateCurrent() && self.engine.Ready()
}

// See sidecar.Engine.
func (self *PlatformHost) Drain() {
	self.engine.Drain()
}

// Ends the engine, then what the block built for it. Safe after Drain, and more than once.
func (self *PlatformHost) Close() {
	self.closeOnce.Do(func() {
		self.engine.Close()
		if self.closeAfter != nil {
			self.closeAfter()
		}
	})
}

// See sidecar.StreamEngine.
func (self *PlatformHost) Serve(listener net.Listener) error {
	return self.engine.Serve(listener)
}

// A role that could not start: never ready, and serving nothing.
type notReadyStreamEngine struct{}

func (notReadyStreamEngine) Ready() bool { return false }
func (notReadyStreamEngine) Drain()      {}
func (notReadyStreamEngine) Close()      {}

func (notReadyStreamEngine) Serve(listener net.Listener) error {
	return listener.Close()
}

// A connection the PROXY listener took, whose socket NetConn reaches, so the engine's reset of a connection
// that sent bytes behind STARTTLS (§15.4) sets the socket's linger and closes with RST, not FIN. RemoteAddr
// and every read stay the PROXY connection's: NetConn is for that reset alone.
type hostSenderConn struct {
	*tlsedgeservice.ProxyHeaderConn
}

// The socket under the PROXY header.
func (self *hostSenderConn) NetConn() net.Conn {
	return self.ProxyHeaderConn.Conn
}

// The PROXY listener, its connections as hostSenderConn.
type hostSenderListener struct {
	net.Listener
}

// See net.Listener.
func (self *hostSenderListener) Accept() (net.Conn, error) {
	conn, err := self.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if proxyConn, ok := conn.(*tlsedgeservice.ProxyHeaderConn); ok {
		return &hostSenderConn{ProxyHeaderConn: proxyConn}, nil
	}
	return conn, nil
}

// RunListener's Wrap for TCP 25: the lb's PROXY header off each connection before the engine sees it, so its
// RemoteAddr is the sender's, and one with no header dropped, unanswered, by the listener. settings is copied,
// its Clock replaced by clock. Ending ctx closes the listener.
func NewProxyListenerWrap(ctx context.Context, settings *tlsedgeservice.ProxyHeaderSettings, clock sidecar.Clock) func(listener net.Listener) (net.Listener, error) {
	proxySettings := *settings
	proxySettings.Clock = clock
	return func(listener net.Listener) (net.Listener, error) {
		proxyListener, err := tlsedgeservice.NewProxyHeaderListener(ctx, listener, &proxySettings)
		if err != nil {
			return nil, err
		}
		return &hostSenderListener{Listener: proxyListener}, nil
	}
}

// What sidecarhost.Run takes for the platform block: TCP 25 behind the PROXY wrap, the intake's Postgres pool
// (plan-SMTP-report-intake-home) and its groups. ctx bounds the PROXY listener.
func platformRunOptions(ctx context.Context, engine sidecar.StreamEngine, settings *HostSettings, deps *HostDeps, ports *HostPorts) *sidecarhost.RunOptions {
	clock := deps.Clock
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	proxySettings := settings.ProxyHeader
	if proxySettings == nil {
		proxySettings = tlsedgeservice.DefaultProxyHeaderSettings()
	}
	return &sidecarhost.RunOptions{
		Service:    hostPlatformService,
		StatusPort: ports.Status,
		Engine:     engine,
		Stream:     engine,
		Listeners: []*sidecarhost.RunListener{{
			Port: ports.Smtp,
			Wrap: NewProxyListenerWrap(ctx, proxySettings, clock),
		}},
		Postgres:   true,
		Groups:     deps.IntakeGroups,
		WarpStatus: deps.WarpStatus,
	}
}

// The platform block's engine on its own stores and deps', or a never-ready one when it cannot start, having
// said why. Everything is built on a context of its own that the engine's Close ends, so ctx's end, a block's
// SIGTERM, starts the drain without ending what the drain needs.
func newPlatformBlockEngine(ctx context.Context, settings *HostSettings, deps *HostDeps) sidecar.StreamEngine {
	hostCtx, hostCancel := context.WithCancel(context.WithoutCancel(ctx))
	host := sidecarhost.NewServiceHost(hostCtx, &sidecarhost.ServiceHostOptions{
		Postgres: true,
		Clock:    deps.Clock,
	})
	if deps.Gates != nil {
		host.Gates = deps.Gates
	}
	platformHost, err := NewPlatformHost(hostCtx, host, settings, deps)
	if err != nil {
		hostCancel()
		glog.Errorf("[%s]the platform role is not ready: %v\n", hostPlatformService, err)
		return notReadyStreamEngine{}
	}
	platformHost.closeAfter = hostCancel
	return platformHost
}

// Runs role as a block until ctx ends, through sidecarhost.Run (§19.4): GET /status on ports.Status from the
// first moment, TCP 25 on ports.Smtp behind the PROXY wrap once ready, then at ctx's end the drain and Close.
// A role that cannot start logs why and serves /status not ready. Returns early for the router role, which
// MAIL-M7-19 builds, a role that is neither, or options no block could run. settings must hold mxedge.yml;
// deps may be nil.
func RunHost(ctx context.Context, role string, settings *HostSettings, deps *HostDeps, ports *HostPorts) error {
	if deps == nil {
		deps = &HostDeps{}
	}
	if settings == nil {
		settings = DefaultHostSettings()
	}
	switch role {
	case HostRolePlatform:
	case HostRoleRouter:
		return errors.New("mxedge host: the router role is MAIL-M7-19's and not built yet")
	default:
		return fmt.Errorf("mxedge host: role %q is neither %s nor %s", role, HostRolePlatform, HostRoleRouter)
	}
	engine := newPlatformBlockEngine(ctx, settings, deps)
	return sidecarhost.Run(ctx, platformRunOptions(ctx, engine, settings, deps, ports))
}

// glog as the engine's sidecar.Logger. The engine's lines name no address (I-13), and neither does a caller
// here.
type hostLogger struct{}

func (self hostLogger) Info(args ...any) {
	glog.Info(args...)
}

func (self hostLogger) Infof(format string, args ...any) {
	glog.Infof(format, args...)
}

func (self hostLogger) Warningf(format string, args ...any) {
	glog.Warningf(format, args...)
}

func (self hostLogger) Errorf(format string, args ...any) {
	glog.Errorf(format, args...)
}

func (self hostLogger) V(level int32) sidecar.Verbose {
	return hostVerbose(glog.V(glog.Level(level)))
}

// glog's verbose level as the engine's sidecar.Verbose.
type hostVerbose glog.Verbose

func (self hostVerbose) Enabled() bool {
	return bool(self)
}

func (self hostVerbose) Info(args ...any) {
	glog.Verbose(self).Info(args...)
}

func (self hostVerbose) Infof(format string, args ...any) {
	glog.Verbose(self).Infof(format, args...)
}
