// Package fleetprobe composes the reusable one-pass provider probes. Process
// lifetime and scheduling belong to callers: the standalone command may loop,
// while the server taskworker runs one bounded batch and persists its successor.
package fleetprobe

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/operator-proxy/bandwidth"
	"github.com/urnetwork/operator-proxy/egresshealth"
	"github.com/urnetwork/operator-proxy/prober"
	"github.com/urnetwork/operator-proxy/providertunnel"
)

// Returns the current certificate-pin set, already validated (see
// ValidatePins). Long-lived commands can refresh it between calls; a task
// normally supplies one snapshot. A nil source, or an empty set, pins nothing:
// every host is then verified by WebPKI.
type PinSource func() map[string][]string

// Returns the current set, or nil for a nil source.
func (self PinSource) pins() map[string][]string {
	if self == nil {
		return nil
	}
	return self()
}

// Concurrency defaults for a pass: how many providers are probed at once,
// each on its own tunnel.
//
// They are sized for tunnels that mostly wait. A run now spends most of its
// wall clock between spaced retries (minutes apart, GEOMAP §11.3) and a check
// up to about fifteen minutes when nothing answers, so a pass holds many
// tunnels open and few of them busy; at the old defaults a pass would sit idle
// on a handful of waiting tunnels. What bounds the number is memory, not CPU or
// bandwidth: every tunnel carries its own gvisor network stack, multi-client
// and transports, and a re-created tunnel briefly doubles that while the dead
// one tears down. So these are settings to calibrate against the prober
// host's memory -- measure the resident size per open tunnel, and set
// concurrency to what the host can hold -- not numbers to raise for
// throughput. Sixteen each follows the §11.3 blackhole default and keeps a
// standalone prober's full and blackhole lanes, together, near the tunnel
// count the previous defaults (4 + 32) already held.
var (
	DefaultFullConcurrency      = 16
	DefaultBlackholeConcurrency = 16
)

// Holds everything needed for one full health pass. Each network
// dependency is explicit so tests can substitute it without a provider, and a
// task can remain a small scheduling adapter.
type FullOptions struct {
	TunnelConfig providertunnel.Config
	// The served pin set, restricted per probe to the hosts it dials.
	Pins PinSource
	// The pass's destination pool (see LoadPool); nil is the built-in
	// table.
	Pool PoolSource
	// The cold-start allowance: the warm-up's timeout, and the
	// ceiling of any one request. Each load attempt gets the smaller of it and
	// egresshealth.DefaultPerRequestTimeout (see EgressHealthOptions).
	ProbeTimeout time.Duration
	// How many providers are probed at once. Zero uses
	// DefaultFullConcurrency.
	Concurrency     int
	AllDestinations bool
	// The operator's /ip echo, reached through each tunnel. Empty
	// derives it from TunnelConfig.ApiUrl (IpEchoUrl), which must then be the
	// api's public address.
	IpEchoUrl string
	// Passed through to egresshealth.Options, like the two fields below;
	// zero uses its defaults (3, 5 minutes, 2).
	LoadAttempts           int
	LoadRetryMeanInterval  time.Duration
	TunnelRecreateAttempts int
	Submit                 prober.Submitter
	Attempts               prober.AttemptReporter
	HealthResults          prober.HealthReporter
	Bandwidth              *bandwidth.Sampler
	BandwidthHosts         []string
}

// Returns Concurrency, or DefaultFullConcurrency when unset.
func (self FullOptions) concurrency() int {
	if 0 < self.Concurrency {
		return self.Concurrency
	}
	return DefaultFullConcurrency
}

// Returns IpEchoUrl, or the echo derived from the tunnel's api url.
func (self FullOptions) ipEchoUrl() string {
	if self.IpEchoUrl != "" {
		return self.IpEchoUrl
	}
	return IpEchoUrl(self.TunnelConfig.ApiUrl)
}

// Rejects configurations that would hang a worker pool or
// turn every provider in a batch into the same synthetic failure.
// Required dependencies and bounds fail before a provider tunnel is opened.
func validateFullOptions(options FullOptions) error {
	if options.ProbeTimeout <= 0 {
		return fmt.Errorf("fleetprobe: probe timeout must be positive (got %s)", options.ProbeTimeout)
	}
	if options.Concurrency < 0 {
		return fmt.Errorf("fleetprobe: concurrency must not be negative (got %d)", options.Concurrency)
	}
	if options.ipEchoUrl() == "" {
		return fmt.Errorf("fleetprobe: no /ip echo url: set TunnelConfig.ApiUrl or IpEchoUrl -- without it no probe has an exit address to submit")
	}
	if options.Submit == nil {
		return fmt.Errorf("fleetprobe: location submitter is required")
	}
	if options.Attempts == nil {
		return fmt.Errorf("fleetprobe: attempt reporter is required")
	}
	return nil
}

// What one probe's tunnel carries beside its client: the path
// that can re-create it, and the pool it was opened against, so the health
// run and the bandwidth sample use exactly what the tunnel's allowlist was
// built for, whatever the pool source says by then.
type probeState struct {
	path *probePath
	pool *egresshealth.Pool
}

// Finds a probe's state from the client the prober hands its
// hooks. Safe for concurrent use: the probes of a pass run at once.
type probeRegistry struct {
	stateLock sync.Mutex
	byClient  map[*http.Client]*probeState
}

// Records the state of the probe whose tunnel client is client.
func (self *probeRegistry) put(client *http.Client, state *probeState) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.byClient == nil {
		self.byClient = map[*http.Client]*probeState{}
	}
	self.byClient[client] = state
}

