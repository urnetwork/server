package main

// the client load driver: a warm pool of long-lived headless clients (each a
// SimClient = multi-client + tun over the pre-provisioned identities), then
// mean M arrivals per minute as a per-second Poisson process, each routed as a
// crawl through a pooled client. The crawl fetches the fake site and follows
// discovered suburls until the tree terminates, emitting one CSV row per
// request to stdout (the only thing on stdout); everything else is stderr.
//
// The pool is warmed once before the measured window (Warmup), so the exchange
// holds a stable, bounded set of connections. Standing up a fresh client per
// arrival instead created a per-arrival connection storm that overwhelmed the
// in-process exchange with auth timeouts.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sdk"
	"github.com/urnetwork/server"
)

// ClientIdentity is one pre-provisioned client pool entry.
type ClientIdentity struct {
	ClientId server.Id
	ByJwt    string
}

type ClientDriver struct {
	ctx    context.Context
	config *Config
	apiUrl string
	wsUrls []string
	// the crawl origin address the providers egress to (host:port)
	siteAddr string
	// the sim region country location id, used as the provider spec
	locationId server.Id
	pool       []ClientIdentity
	// crawl deadline and official failed-observation score ceiling
	requestTimeout time.Duration

	out     *bufio.Writer
	outLock sync.Mutex
	csvHash hash.Hash
	csvSize int64
	// every emitted CSV row, retained for the end-of-run summary (run.json)
	rows []resultRow

	active  sync.WaitGroup
	clients []*pooledClient
	close   sync.Once

	// Measurement completion stops new arrivals without canceling the run
	// context. Crawls admitted before the boundary therefore get their full
	// request deadline instead of being turned into artificial failures by the
	// measurement timer.
	arrivalsStop     chan struct{}
	arrivalsStopOnce sync.Once
}

func NewClientDriver(
	ctx context.Context,
	config *Config,
	apiUrl string,
	wsUrls []string,
	siteAddr string,
	locationId server.Id,
	pool []ClientIdentity,
	requestTimeout time.Duration,
) *ClientDriver {
	if requestTimeout <= 0 {
		requestTimeout = 2 * time.Minute
	}
	return &ClientDriver{
		ctx:            ctx,
		config:         config,
		apiUrl:         apiUrl,
		wsUrls:         wsUrls,
		siteAddr:       siteAddr,
		locationId:     locationId,
		pool:           pool,
		requestTimeout: requestTimeout,
		out:            bufio.NewWriter(os.Stdout),
		csvHash:        sha256.New(),
		arrivalsStop:   make(chan struct{}),
	}
}

// pooledClient is a long-lived, warm client: an established SimClient and its
// http client. Arrivals crawl through these instead of each standing up a fresh
// client, so the in-process exchange holds a stable, bounded set of connections
// rather than a per-arrival connection storm (which overwhelmed it with auth
// timeouts). A warm client serves many concurrent crawls — the tun multiplexes
// flows.
type pooledClient struct {
	simClient  *sdk.SimClient
	httpClient *http.Client
	label      string
}

const (
	// warmupConcurrency bounds how many clients establish at once. Each client
	// brings up a window of provider connections (several window-client auths),
	// so establishing the whole pool at once floods the in-process exchange and
	// the auths time out. A small concurrency lets each client's window settle.
	warmupConcurrency = 4

	// A transient window-auth miss used to invalidate an otherwise healthy
	// 30-minute evaluation at 199/200 clients. Retry only the missing identities,
	// with a new SimClient, after the rest of the pool has settled. The bound is
	// deliberately small so an unhealthy exchange still fails closed promptly.
	warmupAttempts   = 3
	warmupRetryDelay = 2 * time.Second
	// A parallel cohort can legitimately take longer than the old single-flow
	// probe under impairment, while the outer minute still bounds recovery.
	warmupCohortTimeout = 30 * time.Second

	// Warmed HTTP connections are owned by the evaluation context and closed
	// explicitly. Keep their underlying TCP flows beyond any qualification
	// window so a low arrival rate cannot turn the last pool slots cold again.
	warmClientTCPIdleTimeout = 24 * time.Hour

	resultCSVHeader = "t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s"
)

