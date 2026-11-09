package mxedgehost

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/router"
	"github.com/urnetwork/server/sidecarhost"
)

// The names and documentation addresses a test uses, synthetic (connect/CODESTYLE.md's test data rule).
const (
	hostTestBannerHostName = "mx.report.example"
	hostTestReportHostName = "rpt.report.example"
	hostTestRootDomainName = "root.example"
	hostTestAbuseRecipient = "abuse@" + hostTestRootDomainName
	hostTestRootRecipient  = "root@" + hostTestReportHostName
	hostTestSender         = "203.0.113.9"
	hostTestEhloName       = "relay.sender.example"
	hostTestLbAddress      = "192.0.2.25"
	hostTestNetDomain      = "quietnet.root.example"
	// The block's ports as warp names them, which WARP_PORTS maps to loopback ports of the test's.
	hostTestStatusPort = 80
	hostTestSmtpPort   = 25
	// How long a step may wait before the test fails: a bound on a hang, never a pause.
	hostTestStepTimeout = 30 * time.Second
	// The engine's probe interval in a test, apart from every other timer it sets, so a test sees the first
	// probe land by its timer alone.
	hostTestProbeInterval = 7 * time.Second
)

// mxedge.yml for the test's names, its probe on hostTestProbeInterval.
func hostTestMxEdgeSettings() *mxedgeservice.MxEdgeSettings {
	settings := mxedgeservice.DefaultMxEdgeSettings()
	settings.BannerHostName = hostTestBannerHostName
	settings.DeviationUrl = "https://deviation.example/rfc3207"
	settings.PlatformReportHostName = hostTestReportHostName
	settings.PlatformRootDomainName = hostTestRootDomainName
	settings.PlatformAbuseUrl = "https://abuse.example/form"
	settings.ReadyProbeInterval = hostTestProbeInterval
	return settings
}

// A self-signed certificate for names, valid from notBefore to notAfter, as PEM: its chain and its key.
func hostTestCertificatePem(t testing.TB, names []string, notBefore time.Time, notAfter time.Time) (chainPem []byte, keyPem []byte) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(25),
		Subject:      pkix.Name{CommonName: names[0]},
		DNSNames:     names,
		NotBefore:    notBefore,
		NotAfter:     notAfter,
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateBytes}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyBytes})
}

// A secret directory holding the files given.
func hostTestSecrets(t testing.TB, files map[string][]byte) string {
	t.Helper()
	directory := t.TempDir()
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return directory
}

// A secret directory with a certificate for the report host, valid around start.
func hostTestPlatformSecrets(t testing.TB, start time.Time) string {
	t.Helper()
	chainPem, keyPem := hostTestCertificatePem(t, []string{hostTestReportHostName}, start.Add(-time.Hour), start.Add(24*time.Hour))
	return hostTestSecrets(t, map[string][]byte{HostCertificateFileName: chainPem, HostKeyFileName: keyPem})
}

// Waits until a timer is set on clock for at, so a step moves the clock only once the wait it means is armed.
func awaitHostTestTimerAt(t testing.TB, ctx context.Context, clock *sidecartest.FakeClock, at time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, hostTestStepTimeout)
	defer cancel()
	for {
		pending, added := clock.WatchTimers()
		if slices.ContainsFunc(pending, at.Equal) {
			return
		}
		select {
		case <-added:
		case <-ctx.Done():
			t.Fatalf("no timer was set for %v", at)
		}
	}
}

