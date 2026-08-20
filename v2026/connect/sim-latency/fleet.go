package main

// the provider fleet: a shard of egress providers with per-provider network
// impairment, ramp-up, churn, and good/degraded regime dynamics.
//
// One control loop (not a goroutine per provider) drives every provider's
// state on a tick: it staggers first connects across the ramp window, cycles
// each provider offline/online on its uptime/downtime schedule (driving the
// real reliability machinery), and modulates each provider between its base
// and degraded impairment regimes. Transitions are rare relative to the tick,
// so the per-tick O(N) scan is cheap even at 100k providers.

import (
	"context"
	"fmt"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"sync/atomic"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
	"github.com/urnetwork/sdk/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
)

// impairEnabled gates the provider network impairment (default on). Set from
// the --impair flag. When off, providers dial the exchange with a plain
// connection — a clean baseline for isolating impairment from the rest of the
// stack.
var impairEnabled = true

const providerRegistrationTimeout = 15 * time.Second

type simProvider struct {
	entry    ProviderEntry
	provider *sdk.SimProvider
	params   *atomic.Pointer[impairParams]

	base     *impairParams
	degraded *impairParams

	control *rng

	connected  bool
	inDegraded bool
	nextChurn  time.Time
	nextRegime time.Time
}

type Fleet struct {
	ctx          context.Context
	config       *Config
	apiUrl       string
	wsUrls       []string
	wsPorts      map[int]bool
	rampDuration time.Duration
	// The first fixture entry for a network is the admin persisted by
	// provisionIdentityBatch. Providers grouped into that shared network must
	// authenticate as the same admin even though each entry retains its own
	// generated user id as ground truth.
	networkAdminUsers map[string]string

	providers []*simProvider
	// Provider churn and degraded-regime changes begin only after the parent
	// crosses the measurement boundary. Ramp still connects the fleet during
	// setup, but variable client-warmup duration cannot advance the seeded
	// workload to a different phase in an otherwise identical run.
	dynamicsStart   chan chan struct{}
	dynamicsStarted bool
	done            chan struct{}
	closeOnce       sync.Once
	errLock         sync.Mutex
	runErr          error
}

// NewFleet builds and starts the providers for the given entries. Providers
// begin disconnected and are connected by the control loop across the ramp
// window. wsUrls are the exchange websocket urls (providers spread across
// them); wsPorts is the set of their ports, used to impair only the platform
// connection (not the provider's api calls).
func NewFleet(
	ctx context.Context,
	config *Config,
	entries []ProviderEntry,
	apiUrl string,
	wsUrls []string,
	wsPorts map[int]bool,
	rampDuration time.Duration,
) (*Fleet, error) {
	self := &Fleet{
		ctx:               ctx,
		config:            config,
		apiUrl:            apiUrl,
		wsUrls:            wsUrls,
		wsPorts:           wsPorts,
		rampDuration:      rampDuration,
		providers:         make([]*simProvider, 0, len(entries)),
		dynamicsStart:     make(chan chan struct{}),
		done:              make(chan struct{}),
		networkAdminUsers: firstNetworkAdminUsers(config.Fleet),
	}

	now := server.NowUtc()
	for i, entry := range entries {
		sp, err := self.newSimProvider(entry, i)
		if err != nil {
			self.closeAll()
			return nil, fmt.Errorf("provider %d create: %w", entry.Index, err)
		}
		// stagger the first connect uniformly across the ramp window
		sp.control = newRng(entry.Seed)
		sp.nextChurn = now.Add(time.Duration(sp.control.float64() * float64(self.rampDuration)))
		self.providers = append(self.providers, sp)
	}

	go func() {
		defer close(self.done)
		defer self.closeAll()
		server.HandleError(self.run, func(err error) {
			self.errLock.Lock()
			self.runErr = err
			self.errLock.Unlock()
		})
	}()
	return self, nil
}

func firstNetworkAdminUsers(entries []ProviderEntry) map[string]string {
	users := make(map[string]string)
	for _, entry := range entries {
		if _, ok := users[entry.NetworkId]; !ok {
			users[entry.NetworkId] = entry.UserId
		}
	}
	return users
}