// siteHeaderMaxBytes is a structural bound on the fake site's leading JSON
// line. The real site pages are only a few hundred bytes. Keeping a generous
// hard limit prevents a corrupt response from turning completeness validation
// into an unbounded allocation.
const siteHeaderMaxBytes = 1024 * 1024

// siteResponse is the result of consuming one fake-site response completely.
// receivedBytes always reports the bytes actually read, including for an
// incomplete body. A response is complete only when a 200 response contains a
// valid newline-terminated page header, exactly page.Size padding bytes, and
// (when supplied) a matching Content-Length.
type siteResponse struct {
	page          sitePage
	receivedBytes int64
	complete      bool
}

// readSiteResponse consumes and closes response.Body. It is shared by warm-up
// and measured requests so a partial response can never establish a warm
// tunnel and then be treated more strictly only after measurement starts.
func readSiteResponse(response *http.Response) (result siteResponse, retErr error) {
	if response == nil || response.Body == nil {
		return result, fmt.Errorf("missing HTTP response body")
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close response body: %w", err))
			result.complete = false
		}
	}()

	// Non-200 responses are failures regardless of their representation. Drain
	// them so connection reuse remains safe, and still validate Content-Length
	// because a transport read error must remain visible as an incomplete
	// observation rather than being silently swallowed.
	if response.StatusCode != http.StatusOK {
		n, err := io.Copy(io.Discard, response.Body)
		result.receivedBytes = n
		if err != nil {
			return result, fmt.Errorf("read non-200 response body: %w", err)
		}
		if 0 <= response.ContentLength && response.ContentLength != n {
			return result, fmt.Errorf(
				"non-200 content length mismatch: received %d, header declared %d",
				n,
				response.ContentLength,
			)
		}
		return result, nil
	}

	reader := bufio.NewReader(response.Body)
	headerLine, headerBytes, headerErr := readBoundedHeaderLine(reader, siteHeaderMaxBytes)
	result.receivedBytes = headerBytes
	if headerErr != nil {
		// Drain what remains so receivedBytes remains honest even when the
		// leading line itself is truncated or the reader reports an injected
		// transport error.
		n, drainErr := io.Copy(io.Discard, reader)
		result.receivedBytes += n
		return result, errors.Join(
			fmt.Errorf("read page header: %w", headerErr),
			drainErr,
		)
	}
	if len(headerLine) == 0 || headerLine[len(headerLine)-1] != '\n' {
		return result, fmt.Errorf("page header is not newline terminated")
	}
	if err := json.Unmarshal(headerLine[:len(headerLine)-1], &result.page); err != nil {
		n, drainErr := io.Copy(io.Discard, reader)
		result.receivedBytes += n
		return result, errors.Join(fmt.Errorf("invalid page header: %w", err), drainErr)
	}
	if result.page.Size < 0 {
		n, drainErr := io.Copy(io.Discard, reader)
		result.receivedBytes += n
		return result, errors.Join(fmt.Errorf("invalid negative page size %d", result.page.Size), drainErr)
	}

	bodyBytes, bodyErr := io.Copy(io.Discard, reader)
	result.receivedBytes += bodyBytes
	if bodyErr != nil {
		return result, fmt.Errorf("read page body: %w", bodyErr)
	}
	if bodyBytes != int64(result.page.Size) {
		return result, fmt.Errorf(
			"page body size mismatch: received %d, page declared %d",
			bodyBytes,
			result.page.Size,
		)
	}
	expectedBytes := int64(len(headerLine)) + int64(result.page.Size)
	if 0 <= response.ContentLength && response.ContentLength != expectedBytes {
		return result, fmt.Errorf(
			"content length mismatch: header declared %d, page requires %d",
			response.ContentLength,
			expectedBytes,
		)
	}
	if result.receivedBytes != expectedBytes {
		return result, fmt.Errorf(
			"response byte mismatch: received %d, expected %d",
			result.receivedBytes,
			expectedBytes,
		)
	}
	result.complete = true
	return result, nil
}

