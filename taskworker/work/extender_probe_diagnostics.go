package work

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// TEMPORARY: the revocation-loop diagnosis. Once the loop is understood, remove
// this file and the lines in extender_probe_work.go that
// ExtenderProbeFailureDiagnostics gates.
//
// On Main on 2026-11-01, 183 revocations in 24 h hit 65 extenders. One was
// revoked 18 times in 14 h, each 33 to 36 min after it activated: its
// activation probe passes from an api host, six uptime probes in a row then
// fail from a taskworker, and it activates again as soon as it sees its own
// revocation. Both probes run the same connect.ProbeExtenderCarrier, and the
// uptime task only counts its failures, so nothing says how they fail.
//
// While ExtenderProbeFailureDiagnostics is set, a failed uptime probe logs one
// line, at most once an hour for each address over every taskworker:
//
//	[extenderprobe]diagnostic extender=<id> ipv=<4|6> class=<class> detail="<detail>" server_name=<name> elapsed=<d|?> connect=<d|none|?> server_bytes=<n|?> since_activation=<d|?> since_uptime_ok=<d|never|?> failures_before=<n|?>
//
// class is how far the probe got (the extenderProbeFailure constants) and
// detail what the error says beyond that. server_name is the outer name the
// probe presented, a public spoof name, since a filter on the path would fail
// by name. elapsed is how long the probe ran, connect how long its tcp connect
// took (none when it never connected), and server_bytes what the extender sent
// before the failure. since_activation is the time since the address last
// activated, which is when its activation probe last passed; since_uptime_ok
// the time since its last uptime probe passed; failures_before its consecutive
// failures before this one. A ? is what could not be read, or a probe that
// never reached a dial. The line names the extender by id and family, never by
// address: an error's socket ends are dropped, and what is left is scrubbed of
// anything that reads as an ip.

const (
	// TEMPORARY, for the revocation-loop diagnosis: log how each failed uptime
	// probe failed (extender_probe_diagnostics.go). Remove it, and everything
	// it gates, once the loop is understood.
	ExtenderProbeFailureDiagnostics = true

	// One diagnostic line per address in this long, over every taskworker.
	extenderProbeDiagnosticInterval = time.Hour
	// The budget of the redis write that claims an address's line. A line comes
	// before the probe's result is written, so this holds that write up for no
	// longer than a diagnostic is worth.
	extenderProbeDiagnosticClaimTimeout = 1 * time.Second
	// The budget of the read of the address a line reports.
	extenderProbeDiagnosticReadTimeout = 5 * time.Second
	// The longest detail a line carries.
	extenderProbeDiagnosticDetailMaxLength = 160
)

// How a failed uptime probe failed, in the order the probe runs: the tcp
// connect, the outer tls, the extender request, its response.
const (
	// the tcp connect got no answer within the budget: a dropped syn, a host
	// that is down, a black hole
	extenderProbeFailureDialTimeout = "dial_timeout"
	// the address refused the tcp connect: it is up and nothing accepts on
	// the port
	extenderProbeFailureDialRefused = "dial_refused"
	// no route to the address, by the prober's own table or an icmp
	// unreachable
	extenderProbeFailureDialUnreachable = "dial_unreachable"
	// any other connect failure, or no connect because the budget had ended
	extenderProbeFailureDialError = "dial_error"
	// connected, then closed or reset before the extender sent a byte. The
	// socket cannot say who did it: an extender closes at accept a source
	// subnet past its admission refusals (A12) and a connection over its
	// connection caps (A9), and a filter on the path can reset a client hello.
	extenderProbeFailureClosedBeforeTls = "closed_before_tls"
	// connected, and nothing answered the client hello within the budget: a
	// socket the kernel accepted for a process that never served it
	extenderProbeFailureTlsNoAnswer = "tls_no_answer"
	// the outer leaf is not signed by the extender's key: another identity
	// answers at the address (B3)
	extenderProbeFailureTlsIdentity = "tls_identity"
	// any other outer tls failure: an alert from either end, an answer that is
	// not tls, a version the extender would not speak
	extenderProbeFailureTls = "tls_error"
	// the extender answered 429: the prober's source is over one of its
	// admission limits (A12), or every NLayer hop of an NLayer extender is
	extenderProbeFailureAdmission = "admission_limited"
	// the extender answered another refusal: 403 for a header, a destination
	// or a hop count it would not take (A4)
	extenderProbeFailureRefused = "refused"
	// the response named another key, or carried no valid signature over this
	// probe's challenge
	extenderProbeFailureChallenge = "challenge"
	// closed or reset after the extender had sent bytes, before a whole
	// response
	extenderProbeFailureClosed = "closed"
	// the budget ran out after the extender had sent bytes
	extenderProbeFailureTimeout = "timeout"
	extenderProbeFailureOther   = "other"
)

