package work

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The temporary uptime-probe diagnostics (extender_probe_diagnostics.go).
//
// What a socket can be made to do is proved on loopback with the production
// probe: a refused connect, a close at accept, a handshake nobody answers,
// and the admission refusals of a real extender (A12). What loopback cannot
// produce is proved on the errors the probe returns for it.

// The production probe, captured before any test stubs it
// (stubExtenderProbe).
var testProductionProbeExtenderAddress = probeExtenderAddress

// The forward destination and the outer name of the diagnostic probes.
const (
	testDiagnosticApiHost   = "api.example"
	testDiagnosticSpoofName = "spoof.example"
)

// A probe target at a loopback tcp port.
func testDiagnosticTarget(port int, publicKey []byte) *model.NetworkExtenderProbeTarget {
	return &model.NetworkExtenderProbeTarget{
		ExtenderId: server.NewId(),
		PublicKey:  publicKey,
		TcpPort:    port,
		DnsTld:     connect.DefaultExtenderDnsTld,
		IpVersion:  4,
		Ip:         netip.MustParseAddr("127.0.0.1"),
		Carriers:   []string{connect.ExtenderCarrierTcp},
	}
}

// A fresh extender identity key.
func testDiagnosticKey(t testing.TB) (keySeed []byte, publicKey []byte) {
	t.Helper()
	keySeed, err := connect.NewExtenderKeySeed()
	if err != nil {
		t.Fatal(err)
	}
	publicKey, err = connect.ExtenderPublicKeyFromSeed(keySeed)
	if err != nil {
		t.Fatal(err)
	}
	return keySeed, publicKey
}

// Runs the production probe of target within budget, watched as the task
// watches it.
func testDiagnosticProbe(target *model.NetworkExtenderProbeTarget, budget time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	ctx, socket := withExtenderProbeSocket(ctx)
	return socket.observed(testProductionProbeExtenderAddress(ctx, target, testDiagnosticSpoofName, testDiagnosticApiHost))
}

// A loopback listener that hands every connection it accepts to handle, until
// the test ends.
func testDiagnosticListener(t testing.TB, handle func(conn net.Conn)) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		listener.Close()
	})
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go handle(conn)
		}
	}()
	return listener.Addr().(*net.TCPAddr).Port
}

// Starts a real extender on loopback with the tcp carrier alone, as the
// uptime probe dials it. Its admission limits are one action and one refusal
// a minute on a clock that never moves, so a second probe is limited and a
// third closed at accept however slowly the machine runs.
func startTestDiagnosticExtender(t testing.TB) (port int, publicKey []byte) {
	t.Helper()
	keySeed, publicKey := testDiagnosticKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port = listener.Addr().(*net.TCPAddr).Port

	admissionTime := time.Now()
	settings := extender.DefaultExtenderSettings()
	settings.IdentityKeySeed = keySeed
	settings.SpoofDomains = []string{testDiagnosticSpoofName}
	settings.AdmissionActionsPerSubnetPerMinute = 1
	settings.AdmissionRefusalsPerSubnetPerMinute = 1
	settings.AdmissionNow = func() time.Time {
		return admissionTime
	}
	settings.Listen = func(network string, address string) (net.Listener, error) {
		return listener, nil
	}
	// a probe closes its stream as soon as the response verifies, so the
	// forward the extender starts after it has nowhere to go
	settings.DialContext = func(ctx context.Context, network string, address string) (net.Conn, error) {
		return nil, fmt.Errorf("the diagnostic extender forwards nowhere")
	}
	settings.ErrorHandler = func(stage string, err error) {}

	ctx, cancel := context.WithCancel(context.Background())
	extenderServer := extender.NewExtenderServer(
		ctx,
		[]string{},
		[]string{testDiagnosticApiHost},
		map[int][]connect.ExtenderConnectMode{
			port: {connect.ExtenderConnectModeTcpTls},
		},
		&net.Dialer{},
		settings,
	)
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- extenderServer.ListenAndServe()
	}()
	t.Cleanup(func() {
		extenderServer.CloseAndWait()
		cancel()
		select {
		case <-serveDone:
		case <-time.After(10 * time.Second):
			t.Error("the diagnostic extender did not stop")
		}
	})
	select {
	case <-extenderServer.Listening():
	case err := <-serveDone:
		t.Fatalf("the diagnostic extender did not start: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the diagnostic extender did not start")
	}
	return port, publicKey
}