// Returns the state recorded for client, or nil.
func (self *probeRegistry) get(client *http.Client) *probeState {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.byClient[client]
}

// Forgets client's probe once its tunnel is closed.
func (self *probeRegistry) remove(client *http.Client) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	delete(self.byClient, client)
}

// Wires one tunnel per provider -- re-created when it dies
// part-way -- and runs every measurement over it. Callers that need
// validation and bounded scheduling should normally use RunFull.
func NewFullProber(options FullOptions) *prober.Prober {
	echoUrl := options.ipEchoUrl()
	probes := &probeRegistry{}

	providerProber := &prober.Prober{
		Open: func(ctx context.Context, providerClientId string) (*http.Client, func() error, error) {
			clientId, err := connect.ParseId(providerClientId)
			if err != nil {
				return nil, nil, err
			}
			pool := options.Pool.pool()
			hosts := dialHosts(egresshealth.HostsOf(pool.Destinations), echoUrl, options.BandwidthHosts...)
			tunnelConfig := options.TunnelConfig
			tunnelConfig.Pins = options.Pins.pins()
			path, err := openProbePath(ctx, providerTunnelOpener(tunnelConfig, clientId, hosts), hosts, options.ProbeTimeout)
			if err != nil {
				return nil, nil, err
			}
			client, _ := path.Current()
			probes.put(client, &probeState{path: path, pool: pool})
			return client, func() error {
				probes.remove(client)
				if reopens := path.reopens(); 0 < reopens {
					log.Printf("fleetprobe: provider=%s tunnel re-created %d time(s) during the run", providerClientId, reopens)
				}
				return path.Close()
			}, nil
		},
		Health: func(ctx context.Context, client *http.Client, place egresshealth.Place) (*egresshealth.Result, error) {
			opts := EgressHealthOptions(options.ProbeTimeout, options.AllDestinations)
			opts.IpEchoUrl = echoUrl
			opts.LoadAttempts = options.LoadAttempts
			opts.LoadRetryMeanInterval = options.LoadRetryMeanInterval
			opts.TunnelRecreateAttempts = options.TunnelRecreateAttempts
			opts.ProviderPlace = place
			pool := options.Pool.pool()
			if state := probes.get(client); state != nil {
				pool = state.pool
				opts.Path = state.path
			}
			opts.Destinations = pool.Destinations
			opts.Profile = profileOf(pool)
			return egresshealth.Check(ctx, client, opts)
		},
		Submit:        options.Submit,
		Attempts:      options.Attempts,
		HealthResults: options.HealthResults,
	}

	if options.Bandwidth != nil {
		providerProber.Bandwidth = func(ctx context.Context, providerClientId string, client *http.Client) {
			// The tunnel may have been re-created during the health run; the
			// sample rides the one that is up now.
			if state := probes.get(client); state != nil {
				client, _ = state.path.Current()
			}
			results := options.Bandwidth.Sample(ctx, providerClientId, client)
			log.Printf("bandwidth: provider=%s %s", providerClientId, bandwidth.Summary(results))
		}
	}

	return providerProber
}

// Executes one bounded batch. It owns no timer and schedules nothing;
// this is what lets a durable task checkpoint after every batch. Each
// provider's place decides the destinations its sample may draw from.
func RunFull(ctx context.Context, providers []prober.Provider, options FullOptions) (prober.Summary, error) {
	if err := validateFullOptions(options); err != nil {
		return prober.Summary{}, err
	}
	scheduler := &prober.Scheduler{
		Prober:      NewFullProber(options),
		Concurrency: options.concurrency(),
		CacheTtl:    0,
	}
	return scheduler.Run(ctx, providers), nil
}

// Returns how many sequential request rounds one attempt
// of every load of a health run needs at its configured sampling geometry.
func EgressHealthRounds(allDestinations bool) int {
	requestCount := egresshealth.SamplePerRun()
	concurrency := egresshealth.DefaultConcurrency
	if allDestinations {
		requestCount = len(egresshealth.Destinations())
		concurrency = egresshealth.AllConcurrency
	}
	if rounds := (requestCount + concurrency - 1) / concurrency; 1 < rounds {
		return rounds
	}
	return 1
}

// The health run's geometry for one probe timeout.
//
// The probe timeout is the cold-start allowance: it is the warm-up's own
// timeout (the fetch that pays the tunnel's cold start, see egresshealth), and
// each load attempt gets the smaller of it and
// egresshealth.DefaultPerRequestTimeout -- a load starts on a warm path, and a
// slow one is retried minutes later rather than waited on. The run's budget is
// left to egresshealth.Options.RunBudget, which is derived from the retry
// schedule: a run spans minutes by design now, and no longer has to fit in one
// probe timeout the way it did when every load had one try.
//
// A probe timeout under the per-request floor leaves each attempt less than a
// warm request needs; callers check PerRequestTimeout against
// egresshealth.DefaultPerRequestTimeout at startup and refuse such a value.
func EgressHealthOptions(probeTimeout time.Duration, allDestinations bool) egresshealth.Options {
	concurrency := egresshealth.DefaultConcurrency
	if allDestinations {
		concurrency = egresshealth.AllConcurrency
	}
	return egresshealth.Options{
		PerRequestTimeout: min(probeTimeout, egresshealth.DefaultPerRequestTimeout),
		IpEchoTimeout:     probeTimeout,
		AllDestinations:   allDestinations,
		Concurrency:       concurrency,
	}
}
