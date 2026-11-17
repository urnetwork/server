package work

// The uptime prober's own control target (connect/EXTENDER.md C3).
//
// Before and after each pass the prober completes a tls handshake with the
// operator's own api host -- the destination every probe names -- over each
// family it is about to judge, through the same dial settings the probes use.
// That is the one destination the prober can always expect to reach: if it
// cannot, over a family, the prober has lost its own path, and none of that
// family's failures in the pass are the extenders'
// (extender_probe_common_mode.go).
//
// The check is careful about what it claims:
//
//   - the host is resolved first, and a lookup that fails or names no address
//     of the family says nothing: the probes dial addresses, not names, so a
//     resolver fault is not theirs, and a host with no AAAA has no IPv6
//     control;
//   - up to extenderProbeControlAddressCount of the host's addresses are
//     tried, and any one answering is a pass, so one api host down for
//     maintenance is not a prober fault;
//   - a failure holds a pass only once the family's control has answered in
//     this process. A control that cannot answer from where the prober runs,
//     whatever the reason, must not stop every revocation forever; it shows
//     as failed in the control counter and holds nothing.
//
// The control is no proof the prober works, only a cheap refusal to judge
// when it plainly does not. The failures' own spread is the other half.

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/urnetwork/server/controller"
)

// What one control check of one family found.
const (
	// the handshake completed with one of the host's addresses
	extenderProbeControlOk = "ok"
	// every address tried failed
	extenderProbeControlFailed = "failed"
	// the host resolves to no address of the family
	extenderProbeControlAbsent = "absent"
	// nothing was learned: the host did not resolve, or the pass ended
	extenderProbeControlUnknown = "unknown"
)

const (
	// The host's addresses one check tries, in the resolver's order.
	extenderProbeControlAddressCount = 3
	// One address's connect and handshake.
	extenderProbeControlDialTimeout = 5 * time.Second
	// One family's whole check, the lookup included.
	extenderProbeControlTimeout = 12 * time.Second
)

// Checks the control target over one family: one of the host's addresses of
// that family completes a tls handshake for the host. A variable so a test can
// answer it without a network; production never replaces it.
var probeExtenderProbeControl = func(ctx context.Context, host string, ipVersion int) (string, error) {
	checkCtx, checkCancel := context.WithTimeout(ctx, extenderProbeControlTimeout)
	defer checkCancel()

	lookupNetwork := "ip4"
	dialNetwork := "tcp4"
	if ipVersion == 6 {
		lookupNetwork = "ip6"
		dialNetwork = "tcp6"
	}
	ips, err := net.DefaultResolver.LookupNetIP(checkCtx, lookupNetwork, host)
	if err != nil {
		var dnsErr *net.DNSError
		if errors.As(err, &dnsErr) && dnsErr.IsNotFound && ctx.Err() == nil {
			return extenderProbeControlAbsent, nil
		}
		return extenderProbeControlUnknown, errors.New(extenderProbeControlErrorText(err))
	}
	if len(ips) == 0 {
		return extenderProbeControlAbsent, nil
	}

	var lastErr error
	for _, ip := range ips[:min(len(ips), extenderProbeControlAddressCount)] {
		if err := dialExtenderProbeControl(checkCtx, dialNetwork, ip.Unmap(), host); err == nil {
			return extenderProbeControlOk, nil
		} else {
			lastErr = errors.New(extenderProbeControlErrorText(err))
		}
	}
	if ctx.Err() != nil {
		return extenderProbeControlUnknown, lastErr
	}
	return extenderProbeControlFailed, lastErr
}

// One address's connect and tls handshake for host, verified as a client
// verifies the api.
func dialExtenderProbeControl(ctx context.Context, network string, ip netip.Addr, host string) error {
	dialCtx, dialCancel := context.WithTimeout(ctx, extenderProbeControlDialTimeout)
	defer dialCancel()
	settings := controller.ExtenderProbeConnectSettings()
	conn, err := settings.DialContext(dialCtx, network, net.JoinHostPort(ip.String(), "443"))
	if err != nil {
		return err
	}
	defer conn.Close()
	tlsConfig := &tls.Config{}
	if settings.TlsConfig != nil {
		tlsConfig = settings.TlsConfig.Clone()
	}
	tlsConfig.ServerName = host
	tlsConn := tls.Client(conn, tlsConfig)
	defer tlsConn.Close()
	return tlsConn.HandshakeContext(dialCtx)
}

// What a failed control dial says, with no address in it: the operator's own
// addresses, and the prober's, but a log line of the probe names none. A
// socket error is its operation and cause without the ends it names.
func extenderProbeControlErrorText(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// the resolver's address is in the full text
		return fmt.Sprintf("lookup: %s", dnsErr.Err)
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Err != nil {
		cause := opErr.Err
		var innerErr *net.OpError
		for errors.As(cause, &innerErr) && innerErr.Err != nil {
			cause = innerErr.Err
		}
		return fmt.Sprintf("%s: %s", opErr.Op, cause)
	}
	return err.Error()
}

// One family's control check, as a pass reads it.
type extenderProbeControlCheck struct {
	result string
	// the control failed after it had answered in this process: the prober
	// cannot make the family's pass
	holds bool
	// what the failure said, with no address in it, for the log
	detail string
}

// The families whose control has answered in this process. A pass that lands
// on a fresh process proves its control again before a failure of it holds.
type extenderProbeControlState struct {
	stateLock sync.Mutex
	proven    map[int]bool
}

func newExtenderProbeControlState() *extenderProbeControlState {
	return &extenderProbeControlState{proven: map[int]bool{}}
}

// The control state of this process.
var extenderProbeControls = newExtenderProbeControlState()

// Checks the control over each family, the families at once.
func (self *extenderProbeControlState) check(
	ctx context.Context,
	host string,
	ipVersions []int,
) map[int]*extenderProbeControlCheck {
	checks := map[int]*extenderProbeControlCheck{}
	checksLock := sync.Mutex{}
	wait := sync.WaitGroup{}
	for _, ipVersion := range ipVersions {
		wait.Go(func() {
			result, err := probeExtenderProbeControl(ctx, host, ipVersion)
			if ctx.Err() != nil {
				// the pass ended under the check, which learned nothing
				result = extenderProbeControlUnknown
			}
			check := &extenderProbeControlCheck{result: result}
			if err != nil {
				check.detail = err.Error()
			}
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				switch result {
				case extenderProbeControlOk:
					self.proven[ipVersion] = true
				case extenderProbeControlFailed:
					check.holds = self.proven[ipVersion]
				}
			}()
			checksLock.Lock()
			defer checksLock.Unlock()
			checks[ipVersion] = check
		})
	}
	wait.Wait()
	return checks
}