// readBoundedHeaderLine is bufio.ReadBytes with a real allocation bound. It
// reports every byte consumed even after the retained line reaches maxBytes so
// incomplete-response volume accounting stays honest.
func readBoundedHeaderLine(reader *bufio.Reader, maxBytes int) ([]byte, int64, error) {
	line := make([]byte, 0, min(maxBytes, 4096))
	consumed := int64(0)
	for {
		fragment, err := reader.ReadSlice('\n')
		consumed += int64(len(fragment))
		remaining := maxBytes - len(line)
		if 0 < remaining {
			keep := len(fragment)
			if remaining < keep {
				keep = remaining
			}
			line = append(line, fragment[:keep]...)
		}
		if maxBytes < int(consumed) {
			return line, consumed, fmt.Errorf("page header exceeds %d bytes", maxBytes)
		}
		switch err {
		case nil:
			return line, consumed, nil
		case bufio.ErrBufferFull:
			continue
		default:
			return line, consumed, err
		}
	}
}

// Warmup builds the warm client pool (part of the warm-up period, before the
// measured window) so pool-setup time is not counted in the measurement.
// Clients establish in small concurrent batches so their window-client auths
// do not overwhelm the exchange.
func (self *ClientDriver) Warmup(ctx context.Context) error {
	self.writeCsvHeader()
	self.clients = buildWarmClientPool(
		ctx,
		self.pool,
		warmupAttempts,
		warmupRetryDelay,
		self.newWarmClient,
	)
	if err := warmClientPoolError(ctx, len(self.clients), len(self.pool)); err != nil {
		logf("warm client pool incomplete: %d/%d clients", len(self.clients), len(self.pool))
		return err
	}
	logf("warm client pool built: %d/%d clients; validating the measurement boundary", len(self.clients), len(self.pool))
	validated := validateWarmClientPool(
		ctx,
		self.clients,
		warmupAttempts,
		warmupRetryDelay,
		func(validateCtx context.Context, client *pooledClient, _ int) bool {
			return self.warmupTunnel(validateCtx, client.simClient, client.httpClient)
		},
		func(client *pooledClient) bool {
			return client != nil && client.simClient != nil && qualityWindowReady(
				client.simClient.MultiClient().Exits(),
				self.qualityWindowSize(),
			)
		},
	)
	logf("warm client pool ready: %d/%d clients", validated, len(self.pool))
	return warmClientPoolError(ctx, validated, len(self.pool))
}

// A deadline racing with the final successful builder must not invalidate a
// complete pool. An incomplete pool still preserves the context cause so the
// caller can distinguish an exhausted retry budget from its hard deadline.
func warmClientPoolError(ctx context.Context, established int, requested int) error {
	if established == requested {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fmt.Errorf("established %d/%d warm clients", established, requested)
}

// Make one real quality-ranked discovery call under a pool identity. Warm
// clients discover before measurement and may not need a replacement during a
// short window, so this supplies a deterministic in-window audit sample while
// exercising the same submitted matchmaking implementation.
func (self *ClientDriver) ProbeMatchmaking(ctx context.Context) error {
	if len(self.pool) == 0 {
		return errors.New("matchmaking probe has no client identity")
	}
	identity := self.pool[0]
	extraHeaders := http.Header{}
	extraHeaders.Set("X-UR-Forwarded-For", self.clientForwardedFor(identity.ClientId))
	clientStrategySettings := connect.DefaultClientStrategySettings()
	clientStrategySettings.Log = connect.NewNoopLogger()
	clientStrategySettings.EnableNormal = true
	clientStrategySettings.EnableResilient = false
	clientStrategySettings.ExtraHeaders = extraHeaders
	clientStrategy := connect.NewClientStrategy(ctx, clientStrategySettings)
	api := sdk.NewApi(ctx, clientStrategy, self.apiUrl)
	defer api.Close()
	api.SetByJwt(identity.ByJwt)

	locationId, err := sdk.ParseId(self.locationId.String())
	if err != nil {
		return fmt.Errorf("parse matchmaking probe location: %w", err)
	}
	clientId, err := sdk.ParseId(identity.ClientId.String())
	if err != nil {
		return fmt.Errorf("parse matchmaking probe client: %w", err)
	}
	specs := sdk.NewProviderSpecList()
	specs.Add(&sdk.ProviderSpec{LocationId: locationId})
	excludeClientIds := sdk.NewIdList()
	excludeClientIds.Add(clientId)
	count := self.config.Clients.QualityWindowSize
	if count < 1 {
		count = 1
	}
	result, err := api.FindProviders2SyncWithContext(ctx, &sdk.FindProviders2Args{
		Specs:            specs,
		Count:            count,
		ExcludeClientIds: excludeClientIds,
		RankMode:         "quality",
	})
	if err != nil {
		return fmt.Errorf("find providers: %w", err)
	}
	if result == nil || result.ProviderStats == nil || result.ProviderStats.Len() == 0 {
		return errors.New("matchmaking probe returned an empty provider pool")
	}
	return nil
}

// buildWarmClientPool preserves fixture order regardless of goroutine
// completion order. Besides making arrival-to-client assignment reproducible,
// the stable pool index pins each identity to the same exchange host. A later
// attempt touches only slots that are still missing.
func buildWarmClientPool(
	ctx context.Context,
	pool []ClientIdentity,
	attempts int,
	retryDelay time.Duration,
	build func(context.Context, ClientIdentity, int) *pooledClient,
) []*pooledClient {
	if attempts < 1 {
		attempts = 1
	}
	slots := make([]*pooledClient, len(pool))
	sem := make(chan struct{}, warmupConcurrency)

	for attempt := 1; attempt <= attempts && ctx.Err() == nil; attempt++ {
		var wg sync.WaitGroup
		for index, identity := range pool {
			if slots[index] != nil {
				continue
			}
			index := index
			identity := identity
			wg.Add(1)
			go server.HandleError(func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				if ctx.Err() != nil {
					return
				}
				slots[index] = build(ctx, identity, index)
			})
		}
		wg.Wait()

		missing := 0
		for _, client := range slots {
			if client == nil {
				missing++
			}
		}
		if missing == 0 || attempt == attempts || ctx.Err() != nil {
			break
		}
		logf(
			"warm client pool attempt %d/%d left %d/%d missing; retrying only missing clients",
			attempt,
			attempts,
			missing,
			len(pool),
		)
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}

	clients := make([]*pooledClient, 0, len(pool))
	for _, client := range slots {
		if client != nil {
			clients = append(clients, client)
		}
	}
	return clients
}

