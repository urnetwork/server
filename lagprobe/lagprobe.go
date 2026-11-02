// Package lagprobe checks that an extender's lag.dev measurement api answers
// a browser on lag.dev (lag.dev/PLAN.md M1, M2, A4).
//
// One probe is exactly what a page needs before it can open a session, at one
// of the addresses the extender's measurement name publishes:
//
//   - a tcp connection to the address;
//   - a tls handshake with the name as SNI that verifies the name's
//     certificate, a Let's Encrypt one, against the host's roots, and agrees
//     on http/1.1, the only protocol the extender offers (M1);
//   - `GET /lag/v1/session` from the lag.dev origin, answered 400 with that
//     origin allowed for cors (M2). A get that is no websocket upgrade is
//     refused before any ticket is read, so the probe opens no session, costs
//     the extender no admission and carries no ticket.
//
// The outcome names the first step that failed, so a reader can tell an
// extender that stalls, accepting tcp but never finishing tls (tls_timeout),
// from one that refuses the connection (dial), presents another certificate
// (tls), answers another status (status, as 421 for a name it no longer
// serves) or leaves the origin out (cors).
//
// It is network code only: no database, no configuration. The taskworker's
// probe chain (taskworker/work/extender_measurement_probe_work.go) stores the
// outcomes, and the lag.dev sample reads them (controller/lag_extender_reach.go).
package lagprobe

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sync"
	"time"
)

// What one probe found.
type Outcome string

const (
	// the api answered as a page needs it to
	OutcomeOk Outcome = "ok"
	// this host cannot reach the address's family at all, so the probe says
	// nothing about the extender (ProbeAll)
	OutcomeUnprobed Outcome = "unprobed"
	// the tcp connection failed: refused, unreachable, or not made within the
	// handshake timeout
	OutcomeDial Outcome = "dial"
	// the tls handshake failed: the certificate (its chain, its name, its
	// dates), an alert, a reset, or a protocol other than http/1.1
	OutcomeTls Outcome = "tls"
	// the tcp connection was made but the handshake did not finish within the
	// handshake timeout: an extender that stalls
	OutcomeTlsTimeout Outcome = "tls_timeout"
	// the response failed or was not http: the connection reset or closed
	// before a response, or the bytes did not parse
	OutcomeHttp Outcome = "http"
	// no response within the response timeout
	OutcomeHttpTimeout Outcome = "http_timeout"
	// a response with a status other than 400
	OutcomeStatus Outcome = "status"
	// a 400 that does not allow the lag.dev origin
	OutcomeCors Outcome = "cors"
)

// Whether the outcome is a failure of the extender's api, as opposed to a
// success or a probe that could not be made.
func (self Outcome) Failed() bool {
	return self != OutcomeOk && self != OutcomeUnprobed
}

// What a probe asks and how long it waits.
type Settings struct {
	// from the start of the tcp connection to the end of the tls handshake
	HandshakeTimeout time.Duration
	// from the request to the response's status and headers
	ResponseTimeout time.Duration
	// the page origin the request comes from, which the response must allow
	Origin string
	// the api route asked, which answers 400 to a get that is no upgrade
	Path string
	// the roots a certificate must chain to; nil is the host's roots, where
	// Let's Encrypt's are (lag.dev/PLAN.md C)
	RootCAs *x509.CertPool
	// probes at once in ProbeAll
	Concurrency int
	// ProbeAll starts no probe after this long
	Budget time.Duration
	// ProbeAll takes a family more than half of whose probes failed, at least
	// this many of them, to be one this host cannot reach
	FamilyQuorum int
}

// The plan's probe (lag.dev/PLAN.md A4): 5 s for the handshake and 5 s for
// the response, 16 at once, and a minute to start them in.
//
// A healthy extender, even one a whole round trip of a second away, makes the
// connection, the handshake and the response in about four round trips; one
// that stalls answers neither in 5 s nor in 15 min. The concurrency and the
// budget bound one pass at 16 sockets and under 70 s however many extenders
// there are, inside the task's default two-minute limit; at the 30 to 60
// serving extenders of 2026-11, about 60 to 100 addresses, a pass takes a few
// seconds. Most of a family failing at once, and at least three of it, is not
// something extenders on separate networks do, and is what this host losing
// the family looks like (ProbeAll).
func DefaultSettings() Settings {
	return Settings{
		HandshakeTimeout: 5 * time.Second,
		ResponseTimeout:  5 * time.Second,
		Origin:           "https://lag.dev",
		Path:             "/lag/v1/session",
		Concurrency:      16,
		Budget:           60 * time.Second,
		FamilyQuorum:     3,
	}
}

