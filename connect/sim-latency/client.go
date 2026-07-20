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
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"os"
	"strings"
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

	out     *bufio.Writer
	outLock sync.Mutex

	active  sync.WaitGroup
	seq     uint64
	clients []*pooledClient
}

func NewClientDriver(
	ctx context.Context,
	config *Config,
	apiUrl string,
	wsUrls []string,
	siteAddr string,
	locationId server.Id,
	pool []ClientIdentity,
) *ClientDriver {
	return &ClientDriver{
		ctx:        ctx,
		config:     config,
		apiUrl:     apiUrl,
		wsUrls:     wsUrls,
		siteAddr:   siteAddr,
		locationId: locationId,
		pool:       pool,
		out:        bufio.NewWriter(os.Stdout),
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

// warmupConcurrency bounds how many clients establish at once. Each client
// brings up a window of provider connections (several window-client auths), so
// establishing the whole pool at once floods the in-process exchange and the
// auths time out. A small concurrency lets each client's window settle.
const warmupConcurrency = 4

// Warmup builds the warm client pool (part of the warm-up period, before the
// measured window) so pool-setup time is not counted in the measurement.
// Clients establish in small concurrent batches so their window-client auths
// do not overwhelm the exchange.
func (self *ClientDriver) Warmup() {
	self.writeCsvHeader()

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, warmupConcurrency)
	for _, identity := range self.pool {
		identity := identity
		wg.Add(1)
		go server.HandleError(func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-self.ctx.Done():
				return
			}
			if self.ctx.Err() != nil {
				return
			}
			if client := self.newWarmClient(identity); client != nil {
				mu.Lock()
				self.clients = append(self.clients, client)
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	logf("warm client pool ready: %d/%d clients", len(self.clients), len(self.pool))
}

// Run drives Poisson arrivals as crawls routed across the warm pool, until the
// context is done. Call Warmup first.
func (self *ClientDriver) Run() {
	defer func() {
		for _, client := range self.clients {
			client.simClient.Close()
		}
	}()
	if len(self.clients) == 0 {
		logf("no warm clients established; nothing to measure")
		return
	}

	r := newRng(self.config.Seed ^ 0x5eed)
	meanPerSecond := self.config.Clients.MeanPerMinute / 60.0

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	next := 0
	for {
		select {
		case <-self.ctx.Done():
			self.active.Wait()
			self.flush()
			return
		case <-ticker.C:
			arrivals := r.poisson(meanPerSecond)
			for i := 0; i < arrivals; i += 1 {
				client := self.clients[next%len(self.clients)]
				next += 1
				self.active.Add(1)
				go server.HandleError(func() {
					defer self.active.Done()
					crawlCtx, cancel := context.WithTimeout(self.ctx, 2*time.Minute)
					defer cancel()
					self.crawl(crawlCtx, client.label, client.httpClient)
				})
			}
		}
	}
}

func (self *ClientDriver) newWarmClient(identity ClientIdentity) *pooledClient {
	// present a client-subnet ip as the forwarded-for address so the caller
	// geolocates to the sim region; the spec also targets the region directly
	extraHeaders := http.Header{}
	extraHeaders.Set("X-UR-Forwarded-For", self.clientForwardedFor(identity.ClientId))

	specLocationId := self.locationId
	simClient, err := sdk.NewSimClient(self.ctx, &sdk.SimClientConfig{
		ApiUrl:            self.apiUrl,
		PlatformUrl:       self.wsUrls[int(self.nextSeq())%len(self.wsUrls)],
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
		Log:                   connect.NewNoopLogger(),
	})
	if err != nil {
		logf("client %s create err: %s", identity.ClientId, err)
		return nil
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext:         simClient.DialContext,
			MaxIdleConns:        4 * self.config.Clients.ConnectionsPerCrawl,
			MaxConnsPerHost:     4 * self.config.Clients.ConnectionsPerCrawl,
			IdleConnTimeout:     60 * time.Second,
			DisableCompression:  true,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}

	// establish the provider path once (a single dial-until-ready, not a retry
	// storm across many clients), so measured crawls reflect steady-state
	// request latency rather than the one-time cold start.
	if !self.warmupTunnel(simClient, httpClient) {
		simClient.Close()
		logf("client %s did not establish a provider path", identity.ClientId)
		return nil
	}
	return &pooledClient{simClient: simClient, httpClient: httpClient, label: identity.ClientId.String()}
}

// warmupTunnel establishes a provider path by dialing the site root until it
// succeeds or the deadline passes. Each attempt has its own short timeout so a
// slow/failed attempt does not consume the whole budget — the multi-client
// needs several tries to discover, connect, and contract a provider.
func (self *ClientDriver) warmupTunnel(simClient *sdk.SimClient, httpClient *http.Client) bool {
	deadline := time.Now().Add(60 * time.Second)
	requestUrl := fmt.Sprintf("http://%s/", self.siteAddr)
	attempt := 0
	for time.Now().Before(deadline) && self.ctx.Err() == nil {
		attempt += 1
		ok := func() bool {
			attemptCtx, cancel := context.WithTimeout(self.ctx, 8*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(attemptCtx, "GET", requestUrl, nil)
			if err != nil {
				return false
			}
			response, err := httpClient.Do(req)
			if err != nil {
				return false
			}
			io.Copy(io.Discard, response.Body)
			response.Body.Close()
			return response.StatusCode == http.StatusOK
		}()
		if ok {
			return true
		}
		select {
		case <-self.ctx.Done():
			return false
		case <-time.After(500 * time.Millisecond):
		}
	}
	return false
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
	defer response.Body.Close()

	// read the leading JSON line for suburls, then drain the body for throughput
	reader := bufio.NewReader(response.Body)
	headerLine, _ := reader.ReadBytes('\n')
	bodyBytes, _ := io.Copy(io.Discard, reader)
	totalBytes := int64(len(headerLine)) + bodyBytes
	totalTime := time.Since(startTime)

	bytesPerSecond := float64(0)
	if 0 < totalTime {
		bytesPerSecond = float64(totalBytes) / totalTime.Seconds()
	}
	self.writeCsvRow(startTime, clientLabel, path, depth, response.StatusCode, totalBytes, ttfb, totalTime)
	_ = bytesPerSecond

	var page sitePage
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(headerLine))), &page); err != nil {
		return nil
	}
	return page.Urls
}

func (self *ClientDriver) writeCsvHeader() {
	self.outLock.Lock()
	defer self.outLock.Unlock()
	fmt.Fprintln(self.out, "t_start_ms,client,path,depth,status,bytes,ttfb_ms,total_ms,bytes_per_s")
}

func (self *ClientDriver) writeCsvRow(startTime time.Time, client string, path string, depth int, status int, bytes int64, ttfb time.Duration, total time.Duration) {
	bytesPerSecond := float64(0)
	if 0 < total {
		bytesPerSecond = float64(bytes) / total.Seconds()
	}
	self.outLock.Lock()
	defer self.outLock.Unlock()
	fmt.Fprintf(self.out, "%d,%s,%s,%d,%d,%d,%.3f,%.3f,%.0f\n",
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
}

func (self *ClientDriver) flush() {
	self.outLock.Lock()
	defer self.outLock.Unlock()
	self.out.Flush()
}

func (self *ClientDriver) nextSeq() uint64 {
	self.outLock.Lock()
	defer self.outLock.Unlock()
	self.seq += 1
	return self.seq
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