// The plain errors connect's probe returns for a response that does not prove
// the key, and for an outer leaf the key did not sign. Their text is all there
// is to tell them by.
var (
	extenderProbeChallengeErrors = []string{
		"extender sent no response",
		"extender published another identity key",
		"extender challenge signature does not verify",
	}
	extenderProbeLeafErrors = []string{
		"extender public key is not an ed25519 key",
		"extender presented no certificate",
		"extender leaf is signed with",
		"extender leaf is not signed by the record key",
	}
)

// What one uptime probe's socket saw. A probe carries its record in its
// context and is called as it always is; watchExtenderProbeDials fills the
// record in from the probe's dial.
type extenderProbeSocket struct {
	startTime   time.Time
	dialed      atomic.Bool
	connected   atomic.Bool
	connectTime atomic.Int64
	serverBytes atomic.Int64
}

// The context key of a probe's socket record.
type extenderProbeSocketContextKey struct{}

// Watches the dials of the controller's shared probe settings, once, before
// the first probe of this process: the dial the settings make anyway, and for
// a probe that carries a socket record, how it went and the bytes the extender
// sent, so watching changes nothing on the wire. In a taskworker only the
// uptime task reads those settings; the activations that also read them run in
// the api, which does not link this package.
var watchExtenderProbeDials = sync.OnceFunc(func() {
	connectSettings := controller.ExtenderProbeConnectSettings()
	unwatchedSettings := *connectSettings
	connectSettings.DialContextSettings = &connect.DialContextSettings{
		DialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			socket, _ := ctx.Value(extenderProbeSocketContextKey{}).(*extenderProbeSocket)
			if socket == nil {
				return unwatchedSettings.DialContext(ctx, network, address)
			}
			socket.dialed.Store(true)
			dialStartTime := time.Now()
			conn, err := unwatchedSettings.DialContext(ctx, network, address)
			if err != nil {
				return nil, err
			}
			socket.connectTime.Store(int64(time.Since(dialStartTime)))
			socket.connected.Store(true)
			return &extenderProbeSocketConn{
				Conn:   conn,
				socket: socket,
			}, nil
		},
	}
})

// The context of one probe carrying a new socket record. With the diagnostics
// off, the context as it was and no record.
func withExtenderProbeSocket(ctx context.Context) (context.Context, *extenderProbeSocket) {
	if !ExtenderProbeFailureDiagnostics {
		return ctx, nil
	}
	watchExtenderProbeDials()
	socket := &extenderProbeSocket{
		startTime: time.Now(),
	}
	return context.WithValue(ctx, extenderProbeSocketContextKey{}, socket), socket
}

// The error of a probe with what its socket saw. A probe that never reached a
// dial has nothing to add, so its error is returned as it is.
func (self *extenderProbeSocket) observed(err error) error {
	if self == nil || err == nil || !self.dialed.Load() {
		return err
	}
	return &extenderProbeSocketError{
		err:         err,
		elapsed:     time.Since(self.startTime),
		connected:   self.connected.Load(),
		connectTime: time.Duration(self.connectTime.Load()),
		serverBytes: self.serverBytes.Load(),
	}
}

// A watched probe's connection, counting what the extender sent on it.
type extenderProbeSocketConn struct {
	net.Conn
	socket *extenderProbeSocket
}

func (self *extenderProbeSocketConn) Read(b []byte) (int, error) {
	n, err := self.Conn.Read(b)
	self.socket.serverBytes.Add(int64(n))
	return n, err
}

// A failed watched probe's error. It reads as the error it wraps, so every
// other reader of the probe error is unchanged.
type extenderProbeSocketError struct {
	err         error
	elapsed     time.Duration
	connected   bool
	connectTime time.Duration
	serverBytes int64
}

func (self *extenderProbeSocketError) Error() string {
	return self.err.Error()
}

func (self *extenderProbeSocketError) Unwrap() error {
	return self.err
}

