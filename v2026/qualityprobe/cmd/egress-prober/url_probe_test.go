// The standalone command shares the configured, one-URL production workflow.
package main

import (
	"context"
	"flag"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/qualityprobe/confinement"
	"github.com/urnetwork/server/v2026/qualityprobe/egresshealth"
)

var testProbeHosts = []string{"site-a.example", "site-b.example", "site-c.example"}

// Existing confinement cases receive the same explicit synthetic catalog.
func checkTestConfinement(ctx context.Context, dial confinement.DialFunc, lookup confinement.LookupFunc, explicitAddrs []string, timeout time.Duration, extraHosts ...string) error {
	hosts := append(append([]string(nil), testProbeHosts...), extraHosts...)
	return checkConfinement(ctx, dial, lookup, explicitAddrs, timeout, hosts...)
}

// Obsolete modes fail before credentials, discovery, or provider measurement.
func TestUrlProbeFlagsRejectObsoleteMeasurements(t *testing.T) {
	for _, name := range []string{"blackhole-interval", "blackhole-limit", "egress-health-all", "bandwidth-timeout", "skip-bandwidth", "cache-ttl"} {
		flags := flag.NewFlagSet("synthetic", flag.ContinueOnError)
		flags.String(name, "", "")
		if err := flags.Parse([]string{"-" + name, "synthetic"}); err != nil {
			t.Fatal(err)
		}
		if err := validateUrlProbeFlags(flags); err == nil {
			t.Errorf("obsolete flag %s accepted", name)
		}
	}
}

// Confinement covers both catalog scopes and never adds a compiled hostname.
func TestPoolHostsUsesOnlyConfiguredGlobalAndCountryUrls(t *testing.T) {
	pool := &egresshealth.Pool{
		Destinations: []egresshealth.Destination{{Url: "https://global.example/"}},
		Countries: map[string][]egresshealth.Destination{
			"zz": {{Url: "https://country.example/"}, {Url: "https://global.example/another"}},
		},
	}
	hosts := poolHosts(pool)
	if len(hosts) != 2 || hosts[0] != "global.example" || hosts[1] != "country.example" || len(poolHosts(nil)) != 0 {
		t.Fatalf("configured confinement hosts=%v", hosts)
	}
}
