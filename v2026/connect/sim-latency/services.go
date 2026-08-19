package main

// the in-process server environment: a set of exchange hosts + connect
// handlers on websocket ports, a real api server, and a reliability pipeline
// loop that turns provider connections into selectable providers.
//
// Standing the services up in-process (rather than spawning the cli binaries)
// keeps the run self-contained — no child env/vault plumbing — and mirrors the
// full-stack perf test harness. The provider fleet is what benefits from
// process isolation and is sharded into subprocesses; the services and client
// measurement stay here.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/api"
	connectserver "github.com/urnetwork/server/v2026/connect"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/router"
)

// Services holds the running server environment.
type Services struct {
	apiUrl  string
	wsUrls  []string
	wsPorts map[int]bool
	cancel  context.CancelFunc

	httpServers []*http.Server
	exchanges   []*connectserver.Exchange
	wg          sync.WaitGroup
	closeOnce   sync.Once
	errLock     sync.Mutex
	runErr      error

	// when prewarmed, the pipeline does not recompute reliability scores (which
	// would overwrite the prewarmed scores); it only refreshes the location
	// reliabilities (so churn gates selection) and re-exports the redis samples.
	prewarmed atomic.Bool
}

const servicesDrainTimeout = 15 * time.Second

// SetPrewarmed switches the pipeline to prewarmed mode (see the field doc).
func (self *Services) SetPrewarmed(prewarmed bool) {
	self.prewarmed.Store(prewarmed)
}

// ServicesConfig configures the in-process environment.
type ServicesConfig struct {
	// number of exchange hosts (each its own ws port), to spread the ephemeral
	// port pressure of a large fleet across destination ports
	HostCount int
	ApiPort   int
	// first ws port; hosts use consecutive ports from here
	WsPortBase int
	// first internal exchange port; hosts use consecutive ports from here
	ExchangePortBase int

	// --- warm-up tuning (how fast the market reaches a stable state) ---
	// how often the reliability -> score -> redis-sample pipeline advances.
	// A shorter interval propagates provider state (new tests, churn) into the
	// selectable set faster, at more db/redis work.
	PipelineInterval time.Duration
	// how long after a connection with no active traffic the synthetic speed
	// test fires. The score gate needs a completed speed test, so this bounds
	// how soon a freshly connected provider can be selected. Production default
	// is 60s; the sim drops it low to shorten warm-up.
	SpeedTestTimeout time.Duration
	// the announce delay before a connection is registered + tested. Lower =
	// faster warm-up.
	AnnounceTimeout time.Duration
}

func DefaultServicesConfig() *ServicesConfig {
	return &ServicesConfig{
		HostCount:        4,
		ApiPort:          7640,
		WsPortBase:       7650,
		ExchangePortBase: 7750,
		// fast warm-up defaults: providers become selectable within a few
		// seconds of connecting + one pipeline round
		PipelineInterval: 10 * time.Second,
		SpeedTestTimeout: 3 * time.Second,
		AnnounceTimeout:  2 * time.Second,
	}
}

// NewServices stands up the exchanges, connect handlers, api server, and the
// pipeline loop. It blocks until every listener is reachable.
func NewServices(ctx context.Context, servicesConfig *ServicesConfig) (*Services, error) {
	if err := validateServicesConfig(servicesConfig); err != nil {
		return nil, err
	}
	serviceCtx, cancel := context.WithCancel(ctx)
	service := "connect"
	block := "sim"

	// routes: every host name resolves to loopback so residents forward to
	// each other's internal exchange ports
	routes := map[string]string{}
	hostNames := make([]string, servicesConfig.HostCount)
	for i := 0; i < servicesConfig.HostCount; i += 1 {
		hostNames[i] = fmt.Sprintf("sim%d", i)
		routes[hostNames[i]] = "127.0.0.1"
	}

	self := &Services{
		wsPorts: map[int]bool{},
		cancel:  cancel,
	}

	for i := 0; i < servicesConfig.HostCount; i += 1 {
		wsPort := servicesConfig.WsPortBase + i
		exchangePort := servicesConfig.ExchangePortBase + i

		settings := connectserver.DefaultExchangeSettings()
		// run the real per-connection latency + speed tests so scores reflect
		// the simulated conditions
		settings.ConnectionTestConfig = connectserver.DefaultTestConfig()
		// warm-up tuning: the score gate needs a completed speed test (a
		// provider missing it scores at the cutoff and is excluded), so fire the
		// synthetic speed test quickly and register/test the connection promptly.
		settings.ConnectionAnnounceTimeout = servicesConfig.AnnounceTimeout
		settings.ConnectionAnnounceSettings.SyntheticSpeedTimeout = servicesConfig.SpeedTestTimeout
		settings.ConnectionAnnounceSettings.PassiveSpeedWindowDuration = servicesConfig.SpeedTestTimeout
		// a large fleet reconnects often; do not throttle
		settings.ConnectionRateLimitSettings.BurstConnectionCount = 1_000_000
		settings.ConnectionRateLimitSettings.MaxTotalConnectionCount = 10_000_000

		exchange := connectserver.NewExchange(
			serviceCtx,
			hostNames[i],
			service,
			block,
			map[int]int{exchangePort: exchangePort},
			routes,
			settings,
		)
		self.exchanges = append(self.exchanges, exchange)

		connectHandler := connectserver.NewConnectHandler(serviceCtx, server.NewId(), exchange, &settings.ConnectHandlerSettings)
		connectRoutes := []*router.Route{
			router.NewRoute("GET", "/status", router.WarpStatus),
			router.NewRoute("GET", "/", connectHandler.Connect),
		}
		httpServer := &http.Server{
			Addr:    fmt.Sprintf("127.0.0.1:%d", wsPort),
			Handler: router.NewRouter(serviceCtx, connectRoutes),
		}
		self.httpServers = append(self.httpServers, httpServer)
		self.serve(httpServer)

		self.wsUrls = append(self.wsUrls, fmt.Sprintf("ws://127.0.0.1:%d", wsPort))
		self.wsPorts[wsPort] = true
	}

	apiServer := &http.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", servicesConfig.ApiPort),
		Handler: router.NewRouter(serviceCtx, api.Routes()),
	}
	self.httpServers = append(self.httpServers, apiServer)
	self.serve(apiServer)
	self.apiUrl = fmt.Sprintf("http://127.0.0.1:%d", servicesConfig.ApiPort)

	if err := self.waitReachable(serviceCtx, servicesConfig); err != nil {
		self.Close()
		return nil, err
	}

	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		server.HandleError(
			func() { self.runPipeline(serviceCtx, servicesConfig.PipelineInterval) },
			self.recordError,
		)
	}()
	go func() {
		<-serviceCtx.Done()
		self.Close()
	}()

	return self, nil
}