// One address of one measurement name.
type Target struct {
	// the measurement name (lag.dev/PLAN.md N1): the SNI, the certificate's
	// name and the request's host
	Name string
	// a published address of the name and the api's port, 443
	Addr netip.AddrPort
}

// The family of the target's address, 4 or 6.
func (self Target) IpVersion() int {
	if self.Addr.Addr().Unmap().Is4() {
		return 4
	}
	return 6
}

// What one probe found, and why when it failed.
type Result struct {
	Outcome Outcome
	Err     error
}

// The user agent of the request. Nothing on the extender reads it; it names
// the probe to anyone looking at the traffic.
const userAgent = "urnetwork-measurement-probe/1"

// The most of a response read: its status line and headers. The extender is
// untrusted, and net/http's parser grows one line or a header map without bound
// for as long as bytes arrive, so a deadline alone lets one modified extender
// on a gigabit port fill this process's memory: 3 GB in 17 s, measured. The
// refusal's head is a few hundred bytes. connect/net_extender.go guards the same
// call the same way.
const responseHeadMaxBytes = 16 * 1024

// Probe makes one probe of the target. It returns no outcome, and ctx's
// error, when ctx ends before the probe has one: a probe cut short by the
// prober says nothing about the extender.
func Probe(ctx context.Context, target Target, settings Settings) (Outcome, error) {
	handshakeCtx, handshakeCancel := context.WithTimeout(ctx, settings.HandshakeTimeout)
	defer handshakeCancel()

	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(handshakeCtx, "tcp", target.Addr.String())
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return OutcomeDial, err
	}
	defer conn.Close()

	tlsConn := tls.Client(conn, &tls.Config{
		ServerName: target.Name,
		NextProtos: []string{"http/1.1"},
		RootCAs:    settings.RootCAs,
		MinVersion: tls.VersionTLS12,
	})
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if handshakeCtx.Err() != nil || isTimeout(err) {
			return OutcomeTlsTimeout, err
		}
		return OutcomeTls, err
	}
	if protocol := tlsConn.ConnectionState().NegotiatedProtocol; protocol != "http/1.1" {
		return OutcomeTls, fmt.Errorf("the handshake agreed on %q rather than http/1.1", protocol)
	}

	// the response's deadline, brought forward if ctx ends first
	if err := tlsConn.SetDeadline(time.Now().Add(settings.ResponseTimeout)); err != nil {
		return OutcomeHttp, err
	}
	stop := context.AfterFunc(ctx, func() {
		tlsConn.SetDeadline(time.Now())
	})
	defer stop()

	request := &http.Request{
		Method: http.MethodGet,
		URL: &url.URL{
			Scheme: "https",
			Host:   target.Name,
			Path:   settings.Path,
		},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header: http.Header{
			"Origin":     []string{settings.Origin},
			"User-Agent": []string{userAgent},
		},
		Host:  target.Name,
		Close: true,
	}
	if err := request.Write(tlsConn); err != nil {
		return responseFailure(ctx, err)
	}
	response, err := http.ReadResponse(bufio.NewReader(io.LimitReader(tlsConn, responseHeadMaxBytes)), request)
	if err != nil {
		return responseFailure(ctx, err)
	}
	// the body is never read or closed: the refusal has none, nothing in one
	// would change the outcome, closing one drains it until the deadline, and
	// the connection closes on return

	if response.StatusCode != http.StatusBadRequest {
		return OutcomeStatus, fmt.Errorf("answered %d rather than 400", response.StatusCode)
	}
	if origins := response.Header.Values("Access-Control-Allow-Origin"); len(origins) != 1 || origins[0] != settings.Origin {
		return OutcomeCors, fmt.Errorf("allowed the origins %q rather than %s", origins, settings.Origin)
	}
	return OutcomeOk, nil
}

// The outcome of a request or response that failed: none when ctx ended,
// else a timeout or another failure of http.
func responseFailure(ctx context.Context, err error) (Outcome, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if isTimeout(err) {
		return OutcomeHttpTimeout, err
	}
	return OutcomeHttp, err
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

// Whether this host has a route to addr. Connecting a udp socket sends
// nothing: it only looks the route up, so it fails at once for a family this
// host has no route for, as for IPv6 in a container without it. A variable so
// a test can take a family away.
var hasRoute = func(addr netip.Addr) bool {
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(addr, 443)))
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// The error of a probe this host could not make.
var errNoRoute = errors.New("this host has no route to the address")