// The host's engine deps reach tlsedgeservice's functions: a real ClientHello's name and bytes, the routing
// key, each alert on the wire as RFC 8446 writes it, and the gate as host's gates answer it.
func TestEngineDepsReachTlsedgeFunctions(t *testing.T) {
	fakeHost := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil)
	deps := NewEngineDeps(fakeHost.ServiceHost(), nil)

	for _, serverName := range []string{"mx.net.example", ""} {
		serverConn, clientConn := net.Pipe()
		go func() {
			tls.Client(clientConn, &tls.Config{ServerName: serverName, InsecureSkipVerify: true}).Handshake()
		}()
		hello, err := deps.ReadClientHello(serverConn, 16*1024)
		serverConn.Close()
		clientConn.Close()
		if err != nil {
			t.Fatalf("hello naming %q: %v", serverName, err)
		}
		if hello.ServerName != serverName || len(hello.RawBytes) < 5 || hello.RawBytes[0] != 22 {
			t.Fatalf("hello naming %q read as %q with %d bytes", serverName, hello.ServerName, len(hello.RawBytes))
		}
	}
	serverConn, clientConn := net.Pipe()
	go func() {
		clientConn.Write([]byte("EHLO relay.sender.example\r\n"))
	}()
	if _, err := deps.ReadClientHello(serverConn, 16*1024); err == nil {
		t.Fatal("a line of SMTP read as a hello")
	}
	serverConn.Close()
	clientConn.Close()

	if name, ok := deps.NormalizeSni("MX.Net.Example."); !ok || name != "mx.net.example" {
		t.Fatalf("normalized %q, %v", name, ok)
	}
	if _, ok := deps.NormalizeSni("mx_net.example"); ok {
		t.Fatal("an underscore was a routing key")
	}

	for _, alert := range []mxedgeservice.MxTlsAlert{mxedgeservice.MxTlsAlertDecodeError, mxedgeservice.MxTlsAlertInternalError, mxedgeservice.MxTlsAlertUnrecognizedName} {
		serverConn, clientConn := net.Pipe()
		go func() {
			deps.WriteTlsAlert(serverConn, alert)
			serverConn.Close()
		}()
		written, err := io.ReadAll(clientConn)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(written, []byte{21, 3, 3, 0, 2, 2, byte(alert)}) {
			t.Fatalf("alert %d wrote %x", alert, written)
		}
	}

	if deps.GateOpen() {
		t.Fatal("a closed gate read open")
	}
	fakeHost.Gates.OpenService(sidecar.ServiceGateMxedge)
	if !deps.GateOpen() {
		t.Fatal("an open gate read closed")
	}
	if NewEngineDeps(&sidecar.ServiceHost{}, nil).GateOpen() {
		t.Fatal("a host with no gates read open")
	}
}

