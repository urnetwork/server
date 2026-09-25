// Exact monitor observation exclusions preserve desired topology independently
// of the targets this process is authorized to contact.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Return a policy-only copy with sorted exact selectors. Unknown, malformed,
// and ambiguous names fail closed without rendering the supplied value.
func ExcludeHosts(settings SignalSettings, names ...string) (SignalSettings, error) {
	requestedHostNames := map[string]bool{}
	for _, name := range append(append([]string(nil), settings.ExcludedHosts...), names...) {
		if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "*?[]\r\n\x00") {
			return SignalSettings{}, fmt.Errorf("monitor: excluded host selector must be one nonempty exact name")
		}
		requestedHostNames[name] = true
	}
	for name := range requestedHostNames {
		matches := 0
		for _, configured := range settings.Hosts {
			if configured.Name == name {
				matches++
			}
		}
		for _, configured := range settings.Routers {
			if configured.Name == name {
				matches++
			}
		}
		for _, configured := range settings.disabledHosts {
			if configured.Name == name {
				matches++
			}
		}
		if matches == 0 {
			return SignalSettings{}, fmt.Errorf("monitor: excluded host is not configured")
		}
		if matches != 1 {
			return SignalSettings{}, fmt.Errorf("monitor: excluded host selector is ambiguous")
		}
	}
	if len(requestedHostNames) == 0 {
		return settings, nil
	}
	settings.ExcludedHosts = make([]string, 0, len(requestedHostNames))
	for name := range requestedHostNames {
		settings.ExcludedHosts = append(settings.ExcludedHosts, name)
	}
	sort.Strings(settings.ExcludedHosts)
	return settings, nil
}

// Fixed text excludes the selected host, endpoint, and command.
type hostScopeExcludedError struct{}

// Denial is observation policy, not authentication or workload failure.
func (self *hostScopeExcludedError) Error() string {
	return "monitor: inventory-target observation is intentionally excluded"
}

// Keep complete topology, but deny excluded inventory targets before both
// production and synthetic transports. Methods are safe for concurrent use;
// stateLock protects only private per-run blocked-name bookkeeping.
type hostScopeRunner struct {
	probeRunner
	cfg               *monitorConfig
	excludedHostNames map[string]bool
	endpointHostNames map[string][]string
	stateLock         sync.Mutex
	blockedHostNames  map[string]bool
}

// Build endpoint ownership from explicit inventory. DNS and whole-environment
// Warpctl queries do not redefine host ownership.
func newHostScopeRunner(transport probeRunner, cfg *monitorConfig, names []string) *hostScopeRunner {
	self := &hostScopeRunner{
		probeRunner: transport, cfg: cfg,
		excludedHostNames: map[string]bool{}, endpointHostNames: map[string][]string{},
		blockedHostNames: map[string]bool{},
	}
	for _, name := range names {
		self.excludedHostNames[name] = true
	}
	for _, configured := range cfg.scopeHosts() {
		if configured.disabled {
			self.excludedHostNames[configured.name] = true
		}
		endpoints := []string{configured.name, configured.lanIp, configured.overlayIp}
		endpoints = append(endpoints, configured.scopeEndpoints...)
		if cfg.publicDomain != "" {
			endpoints = append(endpoints, configured.name+"."+cfg.publicDomain)
		}
		for _, public := range configured.edgeIPv6 {
			endpoints = append(endpoints, public.Address)
		}
		for _, public := range configured.publicLB {
			endpoints = append(endpoints, public.IPv4Address, public.IPv6Address)
		}
		if configured.proxy != nil {
			endpoints = append(endpoints, configured.proxy.PublicHostname)
		}
		for _, endpoint := range endpoints {
			if normalized := normalizeHostScopeEndpoint(endpoint); normalized != "" {
				self.endpointHostNames[normalized] = append(self.endpointHostNames[normalized], configured.name)
			}
		}
	}
	for _, target := range cfg.publicUdp.Targets {
		for _, endpoint := range []string{target.IPv4Address, target.IPv6Address} {
			if normalized := normalizeHostScopeEndpoint(endpoint); normalized != "" {
				self.endpointHostNames[normalized] = append(self.endpointHostNames[normalized], target.Host)
			}
		}
	}
	return self
}

