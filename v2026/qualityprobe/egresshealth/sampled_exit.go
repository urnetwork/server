// Location evidence is opportunistic and never adds a request to the sample.
package egresshealth

import (
	"net/netip"
	"strings"
	"time"
)

// Canonicalizes independently observed address text using production policy.
func publicSampledExit(value string) string {
	return sampledExitWithPolicy(value, nil)
}

// A package-private predicate lets synthetic tests use documentation ranges
// without weakening the production special-purpose exclusion.
func sampledExitWithPolicy(value string, allowed func(netip.Addr) bool) string {
	ip, err := netip.ParseAddr(strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	ip = ip.Unmap()
	if allowed == nil {
		allowed = publicExitAddress
	}
	if !allowed(ip) {
		return ""
	}
	return ip.String()
}

// Conservatively excludes special-purpose assignments, including globally
// reachable infrastructure anycast, because none is provider exit evidence.
// Source: IANA IPv4/IPv6 Special-Purpose Address Registries (2025-10-09).
var nonExitPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"), netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3ffe::/16"),
	netip.MustParsePrefix("3fff::/20"),
}

// IPv6 exit evidence must be ordinary global allocation, not translation,
// documentation, benchmarking, loopback, link-local or private address space.
func publicExitAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.Zone() != "" {
		return false
	}
	if ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range nonExitPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

// Every valid sampled observation must agree. An empty/conflicting observation
// leaves quality intact and location absent; no majority vote is fabricated.
func collectSampledExit(result *Result) {
	for _, check := range result.Checks {
		if !check.Ok || check.ObservedExitIp == "" || check.ExitObservedAt.IsZero() {
			continue
		}
		if result.ExitIp != "" && result.ExitIp != check.ObservedExitIp {
			result.ExitIp = ""
			// The zero timestamp prevents a location write after disagreement.
			result.ExitObservedAt = time.Time{}
			return
		}
		if result.ExitIp == "" || check.ExitObservedAt.Before(result.ExitObservedAt) {
			result.ExitIp = check.ObservedExitIp
			result.ExitObservedAt = check.ExitObservedAt
		}
	}
}