// How a probe failed (the extenderProbeFailure constants), and what its error
// says beyond that, with no address in it. A watched probe is told by how far
// its socket got as well as by its error; a probe that was not watched is told
// by its error alone.
func classifyExtenderProbeFailure(err error) (class string, detail string) {
	var socketErr *extenderProbeSocketError
	watched := errors.As(err, &socketErr)

	var limitedErr *connect.ExtenderLimitedError
	if errors.As(err, &limitedErr) {
		return extenderProbeFailureAdmission, fmt.Sprintf("retry_after=%s", limitedErr.RetryAfter)
	}
	var refusedErr *connect.ExtenderRefusedError
	if errors.As(err, &refusedErr) {
		return extenderProbeFailureRefused, fmt.Sprintf("status=%d", refusedErr.StatusCode)
	}

	detail = extenderProbeFailureDetail(err)
	message := err.Error()
	for _, challengeError := range extenderProbeChallengeErrors {
		if strings.Contains(message, challengeError) {
			return extenderProbeFailureChallenge, detail
		}
	}
	for _, leafError := range extenderProbeLeafErrors {
		if strings.Contains(message, leafError) {
			return extenderProbeFailureTlsIdentity, detail
		}
	}

	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		switch {
		case opErr.Timeout():
			return extenderProbeFailureDialTimeout, detail
		case errors.Is(err, syscall.ECONNREFUSED):
			return extenderProbeFailureDialRefused, detail
		case errors.Is(err, syscall.ENETUNREACH),
			errors.Is(err, syscall.EHOSTUNREACH),
			errors.Is(err, syscall.EADDRNOTAVAIL):
			return extenderProbeFailureDialUnreachable, detail
		default:
			return extenderProbeFailureDialError, detail
		}
	}
	if watched && !socketErr.connected {
		return extenderProbeFailureDialError, detail
	}

	closed := errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE)
	var netErr net.Error
	timedOut := errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout())
	if watched && socketErr.serverBytes == 0 {
		switch {
		case closed:
			return extenderProbeFailureClosedBeforeTls, detail
		case timedOut:
			return extenderProbeFailureTlsNoAnswer, detail
		}
	}

	var recordHeaderErr tls.RecordHeaderError
	if errors.As(err, &recordHeaderErr) ||
		(errors.As(err, &opErr) && (opErr.Op == "remote error" || opErr.Op == "local error")) ||
		strings.HasPrefix(message, "tls: ") ||
		strings.HasPrefix(message, "x509: ") {
		return extenderProbeFailureTls, detail
	}
	switch {
	case closed:
		return extenderProbeFailureClosed, detail
	case timedOut:
		return extenderProbeFailureTimeout, detail
	default:
		return extenderProbeFailureOther, detail
	}
}

// Anything in a detail that reads as an ip, with its port: a v6 address is
// two colons with hex between, which no sentence in an error has.
var (
	extenderProbeDetailIpv6 = regexp.MustCompile(`\[?[0-9A-Fa-f]*:[0-9A-Fa-f]*:[0-9A-Fa-f:.]*(%[0-9A-Za-z]+)?\]?(:[0-9]+)?`)
	extenderProbeDetailIpv4 = regexp.MustCompile(`[0-9]{1,3}(\.[0-9]{1,3}){3}(:[0-9]+)?`)
)

// What an error says, with no address in it: an OpError's operation and cause
// without the socket ends it names, anything else as it reads, either way
// scrubbed of anything that reads as an ip and cut to a length.
func extenderProbeFailureDetail(err error) string {
	detail := err.Error()
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// an OpError can wrap another; only the innermost cause says why
		cause := opErr.Err
		var innerErr *net.OpError
		for cause != nil && errors.As(cause, &innerErr) {
			cause = innerErr.Err
		}
		causeText := "unknown"
		if cause != nil {
			causeText = cause.Error()
		}
		detail = fmt.Sprintf("%s: %s", opErr.Op, causeText)
	}
	detail = extenderProbeDetailIpv6.ReplaceAllString(detail, "<ip>")
	detail = extenderProbeDetailIpv4.ReplaceAllString(detail, "<ip>")
	if detailRunes := []rune(detail); extenderProbeDiagnosticDetailMaxLength < len(detailRunes) {
		detail = string(detailRunes[:extenderProbeDiagnosticDetailMaxLength]) + "..."
	}
	return detail
}

// What a line reports of the address it is about, before the failure it
// reports is recorded.
type extenderProbeAddressState struct {
	activateTime             time.Time
	lastProbeSuccessTime     *time.Time
	consecutiveProbeFailures int
}