// Normalize numeric addresses and DNS case for transport ownership only;
// command-line selectors still use exact inventory-name equality.
func normalizeHostScopeEndpoint(endpoint string) string {
	if address, err := netip.ParseAddr(strings.Trim(endpoint, "[]")); err == nil {
		return address.Unmap().String()
	}
	if strings.Contains(endpoint, "://") {
		parsed, err := url.Parse(endpoint)
		if err != nil {
			return ""
		}
		endpoint = parsed.Hostname()
	} else if name, _, err := net.SplitHostPort(endpoint); err == nil {
		endpoint = name
	} else if strings.ContainsAny(endpoint, "/@?#") {
		// Curl also accepts scheme-less URLs and credential-bearing proxies.
		parsed, err := url.Parse("//" + endpoint)
		if err != nil {
			return ""
		}
		endpoint = parsed.Hostname()
	}
	endpoint = strings.Trim(endpoint, "[]")
	if address, err := netip.ParseAddr(endpoint); err == nil {
		return address.Unmap().String()
	}
	return strings.ToLower(strings.TrimSuffix(endpoint, "."))
}

// Record private names, never commands, errors, addresses, or value digests.
func (self *hostScopeRunner) blockHosts(names []string) error {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		for _, name := range names {
			self.blockedHostNames[name] = true
		}
	}()
	return &hostScopeExcludedError{}
}

// Canceled work is lifecycle, not a new excluded-coverage event.
func (self *hostScopeRunner) guardHost(ctx context.Context, configured *host) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if configured != nil && self.excludedHostNames[configured.name] {
		return self.blockHosts([]string{configured.name})
	}
	return nil
}

// Named SSH-backed observations dial the selected address, not the logical
// name. Keep non-SSH logical admission separate for nested overlay checks.
func (self *hostScopeRunner) guardSshHost(ctx context.Context, configured *host) error {
	if err := self.guardHost(ctx, configured); err != nil {
		return err
	}
	if configured == nil {
		return nil
	}
	return self.guardEndpoint(ctx, configured.addr(self.cfg.addressMode))
}

// Shared raw endpoints are denied if any owner is excluded: an address alone
// cannot prove that a permitted logical host owns the transport destination.
func (self *hostScopeRunner) guardEndpoint(ctx context.Context, endpoint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var excludedOwners []string
	for _, name := range self.endpointHostNames[normalizeHostScopeEndpoint(endpoint)] {
		if self.excludedHostNames[name] {
			excludedOwners = append(excludedOwners, name)
		}
	}
	if len(excludedOwners) != 0 {
		return self.blockHosts(excludedOwners)
	}
	return nil
}

// The exported PostgreSQL seam omits the host argument; enforce production's
// first pg-primary authority before invoking either underlying transport.
func (self *hostScopeRunner) pg(ctx context.Context, query string) ([]pgRow, error) {
	if err := self.guardSshHost(ctx, self.cfg.hostByRole("pg-primary")); err != nil {
		return nil, err
	}
	return self.probeRunner.pg(ctx, query)
}

// Redis entry and raw commands share the named-host boundary.
func (self *hostScopeRunner) redis(ctx context.Context, configured *host, port int, args ...string) (string, error) {
	if err := self.guardSshHost(ctx, configured); err != nil {
		return "", err
	}
	return self.probeRunner.redis(ctx, configured, port, args...)
}

// Raw output does not bypass the host policy.
func (self *hostScopeRunner) redisRaw(ctx context.Context, configured *host, port int, args ...string) (string, error) {
	if err := self.guardSshHost(ctx, configured); err != nil {
		return "", err
	}
	return self.probeRunner.redisRaw(ctx, configured, port, args...)
}