// Requires a failed production probe of the class given, and returns what
// its socket saw.
func requireTestDiagnosticClass(t testing.TB, err error, class string) *extenderProbeSocketError {
	t.Helper()
	if err == nil {
		t.Fatalf("the probe passed, want %s", class)
	}
	gotClass, detail := classifyExtenderProbeFailure(err)
	if gotClass != class {
		t.Fatalf("class = %s (%s), want %s", gotClass, detail, class)
	}
	if strings.Contains(detail, "127.0.0.1") {
		t.Fatalf("the detail names the address: %s", detail)
	}
	var socketErr *extenderProbeSocketError
	if !errors.As(err, &socketErr) {
		t.Fatalf("the production probe's error carries no socket: %v", err)
	}
	return socketErr
}

// The production probe watches its socket, so a failure says how far it got:
// whether the tcp connect was answered, and whether the extender then sent
// anything at all.
func TestExtenderProbeDiagnosticTellsHowFarTheSocketGot(t *testing.T) {
	_, publicKey := testDiagnosticKey(t)

	t.Run("refused", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := listener.Addr().(*net.TCPAddr).Port
		listener.Close()

		socketErr := requireTestDiagnosticClass(
			t,
			testDiagnosticProbe(testDiagnosticTarget(port, publicKey), 5*time.Second),
			extenderProbeFailureDialRefused,
		)
		connect.AssertEqual(t, socketErr.connected, false)

		// a dial with no socket record in its context is only the dial
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		class, _ := classifyExtenderProbeFailure(testProductionProbeExtenderAddress(
			ctx,
			testDiagnosticTarget(port, publicKey),
			testDiagnosticSpoofName,
			testDiagnosticApiHost,
		))
		connect.AssertEqual(t, class, extenderProbeFailureDialRefused)
	})

	t.Run("closed at accept", func(t *testing.T) {
		port := testDiagnosticListener(t, func(conn net.Conn) {
			conn.Close()
		})

		socketErr := requireTestDiagnosticClass(
			t,
			testDiagnosticProbe(testDiagnosticTarget(port, publicKey), 5*time.Second),
			extenderProbeFailureClosedBeforeTls,
		)
		connect.AssertEqual(t, socketErr.connected, true)
		connect.AssertEqual(t, socketErr.serverBytes, int64(0))
	})

	t.Run("no tls answer", func(t *testing.T) {
		held := make(chan struct{})
		port := testDiagnosticListener(t, func(conn net.Conn) {
			<-held
			conn.Close()
		})
		t.Cleanup(func() {
			close(held)
		})

		socketErr := requireTestDiagnosticClass(
			t,
			testDiagnosticProbe(testDiagnosticTarget(port, publicKey), 1*time.Second),
			extenderProbeFailureTlsNoAnswer,
		)
		connect.AssertEqual(t, socketErr.connected, true)
		connect.AssertEqual(t, socketErr.serverBytes, int64(0))
	})
}

// The admission refusals of A12 are told apart from every other failure:
// past its subnet's actions a probe is answered 429, and past its subnet's
// refusals it is closed before the outer tls. The first probe is the control:
// the watched dial is the probe's own, so a probe of a live extender still
// passes.
func TestExtenderProbeDiagnosticTellsTheAdmissionRefusals(t *testing.T) {
	port, publicKey := startTestDiagnosticExtender(t)
	target := testDiagnosticTarget(port, publicKey)

	if err := testDiagnosticProbe(target, 10*time.Second); err != nil {
		t.Fatalf("the probe of a live extender failed: %v", err)
	}

	socketErr := requireTestDiagnosticClass(
		t,
		testDiagnosticProbe(target, 10*time.Second),
		extenderProbeFailureAdmission,
	)
	_, detail := classifyExtenderProbeFailure(socketErr)
	connect.AssertEqual(t, strings.HasPrefix(detail, "retry_after="), true)
	connect.AssertEqual(t, 0 < socketErr.serverBytes, true)

	socketErr = requireTestDiagnosticClass(
		t,
		testDiagnosticProbe(target, 10*time.Second),
		extenderProbeFailureClosedBeforeTls,
	)
	connect.AssertEqual(t, socketErr.connected, true)
	connect.AssertEqual(t, socketErr.serverBytes, int64(0))
}

