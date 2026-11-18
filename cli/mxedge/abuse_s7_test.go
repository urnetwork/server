//go:build sidecar_pending

package main

// S7's SNI-less abuse@<root> for §21's gate (§22.7's S7 at M5, §22.4; ABUSE-M5-21 jointly with MAIL-M5-04; D-ABUSE-5,
// R-ABUSE-6, I-13, I-18): role mail to abuse@<root> from a reporter whose handshake names no server reaches the
// platform listener, the intake reads its header block and nothing else, and drops it unread with
// plan-ABUSE-auto-reply's 550 5.7.1, so no case, no complaint row and no byte of the message is kept, logged or
// counted anywhere past the listener.
//
// mxedgeservice's platform role runs as mxedgehost.NewPlatformHost builds it, on server's host over the private
// stack's Redis (.stack/server-test.sh, the test's own databases), behind a listener the test feeds through
// net.Pipe as the lb's PROXY listener would. Behind it is the block's own report intake as newPlatformReportIntake
// composes it, SMTP-M5-05's intake over model's SmtpEdgeStore and §21's AbuseSmtpSink on MG5's tables, its log
// taken through its Log seam where the block's goes to glog. The canary rides the message's Subject, its text and
// an enclosed message; the listener's input holds it, which is mx-4's exception to CC1, and CC3 (every Postgres
// table and Redis key, through sidecart3.CaptureStores) and CC4 (the engine's and the intake's log lines and every
// metric) hold none of it, nor the sender's address, its EHLO name or the person's address.
//
// The listener alone on its fakes is sidecar's mail_platform_scenario_test.go (T2), and the whole block, glog's
// lines scanned from a child process with the intake on fakes, is mxedgehost's TestRunHostOnThePrivateStores (T3).
// Nothing sleeps: the engine's first probe and its STARTTLS quiet time are awaited by their timers on a fake clock.

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/mxedgehost"
	"github.com/urnetwork/server/sidecarhost"
	"github.com/urnetwork/server/sidecart3"
)

// The platform's names and documentation addresses, synthetic (connect/CODESTYLE.md's test data rule). The root
// and the report host are testReportIntakeSettings', which the intake routes by.
const (
	s7AbuseBannerHostName = "mx.report.example"
	s7AbuseUrl            = "https://abuse.example/form"
	s7AbuseRecipient      = "abuse@root.example"
	s7AbuseSender         = "203.0.113.9"
	s7AbuseEhloName       = "relay.s7sender.example"
	s7AbusePerson         = "person@person.example"
	// apart from every other timer the engine sets, so the first probe lands by its timer alone
	s7AbuseProbeInterval = 7 * time.Second
)

// The platform role on server's host with the block's intake behind it, serving a listener the test feeds.
type s7AbuseWorld struct {
	t           testing.TB
	ctx         context.Context
	clock       *sidecartest.FakeClock
	start       time.Time
	settings    *mxedgeservice.MxEdgeSettings
	fakes       *sidecartest.FakeHost
	intakeFakes *sidecartest.FakeHost
	log         *t3RouterLog
	intakeLog   *t3RouterLog
	platform    *mxedgehost.PlatformHost
	listener    *t3RouterListener
	certificate *x509.Certificate
	routerWorld *t3RouterWorld
}

// A certificate for the report host, self-signed, valid around now.
func s7AbuseCertificate(t testing.TB, now time.Time, reportHostName string) (*tls.Certificate, *x509.Certificate) {
	t.Helper()
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(77),
		Subject:      pkix.Name{CommonName: reportHostName},
		DNSNames:     []string{reportHostName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
	}
	certificateBytes, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(certificateBytes)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{certificateBytes}, PrivateKey: privateKey, Leaf: parsed}, parsed
}