// Independent role selectors still pass their full-inventory host.
func (self *hostScopeRunner) shell(ctx context.Context, configured *host, command string) (string, error) {
	if err := self.guardSshHost(ctx, configured); err != nil {
		return "", err
	}
	return self.probeRunner.shell(ctx, configured, command)
}

// A longer timeout or stdin cannot bypass the observation pause.
func (self *hostScopeRunner) sshTimeout(ctx context.Context, configured *host, command string, stdin string, timeout time.Duration) (string, error) {
	if err := self.guardSshHost(ctx, configured); err != nil {
		return "", err
	}
	return self.probeRunner.sshTimeout(ctx, configured, command, stdin, timeout)
}

// Guard exact inventory-owned curl --resolve/proxy/URL targets. Other local
// commands and whole-environment service streams keep their semantics.
func (self *hostScopeRunner) local(ctx context.Context, name string, args ...string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if name == "curl" {
		for index := 0; index < len(args); index++ {
			value := args[index]
			if value == "--resolve" || strings.HasPrefix(value, "--resolve=") {
				if value == "--resolve" {
					index++
					if index == len(args) {
						return "", self.blockHosts(self.excludedNames())
					}
					value = args[index]
				} else {
					value = strings.TrimPrefix(value, "--resolve=")
				}
				parts := strings.SplitN(value, ":", 3)
				if len(parts) != 3 {
					return "", self.blockHosts(self.excludedNames())
				}
				for _, endpoint := range strings.Split(parts[2], ",") {
					if err := self.guardEndpoint(ctx, endpoint); err != nil {
						return "", err
					}
				}
				continue
			}
			// No current probe uses this rewrite. Unknown ownership must not
			// let a future alternate rewrite bypass the explicit policy.
			if value == "--connect-to" || strings.HasPrefix(value, "--connect-to=") {
				return "", self.blockHosts(self.excludedNames())
			}
			if value == "--proxy" || value == "--url" || value == "-x" {
				index++
				if index == len(args) {
					return "", self.blockHosts(self.excludedNames())
				}
				value = args[index]
			} else if strings.HasPrefix(value, "-x") {
				value = strings.TrimPrefix(value, "-x")
			}
			value = strings.TrimPrefix(strings.TrimPrefix(value, "--url="), "--proxy=")
			if !strings.HasPrefix(value, "-") {
				if err := self.guardEndpoint(ctx, value); err != nil {
					return "", err
				}
			}
		}
	}
	return self.probeRunner.local(ctx, name, args...)
}

// Raw protocol negotiation cannot contact an excluded public tuple.
func (self *hostScopeRunner) tcpExchange(ctx context.Context, network, address string, payload []byte, responseBytes int) ([]byte, error) {
	if err := self.guardEndpoint(ctx, address); err != nil {
		return nil, err
	}
	return self.probeRunner.tcpExchange(ctx, network, address, payload, responseBytes)
}

// SNI does not change explicitly dialed endpoint ownership.
func (self *hostScopeRunner) tlsCertificates(ctx context.Context, network, address, serverName string) (TLSCertificateObservation, error) {
	if err := self.guardEndpoint(ctx, address); err != nil {
		return TLSCertificateObservation{}, err
	}
	return self.probeRunner.tlsCertificates(ctx, network, address, serverName)
}

// A private per-run request boundary leaves existing HTTP clients, transports,
// pools, and custom resolver behavior untouched.
type hostScopeContextKey struct{}

// Canonical public Grafana remains whole-environment. Only explicit numeric
// or inventory-host URL ownership triggers the same pre-transport host policy.
func guardGrafanaObservationRequest(request *http.Request) error {
	if request == nil || request.URL == nil {
		return fmt.Errorf("monitor: Grafana observation request is unavailable")
	}
	ctx := request.Context()
	if scoped, ok := ctx.Value(hostScopeContextKey{}).(*hostScopeRunner); ok {
		return scoped.guardEndpoint(ctx, request.URL.String())
	}
	return nil
}

// Guard default and injected Do seams alike; credentials never reach a denied
// endpoint, and neither the shared client nor its transport is mutated.
func doScopedGrafanaRequest(client grafanaHTTPClient, request *http.Request) (*http.Response, error) {
	if err := guardGrafanaObservationRequest(request); err != nil {
		return nil, err
	}
	return client.Do(request)
}

