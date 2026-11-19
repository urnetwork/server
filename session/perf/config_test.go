package perf

import (
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"
)

// The production shapes the steady phase reproduces, as rates per second.
// Sources (2026-11-18 Main measurements, recorded with the harness README):
//   - authclient: POST /network/auth-client, about 150 a second that mint
//   - child: hosted derived child mints by the proxies' local authority, about
//     223 a second, each inheriting its parent's session
//   - refresh: /auth/refresh and /auth/network-refresh, about 6 a second
//   - signin: successful password sign-ins, about 1.6 a second
//   - hotproxy: one network that creates and removes 7,300 proxies a day,
//     compressed by the profile's compression factor
//   - code: a code-login fleet at 96 redemptions a day, compressed likewise
//   - api: API request authentication; the api serves about 10k requests a
//     second fleet-wide, scaled here to what one machine drives
//   - lease: connect's periodic credential rechecks, the bulk of the fleet's
//     record checks, scaled the same way
//   - hint: connect's session revision reads for notification hints
//   - list: Sessions screen loads of a large network
//   - revoke: single session revocations
var productionRates = map[string]float64{
	"authclient": 150,
	"child":      223,
	"refresh":    6,
	"signin":     1.6,
	"hotproxy":   7300.0 / 86400.0,
	"code":       96.0 / 86400.0,
	"api":        10000,
	"lease":      14700,
	"hint":       5300,
	"list":       0.05,
	"revoke":     0.05,
}

type perfProfile struct {
	name string
	// how long the steady production-shaped phase runs
	steadyDuration time.Duration
	// compression for the hot proxy network and the code fleet: their daily
	// volume arrives this many times faster than production
	compression float64
	// the check, hint and list rates the machine drives; production's are
	// fleet-wide and far above one machine
	rates map[string]float64
	// how long a client the steady phase creates lives before its removal
	clientLifetime time.Duration
	// the hot network's proxy lifetime, compressed like its create rate
	proxyLifetime time.Duration
	// isolated per-operation samples
	unitSamples   int
	unitCheckRuns int
	// the large networks
	largeSessions int
	// the index outbox backlog sizes the repair is timed at
	outboxSeeds []int
	// the working set of credentials the checks draw from
	checkNetworks          int
	checkClientsPerNetwork int
	// whether the sign-out-all storm runs on a second large network
	signOutAll bool
}

func quickProfile() *perfProfile {
	return &perfProfile{
		name:           "quick",
		steadyDuration: 60 * time.Second,
		compression:    120,
		rates: map[string]float64{
			"authclient": 40,
			"child":      40,
			"refresh":    6,
			"signin":     1.6,
			"api":        800,
			"lease":      800,
			"hint":       100,
			"list":       0.2,
			"revoke":     0.2,
		},
		clientLifetime:         20 * time.Second,
		proxyLifetime:          30 * time.Second,
		unitSamples:            200,
		unitCheckRuns:          5000,
		largeSessions:          5000,
		outboxSeeds:            []int{10000},
		checkNetworks:          100,
		checkClientsPerNetwork: 10,
		signOutAll:             false,
	}
}

func standardProfile() *perfProfile {
	return &perfProfile{
		name:           "standard",
		steadyDuration: 300 * time.Second,
		compression:    120,
		rates: map[string]float64{
			"authclient": 150,
			"child":      223,
			"refresh":    6,
			"signin":     1.6,
			"api":        2500,
			"lease":      2500,
			"hint":       300,
			"list":       0.2,
			"revoke":     0.2,
		},
		clientLifetime:         60 * time.Second,
		proxyLifetime:          60 * time.Second,
		unitSamples:            1000,
		unitCheckRuns:          20000,
		largeSessions:          5000,
		outboxSeeds:            []int{10000, 100000},
		checkNetworks:          200,
		checkClientsPerNetwork: 20,
		signOutAll:             true,
	}
}