// Rechecks every established client after pool construction, when the oldest
// clients have been idle the longest. Successful slots retain their live HTTP
// lanes; only a slot that failed its request cohort or lost its complete
// quality window is retried.
func validateWarmClientPool(
	ctx context.Context,
	clients []*pooledClient,
	attempts int,
	retryDelay time.Duration,
	validate func(context.Context, *pooledClient, int) bool,
	ready func(*pooledClient) bool,
) int {
	if attempts < 1 {
		attempts = 1
	}
	validated := make([]bool, len(clients))
	sem := make(chan struct{}, warmupConcurrency)

	for attempt := 1; attempt <= attempts && ctx.Err() == nil; attempt++ {
		var wg sync.WaitGroup
		for index, client := range clients {
			if validated[index] {
				continue
			}
			index := index
			client := client
			wg.Add(1)
			go server.HandleError(func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					return
				}
				if ctx.Err() == nil && client != nil {
					validated[index] = validate(ctx, client, index)
				}
			})
		}
		wg.Wait()

		validatedCount := 0
		for index, client := range clients {
			// Readiness is a level, not merely the edge observed when the
			// request cohort completed. Rescan it after the whole batch so a
			// window lost during validation cannot cross into measurement.
			if validated[index] && !ready(client) {
				validated[index] = false
			}
			if validated[index] {
				validatedCount++
			}
		}
		if validatedCount == len(clients) || attempt == attempts || ctx.Err() != nil {
			return validatedCount
		}
		logf(
			"warm client validation attempt %d/%d left %d/%d unready; retrying only unready clients",
			attempt,
			attempts,
			len(clients)-validatedCount,
			len(clients),
		)
		select {
		case <-ctx.Done():
			return validatedCount
		case <-time.After(retryDelay):
		}
	}

	validatedCount := 0
	for _, ok := range validated {
		if ok {
			validatedCount++
		}
	}
	return validatedCount
}

// StopArrivals closes the admission boundary exactly once. It deliberately
// does not cancel the driver context: Run drains all crawls admitted before
// this boundary using their original request deadlines.
func (self *ClientDriver) StopArrivals() {
	self.arrivalsStopOnce.Do(func() {
		if self.arrivalsStop != nil {
			close(self.arrivalsStop)
		}
	})
}