// Stable private policy names support conservative unknown rewrite handling.
func (self *hostScopeRunner) excludedNames() []string {
	names := make([]string, 0, len(self.excludedHostNames))
	for name := range self.excludedHostNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Only exact host or host/path/port targets establish per-host attribution.
func hostScopeTargetMatches(target, name string) bool {
	return target == name || strings.HasPrefix(target, name+"/") || strings.HasPrefix(target, name+":")
}

// Preserve permitted per-host findings. Mixed/global conclusions requiring
// a denied input are unknown, not placement/capacity or fleet-health evidence.
func (self *hostScopeRunner) reduceFindings(settings SignalSettings, findings []finding) []finding {
	blockedCount := func() int {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return len(self.blockedHostNames)
	}()
	result := make([]finding, 0, len(findings)+1)
	for _, observed := range findings {
		excludedTarget, allowedTarget := false, false
		for _, configured := range self.cfg.scopeHosts() {
			if hostScopeTargetMatches(observed.target, configured.name) {
				if self.excludedHostNames[configured.name] {
					excludedTarget = true
				} else {
					allowedTarget = true
				}
			}
		}
		if excludedTarget || blockedCount != 0 && !allowedTarget && (observed.probeId != "monitor/visibility" || observed.healthy) {
			continue
		}
		result = append(result, observed)
	}
	if blockedCount != 0 {
		result = append(result, hostScopeCoverageFinding(settings, blockedCount))
	}
	return result
}

// A joined failure is intentional policy only when every leaf is a scope
// denial; another observation failure must not become successful scope.
func hostScopeOnlyError(err error) bool {
	if err == nil {
		return false
	}
	if _, ok := err.(*hostScopeExcludedError); ok {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !hostScopeOnlyError(child) {
				return false
			}
		}
		return true
	}
	return hostScopeOnlyError(errors.Unwrap(err))
}

// Operational partial coverage never pages because the pause persists.
// Policy values, commands, and endpoint identities stay out of Alert fields.
func hostScopeCoverageFinding(settings SignalSettings, blockedCount int) finding {
	excludedHostNames := map[string]bool{}
	for _, name := range settings.ExcludedHosts {
		excludedHostNames[name] = true
	}
	return finding{
		probeId: "monitor/host-scope", tier: tierWarn,
		class: "monitor-host-scope-partial", target: "monitor-host-scope", sustain: 1,
		symptom:   "Some inventory-target host observations are intentionally excluded by this monitor's immutable policy.",
		mechanism: "Desired inventory remains complete, but excluded named-host and exact inventory-owned transport operations are denied before their source is contacted. Their direct state is unknown, not failed workload or missing placement.",
		baseline:  "Required inventory-target observations are enabled, or every explicit operational pause has an owner and re-enable condition.",
		observed:  fmt.Sprintf("configured_hosts=%d excluded_hosts=%d blocked_hosts=%d desired_topology_unchanged=true service_stream_scope=whole-environment", len(settings.Hosts), len(excludedHostNames), blockedCount),
		evidence:  "Only coverage counts and fixed status are retained; selector values, endpoints, commands, resource contents, credentials, and value-derived fingerprints are not rendered.",
		context:   "Permitted per-host evidence and whole-environment service streams remain available. Excluded-target recovery and mixed/global placement, capacity, or fleet-health conclusions requiring a denied input are withheld.",
		action:    "Record the operator reason, owner, UTC start, and re-enable condition in the run ledger. Preserve complete topology and expected denominators; do not remove a selector merely to quiet this operational warning.",
		verify:    "After the re-enable condition is met, validate current settings with the same CLI overrides and promote a newly built watcher. Require concrete restored-target observations; partial coverage or non-emission cannot establish full-fleet recovery.",
		playbook:  "SIGNALS.md §1.6 and RUN-MAIN.md whole-host pause and Safe watcher promotion",
	}
}