// Only a pair that loads, matches and names the report host is the platform's certificate, and no refusal
// quotes either file.
func TestLoadPlatformCertificateRefusesWhatCannotServe(t *testing.T) {
	now := time.Now()
	chainPem, keyPem := hostTestCertificatePem(t, []string{hostTestReportHostName}, now.Add(-time.Hour), now.Add(time.Hour))
	_, otherKeyPem := hostTestCertificatePem(t, []string{hostTestReportHostName}, now.Add(-time.Hour), now.Add(time.Hour))
	otherChainPem, otherNameKeyPem := hostTestCertificatePem(t, []string{"other.example"}, now.Add(-time.Hour), now.Add(time.Hour))
	wildcardChainPem, wildcardKeyPem := hostTestCertificatePem(t, []string{"*.report.example"}, now.Add(-time.Hour), now.Add(time.Hour))
	cases := []struct {
		name  string
		files map[string][]byte
		ok    bool
	}{
		{name: "no files", files: map[string][]byte{}},
		{name: "no key", files: map[string][]byte{HostCertificateFileName: chainPem}},
		{name: "no certificate", files: map[string][]byte{HostKeyFileName: keyPem}},
		{name: "another key", files: map[string][]byte{HostCertificateFileName: chainPem, HostKeyFileName: otherKeyPem}},
		{name: "another name", files: map[string][]byte{HostCertificateFileName: otherChainPem, HostKeyFileName: otherNameKeyPem}},
		{name: "not pem", files: map[string][]byte{HostCertificateFileName: []byte("not a certificate"), HostKeyFileName: keyPem}},
		{name: "the report host", files: map[string][]byte{HostCertificateFileName: chainPem, HostKeyFileName: keyPem}, ok: true},
		{name: "a wildcard over it", files: map[string][]byte{HostCertificateFileName: wildcardChainPem, HostKeyFileName: wildcardKeyPem}, ok: true},
	}
	for _, c := range cases {
		certificate, err := LoadPlatformCertificate(hostTestSecrets(t, c.files), hostTestReportHostName)
		if c.ok {
			if err != nil || certificate == nil || certificate.Leaf == nil || certificate.Leaf.VerifyHostname(hostTestReportHostName) != nil {
				t.Errorf("%s: %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: loaded", c.name)
			continue
		}
		if strings.Contains(err.Error(), "BEGIN") || strings.Contains(err.Error(), string(keyPem[30:60])) {
			t.Errorf("%s: the refusal quotes a file: %v", c.name, err)
		}
	}
}

// A certificate outside its validity is no certificate: the role is ready only while the clock is inside it,
// once a probe has read today's key, and stops being ready when the clock passes its end.
func TestPlatformHostReadyOnlyInsideItsCertificateWindow(t *testing.T) {
	start := sidecartest.DefaultClockStart()
	windows := []struct {
		name      string
		notBefore time.Time
		notAfter  time.Time
		ready     bool
	}{
		{name: "not yet valid", notBefore: start.Add(time.Hour), notAfter: start.Add(2 * time.Hour)},
		{name: "expired", notBefore: start.Add(-2 * time.Hour), notAfter: start.Add(-time.Hour)},
		{name: "current", notBefore: start.Add(-time.Hour), notAfter: start.Add(time.Hour), ready: true},
	}
	for _, window := range windows {
		ctx, cancel := context.WithCancel(t.Context())
		clock := sidecartest.NewFakeClock(start)
		fakeHost := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
		chainPem, keyPem := hostTestCertificatePem(t, []string{hostTestReportHostName}, window.notBefore, window.notAfter)
		settings := DefaultHostSettings()
		settings.MxEdge = hostTestMxEdgeSettings()
		settings.SecretDirectory = hostTestSecrets(t, map[string][]byte{HostCertificateFileName: chainPem, HostKeyFileName: keyPem})
		platformHost, err := NewPlatformHost(ctx, fakeHost.ServiceHost(), settings, &HostDeps{Clock: clock})
		if err != nil {
			t.Fatalf("%s: %v", window.name, err)
		}
		// the first probe has read today's key once its next wait is armed
		awaitHostTestTimerAt(t, ctx, clock, start.Add(hostTestProbeInterval))
		if platformHost.Ready() != window.ready {
			t.Fatalf("%s: ready %t", window.name, platformHost.Ready())
		}
		if window.ready {
			clock.AdvanceTo(window.notAfter.Add(time.Second))
			if platformHost.Ready() {
				t.Fatalf("%s: ready past the certificate's end", window.name)
			}
		}
		platformHost.Close()
		cancel()
	}
}

// A block whose secret_files hold no platform certificate cannot start: its engine is never ready and serves
// nothing, so /status says not ready and a deploy reverts.
func TestBlockWithNoCertificateIsNeverReady(t *testing.T) {
	home := t.TempDir()
	t.Setenv("WARP_VAULT_HOME", filepath.Join(home, "vault"))
	t.Setenv("WARP_CONFIG_HOME", filepath.Join(home, "config"))
	settings := DefaultHostSettings()
	settings.MxEdge = hostTestMxEdgeSettings()
	settings.SecretDirectory = hostTestSecrets(t, map[string][]byte{})
	deps := &HostDeps{Gates: sidecartest.NewFakeServiceGates(nil)}
	engine := newPlatformBlockEngine(t.Context(), settings, deps)
	defer engine.Close()
	if _, ok := engine.(notReadyStreamEngine); !ok {
		t.Fatalf("a block with no certificate built %T", engine)
	}
	if engine.Ready() {
		t.Fatal("a block with no certificate is ready")
	}
	options := platformRunOptions(t.Context(), engine, settings, deps, &HostPorts{Status: hostTestStatusPort, Smtp: hostTestSmtpPort})
	if err := options.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The platform block as sidecarhost.Run takes it: TCP 25 behind the PROXY wrap, the report intake's Postgres
// pool (plan-SMTP-report-intake-home) and its groups, and its own metric prefix beside the engine's.
func TestPlatformRunOptionsHoldPostgresAndTheProxyListener(t *testing.T) {
	groups := []*sidecarhost.RunGroup{{Name: "MG5", Linked: server.SidecarMg5Linked}}
	options := platformRunOptions(t.Context(), notReadyStreamEngine{}, DefaultHostSettings(), &HostDeps{IntakeGroups: groups}, &HostPorts{Status: hostTestStatusPort, Smtp: hostTestSmtpPort})
	if err := options.Validate(); err != nil {
		t.Fatal(err)
	}
	if options.Service != "mxedgeplatform" || options.StatusPort != hostTestStatusPort || !options.Postgres || !slices.Equal(options.Groups, groups) {
		t.Fatalf("options %+v", options)
	}
	if options.Stream == nil || options.Http != nil || len(options.Listeners) != 1 || options.Listeners[0].Port != hostTestSmtpPort || options.Listeners[0].Wrap == nil {
		t.Fatalf("listeners %+v", options.Listeners)
	}
}

// Only the platform role is built: the router is MAIL-M7-19's, and any other role is refused before anything
// is bound.
func TestRunHostRefusesRolesItDoesNotRun(t *testing.T) {
	for _, role := range []string{HostRoleRouter, "", "edge"} {
		if err := RunHost(t.Context(), role, nil, nil, &HostPorts{Status: hostTestStatusPort, Smtp: hostTestSmtpPort}); err == nil {
			t.Fatalf("role %q ran", role)
		}
	}
}

// A loopback listener under the block's PROXY wrap.
func newHostTestProxyListener(t testing.TB, ctx context.Context) net.Listener {
	t.Helper()
	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := NewProxyListenerWrap(ctx, DefaultHostSettings().ProxyHeader, sidecar.RealClock{})(inner)
	if err != nil {
		inner.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

// The wrap takes the lb's PROXY header off before the engine sees a connection, so RemoteAddr is the sender's
// and the socket under it is reachable for the engine's reset, and drops a connection that opens with anything
// else, unanswered and never accepted.
func TestProxyListenerWrapTakesOnlyTheLbsConnections(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), hostTestStepTimeout)
	defer cancel()
	listener := newHostTestProxyListener(t, ctx)
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			accepted <- conn
		}
	}()

	bare, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer bare.Close()
	if _, err := bare.Write([]byte("EHLO " + hostTestEhloName + "\r\n")); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(bare); 0 < len(rest) {
		t.Fatalf("a connection with no PROXY header read %q", rest)
	}

	proxied, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer proxied.Close()
	if _, err := fmt.Fprintf(proxied, "PROXY TCP4 %s %s 49152 25\r\nEHLO", hostTestSender, hostTestLbAddress); err != nil {
		t.Fatal(err)
	}
	var conn net.Conn
	select {
	case conn = <-accepted:
	case <-ctx.Done():
		t.Fatal("the PROXY connection was never accepted")
	}
	defer conn.Close()
	if sender := conn.RemoteAddr().(*net.TCPAddr); sender.IP.String() != hostTestSender {
		t.Fatalf("RemoteAddr %v", sender)
	}
	socket, ok := conn.(interface{ NetConn() net.Conn })
	if !ok {
		t.Fatalf("%T reaches no socket", conn)
	}
	if _, ok := socket.NetConn().(*net.TCPConn); !ok {
		t.Fatalf("the socket is %T", socket.NetConn())
	}
	// bytes behind the header are the sender's, first
	read := make([]byte, 4)
	if _, err := io.ReadFull(conn, read); err != nil || string(read) != "EHLO" {
		t.Fatalf("read %q, %v", read, err)
	}
	select {
	case extra := <-accepted:
		t.Fatalf("a second connection %v was accepted", extra.RemoteAddr())
	default:
	}
}