// Run drives Poisson arrivals as crawls routed across the warm pool until the
// admission boundary is closed or the context is canceled. Call Warmup first.
func (self *ClientDriver) Run() error {
	defer self.Close()
	if len(self.clients) == 0 {
		return fmt.Errorf("no warm clients established; nothing to measure")
	}

	r := newRng(self.config.Seed ^ 0x5eed)
	meanPerSecond := self.config.Clients.MeanPerMinute / 60.0

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	drain := func() error {
		self.active.Wait()
		return self.flush()
	}

	next := 0
	for {
		select {
		case <-self.ctx.Done():
			return drain()
		case <-self.arrivalsStop:
			return drain()
		case <-ticker.C:
			// A timer tick and the stop boundary can become ready together.
			// Recheck admission before spawning the batch so no post-boundary
			// crawl is accidentally admitted by select's random choice.
			select {
			case <-self.arrivalsStop:
				return drain()
			default:
			}
			arrivals := r.poisson(meanPerSecond)
			for i := 0; i < arrivals; i += 1 {
				client := self.clients[next%len(self.clients)]
				next += 1
				self.active.Add(1)
				go server.HandleError(func() {
					defer self.active.Done()
					crawlCtx, cancel := context.WithTimeout(self.ctx, self.requestTimeout)
					defer cancel()
					self.crawl(crawlCtx, client.label, client.httpClient)
				})
			}
		}
	}
}

func (self *ClientDriver) newWarmClient(warmupCtx context.Context, identity ClientIdentity, poolIndex int) *pooledClient {
	// present a client-subnet ip as the forwarded-for address so the caller
	// geolocates to the sim region; the spec also targets the region directly
	extraHeaders := http.Header{}
	extraHeaders.Set("X-UR-Forwarded-For", self.clientForwardedFor(identity.ClientId))

	specLocationId := self.locationId
	multiClientSettings := newWarmMultiClientSettings(self.config.Clients.QualityWindowSize)
	simClient, err := sdk.NewSimClient(self.ctx, &sdk.SimClientConfig{
		ApiUrl:            self.apiUrl,
		PlatformUrl:       self.wsUrls[poolIndex%len(self.wsUrls)],
		ByJwt:             identity.ByJwt,
		ClientId:          connect.Id(identity.ClientId),
		AppVersion:        "0.0.0-sim",
		DeviceDescription: "sim-client",
		DeviceSpec:        "sim-client",
		ExtraHeaders:      extraHeaders,
		Specs: []*connect.ProviderSpec{
			{LocationId: (*connect.Id)(&specLocationId)},
		},
		DisableSecurityPolicy: true,
		MultiClientSettings:   multiClientSettings,
		Log:                   connect.NewNoopLogger(),
	})
	if err != nil {
		logf("client %s create err: %s", identity.ClientId, err)
		return nil
	}

	httpClient := newWarmHTTPClient(simClient.DialContext, self.config.Clients.ConnectionsPerCrawl)

	// Establish the same parallel HTTP/1 lane cohort a measured crawl uses.
	// One successful request is insufficient: it leaves the other lanes cold.
	if !self.warmupTunnel(warmupCtx, simClient, httpClient) {
		httpClient.CloseIdleConnections()
		simClient.Close()
		logf("client %s did not establish a provider path", identity.ClientId)
		return nil
	}
	return &pooledClient{simClient: simClient, httpClient: httpClient, label: identity.ClientId.String()}
}

// Builds the simulator's client policy. An explicit calibration size fixes the
// active profile too: the fake site uses an ephemeral HTTP port, which auto
// mode otherwise routes through the speed window while readiness inspects the
// unused quality window. Zero retains the production auto policy.
func newWarmMultiClientSettings(qualityWindowSize int) *connect.MultiClientSettings {
	multiClientSettings := connect.DefaultMultiClientSettings()
	multiClientSettings.TcpSequenceIdleTimeout = warmClientTCPIdleTimeout
	if qualityWindowSize <= 0 {
		return multiClientSettings
	}

	qualityWindow := multiClientSettings.WindowSizes[connect.WindowTypeQuality]
	qualityWindow.WindowSizeMin = qualityWindowSize
	qualityWindow.WindowSizeMax = qualityWindowSize
	qualityWindow.WindowSizeHardMax = qualityWindowSize
	qualityWindow.FixedWindowSize = qualityWindowSize
	qualityWindow.WindowSizeReconnectScale = 1
	multiClientSettings.WindowSizes[connect.WindowTypeQuality] = qualityWindow
	multiClientSettings.DefaultPerformanceProfile = &connect.PerformanceProfile{
		WindowType: connect.WindowTypeQuality,
		WindowSize: qualityWindow,
	}
	return multiClientSettings
}

