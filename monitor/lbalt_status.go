// The lb status probe of lb_alt interfaces (LBALT.md D17, LBALT-IMPL.md §2.6).
//
// Route53 health-checks every lb interface address with a plain GET of the lb
// status route on TCP 80, with the <env>-lb.<domain> Host and the first lb
// hidden prefix, and counts any 2xx or 3xx as healthy without reading the
// body. On an lb_alt interface connect's front answers that route, so an
// accidental redirect to https, or a status other than ok, keeps a broken
// interface in DNS. This probe requires what Route53 cannot: a 200, never a
// redirect followed, with JSON whose status is "ok".
//
// One probe source hashes into one HMARK bucket, so it only ever reaches one
// block of an interface. Each enrolled interface is therefore also probed per
// block, through the lb's hidden per-block route on 443
// (<prefix>/by/b/<service>/<block>/status), which the front forwards to that
// block. The block must report "ok" under its own service and block names, and
// a connect block that reports its front not ready (X-UR-Front-Ready: 0) fails.
//
// Enrollment is explicit and off by default (monitor.yml lbalt_status): it
// names the interfaces and their families. Everything else comes from its
// source of truth: the addresses from the services.yml lb inventory of each
// host, the lb name from the environment and the primary domain, the prefix
// from services.yml lb_hidden_prefixes, and the blocks from the active
// services.yml blocks of the lb_alt service, held to a declared count.
//
// The prefix and every client address stay private. Only the transport sees a
// request path; observations and verdicts hold fixed domains, flags and
// counts, errors are fixed text, and nothing here logs.
//
// lbAltStatusRequests validates the matrix, lbAltStatusProbe.observe runs the
// attempts, and each result carries its fixed verdict (lbAltStatusEvaluate).
// The lbalt-status signal (signal_lbalt_status.go, SIGNALS.md §16.10) runs a
// pass at the probe settings' cadence and turns the verdicts into findings.
// Its enrollment reaches it through SignalSettings.LbAltStatus and
// monitorConfig.lbAltStatus, read from monitor.yml lbalt_status with the
// route prefix from services.yml lb_hidden_prefixes.
package monitor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// the lb status Route53 checks, on plain HTTP
	lbAltStatusRouteLb = "lb"
	// one block's status through the front's hidden per-block route, on TLS
	lbAltStatusRouteBlock = "block"
	// Route53's check and nginx's lb status server
	lbAltStatusHttpPort  = 80
	lbAltStatusHttpsPort = 443
	// the front's readiness on a connect block's private status
	// (LBALT-IMPL.md §2.4), "1" or "0"
	lbAltStatusFrontReadyHeader = "X-UR-Front-Ready"
	// the front's name for itself in its lb status JSON (LBALT-IMPL.md §2.6)
	lbAltStatusFrontName = "lbalt"
)

// Explicit enrollment, from monitor.yml lbalt_status. An absent block is off.
// A present block that does not decode is invalid, never off, so a typo cannot
// silently disarm the probe.
type LbAltStatusSettings struct {
	Enabled         bool                        `yaml:"enabled"`
	ExpectedTargets int                         `yaml:"expected_targets"`
	Targets         []LbAltStatusTargetSettings `yaml:"targets"`
	// the service whose blocks each interface is probed for: the lb_alt
	// service, which validation allows only to be connect (LBALT-IMPL.md §2.2)
	BlockService string `yaml:"block_service"`
	// the declared count of that service's active blocks, an independent
	// denominator for the block list services.yml supplies
	ExpectedBlocks int  `yaml:"expected_blocks"`
	Invalid        bool `yaml:"-"`
	// "/" and the first services.yml lb_hidden_prefixes entry, the path
	// prefix of Route53's check. Never rendered.
	routePrefix string
}

// One enrolled lb interface. Its addresses come from the services.yml lb
// inventory of its host, so an interface that left the inventory refuses the
// whole matrix rather than shrinking it. Families are explicit.
type LbAltStatusTargetSettings struct {
	Name      string   `yaml:"name"`
	Host      string   `yaml:"host"`
	Interface string   `yaml:"interface"`
	Families  []string `yaml:"families"`
}