// The world, once the engine's first probe has read today's key and it is ready.
func newS7AbuseWorld(t testing.TB) *s7AbuseWorld {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	start := time.Now().UTC().Truncate(time.Second)
	clock := sidecartest.NewFakeClock(start)
	fakes := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	intakeFakes := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	gates := sidecartest.NewFakeServiceGates(nil)
	gates.OpenService(sidecar.ServiceGateMxedge)

	reportSettings := testReportIntakeSettings()
	intakeLog := &t3RouterLog{}
	reportIntake, err := smtpedgeservice.NewSmtpReportIntake(intakeFakes.ServiceHost(), model.NewSmtpEdgeStore(nil), &smtpedgeservice.SmtpReportIntakeDeps{
		Tokens: sidecartest.NewFakeSmtpNetTokens(),
		Labels: &reportLabelSource{root: reportSettings.Root},
		Txt:    sidecartest.NewFakeSmtpDnsTxt(),
		Sink:   model.NewAbuseSmtpSink(nil),
		Log:    intakeLog,
	}, reportSettings)
	if err != nil {
		t.Fatal(err)
	}
	intake := &platformReportIntake{intake: reportIntake, root: reportIntakeName(reportSettings.Root)}

	settings := mxedgeservice.DefaultMxEdgeSettings()
	settings.BannerHostName = s7AbuseBannerHostName
	settings.DeviationUrl = t3RouterDeviationUrl
	settings.PlatformReportHostName = reportSettings.ReportHostName
	settings.PlatformRootDomainName = reportSettings.Root
	settings.PlatformAbuseUrl = s7AbuseUrl
	settings.ReadyProbeInterval = s7AbuseProbeInterval

	host := sidecarhost.NewServiceHost(ctx, &sidecarhost.ServiceHostOptions{Clock: clock})
	host.Redis = sidecarhost.NewRedis(nil)
	host.Gates = gates
	// the fakes' registry, so the test reads every metric the engine counts
	host.Metrics = fakes.Metrics

	certificate, leaf := s7AbuseCertificate(t, start, reportSettings.ReportHostName)
	log := &t3RouterLog{}
	hostSettings := mxedgehost.DefaultHostSettings()
	hostSettings.MxEdge = settings
	platform, err := mxedgehost.NewPlatformHost(ctx, host, hostSettings, &mxedgehost.HostDeps{
		Intake:      intake,
		Certificate: certificate,
		Gates:       gates,
		Clock:       clock,
		Log:         log,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(platform.Close)
	world := &s7AbuseWorld{
		t: t, ctx: ctx, clock: clock, start: start, settings: settings, fakes: fakes, intakeFakes: intakeFakes,
		log: log, intakeLog: intakeLog, platform: platform, certificate: leaf,
		listener: &t3RouterListener{conns: make(chan net.Conn, 1), closed: make(chan struct{})},
	}
	// t3RouterWorld's timer wait and client, over this world's clock, settings and listener
	world.routerWorld = &t3RouterWorld{t: t, ctx: ctx, clock: clock, settings: settings, listener: world.listener, start: start}
	world.routerWorld.awaitTimerAt(start.Add(s7AbuseProbeInterval))
	if !platform.Ready() {
		t.Fatal("the platform is not ready on the stack's stores")
	}
	go platform.Serve(world.listener)
	t.Cleanup(func() { world.listener.Close() })
	return world
}

// One reporter's session that names no server: the dialog, STARTTLS, a handshake under the platform's
// certificate with no SNI, and EHLO again inside TLS.
func (self *s7AbuseWorld) openWithNoSni() *textproto.Conn {
	self.t.Helper()
	client := self.routerWorld.connect(s7AbuseSender)
	client.expect("220 " + s7AbuseBannerHostName + " ESMTP")
	client.write("EHLO " + s7AbuseEhloName + "\r\n")
	client.expect("250-" + s7AbuseBannerHostName)
	client.expect("250-ENHANCEDSTATUSCODES")
	client.expect("250 STARTTLS")
	client.startTls()
	tlsConn := tls.Client(&t3RouterBufferedConn{Conn: client.conn, reader: client.reader}, &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true,
		Time:               func() time.Time { return self.start },
		VerifyConnection: func(state tls.ConnectionState) error {
			if state.ServerName != "" {
				return errors.New("the reporter named a server")
			}
			if len(state.PeerCertificates) == 0 || !state.PeerCertificates[0].Equal(self.certificate) {
				return errors.New("not the platform's certificate")
			}
			return nil
		},
	})
	if err := tlsConn.HandshakeContext(self.ctx); err != nil {
		self.t.Fatalf("the handshake with no SNI: %v", err)
	}
	text := textproto.NewConn(tlsConn)
	if code, reply := s7AbuseCommand(self.t, text, "EHLO "+s7AbuseEhloName); code != 250 {
		self.t.Fatalf("EHLO inside TLS: %d %s", code, reply)
	}
	return text
}

// Sends line and answers the reply's code and text.
func s7AbuseCommand(t testing.TB, text *textproto.Conn, line string) (int, string) {
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

// A person's mail to abuse@<root>: the canary in its Subject, its text and a message it encloses from a network's
// domain, which a reader of the body could attribute.
func s7AbuseRoleMail(canary string) []byte {
	enclosed := (&sidecartest.SmtpFixtureMessage{FromDomain: "quietnet.root.example", MessageId: "<m2@quietnet.root.example>", Subject: canary, Body: canary}).Bytes()
	return []byte("From: A Person <" + s7AbusePerson + ">\r\nTo: " + s7AbuseRecipient + "\r\nSubject: spam from " + canary + "\r\n" +
		"Content-Type: multipart/mixed; boundary=\"" + sidecartest.SmtpFixtureBoundary + "\"\r\n\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: text/plain\r\n\r\nPlease look at " + canary + ".\r\n" +
		"--" + sidecartest.SmtpFixtureBoundary + "\r\nContent-Type: message/rfc822\r\n\r\n" + string(enclosed) + "\r\n--" + sidecartest.SmtpFixtureBoundary + "--\r\n")
}

// The rows a table holds.
func s7AbuseRowCount(t testing.TB, ctx context.Context, table string) (count int) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM `+table).Scan(&count))
	})
	return
}

// The case in the file header.
func TestS7AbuseMailWithNoSniIsDroppedUnreadAndKeptNowhere(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		world := newS7AbuseWorld(t)
		captureNet := sidecartest.NewNet(world.ctx, nil)
		defer captureNet.Close()
		capture := captureNet.Capture()
		canary := capture.CanaryText("abuse mail body")

		text := world.openWithNoSni()
		if code, reply := s7AbuseCommand(t, text, "MAIL FROM:<"+s7AbusePerson+">"); code != 250 {
			t.Fatalf("MAIL: %d %s", code, reply)
		}
		if code, reply := s7AbuseCommand(t, text, "RCPT TO:<"+s7AbuseRecipient+">"); code != 250 {
			t.Fatalf("RCPT abuse@ with no SNI: %d %s", code, reply)
		}
		if code, reply := s7AbuseCommand(t, text, "DATA"); code != 354 {
			t.Fatalf("DATA: %d %s", code, reply)
		}
		writer := text.DotWriter()
		if _, err := writer.Write(s7AbuseRoleMail(canary)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		// dropped unread, with the form's pointer
		code, reply, err := text.ReadResponse(0)
		if code != 550 || reply != "5.7.1 Not read. Report abuse at "+s7AbuseUrl {
			t.Fatalf("after DATA: %d %q, %v", code, reply, err)
		}
		if code, reply := s7AbuseCommand(t, text, "QUIT"); code != 221 {
			t.Fatalf("QUIT: %d %s", code, reply)
		}
		text.Close()
		// the engine's close ends the session and its counting
		world.platform.Drain()
		world.platform.Close()

		// no case, no report row: the header block named no network, and the body was never read
		inspect := sidecartest.InspectContext(world.ctx)
		for _, table := range []string{"abuse_case", "abuse_action", "smtp_complaint", "smtp_dmarc_daily"} {
			if count := s7AbuseRowCount(t, inspect, table); count != 0 {
				t.Errorf("%s holds %d rows after role mail it dropped", table, count)
			}
		}
		if value := world.fakes.Metrics.Value("urnetwork_mxedge_sessions_total", "platform_served"); value != 1 {
			t.Errorf("platform_served counted %v", value)
		}

		// CC3 and CC4: no stored byte, log line or metric holds the canary, the sender, its EHLO name or the person
		captureSettings := sidecart3.DefaultCaptureStoresSettings()
		// sidecart3's path is from its own directory
		captureSettings.SidecarColumnsPath = "../../local/sidecar-columns.yml"
		if _, err := sidecart3.CaptureStores(captureNet, captureSettings); err != nil {
			t.Fatal(err)
		}
		for _, needle := range []struct {
			label string
			value string
		}{
			{"sender address", s7AbuseSender},
			{"ehlo name", s7AbuseEhloName},
			{"person's address", s7AbusePerson},
		} {
			capture.Forbid(needle.label, []byte(needle.value))
		}
		for _, line := range append(world.log.Lines(), world.intakeLog.Lines()...) {
			capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassLog, Name: "log", Body: []byte(line)})
		}
		for _, line := range append(world.fakes.Metrics.Dump(), world.intakeFakes.Metrics.Dump()...) {
			capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassMetric, Name: "metric", Body: []byte(line)})
		}
		families, err := prometheus.DefaultGatherer.Gather()
		if err != nil {
			t.Fatal(err)
		}
		exposition := &bytes.Buffer{}
		for _, family := range families {
			if _, err := expfmt.MetricFamilyToText(exposition, family); err != nil {
				t.Fatal(err)
			}
		}
		capture.AddItem(&sidecartest.PrivacyItem{Class: sidecartest.PrivacyClassMetric, Name: "registry", Body: exposition.Bytes()})
		if capture.ItemCount(sidecartest.PrivacyClassRedis) == 0 || capture.ItemCount(sidecartest.PrivacyClassPostgres) == 0 {
			t.Fatal("the scan read no Redis key or no Postgres row")
		}
		if !strings.Contains(exposition.String(), "urnetwork_") {
			t.Fatal("the registry holds no metric of ours, so its scan proves nothing")
		}
		for _, finding := range capture.Scan() {
			t.Errorf("%s", finding)
		}
	})
}