// Keeps every measured HTTP/1 lane idle and reusable for the lifetime of its
// pool client. MaxIdleConns alone is not sufficient: without the per-host
// value, net/http silently retains only two of the six crawl connections.
func newWarmHTTPClient(
	dialContext func(context.Context, string, string) (net.Conn, error),
	connectionsPerCrawl int,
) *http.Client {
	if connectionsPerCrawl < 1 {
		connectionsPerCrawl = 1
	}
	maxConnections := 4 * connectionsPerCrawl
	transport := &http.Transport{
		DialContext:           dialContext,
		MaxIdleConns:          maxConnections,
		MaxIdleConnsPerHost:   maxConnections,
		MaxConnsPerHost:       maxConnections,
		IdleConnTimeout:       0,
		DisableCompression:    true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 0,
	}
	return &http.Client{Transport: transport}
}

// Requires distinct, usable quality exits. A warning, quarantine, completed,
// or peer-only channel cannot carry a new measured public-network flow.
func qualityWindowReady(exits []*connect.ExitInfo, required int) bool {
	if required < 1 {
		required = 1
	}
	readyClientIds := map[connect.Id]bool{}
	for _, exit := range exits {
		if exit == nil || exit.WindowType != connect.WindowTypeQuality ||
			exit.Warning || exit.Quarantined || exit.Done || exit.P2pOnly {
			continue
		}
		readyClientIds[exit.ClientId] = true
	}
	return required <= len(readyClientIds)
}

// Exercises all lanes concurrently and accepts the cohort only after every
// response is complete. Serial requests could all reuse one healthy flow and
// leave the parallel crawl path untested.
func warmupRequestCohort(
	ctx context.Context,
	httpClient *http.Client,
	requestUrl string,
	requestCount int,
) bool {
	if requestCount < 1 {
		requestCount = 1
	}
	complete := make([]bool, requestCount)
	var wg sync.WaitGroup
	for index := 0; index < requestCount; index++ {
		index := index
		wg.Add(1)
		go server.HandleError(func() {
			defer wg.Done()
			if ctx.Err() != nil {
				return
			}
			req, err := http.NewRequestWithContext(ctx, "GET", requestUrl, nil)
			if err != nil {
				return
			}
			response, err := httpClient.Do(req)
			if err != nil {
				return
			}
			result, err := readSiteResponse(response)
			complete[index] = err == nil && result.complete
		})
	}
	wg.Wait()
	for _, ok := range complete {
		if !ok {
			return false
		}
	}
	return true
}

// Initiates the request cohort before checking the quality-window level. The
// window expands lazily on the first dial, so a readiness precheck would wait
// forever for the very exits the cohort creates.
func warmupRequestAttempt(
	ctx context.Context,
	requestCohort func(context.Context) bool,
	ready func() bool,
) bool {
	complete := requestCohort(ctx)
	return complete && ready()
}