// Tunables of the probe.
type LbAltStatusProbeSettings struct {
	// Between passes. Route53 checks every 10 s already; this probe adds what
	// Route53 cannot see, at the cadence of the monitor's other public probes.
	// Non-positive takes the default, since the scheduler cannot run a
	// non-positive interval.
	Cadence time.Duration
	// Bound on one attempt: the TCP connect, the TLS handshake on 443, the
	// request and the whole response, as the other public probes' 5 s. Must
	// be positive: a non-positive bound would expire every attempt at its
	// start, so the matrix and the transport refuse it before any dial.
	AttemptTimeout time.Duration
	// Attempts in flight at once. Non-positive runs one at a time.
	WorkerCount int
	// The largest declared target count, one lb interface each. Main has ten
	// lb interfaces. Non-positive refuses every matrix.
	MaxTargetCount int
	// The largest declared block count. Connect runs five blocks per host.
	// Non-positive refuses every matrix.
	MaxBlockCount int
	// Bytes of response header accepted. A status answer carries a few
	// hundred. Non-positive takes the net/http default.
	ResponseHeaderByteLimit int
	// Bytes of response body read before the answer counts as too large. A
	// status body is under 256 bytes. Must be positive: the matrix and the
	// transport refuse a non-positive bound before any dial.
	ResponseBodyByteLimit int
}

// A new instance on every call.
func DefaultLbAltStatusProbeSettings() *LbAltStatusProbeSettings {
	return &LbAltStatusProbeSettings{
		Cadence:                 5 * time.Minute,
		AttemptTimeout:          5 * time.Second,
		WorkerCount:             4,
		MaxTargetCount:          32,
		MaxBlockCount:           16,
		ResponseHeaderByteLimit: 16 * 1024,
		ResponseBodyByteLimit:   16 * 1024,
	}
}

// One attempt's exact pin and authority. Path carries the hidden prefix and
// is the transport's alone: no verdict, finding, error or log renders it.
// AttemptId is private correlation data, never an alert identity.
type LbAltStatusRequest struct {
	AttemptId string
	Host      string
	Target    string
	Family    string
	// lbAltStatusRouteLb or lbAltStatusRouteBlock
	Route string
	// the block route's service and block; empty on the lb route
	Service string
	Block   string
	Address string
	Port    int
	// the lb name: the Host on both routes, and the SNI on 443
	ServerName string
	Path       string
}

// Only fixed failure domains cross the adapter. They describe the bounded
// observation, not an inferred cause.
type LbAltStatusFailure string

const (
	LbAltStatusFailureNone   LbAltStatusFailure = ""
	LbAltStatusFailureSource LbAltStatusFailure = "source-unavailable"
	// the monitor host has no route or address in the pin's family
	LbAltStatusFailureRoute LbAltStatusFailure = "observer-route-unavailable"
	// the connection to the pin was refused or failed, a router's host
	// unreachable included
	LbAltStatusFailureConnect LbAltStatusFailure = "connect-failed"
	// the attempt deadline passed before the whole response arrived
	LbAltStatusFailureTimeout LbAltStatusFailure = "timeout"
	// the certificate was not verified for the lb name
	LbAltStatusFailureTls LbAltStatusFailure = "tls-authentication-unverified"
	// the exchange broke after the connection was made
	LbAltStatusFailureRequest LbAltStatusFailure = "request-failed"
	// the body ran past its bound
	LbAltStatusFailureBudget        LbAltStatusFailure = "response-budget-exhausted"
	LbAltStatusFailureConfiguration LbAltStatusFailure = "configuration-invalid"
)

// What a body's "status" said, reduced to a fixed domain so no response text
// reaches a finding.
type LbAltStatusState string

const (
	LbAltStatusStateAbsent   LbAltStatusState = ""
	LbAltStatusStateOk       LbAltStatusState = "ok"
	LbAltStatusStateDraining LbAltStatusState = "draining"
	// warpctl's error status, "error" then a space or colon
	LbAltStatusStateError LbAltStatusState = "error"
	LbAltStatusStateOther LbAltStatusState = "other"
)

// What X-UR-Front-Ready said. Only a connect block's private status carries
// it, and only when the front is configured, so absence is no evidence.
type LbAltStatusFrontReadiness string

const (
	LbAltStatusFrontReadinessAbsent   LbAltStatusFrontReadiness = ""
	LbAltStatusFrontReadinessReady    LbAltStatusFrontReadiness = "ready"
	LbAltStatusFrontReadinessNotReady LbAltStatusFrontReadiness = "not-ready"
	LbAltStatusFrontReadinessInvalid  LbAltStatusFrontReadiness = "invalid"
)