// Every class the probe's errors can be told by, and no address in any
// detail: the extender's, the prober's, or one quoted inside an error.
func TestExtenderProbeDiagnosticClassifiesTheProbeErrors(t *testing.T) {
	extenderAddr := &net.TCPAddr{IP: net.ParseIP("192.0.2.80"), Port: 443}
	localAddr := &net.TCPAddr{IP: net.ParseIP("10.20.30.40"), Port: 50123}
	dialErr := func(errno syscall.Errno) error {
		return &net.OpError{
			Op:   "dial",
			Net:  "tcp",
			Addr: extenderAddr,
			Err:  &os.SyscallError{Syscall: "connect", Err: errno},
		}
	}
	readErr := func(cause error) error {
		return &net.OpError{
			Op:     "read",
			Net:    "tcp",
			Source: localAddr,
			Addr:   extenderAddr,
			Err:    cause,
		}
	}
	unconnected := func(err error) error {
		return &extenderProbeSocketError{
			err:     err,
			elapsed: time.Second,
		}
	}
	connected := func(err error, serverBytes int64) error {
		return &extenderProbeSocketError{
			err:         err,
			elapsed:     time.Second,
			connected:   true,
			connectTime: 20 * time.Millisecond,
			serverBytes: serverBytes,
		}
	}

	type classCase struct {
		name   string
		err    error
		class  string
		detail string
	}
	cases := []classCase{
		{"429", connected(&connect.ExtenderLimitedError{RetryAfter: 37 * time.Second}, 2900), extenderProbeFailureAdmission, "retry_after=37s"},
		{"403", connected(&connect.ExtenderRefusedError{StatusCode: 403}, 2900), extenderProbeFailureRefused, "status=403"},
		{"bad signature", connected(errors.New("extender challenge signature does not verify"), 3100), extenderProbeFailureChallenge, "does not verify"},
		{"another key", connected(errors.New("extender published another identity key"), 3100), extenderProbeFailureChallenge, "another identity key"},
		{"another leaf", connected(errors.New("extender leaf is not signed by the record key"), 1800), extenderProbeFailureTlsIdentity, "record key"},
		{"dial refused", unconnected(dialErr(syscall.ECONNREFUSED)), extenderProbeFailureDialRefused, "dial: connect: connection refused"},
		{"dial timeout", unconnected(dialErr(syscall.ETIMEDOUT)), extenderProbeFailureDialTimeout, "dial: connect:"},
		{"no network", unconnected(dialErr(syscall.ENETUNREACH)), extenderProbeFailureDialUnreachable, "dial: connect:"},
		{"no host", unconnected(dialErr(syscall.EHOSTUNREACH)), extenderProbeFailureDialUnreachable, "dial: connect:"},
		{"no dial", unconnected(context.DeadlineExceeded), extenderProbeFailureDialError, "deadline"},
		{"eof at accept", connected(io.EOF, 0), extenderProbeFailureClosedBeforeTls, "EOF"},
		{"reset at accept", connected(readErr(&os.SyscallError{Syscall: "read", Err: syscall.ECONNRESET}), 0), extenderProbeFailureClosedBeforeTls, "connection reset"},
		{"silent handshake", connected(context.DeadlineExceeded, 0), extenderProbeFailureTlsNoAnswer, "deadline"},
		{"not tls", connected(tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, 5), extenderProbeFailureTls, "does not look like"},
		{"tls alert", connected(&net.OpError{Op: "remote error", Err: errors.New("tls: handshake failure")}, 7), extenderProbeFailureTls, "remote error: tls: handshake failure"},
		{"our own tls alert", connected(&net.OpError{Op: "local error", Err: errors.New("tls: bad record MAC")}, 2900), extenderProbeFailureTls, "local error: tls: bad record MAC"},
		{"tls version", connected(errors.New("tls: server selected unsupported protocol version 303"), 90), extenderProbeFailureTls, "unsupported protocol version"},
		{"closed after bytes", connected(io.ErrUnexpectedEOF, 2900), extenderProbeFailureClosed, "unexpected EOF"},
		{"timeout after bytes", connected(readErr(os.ErrDeadlineExceeded), 2900), extenderProbeFailureTimeout, "read: i/o timeout"},
		{"unwatched eof", io.EOF, extenderProbeFailureClosed, "EOF"},
		{"unwatched", errors.New("the extender did not answer"), extenderProbeFailureOther, "did not answer"},
		{"quoted addresses", connected(errors.New("odd answer from 192.0.2.80:443 via [2001:db8::80]:443 and ::ffff:192.0.2.81"), 40), extenderProbeFailureOther, "odd answer from <ip> via <ip> and <ip>"},
	}
	for _, c := range cases {
		class, detail := classifyExtenderProbeFailure(c.err)
		if class != c.class {
			t.Errorf("%s: class = %s (%s), want %s", c.name, class, detail, c.class)
		}
		if !strings.Contains(detail, c.detail) {
			t.Errorf("%s: detail = %q, want it to contain %q", c.name, detail, c.detail)
		}
		for _, address := range []string{"192.0.2.8", "10.20.30.40", "50123", "2001:db8", "ffff"} {
			if strings.Contains(detail, address) {
				t.Errorf("%s: detail %q names an address", c.name, detail)
			}
		}
	}
}