func validateServicesConfig(config *ServicesConfig) error {
	if config == nil {
		return errors.New("nil services config")
	}
	if config.HostCount <= 0 || 65535 < config.HostCount ||
		config.ApiPort <= 0 || 65535 < config.ApiPort ||
		config.WsPortBase <= 0 || 65535-(config.HostCount-1) < config.WsPortBase ||
		config.ExchangePortBase <= 0 || 65535-(config.HostCount-1) < config.ExchangePortBase {
		return errors.New("service host count or port range is invalid")
	}
	if config.PipelineInterval <= 0 || config.SpeedTestTimeout <= 0 || config.AnnounceTimeout <= 0 {
		return errors.New("service timing must be positive")
	}
	return nil
}

func (self *Services) serve(httpServer *http.Server) {
	self.wg.Add(1)
	go func() {
		defer self.wg.Done()
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			self.recordError(err)
		}
	}()
}

func (self *Services) recordError(err error) {
	if err == nil {
		return
	}
	self.errLock.Lock()
	defer self.errLock.Unlock()
	self.runErr = errors.Join(self.runErr, err)
}

func (self *Services) waitReachable(ctx context.Context, servicesConfig *ServicesConfig) error {
	statusOk := func(port int) bool {
		client := &http.Client{Timeout: 1 * time.Second}
		response, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/status", port))
		if err != nil {
			return false
		}
		defer response.Body.Close()
		return response.StatusCode == 200
	}
	ports := []int{servicesConfig.ApiPort}
	for i := 0; i < servicesConfig.HostCount; i += 1 {
		ports = append(ports, servicesConfig.WsPortBase+i)
	}
	deadline := time.Now().Add(30 * time.Second)
	for _, port := range ports {
		for !statusOk(port) {
			if deadline.Before(time.Now()) {
				return fmt.Errorf("service on port %d did not come up", port)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
	}
	return nil
}

// runPipeline advances the reliability -> score -> redis-sample pipeline that
// turns connected providers into selectable ones. Mirrors the taskworker's
// serial reliability tasks, driven on a fixed cadence for deterministic
// settle timing.
func (self *Services) runPipeline(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			self.RunPipelineOnce(ctx)
			logf("pipeline advanced")
		}
	}
}

// RunPipelineOnce advances the reliability->score->sample pipeline a single
// time. Used after prewarm to make providers selectable immediately.
func (self *Services) RunPipelineOnce(ctx context.Context) {
	ttl := 300 * time.Minute
	now := server.NowUtc()
	if self.prewarmed.Load() {
		// keep the location reliabilities fresh (churn -> connected state), then
		// re-export the redis samples from the prewarmed scores. Do not recompute
		// reliability scores (that would wipe the prewarm).
		server.HandleError(func() { model.UpdateClientLocationReliabilities(ctx, now.Add(-12*time.Hour), now) })
	} else {
		server.HandleError(func() { model.RollupClientReliabilityStats(ctx, now) })
		server.HandleError(func() { model.UpdateClientReliabilityScores(ctx, now, true) })
	}
	server.HandleError(func() {
		if err := model.UpdateClientScores(ctx, ttl, 8); err != nil {
			logf("pipeline UpdateClientScores err: %s", err)
		}
	})
}

func (self *Services) ApiUrl() string        { return self.apiUrl }
func (self *Services) WsUrls() []string      { return self.wsUrls }
func (self *Services) WsPorts() map[int]bool { return self.wsPorts }

func (self *Services) Close() error {
	self.closeOnce.Do(func() {
		self.cancel()
		drainCtx, cancel := context.WithTimeout(context.Background(), servicesDrainTimeout)
		defer cancel()
		for _, httpServer := range self.httpServers {
			if err := httpServer.Shutdown(drainCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				self.recordError(fmt.Errorf("HTTP shutdown: %w", err))
				if closeErr := httpServer.Close(); closeErr != nil && !errors.Is(closeErr, http.ErrServerClosed) {
					self.recordError(fmt.Errorf("HTTP forced close: %w", closeErr))
				}
			}
		}
		for _, exchange := range self.exchanges {
			exchange.Close()
		}
		for i, exchange := range self.exchanges {
			if !exchange.WaitForIdle(drainCtx) {
				self.recordError(fmt.Errorf("exchange %d did not drain within %s", i, servicesDrainTimeout))
			}
		}
	})
	self.wg.Wait()
	self.errLock.Lock()
	defer self.errLock.Unlock()
	return self.runErr
}