// The bounded facts of one attempt: fixed domains, flags and counts, with no
// body, header value, path or address beyond the requested pin.
type LbAltStatusObservation struct {
	AttemptId      string
	RequestedTuple string
	// a connection to the pin was attempted
	Dialed bool
	// exactly one new connection to the pin, made for this attempt
	FreshConnection bool
	// a certificate chain verified for the lb name (443 only)
	TlsVerified bool
	// 0 when no response arrived
	StatusCode      int
	JsonContentType bool
	// body bytes read, at most the probe's body bound
	ResponseBytes int
	// the body is one JSON object of string fields where the routes name them
	BodyJson bool
	Status   LbAltStatusState
	// the block route's body names the requested service and block
	BlockMatched   bool
	FrontReadiness LbAltStatusFrontReadiness
	// the lb route's body names the lb_alt front, as nginx's does not
	LbAltFront bool
	Failure    LbAltStatusFailure
}

// Synthetic and alternate sources must implement this explicitly. There is no
// native network fallback when SignalSettings.Source is supplied.
type LbAltStatusSignalSource interface {
	LbAltStatus(context.Context, LbAltStatusRequest) (LbAltStatusObservation, error)
}

// Optional internal seam leaves unrelated transports unchanged.
type lbAltStatusRunner interface {
	lbAltStatus(context.Context, LbAltStatusRequest, *LbAltStatusProbeSettings) (LbAltStatusObservation, error)
}

// The fixed outcome of one attempt. Healthy needs a fresh, correlated and
// bounded answer from the exact pin that meets its route's contract. A path
// verdict is such an answer that does not, or a failed exchange after a
// connection to the pin was attempted. Everything else is unknown, never
// recovery.
type lbAltStatusVerdict struct {
	healthy bool
	path    bool
	reason  string
}

// One attempt, its observation and its verdict.
type lbAltStatusResult struct {
	request     LbAltStatusRequest
	observation LbAltStatusObservation
	verdict     lbAltStatusVerdict
}

// Runs the attempts of one pass at its settings' cadence. A signal gives it an
// identity and findings.
type lbAltStatusProbe struct {
	// nil takes the defaults
	settings *LbAltStatusProbeSettings
	// Local deterministic deadline controls may replace context construction,
	// never the protocol or source. Production uses the attempt timeout.
	attemptContext func(context.Context) (context.Context, context.CancelFunc)
}

// The settings' cadence, or the default's when it is not positive.
func (self lbAltStatusProbe) cadence() time.Duration {
	if self.settings != nil && 0 < self.settings.Cadence {
		return self.settings.Cadence
	}
	return DefaultLbAltStatusProbeSettings().Cadence
}

// Reads the monitor.yml lbalt_status block. An absent block is off; a present
// one that is not a mapping, or names a key this schema lacks, is invalid. The
// route prefix comes from services.yml lb_hidden_prefixes, whose first entry
// is Route53's check path (warpctl dnsStatusPath), never from monitor.yml.
func lbAltStatusSettingsFromNode(node yaml.Node, lbHiddenPrefixes []string) LbAltStatusSettings {
	if node.Kind == 0 {
		return LbAltStatusSettings{}
	}
	invalid := LbAltStatusSettings{Enabled: true, Invalid: true}
	if node.Kind != yaml.MappingNode {
		return invalid
	}
	raw, err := yaml.Marshal(&node)
	if err != nil {
		return invalid
	}
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	settings := LbAltStatusSettings{}
	if decoder.Decode(&settings) != nil || decoder.Decode(new(any)) != io.EOF {
		return invalid
	}
	if 0 < len(lbHiddenPrefixes) {
		settings.routePrefix = "/" + strings.Trim(lbHiddenPrefixes[0], "/")
	}
	return settings
}

// Copies every nested slice, so startup generation and observation scope stay
// independent of a caller's later mutation.
func cloneLbAltStatusSettings(settings LbAltStatusSettings) LbAltStatusSettings {
	settings.Targets = append([]LbAltStatusTargetSettings(nil), settings.Targets...)
	for index := range settings.Targets {
		settings.Targets[index].Families = append([]string(nil), settings.Targets[index].Families...)
	}
	return settings
}

