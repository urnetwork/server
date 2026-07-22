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
	"time"

	"sync/atomic"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
)

// impairEnabled gates the provider network impairment (default on). Set from
// the --impair flag. When off, providers dial the exchange with a plain
// connection — a clean baseline for isolating impairment from the rest of the
// stack.
var impairEnabled = true

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

	providers []*simProvider
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
) *Fleet {
	self := &Fleet{
		ctx:          ctx,
		config:       config,
		apiUrl:       apiUrl,
		wsUrls:       wsUrls,
		wsPorts:      wsPorts,
		rampDuration: rampDuration,
		providers:    make([]*simProvider, 0, len(entries)),
	}

	now := server.NowUtc()
	for i, entry := range entries {
		sp, err := self.newSimProvider(entry, i)
		if err != nil {
			logf("provider %d create err: %s", entry.Index, err)
			continue
		}
		// stagger the first connect uniformly across the ramp window
		sp.control = newRng(entry.Seed)
		sp.nextChurn = now.Add(time.Duration(sp.control.float64() * float64(self.rampDuration)))
		sp.nextRegime = now.Add(self.regimeDwell(sp, false))
		self.providers = append(self.providers, sp)
	}

	go server.HandleError(self.run)
	return self
}

func (self *Fleet) newSimProvider(entry ProviderEntry, index int) (*simProvider, error) {
	networkId, err := server.ParseId(entry.NetworkId)
	if err != nil {
		return nil, err
	}
	userId, err := server.ParseId(entry.UserId)
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
		case <-ticker.C:
			self.tick()
		}
	}
}

func (self *Fleet) tick() {
	now := server.NowUtc()
	connectedCount := 0
	for _, sp := range self.providers {
		if !now.Before(sp.nextChurn) {
			sp.connected = !sp.connected
			sp.provider.SetConnected(sp.connected)
			if sp.connected {
				sp.nextChurn = now.Add(secondsDur(sp.entry.UptimeSeconds, sp.control))
			} else {
				sp.nextChurn = now.Add(secondsDur(sp.entry.DowntimeSeconds, sp.control))
			}
		}
		if !now.Before(sp.nextRegime) {
			sp.inDegraded = !sp.inDegraded
			if sp.inDegraded {
				sp.params.Store(sp.degraded)
			} else {
				sp.params.Store(sp.base)
			}
			sp.nextRegime = now.Add(self.regimeDwell(sp, sp.inDegraded))
		}
		if sp.connected {
			connectedCount += 1
		}
	}
	logf("fleet tick: %d/%d providers connected", connectedCount, len(self.providers))
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
	for _, sp := range self.providers {
		sp.provider.Close()
	}
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

// jwtSign mints a client jwt (network + user + device + client), the auth a
// SimProvider/SimClient presents. Signature-validated server-side, no db.
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