func (self *Fleet) newSimProvider(entry ProviderEntry, index int) (*simProvider, error) {
	networkId, err := server.ParseId(entry.NetworkId)
	if err != nil {
		return nil, err
	}
	adminUserId := entry.UserId
	if configuredAdminUserId, ok := self.networkAdminUsers[entry.NetworkId]; ok {
		adminUserId = configuredAdminUserId
	}
	userId, err := server.ParseId(adminUserId)
	if err != nil {
		return nil, err
	}
	deviceId, err := server.ParseId(entry.DeviceId)
	if err != nil {
		return nil, err
	}
	clientId, err := server.ParseId(entry.ClientId)
	if err != nil {
		return nil, err
	}

	byJwt := jwtSign(networkId, userId, entry.NetworkId, deviceId, clientId)

	params := &atomic.Pointer[impairParams]{}
	base := baseParams(entry)
	params.Store(base)

	// present the provider's fake ip as the forwarded-for address so the
	// server geolocates it to the sim region (via the ip_overrides hook)
	extraHeaders := http.Header{}
	extraHeaders.Set("X-UR-Forwarded-For", fmt.Sprintf("%s:%d", entry.Ip, 40000+index%20000))

	// impair only the platform websocket connection, not the provider's api
	// calls (matched by destination port). `--impair=false` establishes a clean
	// baseline (useful for isolating impairment from the rest of the stack).
	dialContext := func(ctx context.Context, network string, addr string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if impairEnabled && self.isWsAddr(addr) {
			return newImpairConn(conn, params, entry.Seed), nil
		}
		return conn, nil
	}

	// spread providers across the exchange ws urls
	wsUrl := self.wsUrls[index%len(self.wsUrls)]

	provider := sdk.NewSimProvider(self.ctx, &sdk.SimProviderConfig{
		ApiUrl:                self.apiUrl,
		PlatformUrl:           wsUrl,
		ByJwt:                 byJwt,
		ClientId:              connect.Id(clientId),
		InstanceId:            connect.NewId(),
		AppVersion:            "0.0.0-sim",
		ExtraHeaders:          extraHeaders,
		DialContext:           dialContext,
		DisableSecurityPolicy: true,
		MaxConcurrentFlows:    entry.MaxConnections,
		Log:                   connect.NewNoopLogger(),
	})

	// UpdateClientScores now admits only providers whose Public or Network
	// provide key has been committed server-side. SimProvider first publishes
	// its provide frame before its platform transport exists, which can race or
	// lose that initial send. Re-publish after NewSimProvider has connected its
	// transport and wait for the acknowledgement before this provider
	// participates in the fleet. The later prewarm/settle interval provides the
	// server-side commit barrier before UpdateClientScores reads the keys.
	registerCtx, registerCancel := context.WithTimeout(self.ctx, providerRegistrationTimeout)
	defer registerCancel()
	connectedTicker := time.NewTicker(10 * time.Millisecond)
	defer connectedTicker.Stop()
	for !provider.IsConnected() {
		select {
		case <-connectedTicker.C:
		case <-registerCtx.Done():
			provider.Close()
			return nil, fmt.Errorf("wait for provider transport: %w", registerCtx.Err())
		}
	}
	registered := make(chan error, 1)
	provider.Client().ContractManager().SetProvideModesWithReturnTrafficWithOobAckCallback(
		map[protocol.ProvideMode]bool{
			protocol.ProvideMode_Network: true,
			protocol.ProvideMode_Public:  true,
		},
		func(err error) {
			select {
			case registered <- err:
			default:
			}
		},
	)
	select {
	case err := <-registered:
		if err != nil {
			provider.Close()
			return nil, fmt.Errorf("register provide keys: %w", err)
		}
	case <-registerCtx.Done():
		provider.Close()
		return nil, fmt.Errorf("register provide keys: %w", registerCtx.Err())
	}
	// providers start offline; the control loop connects them across the ramp
	provider.SetConnected(false)

	return &simProvider{
		entry:    entry,
		provider: provider,
		params:   params,
		base:     base,
		degraded: degradedParams(entry),
	}, nil
}

func (self *Fleet) isWsAddr(addr string) bool {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return false
	}
	return self.wsPorts[port]
}

// run drives every provider's churn and regime schedule.
func (self *Fleet) run() {
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-self.ctx.Done():
			self.closeAll()
			return
		case started := <-self.dynamicsStart:
			if !self.dynamicsStarted {
				self.startDynamicsAt(server.NowUtc())
			}
			close(started)
		case <-ticker.C:
			self.tick()
		}
	}
}

func (self *Fleet) tick() {
	now := server.NowUtc()
	connectedCount := 0
	for _, sp := range self.providers {
		if self.advanceProviderState(sp, now) {
			sp.provider.SetConnected(sp.connected)
		}
		if sp.connected {
			connectedCount += 1
		}
	}
	logf("fleet tick: %d/%d providers connected", connectedCount, len(self.providers))
}

// Applies one control-loop tick. Before measurement it performs only each
// provider's one-way ramp transition. Once dynamics start, it advances churn
// and degraded regimes from the measurement-anchored seeded schedule.
func (self *Fleet) advanceProviderState(sp *simProvider, now time.Time) bool {
	wasConnected := sp.connected
	if !self.dynamicsStarted {
		if !sp.connected && !now.Before(sp.nextChurn) {
			sp.connected = true
			sp.nextChurn = time.Time{}
		}
		return wasConnected != sp.connected
	}

	if !now.Before(sp.nextChurn) {
		sp.connected = !sp.connected
		if sp.connected {
			sp.nextChurn = now.Add(secondsDur(sp.entry.UptimeSeconds, sp.control))
		} else {
			sp.nextChurn = now.Add(secondsDur(sp.entry.DowntimeSeconds, sp.control))
		}
	}
	if !sp.nextRegime.IsZero() && !now.Before(sp.nextRegime) {
		sp.inDegraded = !sp.inDegraded
		if sp.inDegraded {
			sp.params.Store(sp.degraded)
		} else {
			sp.params.Store(sp.base)
		}
		sp.nextRegime = now.Add(self.regimeDwell(sp, sp.inDegraded))
	}
	return wasConnected != sp.connected
}