// Whether a route prefix is "/" and path segments of unreserved URL
// characters, as warp's hidden prefixes are. Anything else could not be
// Route53's path.
func lbAltStatusRoutePrefixValid(routePrefix string) bool {
	if len(routePrefix) < 2 || routePrefix[0] != '/' {
		return false
	}
	for _, segment := range strings.Split(routePrefix[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
		for _, character := range segment {
			if !('a' <= character && character <= 'z' || 'A' <= character && character <= 'Z' || '0' <= character && character <= '9' ||
				character == '-' || character == '.' || character == '_' || character == '~') {
				return false
			}
		}
	}
	return true
}

// The lb route's path under a route prefix.
func lbAltStatusLbPath(routePrefix string) string {
	return routePrefix + "/status"
}

// A block route's path under a route prefix, as nginx's lb names it.
func lbAltStatusBlockPath(routePrefix string, service string, block string) string {
	return routePrefix + "/by/b/" + service + "/" + block + "/status"
}

// The request has been validated; no name lookup is permitted here.
func lbAltStatusTuple(request LbAltStatusRequest) string {
	address, err := netip.ParseAddr(request.Address)
	if err != nil || request.Port < 1 || request.Port > 65535 {
		return ""
	}
	return netip.AddrPortFrom(address, uint16(request.Port)).String()
}

// Validates the complete declared matrix against the inventory before any
// connection, and expands each target family into its lb route and one block
// route per block, in a fixed order. Probe settings without a positive
// attempt timeout or body bound refuse the matrix too, since every attempt
// under them would fail at its start. The reason is a fixed domain, never an
// input value.
func lbAltStatusRequests(settings LbAltStatusSettings, probeSettings *LbAltStatusProbeSettings, cfg *monitorConfig) ([]LbAltStatusRequest, string) {
	if probeSettings == nil || probeSettings.AttemptTimeout <= 0 || probeSettings.ResponseBodyByteLimit <= 0 {
		return nil, "probe-settings"
	}
	if settings.Invalid || settings.ExpectedTargets < 1 || settings.ExpectedTargets > probeSettings.MaxTargetCount ||
		len(settings.Targets) != settings.ExpectedTargets {
		return nil, "target-denominator"
	}
	if !lbAltStatusRoutePrefixValid(settings.routePrefix) {
		return nil, "route-prefix"
	}
	serverName := cfg.env + "-lb." + cfg.publicDomain
	if !publicUdpServerName(serverName) {
		return nil, "lb-authority"
	}
	blocks := cfg.logServiceBlocks[settings.BlockService]
	if !validDNSLabel(settings.BlockService) || settings.ExpectedBlocks < 1 || settings.ExpectedBlocks > probeSettings.MaxBlockCount ||
		len(blocks) != settings.ExpectedBlocks {
		return nil, "block-denominator"
	}
	uniqueBlocks := map[string]bool{}
	for _, block := range blocks {
		if !validDNSLabel(block) || uniqueBlocks[block] {
			return nil, "block-denominator"
		}
		uniqueBlocks[block] = true
	}
	hostCounts := map[string]int{}
	for _, configured := range cfg.hosts {
		hostCounts[configured.name]++
	}
	names := map[string]bool{}
	paths := map[string]bool{}
	requests := []LbAltStatusRequest{}
	for _, target := range settings.Targets {
		if !validDNSLabel(target.Name) || names[target.Name] || hostCounts[target.Host] != 1 {
			return nil, "target-identity"
		}
		names[target.Name] = true
		var inventory *PublicLBInterfaceSettings
		inventoryCount := 0
		for _, configured := range cfg.hosts {
			if configured.name != target.Host {
				continue
			}
			for index := range configured.publicLB {
				if configured.publicLB[index].Interface == target.Interface {
					inventory = &configured.publicLB[index]
					inventoryCount++
				}
			}
		}
		if target.Interface == "" || strings.TrimSpace(target.Interface) != target.Interface || inventoryCount != 1 {
			return nil, "interface-inventory"
		}
		if len(target.Families) < 1 || len(target.Families) > 2 {
			return nil, "family-denominator"
		}
		families := map[string]bool{}
		for _, family := range target.Families {
			if families[family] || family != "ipv4" && family != "ipv6" {
				return nil, "family-denominator"
			}
			families[family] = true
			addressText := inventory.IPv4Address
			if family == "ipv6" {
				addressText = inventory.IPv6Address
			}
			address, err := netip.ParseAddr(addressText)
			if err != nil || address.Zone() != "" || address.Is4In6() || !address.IsGlobalUnicast() || address.IsPrivate() ||
				(family == "ipv4") != address.Is4() {
				return nil, "family-address"
			}
			key := strings.Join([]string{target.Host, target.Interface, family}, "/")
			if paths[key] {
				return nil, "duplicate-path"
			}
			paths[key] = true
			requests = append(requests, LbAltStatusRequest{
				Host:       target.Host,
				Target:     target.Name,
				Family:     family,
				Route:      lbAltStatusRouteLb,
				Address:    address.String(),
				Port:       lbAltStatusHttpPort,
				ServerName: serverName,
				Path:       lbAltStatusLbPath(settings.routePrefix),
			})
			for _, block := range blocks {
				requests = append(requests, LbAltStatusRequest{
					Host:       target.Host,
					Target:     target.Name,
					Family:     family,
					Route:      lbAltStatusRouteBlock,
					Service:    settings.BlockService,
					Block:      block,
					Address:    address.String(),
					Port:       lbAltStatusHttpsPort,
					ServerName: serverName,
					Path:       lbAltStatusBlockPath(settings.routePrefix, settings.BlockService, block),
				})
			}
		}
		if inventory.IPv4Address != "" && !families["ipv4"] || inventory.IPv6Address != "" && !families["ipv6"] {
			return nil, "unprobed-family"
		}
	}
	return requests, ""
}

// Runs every request on a bounded worker pool, each in its own attempt
// context with a fresh correlation id, and gives each its verdict. Results
// keep the order of the requests. A canceled pass returns its error and no
// results: neither failure nor recovery.
func (self lbAltStatusProbe) observe(ctx context.Context, transport probeRunner, requests []LbAltStatusRequest) ([]lbAltStatusResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	settings := self.settings
	if settings == nil {
		settings = DefaultLbAltStatusProbeSettings()
	}
	runner, available := transport.(lbAltStatusRunner)
	newAttempt := self.attemptContext
	if newAttempt == nil {
		newAttempt = func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, settings.AttemptTimeout)
		}
	}
	results := make([]lbAltStatusResult, len(requests))
	jobs := make(chan int, len(requests))
	for index := range requests {
		jobs <- index
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(max(settings.WorkerCount, 1), len(requests)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				if ctx.Err() != nil {
					return
				}
				request := requests[index]
				nonce := make([]byte, 16)
				if _, err := rand.Read(nonce); err != nil {
					results[index] = lbAltStatusResult{request: request, verdict: lbAltStatusVerdict{reason: "attempt-authority-unavailable"}}
					continue
				}
				request.AttemptId = hex.EncodeToString(nonce)
				if !available {
					results[index] = lbAltStatusResult{request: request, verdict: lbAltStatusVerdict{reason: "source-unavailable"}}
					continue
				}
				attemptCtx, cancel := newAttempt(ctx)
				observation, err := runner.lbAltStatus(attemptCtx, request, settings)
				// Capture deadline authority before our own cleanup cancel. A
				// source returning nil after expiry cannot establish health.
				if attemptErr := attemptCtx.Err(); err == nil && attemptErr != nil {
					err = attemptErr
				}
				cancel()
				results[index] = lbAltStatusResult{
					request:     request,
					observation: observation,
					verdict:     lbAltStatusEvaluate(request, observation, err, settings),
				}
			}
		}()
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