// What one pass found: Results[i] answers targets[i], nil when the target was
// not probed, because the budget ran out or ctx ended first. A panic while
// probing an address is that address's failure.
type Pass struct {
	Results []*Result
	// the families most of whose probes failed, at least the quorum of them,
	// whose failures were recorded unprobed (applyFamilyQuorum)
	UnreachableIpVersions []int
}

// ProbeAll probes every target, at most settings.Concurrency at once, and
// starts none after settings.Budget or once ctx ends. To probe part of a family
// again, alreadySucceeded counts by family, 4 or 6, the addresses that succeeded
// in its pass and are not among the targets. A family judged only on what failed
// would always look like most of it had. It is nil for a whole pass.
//
// An address this host has no route to is not probed but recorded unprobed:
// that says nothing about the extender. So is every failure of a family more
// than half of whose probes failed, at least settings.FamilyQuorum of them:
// extenders on separate networks do not stop together, and this host losing
// the family, its network for a moment, or the roots that verify the
// certificates, looks exactly like that. One such moment, probing from a
// laptop, failed 17 of 22 IPv4 addresses and all 15 IPv6 ones in one pass, with
// the 5 that connected first succeeding. Such a family's extenders are left to
// their other family, or offered.
func ProbeAll(ctx context.Context, targets []Target, alreadySucceeded map[int]int, settings Settings) *Pass {
	results := make([]*Result, len(targets))
	budgetEnd := time.Now().Add(settings.Budget)
	workers := make(chan struct{}, max(settings.Concurrency, 1))
	wait := sync.WaitGroup{}
targets:
	for i, target := range targets {
		select {
		case workers <- struct{}{}:
		case <-ctx.Done():
			break targets
		}
		if ctx.Err() != nil || !time.Now().Before(budgetEnd) {
			<-workers
			break
		}
		wait.Add(1)
		go func() {
			defer func() {
				<-workers
				wait.Done()
			}()
			// the peer is untrusted, the parsers run on its bytes, and a panic in
			// a goroutine of ours would take the whole worker down
			defer func() {
				if recovered := recover(); recovered != nil {
					results[i] = &Result{Outcome: OutcomeHttp, Err: fmt.Errorf("the probe panicked: %v", recovered)}
				}
			}()
			if !hasRoute(target.Addr.Addr()) {
				results[i] = &Result{Outcome: OutcomeUnprobed, Err: errNoRoute}
				return
			}
			outcome, err := Probe(ctx, target, settings)
			if outcome == "" {
				// cut short by ctx
				return
			}
			results[i] = &Result{Outcome: outcome, Err: err}
		}()
	}
	wait.Wait()

	return &Pass{
		Results:               results,
		UnreachableIpVersions: applyFamilyQuorum(targets, results, alreadySucceeded, settings.FamilyQuorum),
	}
}

// Records unprobed every failure of a family more than half of whose probes
// failed, at least quorum of them, and returns those families. A probe with no
// result, or that was already unprobed, counts for neither side, and the
// alreadySucceeded of the family count as successes.
func applyFamilyQuorum(targets []Target, results []*Result, alreadySucceeded map[int]int, quorum int) []int {
	unreachable := []int{}
	for _, ipVersion := range []int{4, 6} {
		failed := 0
		succeeded := alreadySucceeded[ipVersion]
		for i, result := range results {
			if result == nil || targets[i].IpVersion() != ipVersion {
				continue
			}
			switch {
			case result.Outcome == OutcomeOk:
				succeeded += 1
			case result.Outcome.Failed():
				failed += 1
			}
		}
		if failed <= succeeded || failed < max(quorum, 1) {
			continue
		}
		unreachable = append(unreachable, ipVersion)
		for i, result := range results {
			if result == nil || targets[i].IpVersion() != ipVersion || !result.Outcome.Failed() {
				continue
			}
			results[i] = &Result{
				Outcome: OutcomeUnprobed,
				Err: fmt.Errorf(
					"%d of this pass's %d ipv%d probes failed, so this host is taken not to reach ipv%d (%s: %w)",
					failed,
					failed+succeeded,
					ipVersion,
					ipVersion,
					result.Outcome,
					result.Err,
				),
			}
		}
	}
	return unreachable
}