// Anchors seeded churn and degradation to measurement rather than fleet
// construction. Initial degradation is sampled from its stationary fraction,
// with a uniform residual dwell so providers do not change regime in lockstep.
func (self *Fleet) startDynamicsAt(now time.Time) {
	self.dynamicsStarted = true
	for _, sp := range self.providers {
		if !sp.connected {
			sp.connected = true
			sp.provider.SetConnected(true)
		}
		sp.nextChurn = now.Add(secondsDur(sp.entry.UptimeSeconds, sp.control))

		degradedFraction := sp.entry.DegradedFraction
		if degradedFraction <= 0 {
			sp.inDegraded = false
			sp.params.Store(sp.base)
			sp.nextRegime = time.Time{}
			continue
		}
		if 0.99 < degradedFraction {
			degradedFraction = 0.99
		}
		sp.inDegraded = sp.control.float64() < degradedFraction
		if sp.inDegraded {
			sp.params.Store(sp.degraded)
		} else {
			sp.params.Store(sp.base)
		}
		regimeRemaining := time.Duration(
			sp.control.float64() * float64(self.regimeDwell(sp, sp.inDegraded)),
		)
		if regimeRemaining < time.Second {
			regimeRemaining = time.Second
		}
		sp.nextRegime = now.Add(regimeRemaining)
	}
	logf("provider dynamics started at the measurement boundary")
}

// Starts measurement dynamics exactly once and waits for the fleet control
// goroutine to apply the boundary. Repeated requests are acknowledged without
// drawing another seeded schedule.
func (self *Fleet) StartDynamics(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("provider dynamics context is nil")
	}
	started := make(chan struct{})
	select {
	case self.dynamicsStart <- started:
	case <-ctx.Done():
		return ctx.Err()
	case <-self.done:
		return fmt.Errorf("provider fleet stopped before dynamics started")
	}
	select {
	case <-started:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-self.done:
		return fmt.Errorf("provider fleet stopped while dynamics started")
	}
}

// regimeDwell returns how long a provider stays in the given regime, so the
// long-run fraction in the degraded regime matches the entry's
// DegradedFraction. The good-regime dwell is a base period; the degraded
// dwell is scaled to hit the target fraction.
func (self *Fleet) regimeDwell(sp *simProvider, degraded bool) time.Duration {
	base := 60 * time.Second
	f := sp.entry.DegradedFraction
	if f <= 0 {
		if degraded {
			return time.Second
		}
		return 10 * time.Minute
	}
	if f >= 1 {
		f = 0.99
	}
	if degraded {
		// degradedDwell / (goodDwell + degradedDwell) = f
		return time.Duration(float64(base) * f / (1 - f))
	}
	return base
}

func (self *Fleet) closeAll() {
	self.closeOnce.Do(func() {
		for _, sp := range self.providers {
			sp.provider.Close()
		}
	})
}

func (self *Fleet) ConnectedCount() int {
	count := 0
	for _, sp := range self.providers {
		if sp.connected {
			count += 1
		}
	}
	return count
}

// Returns the cumulative bytes that providers successfully handed to their
// client-facing return path. Each provider counter is atomic; summing a live
// fleet is a boundary snapshot rather than a transaction across providers.
func (self *Fleet) ProviderEgressByteCount() int64 {
	var byteCount int64
	for _, sp := range self.providers {
		byteCount += int64(sp.provider.PacketStats().RemoteEgressByteCount)
	}
	return byteCount
}

// Wait joins the fleet control goroutine after its context is canceled.
func (self *Fleet) Wait() error {
	<-self.done
	self.errLock.Lock()
	defer self.errLock.Unlock()
	return self.runErr
}

// jwtSign mints a client jwt (network + user + device + client), the auth a
// SimProvider/SimClient presents. Current server validation verifies both the
// signature and the corresponding active identity rows provisioned for the run.
func jwtSign(networkId server.Id, userId server.Id, networkName string, deviceId server.Id, clientId server.Id) string {
	return jwt.NewByJwt(networkId, userId, networkName, false, false).
		Client(deviceId, clientId).Sign()
}

func secondsDur(seconds float64, r *rng) time.Duration {
	if seconds <= 0 {
		return time.Second
	}
	// exponential around the mean so churn is not lockstep
	u := r.float64()
	if u <= 0 {
		u = 1e-9
	}
	d := -seconds * math.Log(u)
	return time.Duration(d * float64(time.Second))
}