// SMTP-M5-05's intake as the platform listener reaches it, for the joint run alone: what SMTP-M5-06's adapter
// does, written here so this test needs no part of that task. An outcome that kept nothing is unread, which the
// listener refuses only for a role recipient (plan-ABUSE-auto-reply).
type hostTestReportIntake struct {
	intake *smtpedgeservice.SmtpReportIntake
}

func (self *hostTestReportIntake) Accept(ctx context.Context, message *mxedgeservice.PlatformMessage) (mxedgeservice.PlatformOutcome, error) {
	outcome, err := self.intake.Accept(ctx, &smtpedgeservice.SmtpReportMessage{
		Recipient:   message.Recipient,
		ArrivalTime: message.ArrivalTime,
		Raw:         message.Raw,
	})
	if err != nil {
		return 0, err
	}
	switch outcome {
	case smtpedgeservice.SmtpReportOutcomeCase, smtpedgeservice.SmtpReportOutcomeComplaint,
		smtpedgeservice.SmtpReportOutcomeCounted, smtpedgeservice.SmtpReportOutcomeDuplicate,
		smtpedgeservice.SmtpReportOutcomeEmpty:
		return mxedgeservice.PlatformOutcomeTaken, nil
	}
	return mxedgeservice.PlatformOutcomeUnread, nil
}

// Metrics whose ready gauge says each value it is set to, so a test waits for the block's latch.
type hostTestMetrics struct {
	sidecar.Metrics
	readyName string
	ready     chan float64
}

func (self *hostTestMetrics) Gauge(name string, help string, labelNames ...string) sidecar.MetricsGauge {
	gauge := self.Metrics.Gauge(name, help, labelNames...)
	if name != self.readyName {
		return gauge
	}
	return &hostTestGauge{inner: gauge, values: self.ready}
}

type hostTestGauge struct {
	inner  sidecar.MetricsGauge
	values chan float64
}

func (self *hostTestGauge) Set(value float64, labelValues ...string) {
	self.inner.Set(value, labelValues...)
	self.values <- value
}