// Gives one attempt its fixed verdict. The reasons are a closed set:
// source-unavailable, attempt-authority-unavailable,
// observation-authority-invalid, observation-incomplete, the failure domains,
// redirect, http-status, content-type, body-invalid, status-not-ok,
// block-mismatch, front-not-ready, front-readiness-invalid and healthy.
func lbAltStatusEvaluate(request LbAltStatusRequest, observation LbAltStatusObservation, err error, settings *LbAltStatusProbeSettings) lbAltStatusVerdict {
	unknown := func(reason string) lbAltStatusVerdict {
		return lbAltStatusVerdict{reason: reason}
	}
	path := func(reason string) lbAltStatusVerdict {
		return lbAltStatusVerdict{path: true, reason: reason}
	}
	if observation.Failure == LbAltStatusFailureSource {
		return unknown(string(LbAltStatusFailureSource))
	}
	if observation.AttemptId != request.AttemptId || observation.RequestedTuple != lbAltStatusTuple(request) ||
		observation.StatusCode != 0 && (observation.StatusCode < 100 || 599 < observation.StatusCode) ||
		observation.ResponseBytes < 0 || settings.ResponseBodyByteLimit < observation.ResponseBytes ||
		observation.FreshConnection && !observation.Dialed {
		return unknown("observation-authority-invalid")
	}
	switch observation.Failure {
	case LbAltStatusFailureNone:
	case LbAltStatusFailureRoute, LbAltStatusFailureTls, LbAltStatusFailureConfiguration:
		return unknown(string(observation.Failure))
	case LbAltStatusFailureConnect, LbAltStatusFailureTimeout:
		if !observation.Dialed {
			return unknown("observation-incomplete")
		}
		return path(string(observation.Failure))
	case LbAltStatusFailureRequest, LbAltStatusFailureBudget:
		if !observation.FreshConnection {
			return unknown("observation-incomplete")
		}
		return path(string(observation.Failure))
	default:
		return unknown("observation-authority-invalid")
	}
	if err != nil || !observation.FreshConnection || observation.StatusCode == 0 ||
		request.Port == lbAltStatusHttpsPort && !observation.TlsVerified {
		return unknown("observation-incomplete")
	}
	if 300 <= observation.StatusCode && observation.StatusCode < 400 {
		// Route53 counts a redirect healthy; this probe never does
		return path("redirect")
	}
	if observation.StatusCode != http.StatusOK {
		return path("http-status")
	}
	if !observation.JsonContentType {
		return path("content-type")
	}
	if !observation.BodyJson {
		return path("body-invalid")
	}
	if observation.Status != LbAltStatusStateOk {
		return path("status-not-ok")
	}
	if request.Route == lbAltStatusRouteBlock {
		if !observation.BlockMatched {
			return path("block-mismatch")
		}
		switch observation.FrontReadiness {
		case LbAltStatusFrontReadinessNotReady:
			return path("front-not-ready")
		case LbAltStatusFrontReadinessInvalid:
			return path("front-readiness-invalid")
		}
	}
	return lbAltStatusVerdict{healthy: true, reason: "healthy"}
}