// The line names the extender by id and family, never by address, and says
// how far the probe got and how long after the address's activation.
func TestExtenderProbeDiagnosticLineNamesTheExtenderNotTheAddress(t *testing.T) {
	now := server.NowUtc()
	extenderId := server.NewId()
	target := &model.NetworkExtenderProbeTarget{
		ExtenderId: extenderId,
		TcpPort:    443,
		IpVersion:  4,
		Ip:         netip.MustParseAddr("192.0.2.90"),
	}
	state := &extenderProbeAddressState{
		activateTime:             now.Add(-7 * time.Minute),
		consecutiveProbeFailures: 2,
	}

	refused := &extenderProbeSocketError{
		err: &net.OpError{
			Op:   "dial",
			Net:  "tcp",
			Addr: &net.TCPAddr{IP: net.ParseIP("192.0.2.90"), Port: 443},
			Err:  &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
		},
		elapsed: 1500 * time.Millisecond,
	}
	connect.AssertEqual(
		t,
		extenderProbeDiagnosticLine(target, testDiagnosticSpoofName, refused, state, now),
		fmt.Sprintf(
			`[extenderprobe]diagnostic extender=%s ipv=4 class=dial_refused detail="dial: connect: connection refused" server_name=spoof.example elapsed=1.5s connect=none server_bytes=0 since_activation=7m0s since_uptime_ok=never failures_before=2`,
			extenderId,
		),
	)

	lastProbeSuccessTime := now.Add(-time.Hour)
	state.lastProbeSuccessTime = &lastProbeSuccessTime
	closedBeforeTls := &extenderProbeSocketError{
		err:         io.EOF,
		elapsed:     30 * time.Millisecond,
		connected:   true,
		connectTime: 12 * time.Millisecond,
	}
	connect.AssertEqual(
		t,
		extenderProbeDiagnosticLine(target, testDiagnosticSpoofName, closedBeforeTls, state, now),
		fmt.Sprintf(
			`[extenderprobe]diagnostic extender=%s ipv=4 class=closed_before_tls detail="EOF" server_name=spoof.example elapsed=30ms connect=12ms server_bytes=0 since_activation=7m0s since_uptime_ok=1h0m0s failures_before=2`,
			extenderId,
		),
	)

	// a probe that never reached a dial, as the task tests' stubs do not, has
	// no socket to report, and a state that could not be read is left out
	probeErr := errors.New("the extender did not answer")
	socket := &extenderProbeSocket{
		startTime: time.Now(),
	}
	connect.AssertEqual(t, socket.observed(probeErr) == probeErr, true)
	connect.AssertEqual(t, socket.observed(nil) == nil, true)
	var noSocket *extenderProbeSocket
	connect.AssertEqual(t, noSocket.observed(probeErr) == probeErr, true)
	line := extenderProbeDiagnosticLine(target, testDiagnosticSpoofName, socket.observed(probeErr), nil, now)
	connect.AssertEqual(t, strings.Contains(line, " class=other "), true)
	connect.AssertEqual(t, strings.Contains(line, " elapsed=? connect=? server_bytes=? since_activation=? since_uptime_ok=? failures_before=?"), true)
	connect.AssertEqual(t, strings.Contains(line, "192.0.2.90"), false)
}