// A loopback port free a moment ago, for WARP_PORTS to map a block's port to.
func hostTestFreePort(t testing.TB) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().(*net.TCPAddr).Port
}

// A person's mail to a role address enclosing a message of fromDomain.
func hostTestRoleMail(fromDomain string, subject string, bodyText string) []byte {
	enclosed := (&sidecartest.SmtpFixtureMessage{FromDomain: fromDomain, MessageId: "<m2@" + fromDomain + ">", Subject: subject, Body: bodyText}).Bytes()
	return []byte("From: A Person <person@person.example>\r\nTo: " + hostTestAbuseRecipient + "\r\nSubject: spam from your network\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + sidecartest.SmtpFixtureBoundary + "\"\r\n\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: text/plain\r\n\r\nPlease look at the attached.\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: message/rfc822\r\n\r\n" + string(enclosed) + "\r\n--" + sidecartest.SmtpFixtureBoundary + "--\r\n")
}

// An ARF report from a feedback loop's key about an enclosed message of fromDomain, DKIM-signed.
func hostTestArfReport(key *sidecartest.SmtpDkimFixtureKey, fromDomain string, subject string, bodyText string) []byte {
	enclosed := (&sidecartest.SmtpFixtureMessage{
		FromDomain: fromDomain,
		MessageId:  "<m1@" + fromDomain + ">",
		XUrRef:     "1.abcdefghijklmnopqrstuvwxyz234567ab",
		Subject:    subject,
		Body:       bodyText,
	}).Bytes()
	report := sidecartest.BuildSmtpArfReport(key.Domain, "abuse", enclosed, false)
	return sidecartest.SignSmtpDkimFixture(report, key, &sidecartest.SmtpDkimFixtureOptions{
		HeaderRelaxed: true,
		BodyRelaxed:   true,
		Headers:       []string{"from", "to", "subject", "date", "message-id"},
	})
}

// A sender's end of one connection to the block's TCP 25.
type hostTestClient struct {
	t      testing.TB
	conn   net.Conn
	reader *bufio.Reader
}

// Connects to address and writes the lb's PROXY v1 line for the sender.
func dialHostTestClient(t testing.TB, address string) *hostTestClient {
	t.Helper()
	conn, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := fmt.Fprintf(conn, "PROXY TCP4 %s %s 49152 25\r\n", hostTestSender, hostTestLbAddress); err != nil {
		t.Fatal(err)
	}
	return &hostTestClient{t: t, conn: conn, reader: bufio.NewReader(conn)}
}

// Writes text.
func (self *hostTestClient) write(text string) {
	self.t.Helper()
	if _, err := self.conn.Write([]byte(text)); err != nil {
		self.t.Fatalf("write: %v", err)
	}
}

// Reads one line and compares it, CRLF aside.
func (self *hostTestClient) expect(want string) {
	self.t.Helper()
	line, err := self.reader.ReadString('\n')
	if err != nil {
		self.t.Fatalf("expected %q: %v", want, err)
	}
	if line != want+"\r\n" {
		self.t.Fatalf("read %q, want %q", line, want)
	}
}

// Through the dialog to EHLO's reply.
func (self *hostTestClient) ehlo() {
	self.t.Helper()
	self.expect("220 " + hostTestBannerHostName + " ESMTP")
	self.write("EHLO " + hostTestEhloName + "\r\n")
	self.expect("250-" + hostTestBannerHostName)
	self.expect("250-ENHANCEDSTATUSCODES")
	self.expect("250 STARTTLS")
}

// Sends line and answers the reply's code and text.
func hostTestCommand(t testing.TB, text *textproto.Conn, line string) (int, string) {
	t.Helper()
	if err := text.PrintfLine("%s", line); err != nil {
		t.Fatalf("%s: %v", line, err)
	}
	code, message, err := text.ReadResponse(0)
	if err != nil && code == 0 {
		t.Fatalf("%s: %v", line, err)
	}
	return code, message
}

// One transaction to recipient carrying message, answering DATA's final reply.
func hostTestSend(t testing.TB, text *textproto.Conn, recipient string, message []byte) (int, string) {
	t.Helper()
	if code, reply := hostTestCommand(t, text, "MAIL FROM:<reporter@sender.example>"); code != 250 {
		t.Fatalf("MAIL: %d %s", code, reply)
	}
	if code, reply := hostTestCommand(t, text, "RCPT TO:<"+recipient+">"); code != 250 {
		t.Fatalf("RCPT: %d %s", code, reply)
	}
	if code, reply := hostTestCommand(t, text, "DATA"); code != 354 {
		t.Fatalf("DATA: %d %s", code, reply)
	}
	writer := text.DotWriter()
	if _, err := writer.Write(message); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	code, reply, err := text.ReadResponse(0)
	if err != nil && code == 0 {
		t.Fatalf("after DATA: %v", err)
	}
	return code, reply
}

