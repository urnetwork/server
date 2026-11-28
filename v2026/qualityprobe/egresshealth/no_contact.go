// A load that never reached its provider is no measurement of the site it was
// sent to. One definition serves the URL common-mode guard, the site tally and
// the pool refresh's prober-fault line, which reads it back from stored URL
// evidence.
package egresshealth

import (
	"slices"
)

// Final stages of an attempt that ended before any destination answered: the
// target's DNS lookup through the tunnel, or a TCP connect through it that
// never completed, whichever clock saw the attempt end there, and the
// combined stage of a dial that could not tell the two apart. An offline or
// unreachable provider ends here, and so does a platform failure on the
// tunnel path, e.g. contract setup on both tunnel legs consuming the URL DNS
// phase. TLS, policy, write, response and generic timeout stages are left
// out: a peer answered, or the phase is unknown.
var noContactFailureStages = [...]string{"dial_dns", "dial_dns_or_socket", "request_dns_timeout", "dial_tcp", "request_dial_timeout"}

// A copy of the no-contact stages, for a reader of stored evidence that
// cannot call NoContactFailure.
func NoContactFailureStages() []string {
	return slices.Clone(noContactFailureStages[:])
}

// Whether the load failed without reaching its provider: its last attempt
// ended in a no-contact stage, and nothing answered through the tunnel first
// -- no status, no response byte, no redirect, no TLS verdict and no security
// evidence. Whether the load was measured, or was a canary, is the caller's to
// weigh.
func (self *CheckResult) NoContactFailure() bool {
	return !self.Ok && self.StatusCode == 0 && !self.FirstByteReceived && self.RedirectCount == 0 &&
		!self.TlsAuthenticationFailure && len(self.UrlProbeSecurity) == 0 &&
		slices.Contains(noContactFailureStages[:], self.FailureStage)
}