// One line per address an hour over every taskworker: the first failure in
// the hour claims the address, and every later one finds it claimed.
func TestExtenderProbeDiagnosticClaimsOneLinePerAddressAnHour(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false, RerunCount: 0}).Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := server.NewId()

		connect.AssertEqual(t, claimExtenderProbeDiagnostic(ctx, extenderId, 4), true)
		connect.AssertEqual(t, claimExtenderProbeDiagnostic(ctx, extenderId, 4), false)
		// the other family and another extender are other addresses
		connect.AssertEqual(t, claimExtenderProbeDiagnostic(ctx, extenderId, 6), true)
		connect.AssertEqual(t, claimExtenderProbeDiagnostic(ctx, server.NewId(), 4), true)
		// and a context that has ended claims nothing
		doneCtx, doneCancel := context.WithCancel(ctx)
		doneCancel()
		connect.AssertEqual(t, claimExtenderProbeDiagnostic(doneCtx, server.NewId(), 4), false)

		var ttl time.Duration
		server.Redis(ctx, func(r server.RedisClient) {
			var err error
			ttl, err = r.TTL(ctx, extenderProbeDiagnosticKey(extenderId, 4)).Result()
			server.Raise(err)
		})
		if ttl <= extenderProbeDiagnosticInterval-time.Minute || extenderProbeDiagnosticInterval < ttl {
			t.Fatalf("the claim holds for %s, want about %s", ttl, extenderProbeDiagnosticInterval)
		}
	})
}

// A line reports the address as it stood before the failure, read by its
// family, and nothing for an address that is not there.
func TestExtenderProbeDiagnosticReadsTheAddressState(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		extenderId := server.NewId()
		activateTime := server.NowUtc().Add(-time.Hour)
		lastProbeSuccessTime := activateTime.Add(10 * time.Minute)
		model.Testing_CreateNetworkExtender(
			ctx,
			&model.NetworkExtender{
				ExtenderId:  extenderId,
				NetworkId:   server.NewId(),
				ClientId:    server.NewId(),
				PublicKey:   []byte("extender-public-key-diagnostic-1"),
				CreateTime:  activateTime,
				TcpPort:     443,
				UdpPort:     443,
				DnsPort:     53,
				DnsTld:      connect.DefaultExtenderDnsTld,
				CountryCode: "US",
				Active:      true,
			},
			[]*model.NetworkExtenderAddress{
				{
					IpVersion:                4,
					Ip:                       netip.MustParseAddr("192.0.2.91"),
					Carriers:                 []string{connect.ExtenderCarrierTcp},
					ActivateTime:             activateTime,
					LastProbeSuccessTime:     &lastProbeSuccessTime,
					ConsecutiveProbeFailures: 2,
					Active:                   true,
				},
				{
					IpVersion:    6,
					Ip:           netip.MustParseAddr("2001:db8::91"),
					Carriers:     []string{connect.ExtenderCarrierTcp},
					ActivateTime: activateTime,
					Active:       true,
				},
			},
		)

		state := getExtenderProbeAddressState(ctx, extenderId, 4)
		if state == nil {
			t.Fatal("the ipv4 address was not read")
		}
		connect.AssertEqual(t, state.activateTime.Equal(activateTime), true)
		connect.AssertEqual(t, state.lastProbeSuccessTime.Equal(lastProbeSuccessTime), true)
		connect.AssertEqual(t, state.consecutiveProbeFailures, 2)

		state = getExtenderProbeAddressState(ctx, extenderId, 6)
		if state == nil {
			t.Fatal("the ipv6 address was not read")
		}
		connect.AssertEqual(t, state.lastProbeSuccessTime == nil, true)
		connect.AssertEqual(t, state.consecutiveProbeFailures, 0)

		connect.AssertEqual(t, getExtenderProbeAddressState(ctx, server.NewId(), 4) == nil, true)
	})
}
