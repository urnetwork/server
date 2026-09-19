// DoH evidence has fixed enums only. Legacy events cannot establish a dial
// path, an attempted family, or a final logical resolver outcome.
package monitor

import (
	"fmt"
	"regexp"
	"strconv"
)

var dohDialObservationRe = regexp.MustCompile(
	`\[family\]dial tag=doh observable=v1 path=(unknown|host|caller|tun) attempted_family=(unknown|4|6) result=(success|canceled|timeout|unsupported_family|refused|error) resolver_owner=(unknown|host|caller|tun) resolver_scope=(unknown|address|forward|oneshot) resolver_outcome=(unknown|pending|answer|authoritative_empty|stale|failed|canceled|timeout)(?: err=(i/o timeout|connect: connection refused|address family not supported by protocol|context canceled|dial error))?[[:space:]]*$`,
)

// A fixed legacy suffix is optional for compatible producers, but when present
// must agree with the finite result. Arbitrary error strings are never accepted.
func parseDohDialObservation(line string) []string {
	match := dohDialObservationRe.FindStringSubmatch(line)
	if len(match) != 8 {
		return nil
	}
	if match[7] != "" {
		expected := ""
		switch match[3] {
		case "timeout":
			expected = "i/o timeout"
		case "refused":
			expected = "connect: connection refused"
		case "unsupported_family":
			expected = "address family not supported by protocol"
		case "canceled":
			expected = "context canceled"
		case "error":
			expected = "dial error"
		}
		if match[7] != expected {
			return nil
		}
	}
	return match
}

var dohResolverObservationRe = regexp.MustCompile(
	`\[doh\]resolver observable=v1 owner_path=(unknown|host|caller|tun) scope=(address|forward|oneshot) answer=([0-9]{1,20}) authoritative_empty=([0-9]{1,20}) stale=([0-9]{1,20}) failed=([0-9]{1,20}) canceled=([0-9]{1,20}) timeout=([0-9]{1,20})[[:space:]]*$`,
)

// Producer counters are unsigned 64-bit values, not arbitrary numeric text.
func dohResolverObserved(line string) bool {
	match := dohResolverObservationRe.FindStringSubmatch(line)
	if len(match) != 9 {
		return false
	}
	for _, counter := range match[3:] {
		if _, err := strconv.ParseUint(counter, 10, 64); err != nil {
			return false
		}
	}
	return true
}

// Exact new timeout records share the old service-wide page threshold. Other
// new terminal kinds are observations, never inferred timeouts from error text.
func dohDialTimeoutObserved(line string) bool {
	match := parseDohDialObservation(line)
	return match != nil && match[3] == "timeout"
}

// Success/cancellation and capability observations have no generic error rate.
// Refusals retain the existing refusal page; unknown failures retain novelty.
func dohDialInformational(line string) bool {
	match := parseDohDialObservation(line)
	if match == nil {
		return false
	}
	return match[3] == "success" || match[3] == "canceled" || match[3] == "unsupported_family"
}

// Discriminators remain finite; neither outer Warp metadata nor arbitrary
// trailing fields can become an alert sample. Legacy data stays unknown even
// when a policy, network name, or redacted endpoint appears to suggest a family.
func dohDialTimeoutSample(line string) string {
	match := parseDohDialObservation(line)
	if match != nil && match[3] == "timeout" {
		return fmt.Sprintf(
			"DoH resolver dial attempt: i/o timeout; path=%s attempted_family=%s resolver_owner=%s resolver_scope=%s resolver_outcome=%s (identity-free initiating-call evidence)",
			match[1], match[2], match[4], match[5], match[6],
		)
	}
	return "DoH resolver dial attempt: i/o timeout; path=unknown attempted_family=unknown resolver_outcome=unknown (legacy event; endpoint and correlation metadata omitted)"
}