type perfConfig struct {
	// one master of the disposable cluster, host:port
	clusterAuthority string
	// the master count run.sh started; any other count is refused
	clusterNodes int
	outDir       string
	label        string
	profile      *perfProfile
	// SESSION_PERF_PHASES selects phases by name; empty runs all
	phases map[string]bool
}

// Reads the harness configuration. ok is false when no disposable cluster is
// named, and the test skips.
func loadPerfConfig() (*perfConfig, bool, error) {
	authority := strings.TrimSpace(os.Getenv("SESSION_PERF_REDIS_CLUSTER"))
	if authority == "" {
		return nil, false, nil
	}
	if err := refuseTunnelEndpoint(authority); err != nil {
		return nil, true, err
	}
	nodes, err := strconv.Atoi(os.Getenv("SESSION_PERF_REDIS_NODES"))
	if err != nil || nodes < 3 || 6 < nodes {
		return nil, true, fmt.Errorf("SESSION_PERF_REDIS_NODES must name the 3 to 6 masters run.sh started")
	}
	config := &perfConfig{
		clusterAuthority: authority,
		clusterNodes:     nodes,
		outDir:           os.Getenv("SESSION_PERF_OUT"),
		label:            os.Getenv("SESSION_PERF_LABEL"),
		phases:           map[string]bool{},
	}
	switch os.Getenv("SESSION_PERF_PROFILE") {
	case "", "standard":
		config.profile = standardProfile()
	case "quick":
		config.profile = quickProfile()
	default:
		return nil, true, fmt.Errorf("SESSION_PERF_PROFILE must be quick or standard")
	}
	if value := os.Getenv("SESSION_PERF_STEADY_SECONDS"); value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 10 {
			return nil, true, fmt.Errorf("SESSION_PERF_STEADY_SECONDS must be at least 10")
		}
		config.profile.steadyDuration = time.Duration(seconds) * time.Second
	}
	// SESSION_PERF_RATES=api=4000,lease=3000 overrides single rates
	if value := os.Getenv("SESSION_PERF_RATES"); value != "" {
		for _, pair := range strings.Split(value, ",") {
			name, rate, found := strings.Cut(strings.TrimSpace(pair), "=")
			parsed, err := strconv.ParseFloat(rate, 64)
			if !found || err != nil || parsed < 0 {
				return nil, true, fmt.Errorf("SESSION_PERF_RATES entry %q is not name=rate", pair)
			}
			config.profile.rates[name] = parsed
		}
	}
	for _, phase := range strings.Split(os.Getenv("SESSION_PERF_PHASES"), ",") {
		if phase = strings.TrimSpace(phase); phase != "" {
			config.phases[phase] = true
		}
	}
	return config, true, nil
}

func (self *perfConfig) runs(phase string) bool {
	return len(self.phases) == 0 || self.phases[phase]
}

// The loopback ports a production data tunnel forwards: PostgreSQL, PgBouncer
// and Main's Redis cluster nodes. The harness never names one, on any host,
// and never a loopback address at all: the disposable cluster listens on the
// local launcher's dedicated address.
func refuseTunnelEndpoint(authority string) error {
	addrPort, err := netip.ParseAddrPort(authority)
	if err != nil {
		host, port, splitErr := net.SplitHostPort(authority)
		if splitErr != nil {
			return fmt.Errorf("%q is not host:port", authority)
		}
		return fmt.Errorf("%s:%s must be an address literal", host, port)
	}
	port := addrPort.Port()
	if port == 5432 || port == 6432 || (6379 <= port && port <= 6427) {
		return fmt.Errorf("%s is on a production data tunnel port; the disposable cluster uses other ports", authority)
	}
	if addrPort.Addr().IsLoopback() || addrPort.Addr().IsUnspecified() {
		return fmt.Errorf("%s is loopback, where a production data tunnel can listen; use the launcher's dedicated address", authority)
	}
	return nil
}