// Proves every measured lane with a bounded request cohort, then requires the
// complete quality window. The post-request readiness check closes the race
// where a provider starts draining while its last response is in flight.
func (self *ClientDriver) warmupTunnel(
	ctx context.Context,
	simClient *sdk.SimClient,
	httpClient *http.Client,
) bool {
	warmupCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	requestUrl := fmt.Sprintf("http://%s/", self.siteAddr)
	qualityWindowSize := self.qualityWindowSize()
	for warmupCtx.Err() == nil {
		attemptCtx, cancelAttempt := context.WithTimeout(warmupCtx, warmupCohortTimeout)
		ready := warmupRequestAttempt(
			attemptCtx,
			func(requestCtx context.Context) bool {
				return warmupRequestCohort(
					requestCtx,
					httpClient,
					requestUrl,
					self.config.Clients.ConnectionsPerCrawl,
				)
			},
			func() bool {
				return qualityWindowReady(simClient.MultiClient().Exits(), qualityWindowSize)
			},
		)
		cancelAttempt()
		if ready {
			return true
		}
		select {
		case <-warmupCtx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
}

// Uses the fixed official size when present and otherwise the production
// quality-window floor cloned into each simulator client.
func (self *ClientDriver) qualityWindowSize() int {
	if 0 < self.config.Clients.QualityWindowSize {
		return self.config.Clients.QualityWindowSize
	}
	windowSize := connect.DefaultMultiClientSettings().WindowSizes[connect.WindowTypeQuality].WindowSizeMin
	if windowSize < 1 {
		return 1
	}
	return windowSize
}

// crawl fetches "/" then walks discovered suburls with a bounded worker pool.
// It fully unwinds on ctx cancel: queued-but-unconsumed jobs are balanced so
// the closer goroutine's pending.Wait() always completes (it used to leak one
// goroutine per timed-out crawl — thousands over a long run).
func (self *ClientDriver) crawl(ctx context.Context, clientLabel string, httpClient *http.Client) {
	type job struct {
		path  string
		depth int
	}

	jobs := make(chan job, 4096)
	var pending sync.WaitGroup

	submit := func(path string, depth int) {
		// checking ctx before Add narrows the enqueue-after-cancel window
		// (select can pick the send even with ctx done); anything that still
		// slips through is balanced by the post-worker drain below
		if ctx.Err() != nil {
			return
		}
		pending.Add(1)
		select {
		case jobs <- job{path: path, depth: depth}:
		case <-ctx.Done():
			pending.Done()
		}
	}

	workers := self.config.Clients.ConnectionsPerCrawl
	if workers < 1 {
		workers = 1
	}
	var workerWg sync.WaitGroup
	for w := 0; w < workers; w += 1 {
		workerWg.Add(1)
		go func() {
			defer workerWg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case j, ok := <-jobs:
					if !ok {
						return
					}
					childUrls := self.fetch(ctx, httpClient, clientLabel, j.path, j.depth)
					for _, childUrl := range childUrls {
						submit(childUrl, j.depth+1)
					}
					pending.Done()
				}
			}
		}()
	}

	submit("/", 0)

	// close jobs once all pending work drains
	closerDone := make(chan struct{})
	go func() {
		defer close(closerDone)
		pending.Wait()
		close(jobs)
	}()

	workerWg.Wait()
	// on cancel the workers exit without draining the queue; every queued job
	// still holds a pending count, so balance them here or pending.Wait()
	// above never returns. No submitter is live at this point: submit runs
	// only on this goroutine (above) and the workers (joined). pending == 0
	// exactly when the queue is empty, so the closer can only close(jobs)
	// once there is nothing left to balance.
	for balanced := false; !balanced; {
		select {
		case _, ok := <-jobs:
			if !ok {
				balanced = true // closed: all pending work was consumed
			} else {
				pending.Done()
			}
		default:
			balanced = true // empty: every queued job was balanced
		}
	}
	// joining the closer makes the no-leak invariant structural: crawl cannot
	// return while pending.Wait() is still blocked
	<-closerDone
}

// fetch performs one request through the tunnel, emits its CSV row, and
// returns the discovered child suburls.
func (self *ClientDriver) fetch(ctx context.Context, httpClient *http.Client, clientLabel string, path string, depth int) []string {
	requestUrl := fmt.Sprintf("http://%s%s", self.siteAddr, path)

	startTime := time.Now()
	var ttfb time.Duration
	trace := &httptrace.ClientTrace{
		GotFirstResponseByte: func() {
			ttfb = time.Since(startTime)
		},
	}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), "GET", requestUrl, nil)
	if err != nil {
		self.writeCsvRow(startTime, clientLabel, path, depth, 0, 0, 0, 0)
		return nil
	}

	response, err := httpClient.Do(req)
	if err != nil {
		self.writeCsvRow(startTime, clientLabel, path, depth, 0, 0, time.Since(startTime), 0)
		return nil
	}
	status := response.StatusCode
	read, readErr := readSiteResponse(response)
	// Preserve received bytes for volume/path-integrity accounting, but never
	// let a malformed or partial 200 enter the success population. The CSV
	// format stays stable: status=0 is the existing incomplete-request marker;
	// a completely drained non-200 retains its actual HTTP status.
	if readErr != nil || (status == http.StatusOK && !read.complete) {
		status = 0
	}
	totalTime := time.Since(startTime)
	self.writeCsvRow(startTime, clientLabel, path, depth, status, read.receivedBytes, ttfb, totalTime)
	if readErr != nil || !read.complete {
		return nil
	}
	return read.page.Urls
}