// Reads what a line reports of an address, nil when it cannot be read. A line
// is logged without it rather than not at all.
func getExtenderProbeAddressState(
	ctx context.Context,
	extenderId server.Id,
	ipVersion int,
) (state *extenderProbeAddressState) {
	readCtx, readCancel := context.WithTimeout(ctx, extenderProbeDiagnosticReadTimeout)
	defer readCancel()
	server.HandleError(func() {
		server.Db(readCtx, func(conn server.PgConn) {
			result, err := conn.Query(
				readCtx,
				`
				SELECT
					activate_time,
					last_probe_success_time,
					consecutive_probe_failures
				FROM network_extender_address
				WHERE extender_id = $1 AND ip_version = $2
				`,
				extenderId,
				ipVersion,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					found := &extenderProbeAddressState{}
					server.Raise(result.Scan(
						&found.activateTime,
						&found.lastProbeSuccessTime,
						&found.consecutiveProbeFailures,
					))
					state = found
				}
			})
		})
	})
	return
}

// The diagnostic line of one failed probe of target, as of now.
func extenderProbeDiagnosticLine(
	target *model.NetworkExtenderProbeTarget,
	serverName string,
	probeErr error,
	state *extenderProbeAddressState,
	now time.Time,
) string {
	class, detail := classifyExtenderProbeFailure(probeErr)
	elapsed := "?"
	connectTime := "?"
	serverBytes := "?"
	var socketErr *extenderProbeSocketError
	if errors.As(probeErr, &socketErr) {
		elapsed = socketErr.elapsed.Round(time.Millisecond).String()
		connectTime = "none"
		if socketErr.connected {
			connectTime = socketErr.connectTime.Round(time.Millisecond).String()
		}
		serverBytes = strconv.FormatInt(socketErr.serverBytes, 10)
	}
	sinceActivation := "?"
	sinceUptimeOk := "?"
	failuresBefore := "?"
	if state != nil {
		sinceActivation = now.Sub(state.activateTime).Round(time.Second).String()
		sinceUptimeOk = "never"
		if state.lastProbeSuccessTime != nil {
			sinceUptimeOk = now.Sub(*state.lastProbeSuccessTime).Round(time.Second).String()
		}
		failuresBefore = strconv.Itoa(state.consecutiveProbeFailures)
	}
	return fmt.Sprintf(
		"[extenderprobe]diagnostic extender=%s ipv=%d class=%s detail=%q server_name=%s elapsed=%s connect=%s server_bytes=%s since_activation=%s since_uptime_ok=%s failures_before=%s",
		target.ExtenderId,
		target.IpVersion,
		class,
		detail,
		serverName,
		elapsed,
		connectTime,
		serverBytes,
		sinceActivation,
		sinceUptimeOk,
		failuresBefore,
	)
}

// The redis key that holds an address's diagnostic line for an interval.
func extenderProbeDiagnosticKey(extenderId server.Id, ipVersion int) string {
	return fmt.Sprintf("extender_probe_diagnostic_%s_%d", extenderId, ipVersion)
}

// Claims the one diagnostic line an address may log in an interval, over
// every taskworker: the first failure in the interval sets the address's key,
// and every later one finds it set. It runs on the deadline pool, since the
// ordinary one holds a stalled redis to its own 15 s whatever the context says.
// A claim that fails logs nothing, rather than every failure.
func claimExtenderProbeDiagnostic(
	ctx context.Context,
	extenderId server.Id,
	ipVersion int,
) bool {
	claimCtx, claimCancel := context.WithTimeout(ctx, extenderProbeDiagnosticClaimTimeout)
	defer claimCancel()
	claimed := false
	err := server.RedisWithDeadline(
		claimCtx,
		func(r server.RedisClient) error {
			var err error
			claimed, err = r.SetNX(
				claimCtx,
				extenderProbeDiagnosticKey(extenderId, ipVersion),
				"1",
				extenderProbeDiagnosticInterval,
			).Result()
			return err
		},
		server.OptRedisNoPreflight(),
	)
	return err == nil && claimed
}

// Logs how one uptime probe of target failed, when the address has not logged
// a line within the interval. Whatever happens here is contained, so a
// diagnostic can never cost a probe its result.
func logExtenderProbeFailure(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	serverName string,
	probeErr error,
	now time.Time,
) {
	server.HandleError(func() {
		if !claimExtenderProbeDiagnostic(ctx, target.ExtenderId, target.IpVersion) {
			return
		}
		state := getExtenderProbeAddressState(ctx, target.ExtenderId, target.IpVersion)
		glog.Infof("%s\n", extenderProbeDiagnosticLine(target, serverName, probeErr, state, now))
	})
}