// What the run on the stores reads from its parent: that it is the child, and the canary its messages carry.
const (
	hostTestChildEnv  = "MXEDGEHOST_TEST_RUN_CHILD"
	hostTestCanaryEnv = "MXEDGEHOST_TEST_RUN_CANARY"
)

// The block on the private stores (.stack/server-test.sh), T3 of S7 and FS-39, run in a child process whose
// glog writes to stderr, so the parent reads every line the block logged.
//
// The child runs sidecarhost.Run with the block's real Redis and Postgres pools, the engine and the PROXY
// listener on a fake clock, the gate open and SMTP-M5-05's intake on its fakes behind the listener. Through the
// block's TCP 25 port: a connection with no PROXY header gets nothing; one that sends a byte behind STARTTLS is
// reset with RST through the socket under the PROXY connection; and a reporter that sends no SNI delivers an ARF
// report to root@<report host> and mail to abuse@<root>, each 250, while role mail naming no network gets 550
// 5.7.1. None of those sessions reaches for Postgres once the block is ready (I-12), no metric of the process
// holds an address, a recipient or the canary its messages carry, and the block drains and closes at ctx's end.
//
// The parent then scans everything the child wrote, its glog lines included, for the sender's address, its EHLO
// name, the recipients and the canary: no log line may hold any of them (I-13, FS-39).
func TestRunHostOnThePrivateStores(t *testing.T) {
	if os.Getenv(hostTestChildEnv) == "1" {
		runHostTestOnTheStores(t, os.Getenv(hostTestCanaryEnv))
		return
	}
	capture := sidecartest.NewPrivacyCapture()
	canary := capture.CanaryText("enclosed message")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRunHostOnThePrivateStores$", "-test.count=1", "-test.v", "-logtostderr=true", "-v=4")
	command.Env = append(os.Environ(), hostTestChildEnv+"=1", hostTestCanaryEnv+"="+canary)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the block's run failed: %v\n%s", err, output)
	}
	for _, want := range []string{"--- PASS: TestRunHostOnThePrivateStores", "[" + hostPlatformService + "]ready", "[" + hostPlatformService + "]closed"} {
		if !bytes.Contains(output, []byte(want)) {
			t.Fatalf("the child's output lacks %q\n%s", want, output)
		}
	}
	capture.Forbid("sender address", []byte(hostTestSender))
	capture.Forbid("ehlo name", []byte(hostTestEhloName))
	capture.Forbid("abuse recipient", []byte(hostTestAbuseRecipient))
	capture.Forbid("root recipient", []byte(hostTestRootRecipient))
	capture.AddItem(&sidecartest.PrivacyItem{Class: "CC4", Name: "log", Body: output})
	if findings := capture.Scan(); len(findings) != 0 {
		t.Fatalf("%v", findings)
	}
}