func (self *ClientDriver) writeCsvHeader() {
	self.outLock.Lock()
	defer self.outLock.Unlock()
	self.writeCsvLineLocked(resultCSVHeader + "\n")
}

func (self *ClientDriver) writeCsvRow(startTime time.Time, client string, path string, depth int, status int, bytes int64, ttfb time.Duration, total time.Duration) {
	bytesPerSecond := float64(0)
	if 0 < total {
		bytesPerSecond = float64(bytes) / total.Seconds()
	}
	self.outLock.Lock()
	defer self.outLock.Unlock()
	line := fmt.Sprintf("%d,%s,%s,%d,%d,%d,%.3f,%.3f,%.0f\n",
		startTime.UnixMilli(),
		client,
		path,
		depth,
		status,
		bytes,
		float64(ttfb)/float64(time.Millisecond),
		float64(total)/float64(time.Millisecond),
		bytesPerSecond,
	)
	self.writeCsvLineLocked(line)
	self.rows = append(self.rows, resultRow{
		tStartMs: startTime.UnixMilli(),
		status:   status,
		bytes:    bytes,
		ttfbMs:   float64(ttfb) / float64(time.Millisecond),
		totalMs:  float64(total) / float64(time.Millisecond),
	})
}

func (self *ClientDriver) writeCsvLineLocked(line string) {
	if self.csvHash == nil {
		self.csvHash = sha256.New()
	}
	_, _ = self.out.WriteString(line)
	_, _ = self.csvHash.Write([]byte(line))
	self.csvSize += int64(len(line))
}

// resultRows snapshots the rows recorded so far, for the run.json summary.
func (self *ClientDriver) resultRows() []resultRow {
	self.outLock.Lock()
	defer self.outLock.Unlock()
	return append([]resultRow{}, self.rows...)
}

// csvIdentity returns the digest of the exact header and rows emitted by this
// driver. The schema-2 sidecar and its completion marker bind this identity so
// a results CSV from another job cannot be mixed into an otherwise valid
// artifact bundle. Legacy non-driver log lines are intentionally outside it.
func (self *ClientDriver) csvIdentity() (string, int64) {
	self.outLock.Lock()
	defer self.outLock.Unlock()
	if self.csvHash == nil {
		self.csvHash = sha256.New()
	}
	return fmt.Sprintf("%x", self.csvHash.Sum(nil)), self.csvSize
}

// EstablishedCount is the number of warm clients that established a provider
// path during Warmup.
func (self *ClientDriver) EstablishedCount() int {
	return len(self.clients)
}

func (self *ClientDriver) flush() error {
	self.outLock.Lock()
	defer self.outLock.Unlock()
	return self.out.Flush()
}

// Close releases every established warm client. It is safe to call after Run
// and from setup-error teardown.
func (self *ClientDriver) Close() {
	self.close.Do(func() {
		for _, client := range self.clients {
			if client != nil && client.httpClient != nil {
				client.httpClient.CloseIdleConnections()
			}
			if client != nil && client.simClient != nil {
				client.simClient.Close()
			}
		}
	})
}

// clientForwardedFor derives a stable client-subnet address for a client id,
// so the caller geolocates to the sim region (via the ip_overrides hook).
func (self *ClientDriver) clientForwardedFor(clientId server.Id) string {
	prefix, err := netip.ParsePrefix(self.config.Subnets.Client)
	if err != nil {
		return "198.20.0.1:40000"
	}
	base := prefix.Masked().Addr().As4()
	idBytes := clientId.Bytes()
	// vary the low 16 bits from the id, keeping the /16 network
	base[2] = idBytes[14]
	base[3] = idBytes[15]
	addr := netip.AddrFrom4(base)
	port := 40000 + int(idBytes[13])%20000
	return net.JoinHostPort(addr.String(), fmt.Sprintf("%d", port))
}