// Source capability absence is a bounded observation error, not permission
// for a synthetic test or embedding process to make an unrequested live dial.
func (self *sourceRunner) lbAltStatus(ctx context.Context, request LbAltStatusRequest, _ *LbAltStatusProbeSettings) (LbAltStatusObservation, error) {
	if err := ctx.Err(); err != nil {
		return LbAltStatusObservation{}, err
	}
	source, ok := self.source.(LbAltStatusSignalSource)
	if !ok {
		return LbAltStatusObservation{Failure: LbAltStatusFailureSource}, errors.New("monitor: lb status source capability unavailable")
	}
	return source.LbAltStatus(ctx, request)
}

// Enforces both logical ownership and shared-endpoint exclusions before the
// underlying real or synthetic adapter is contacted.
func (self *hostScopeRunner) lbAltStatus(ctx context.Context, request LbAltStatusRequest, settings *LbAltStatusProbeSettings) (LbAltStatusObservation, error) {
	var configured *host
	for _, candidate := range self.cfg.hosts {
		if candidate.name == request.Host {
			if configured != nil {
				return LbAltStatusObservation{}, errors.New("monitor: lb status host authority is ambiguous")
			}
			configured = candidate
		}
	}
	if configured == nil {
		return LbAltStatusObservation{}, errors.New("monitor: lb status host authority is unavailable")
	}
	if err := self.guardHost(ctx, configured); err != nil {
		return LbAltStatusObservation{}, err
	}
	if err := self.guardEndpoint(ctx, request.Address); err != nil {
		return LbAltStatusObservation{}, err
	}
	transport, ok := self.probeRunner.(lbAltStatusRunner)
	if !ok {
		return LbAltStatusObservation{Failure: LbAltStatusFailureSource}, errors.New("monitor: lb status source capability unavailable")
	}
	return transport.lbAltStatus(ctx, request, settings)
}