// The child's run (see TestRunHostOnThePrivateStores).
func runHostTestOnTheStores(t *testing.T, canary string) {
	if len(canary) < 32 {
		t.Fatal("no canary from the parent")
	}
	statusHostPort := hostTestFreePort(t)
	smtpHostPort := hostTestFreePort(t)
	t.Setenv("WARP_HOST_IPV4", "127.0.0.1")
	t.Setenv("WARP_PORTS", fmt.Sprintf("%d:%d,%d:%d", hostTestStatusPort, statusHostPort, hostTestSmtpPort, smtpHostPort))
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithCancel(tb.Context())
		defer cancel()
		start := sidecartest.DefaultClockStart()
		engineClock := sidecartest.NewFakeClock(start)
		// Run's own, so nothing but its readiness and gauge wait on it
		runClock := sidecartest.NewFakeClock(start)
		gates := sidecartest.NewFakeServiceGates(nil)
		gates.OpenService(sidecar.ServiceGateMxedge)

		// SMTP-M5-05's intake on its fakes, logging through glog as the block does
		intakeHost := sidecartest.NewFakeHost(ctx, engineClock, nil, nil, nil)
		store := sidecartest.NewFakeSmtpEdgeStore(engineClock, nil)
		txt := sidecartest.NewFakeSmtpDnsTxt()
		loopKey := sidecartest.NewSmtpDkimFixtureEd25519Key("loop", "fbl1", "loop.example")
		txt.Set(loopKey.RecordName(), loopKey.TxtRecord())
		keys := sidecartest.NewFakeSmtpReportKeys()
		join := sidecartest.NewFakeSmtpReportJoin()
		sink := sidecartest.NewFakeSmtpAbuseSink()
		networkId := sidecar.NewId()
		keys.Add(networkId)
		join.Attribute(hostTestNetDomain, networkId, "high")
		smtpSettings := smtpedgeservice.DefaultSmtpEdgeSettings()
		smtpSettings.Root = hostTestRootDomainName
		smtpSettings.ReportHostName = hostTestReportHostName
		smtpSettings.LoopAllowlist = []string{"loop.example"}
		reportIntake, err := smtpedgeservice.NewSmtpReportIntake(intakeHost.ServiceHost(), store, &smtpedgeservice.SmtpReportIntakeDeps{
			Tokens: sidecartest.NewFakeSmtpNetTokens(),
			Labels: sidecartest.NewFakeSmtpLabels(),
			Keys:   keys,
			Txt:    txt,
			Join:   join,
			Sink:   sink,
			Log:    hostLogger{},
		}, smtpSettings)
		if err != nil {
			tb.Fatal(err)
		}

		settings := DefaultHostSettings()
		settings.MxEdge = hostTestMxEdgeSettings()
		settings.SecretDirectory = hostTestPlatformSecrets(tb, start)
		chainPem, err := os.ReadFile(filepath.Join(settings.SecretDirectory, HostCertificateFileName))
		if err != nil {
			tb.Fatal(err)
		}
		block, _ := pem.Decode(chainPem)
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			tb.Fatal(err)
		}
		deps := &HostDeps{
			Intake:     &hostTestReportIntake{intake: reportIntake},
			Gates:      gates,
			Clock:      engineClock,
			WarpStatus: &router.WarpStatusState{},
		}
		engine := newPlatformBlockEngine(ctx, settings, deps)
		platformHost, ok := engine.(*PlatformHost)
		if !ok {
			tb.Fatalf("the block did not start: %T", engine)
		}
		// the first probe has made or read today's key in the stores' Redis once its next wait is armed
		awaitHostTestTimerAt(tb, ctx, engineClock, start.Add(hostTestProbeInterval))
		if !platformHost.Ready() {
			tb.Fatal("not ready with its certificate and today's key")
		}

		metrics := &hostTestMetrics{
			Metrics:   sidecartest.NewFakeMetrics(nil),
			readyName: "urnetwork_" + hostPlatformService + "_ready",
			ready:     make(chan float64, 8),
		}
		options := platformRunOptions(ctx, engine, settings, deps, &HostPorts{Status: hostTestStatusPort, Smtp: hostTestSmtpPort})
		options.Clock = runClock
		options.Metrics = metrics
		// the drain's keepalive grace is the lb's, and no lb holds this test's connections
		options.Settings = sidecarhost.DefaultRunSettings()
		options.Settings.HttpServer.KeepaliveDrainTimeout = 0
		done := make(chan error, 1)
		go func() {
			done <- sidecarhost.Run(ctx, options)
		}()
		awaitReady := time.After(hostTestStepTimeout)
		for latched := false; !latched; {
			select {
			case value := <-metrics.ready:
				latched = value == 1
			case err := <-done:
				tb.Fatalf("Run returned before the latch: %v", err)
			case <-awaitReady:
				tb.Fatal("the block never latched ready")
			}
		}
		smtpAddress := fmt.Sprintf("127.0.0.1:%d", smtpHostPort)
		// from here every session's path is TCP 25's alone, which never reaches Postgres (I-12): the intake's
		// store is SMTP-M5-05's fake, so an acquisition could only be the block's own
		postgresAttempts, stopPostgresGuard := server.DenyPostgresForTest(tb)

		// a connection that does not open with the lb's PROXY header is dropped, and told nothing
		bare, err := net.Dial("tcp", smtpAddress)
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := bare.Write([]byte("EHLO " + hostTestEhloName + "\r\n")); err != nil {
			tb.Fatal(err)
		}
		if rest, _ := io.ReadAll(bare); 0 < len(rest) {
			tb.Fatalf("a connection with no PROXY header read %q", rest)
		}
		bare.Close()

		// a byte behind STARTTLS resets the connection (§15.4), with RST through the socket under the PROXY one
		reset := dialHostTestClient(tb, smtpAddress)
		reset.ehlo()
		reset.write("STARTTLS\r\nNOOP\r\n")
		if rest, err := io.ReadAll(reset.reader); 0 < len(rest) || !errors.Is(err, syscall.ECONNRESET) {
			tb.Fatalf("bytes behind STARTTLS read %q, %v", rest, err)
		}

		// S7: no SNI; FS-39: an ARF report and role mail, carrying the canary
		client := dialHostTestClient(tb, smtpAddress)
		client.ehlo()
		quietEnd := engineClock.Now().Add(settings.MxEdge.StartTlsQuietTimeout)
		client.write("STARTTLS\r\n")
		awaitHostTestTimerAt(tb, ctx, engineClock, quietEnd)
		engineClock.Advance(settings.MxEdge.StartTlsQuietTimeout)
		client.expect("220 2.0.0 Ready to start TLS")
		tlsConn := tls.Client(client.conn, &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true,
			VerifyConnection: func(state tls.ConnectionState) error {
				if len(state.PeerCertificates) == 0 || !state.PeerCertificates[0].Equal(certificate) {
					return errors.New("not the platform's certificate")
				}
				return nil
			},
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			tb.Fatalf("handshake: %v", err)
		}
		text := textproto.NewConn(tlsConn)
		if code, reply := hostTestCommand(tb, text, "EHLO "+hostTestEhloName); code != 250 {
			tb.Fatalf("EHLO after TLS: %d %s", code, reply)
		}
		if code, reply := hostTestSend(tb, text, hostTestRootRecipient, hostTestArfReport(loopKey, hostTestNetDomain, canary, canary)); code != 250 {
			tb.Fatalf("an ARF report: %d %s", code, reply)
		}
		if code, reply := hostTestSend(tb, text, hostTestAbuseRecipient, hostTestRoleMail(hostTestNetDomain, canary, canary)); code != 250 {
			tb.Fatalf("abuse@ without SNI: %d %s", code, reply)
		}
		code, reply := hostTestSend(tb, text, hostTestAbuseRecipient, []byte("From: A Person <person@person.example>\r\nContent-Type: text/plain\r\n\r\n"+canary+"\r\n"))
		if code != 550 || !strings.HasPrefix(reply, "5.7.1 ") {
			tb.Fatalf("unattributed role mail: %d", code)
		}
		if code, reply := hostTestCommand(tb, text, "QUIT"); code != 221 {
			tb.Fatalf("QUIT: %d %s", code, reply)
		}
		text.Close()
		if complaints, cases := store.Complaints(), sink.Reports(); len(complaints) != 2 || len(cases) != 1 || cases[0].NetworkId != networkId {
			tb.Fatalf("%d complaints, %d cases", len(complaints), len(cases))
		}
		if attempts := postgresAttempts(); attempts != 0 {
			tb.Fatalf("the sessions reached for Postgres %d times", attempts)
		}
		stopPostgresGuard()

		// no metric of the process, the engine's on prometheus's registry and the intake's, holds a needle
		capture := sidecartest.NewPrivacyCapture()
		capture.Forbid("canary", []byte(canary))
		capture.Forbid("sender address", []byte(hostTestSender))
		capture.Forbid("ehlo name", []byte(hostTestEhloName))
		capture.Forbid("abuse recipient", []byte(hostTestAbuseRecipient))
		capture.Forbid("root recipient", []byte(hostTestRootRecipient))
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			tb.Fatal(err)
		}
		exposition := &bytes.Buffer{}
		for _, family := range families {
			if _, err := expfmt.MetricFamilyToText(exposition, family); err != nil {
				tb.Fatal(err)
			}
		}
		if !bytes.Contains(exposition.Bytes(), []byte("urnetwork_mxedge_")) {
			tb.Fatal("the engine's metrics are not on the registry")
		}
		capture.AddItem(&sidecartest.PrivacyItem{Class: "CC4", Name: "metric", Body: exposition.Bytes()})
		for _, line := range intakeHost.Metrics.Dump() {
			capture.AddItem(&sidecartest.PrivacyItem{Class: "CC4", Name: "metric", Body: []byte(line)})
		}
		if findings := capture.Scan(); len(findings) != 0 {
			tb.Fatalf("%v", findings)
		}

		// the block's SIGTERM: drained, then the engine closed
		cancel()
		select {
		case err := <-done:
			if err != nil {
				tb.Fatal(err)
			}
		case <-time.After(hostTestStepTimeout):
			tb.Fatal("Run did not return")
		}
		if platformHost.Ready() {
			tb.Fatal("ready after the drain")
		}
	})
}
