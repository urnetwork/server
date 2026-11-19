package nginx

// One forwarded service's upstream blocks (owner: s-forward): a keepalive
// pool per block (upstream_conn.go), nginx's balancers (upstream_balance.go),
// and D7's retries as the review amended them:
//   - at most ForwardSettings.MaxUpstreamAttempts attempts that may have
//     reached a block, each on a distinct eligible block, never more than
//     the service has balanced blocks (nginx's tries), and only before any
//     response byte reached the client;
//   - an attempt is retried after a connect error, a timeout or an upstream
//     503, the method notwithstanding (`non_idempotent`); a final 503 passes
//     through;
//   - never after a 500, 502 or 504, which pass through, a malformed or
//     oversized response head, or a response cut short;
//   - never for a per-block lb route, which reports its block's own result;
//   - never for a streamed body once its request began going upstream,
//     unless its first read returned it whole and it was kept, as nginx
//     buffers a body its first pass reads whole (body_stream.go); a declared
//     empty body on a streamed route has nothing to stream, so it is retried
//     too;
//   - a reused keepalive connection that failed before any request byte was
//     written is redone at once on a fresh connection to the same block, and
//     isn't an attempt. nginx's rule is wider: any error on a cached
//     connection costs no try, written or not (ngx_http_upstream_next's
//     tries++ for u->peer.cached and NGX_HTTP_UPSTREAM_FT_ERROR), and with
//     one server it redoes the request there. A failure after the write may
//     have reached the block, so the front counts it as one of D7's attempts
//     and moves only to another block: a single-block service, or a
//     per-block lb route, answers that race with a 502 where nginx redoes the
//     request (LBALT-IMPL.md §4). Where nginx moves a redo to another block,
//     the front stays on the block it chose, as Go's transport does.
//
// With no attempt left, a connect error or a failed exchange is a 502 and a
// timeout a 504, as nginx maps them; a retry that finds no eligible block is
// nginx's "no live upstreams", a 502.
//
// Every physical attempt is counted (urnetwork_lbalt_upstream_attempts_total),
// so the parity harness can compare attempt counts with a recording
// upstream.

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A service's blocks and their pools. Safe for concurrent use.
type upstreamGroup struct {
	service   *RouteService
	settings  *FrontSettings
	bodyStore *bodyStore
	// in nginx's server order: sorted by name
	blocks          []*upstreamBlock
	blockNameBlocks map[string]*upstreamBlock
	// blocks with weight, the ones balancing picks from
	balancedBlockCount int
	// nginx's consistent-hash ring, for a stateful service; nil otherwise
	ring []upstreamRingPoint
	// dials a block; tests replace it before use
	dialContext func(ctx context.Context, network string, address string) (net.Conn, error)
	// a channel that fires once a duration has passed, for the group's
	// bounded waits: time.After; tests replace it before use to order their
	// barriers
	waitAfter   func(d time.Duration) <-chan time.Time
	copyBuffers sync.Pool
	ctx         context.Context
	cancel      context.CancelFunc

	stateLock sync.Mutex
	closed    bool
	tunnels   map[*forwardTunnel]bool
}

// A group is the forwarder routing hands a service's requests to.
var _ serviceForwarder = (*upstreamGroup)(nil)

// One block of the service.
type upstreamBlock struct {
	// the block's position in its group's blocks
	index  int
	name   string
	weight int
	// the dial target, `<upstream ipv4>:<port>`
	target string
	// nginx's server string, `<upstream host>:<port>`, which keys the ring
	hashServer string
	pool       *upstreamPool

	// guarded by the group's stateLock: balanced requests in flight to the
	// block, nginx's peer->conns, and its smooth round robin weight
	activeRequests int
	currentWeight  int
}

// Builds the group for one service of a parsed table. Upstream targets are
// `<upstream.ipv4>:<block port>`. bodyStore holds buffered bodies for
// retries, and is shared by every service of the front. The group's sweep of
// idle connections runs until close.
func newUpstreamGroup(routeTable *RouteTable, service *RouteService, bodyStore *bodyStore, settings *FrontSettings) *upstreamGroup {
	ctx, cancel := context.WithCancel(context.Background())
	dialer := &net.Dialer{
		Timeout: max(settings.Forward.ProxyConnectTimeout, 0),
		// nginx's `proxy_socket_keepalive on`: SO_KEEPALIVE with the
		// system's probe timing
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     -1,
			Interval: -1,
			Count:    -1,
		},
	}
	group := &upstreamGroup{
		service:         service,
		settings:        settings,
		bodyStore:       bodyStore,
		blocks:          []*upstreamBlock{},
		blockNameBlocks: map[string]*upstreamBlock{},
		dialContext:     dialer.DialContext,
		waitAfter:       time.After,
		ctx:             ctx,
		cancel:          cancel,
		tunnels:         map[*forwardTunnel]bool{},
	}
	group.copyBuffers.New = func() any {
		buffer := make([]byte, max(settings.Forward.CopyBufferBytes, 1))
		return &buffer
	}

	routeBlocks := slices.Clone(service.Blocks)
	slices.SortStableFunc(routeBlocks, func(a *RouteBlock, b *RouteBlock) int {
		return strings.Compare(a.Name, b.Name)
	})
	for i, routeBlock := range routeBlocks {
		port := strconv.Itoa(routeBlock.Port)
		block := &upstreamBlock{
			index:      i,
			name:       routeBlock.Name,
			weight:     routeBlock.Weight,
			target:     net.JoinHostPort(routeTable.Upstream.Ipv4, port),
			hashServer: routeTable.Upstream.Host + ":" + port,
		}
		block.pool = newUpstreamPool(group, block)
		group.blocks = append(group.blocks, block)
		group.blockNameBlocks[block.name] = block
		if 0 < block.weight {
			group.balancedBlockCount += 1
		}
	}
	if service.Stateful {
		group.ring = newUpstreamRing(group.blocks)
	}
	go group.run()
	return group
}

// Sweeps idle connections past their keepalive bounds every
// ForwardSettings.KeepaliveSweepInterval, until close.
func (self *upstreamGroup) run() {
	sweepInterval := self.settings.Forward.KeepaliveSweepInterval
	if sweepInterval <= 0 {
		return
	}
	for {
		select {
		case <-self.ctx.Done():
			return
		case <-time.After(sweepInterval):
		}
		now := time.Now()
		for _, block := range self.blocks {
			block.pool.sweep(now)
		}
	}
}

// See serviceForwarder.
func (self *upstreamGroup) serveHttp(w http.ResponseWriter, r *http.Request, options forwardOptions) {
	self.resolveOptions(&options)
	ctx := r.Context()
	responseController := http.NewResponseController(w)
	setClientReadDeadline := func(deadline time.Time) error {
		return responseController.SetReadDeadline(deadline)
	}

	upgrade := forwardUpgradeRequested(r, options.websocket)
	request := &upstreamRequest{
		method:       r.Method,
		upgrade:      upgrade,
		readTimeout:  options.readTimeout,
		writeTimeout: self.sendTimeout(&options),
	}
	if forwardRequestHasBody(r) {
		maxBodyBytes := self.settings.Body.MaxBodyBytes
		if 0 < maxBodyBytes && maxBodyBytes < r.ContentLength {
			self.refuse(w, r, responseController, &bodyRefusal{status: http.StatusRequestEntityTooLarge, reason: bodyRefusalTooLarge})
			return
		}
		switch {
		case options.streamed && r.ContentLength != 0:
			// a body its first read returns whole is kept for a retry, as
			// nginx buffers a body its first pass reads whole: an h1 body of
			// declared length, not one that waits for 100 Continue, which
			// nginx's first pass reads before the client sends it
			// (body_stream.go)
			keepBodyStore := func() *bodyStore {
				if r.ProtoMajor != 1 || r.ContentLength <= 0 {
					return nil
				}
				for _, expect := range r.Header.Values("Expect") {
					for _, token := range strings.Split(expect, ",") {
						if strings.EqualFold(strings.TrimSpace(token), "100-continue") {
							return nil
						}
					}
				}
				return self.bodyStore
			}()
			// the body keeps arriving while the response flows
			responseController.EnableFullDuplex()
			streamedBody := newForwardStreamedBody(ctx, r.Body, r.ContentLength, maxBodyBytes, options.clientBodyTimeout, setClientReadDeadline, keepBodyStore)
			defer streamedBody.release()
			request.streamedBody = streamedBody
			if 0 <= r.ContentLength {
				request.bodyFraming = forwardBodyFraming{known: true, length: r.ContentLength}
			} else {
				request.bodyFraming = forwardBodyFraming{chunked: true}
			}
		case options.streamed:
			// a declared empty body is whole before any read, so it goes with
			// Content-Length: 0 and is retried, as nginx buffers it on h1
			// (rb->rest == 0) and on h2 when the HEADERS frame ends the
			// stream (stream->in_closed), as clients send an empty POST. Its
			// end isn't waited for: on h2, nginx streams one whose stream
			// ends later, so the front retries what nginx wouldn't there.
			request.bodyFraming = forwardBodyFraming{known: true, length: 0}
		default:
			storedBody, refusal := self.bodyStore.read(ctx, r.Body, bodyReadOptions{
				declaredLength:  r.ContentLength,
				prefixBytes:     options.clientBodyBufferSize,
				readTimeout:     options.clientBodyTimeout,
				setReadDeadline: setClientReadDeadline,
				// the vendored http3 gives every request without a
				// content-length a body of unknown length, an h3 GET's
				// included. One whose stream ends with no data has none to
				// frame, as nginx's h3 frames none for a stream it finds
				// ended with its head (ngx_http_v3_process_request_header),
				// so it goes like an h1 or h2 GET: unframed, with nothing
				// reserved for it, so a full body budget doesn't refuse it
				absentWhenEmpty: r.ProtoMajor == 3,
			})
			if refusal != nil {
				self.refuse(w, r, responseController, refusal)
				return
			}
			if storedBody != nil {
				defer storedBody.release()
				request.storedBody = storedBody
				request.bodyFraming = forwardBodyFraming{known: true, length: storedBody.length()}
			}
		}
	}
	request.head = forwardRequestHead(r, &options, upgrade, request.bodyFraming)
	self.forward(w, r, responseController, request, &options)
}

// Fills the options routing left zero with the defaults it would have set.
func (self *upstreamGroup) resolveOptions(options *forwardOptions) {
	if options.clientBodyTimeout == 0 {
		options.clientBodyTimeout = self.settings.Body.ClientBodyTimeout
	}
	if options.clientBodyBufferSize == 0 {
		options.clientBodyBufferSize = self.settings.Body.MemoryPrefixBytes
	}
	if options.readTimeout == 0 {
		if options.websocket || options.streamed {
			options.readTimeout = self.settings.Forward.StreamTimeout
		} else {
			options.readTimeout = self.settings.Forward.ProxyReadTimeout
		}
	}
}

// The upstream send timeout: ForwardSettings.StreamTimeout on websocket and
// streamed routes, as nginx's proxy_send_timeout is there, else
// ForwardSettings.ProxySendTimeout.
func (self *upstreamGroup) sendTimeout(options *forwardOptions) time.Duration {
	if options.websocket || options.streamed {
		return self.settings.Forward.StreamTimeout
	}
	return self.settings.Forward.ProxySendTimeout
}

// Attempts that may reach a block for this request: MaxUpstreamAttempts, at
// least 1, at most the balanced blocks (nginx's tries = min(peers,
// proxy_next_upstream_tries)); 1 for a per-block route.
func (self *upstreamGroup) attemptLimit(options *forwardOptions) int {
	if options.blockName != "" {
		return 1
	}
	return max(min(self.settings.Forward.MaxUpstreamAttempts, self.balancedBlockCount), 1)
}

// One request's attempts under D7, kept by its forward goroutine.
type forwardAttempts struct {
	ctx     context.Context
	request *upstreamRequest
	options *forwardOptions
	// attempts that may reach a block (attemptLimit)
	attemptLimit int
	// attempts made, the current one included
	attemptCount int
}

// Whether the request may move to another block after a failed attempt:
// attempts are left, the route names no block, the client is still there,
// and a streamed body never began going upstream, or was kept whole by its
// first read.
func (self *forwardAttempts) retryAllowed() bool {
	if self.options.blockName != "" || self.attemptLimit <= self.attemptCount || self.ctx.Err() != nil {
		return false
	}
	streamedBody := self.request.streamedBody
	return streamedBody == nil || !streamedBody.began() || streamedBody.kept() != nil
}

// Whether the request may move to another block after exchange's 503. The
// exchange is live, and its write may be about to begin a streamed body, so
// the body is first held back from it under its gate's lock: a body held
// back is whole for the retry, and one that began goes on, its 503 the
// final answer, unless its first read kept it whole. A first read still
// under way when the 503 comes, which nginx's first pass would have found
// empty, keeps nothing in time.
func (self *forwardAttempts) retry503Allowed(exchange *upstreamExchange) bool {
	if !self.retryAllowed() {
		return false
	}
	streamedBody := self.request.streamedBody
	return streamedBody == nil || !exchange.holdBody() || streamedBody.kept() != nil
}

// Moves the request on to its next attempt, once a retry is allowed: a
// streamed body that began, which the retry rules allow only once it is kept
// whole, goes as a buffered body from here on, its head unchanged, and one
// held back streams again from its start.
func (self *forwardAttempts) next() {
	streamedBody := self.request.streamedBody
	if streamedBody == nil || !streamedBody.began() {
		return
	}
	self.request.storedBody = streamedBody.kept()
	self.request.streamedBody = nil
}

// Runs the attempts and relays the final response, or answers the front's
// own error.
func (self *upstreamGroup) forward(w http.ResponseWriter, r *http.Request, responseController *http.ResponseController, request *upstreamRequest, options *forwardOptions) {
	ctx := r.Context()
	triedBlocks := make([]bool, len(self.blocks))
	var ringWalk *upstreamRingWalk
	if self.ring != nil && options.blockName == "" {
		ringWalk = newUpstreamRingWalk(nginxRemoteAddrText(options.clientAddrPort.Addr()))
	}
	attempts := &forwardAttempts{
		ctx:          ctx,
		request:      request,
		options:      options,
		attemptLimit: self.attemptLimit(options),
	}
	for {
		block := self.selectBlock(options.blockName, triedBlocks, ringWalk)
		if block == nil {
			// nginx's "no live upstreams"
			self.respondError(w, http.StatusBadGateway)
			return
		}
		triedBlocks[block.index] = true
		attempts.attemptCount += 1
		exchange, failure := self.attempt(ctx, block, request)
		if failure == upstreamFailureNone {
			if exchange.response.StatusCode == http.StatusServiceUnavailable && attempts.retry503Allowed(exchange) {
				exchange.finish(false)
				self.releaseBlock(block, options)
				upstreamRetriesCounter.WithLabelValues(self.service.Name, upstreamAttemptHttp503).Inc()
				attempts.next()
				continue
			}
			self.relay(w, responseController, block, exchange, options)
			return
		}
		self.releaseBlock(block, options)
		switch failure {
		case upstreamFailureClient:
			if request.streamedBody != nil {
				if refusal := request.streamedBody.failure(); refusal != nil {
					self.refuse(w, r, responseController, refusal)
					return
				}
			}
			// the request's context ended: its client went away, nginx's
			// 499, which ngx_http_upstream_finalize_request terminates with
			// nothing sent, or connect's forced close cut the server's
			// connections. Either way no response goes out, and the request
			// is recorded with none, not as one of the front's own 502s.
			panic(http.ErrAbortHandler)
		case upstreamFailureError, upstreamFailureTimeout:
			if attempts.retryAllowed() {
				upstreamRetriesCounter.WithLabelValues(self.service.Name, failure.String()).Inc()
				attempts.next()
				continue
			}
		}
		if failure == upstreamFailureTimeout {
			self.respondError(w, http.StatusGatewayTimeout)
		} else {
			self.respondError(w, http.StatusBadGateway)
		}
		return
	}
}

// Picks the request's next block, counting it as in flight when balanced. A
// per-block route takes its block, whatever its weight or load. Nil when
// none is eligible or the group is closed.
func (self *upstreamGroup) selectBlock(blockName string, triedBlocks []bool, ringWalk *upstreamRingWalk) *upstreamBlock {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.closed {
		return nil
	}
	var block *upstreamBlock
	switch {
	case blockName != "":
		// nginx's per-block upstream is its own, so its requests never
		// weigh on the service's balancing
		return self.blockNameBlocks[blockName]
	case ringWalk != nil:
		block = self.selectRingWithLock(ringWalk, triedBlocks)
	default:
		block = self.selectLeastConnWithLock(triedBlocks)
	}
	if block != nil {
		block.activeRequests += 1
	}
	return block
}

// Ends a balanced request's hold on its block, nginx's peer free.
func (self *upstreamGroup) releaseBlock(block *upstreamBlock, options *forwardOptions) {
	if options.blockName != "" {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	block.activeRequests -= 1
}

// One of D7's attempts on a block: a pooled connection if a live one is
// idle, else a new one, redone once on a fresh connection when a reused one
// failed before any request byte was written. Each physical attempt is
// counted.
func (self *upstreamGroup) attempt(ctx context.Context, block *upstreamBlock, request *upstreamRequest) (*upstreamExchange, upstreamFailure) {
	countAttempt := func(result string) {
		upstreamAttemptsCounter.WithLabelValues(self.service.Name, block.name, result).Inc()
	}
	upstreamConn := block.pool.takeIdle()
	reused := upstreamConn != nil
	for {
		if upstreamConn == nil {
			var err error
			upstreamConn, err = block.pool.dial(ctx)
			if err != nil {
				failure := upstreamIoFailure(err)
				if ctx.Err() != nil {
					failure = upstreamFailureClient
				}
				countAttempt(failure.String())
				return nil, failure
			}
		}
		exchange, failure, nothingWritten := self.exchange(ctx, upstreamConn, request)
		if failure == upstreamFailureNone {
			if exchange.response.StatusCode == http.StatusServiceUnavailable {
				countAttempt(upstreamAttemptHttp503)
			} else {
				countAttempt(upstreamAttemptResponse)
			}
			return exchange, upstreamFailureNone
		}
		if reused && nothingWritten && failure == upstreamFailureError {
			// a stale keepalive connection: the block never saw the request
			countAttempt(upstreamAttemptStaleRedo)
			reused = false
			upstreamConn = nil
			continue
		}
		countAttempt(failure.String())
		return nil, failure
	}
}

// Relays the final response: its head at once, then its body as it arrives,
// each read written and flushed, as nginx's `proxy_buffering off` sends it. A
// 101 becomes a tunnel. A body the block cuts short, or the client stops
// taking, aborts the response. So does a streamed request body that fails on
// the client's side while the response is relayed, a stall past
// client_body_timeout included: the exchange cuts the block's side then, as
// nginx ends the request (ngx_http_upstream_finalize_request terminates one
// whose body timed out, and ends one with a broken body without its last
// bytes), and the refusal is counted, as one before the response head is.
func (self *upstreamGroup) relay(w http.ResponseWriter, responseController *http.ResponseController, block *upstreamBlock, exchange *upstreamExchange, options *forwardOptions) {
	// the block that answered labels the request's metrics and access log
	// line (D17)
	if recorder, ok := w.(upstreamBlockRecorder); ok {
		recorder.recordUpstreamBlock(block.name)
	}
	response := exchange.response
	if response.StatusCode == http.StatusSwitchingProtocols {
		self.tunnel(w, responseController, block, exchange, options)
		return
	}
	completed := false
	defer func() {
		exchange.finish(completed && !response.Close)
		self.releaseBlock(block, options)
	}()

	forwardResponseHeaders(w.Header(), response)
	w.WriteHeader(response.StatusCode)
	if response.Body == http.NoBody || response.ContentLength == 0 {
		completed = true
		return
	}
	if exchange.conn.reader.Buffered() == 0 {
		// no body yet: send the head alone now, as nginx does, so a client
		// of a slow or streaming response isn't left waiting for it
		responseController.Flush()
	}
	clientErr, upstreamErr := self.copyToClient(w, responseController, response.Body, func() {
		exchange.stopCancelHook()
	})
	if clientErr != nil || upstreamErr != nil {
		// the request's write ends first, so a streamed body's failure is in
		// before it is counted: on h1, Go's server cancels the request inside
		// the client's timed out read, and the cut that cancellation makes
		// can fail this copy before that read returns to the write
		exchange.finish(false)
		if streamedBody := exchange.request.streamedBody; streamedBody != nil {
			if refusal := streamedBody.failure(); refusal != nil {
				forwardBodyRefusalsCounter.WithLabelValues(self.service.Name, refusal.reason).Inc()
			}
		}
		// a truncated response must not look complete to the client
		panic(http.ErrAbortHandler)
	}
	completed = true
}

// Copies a response body to the client, writing and flushing each read, with
// HttpSettings.SendTimeout bounding each write, nginx's send_timeout. Once
// the body's end is read, bodyEnded runs before the last bytes are written,
// so a client that closes as soon as it has them finds the block's side
// done. The errors say which side failed.
func (self *upstreamGroup) copyToClient(w http.ResponseWriter, responseController *http.ResponseController, body io.Reader, bodyEnded func()) (clientErr error, upstreamErr error) {
	sendTimeout := self.settings.Http.SendTimeout
	if 0 < sendTimeout {
		defer responseController.SetWriteDeadline(time.Time{})
	}
	copyBuffer := self.copyBuffers.Get().(*[]byte)
	defer self.copyBuffers.Put(copyBuffer)
	for {
		n, readErr := body.Read(*copyBuffer)
		if readErr == io.EOF && bodyEnded != nil {
			bodyEnded()
		}
		if 0 < n {
			if 0 < sendTimeout {
				responseController.SetWriteDeadline(time.Now().Add(sendTimeout))
			}
			if _, err := w.Write((*copyBuffer)[:n]); err != nil {
				return err, nil
			}
			if err := responseController.Flush(); err != nil && !errors.Is(err, http.ErrNotSupported) {
				return err, nil
			}
		}
		if readErr == io.EOF {
			return nil, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

// Answers a body refusal and counts it.
//
// A client that went away gets no response, and the request is recorded with
// none: the handler aborts with http.ErrAbortHandler, as nginx terminates a
// request whose client connection failed (c->error, which
// ngx_http_finalize_request answers with ngx_http_terminate_request), so the
// front's default 200 for a handler that wrote nothing never goes out. On h1
// a client that ends its body early is one: Go's server reports its closed
// side by canceling the request.
//
// Nor does a body that stalled past client_body_timeout, on h1 and h2: nginx
// finalizes that request with NGX_HTTP_REQUEST_TIME_OUT and c->timedout set,
// before it reaches a block (ngx_http_read_client_request_body_handler) or
// while it streams (ngx_http_upstream_read_request_handler), and
// ngx_http_finalize_request terminates such a request with no response. So
// the handler aborts with http.ErrAbortHandler: Go's h1 server closes the
// connection, as nginx does, and its h2 server resets the stream, which
// nginx resets too. Unlike nginx, Go's h1 server sends TLS's close_notify
// first, where nginx's quiet shutdown after a timeout sends none, and its h2
// reset is INTERNAL_ERROR, where nginx's is PROTOCOL_ERROR, since Go's
// server takes no reset code from a handler. h3, which nginx doesn't serve
// here, still gets the 408. A timed out read's deadline has passed and
// stays, so nothing waits on the client after it.
//
// Any other refused body may be partly unread. Go's server reads up to 256
// KiB of what remains before it writes the answer, so that read is bounded
// first by HttpSettings.LingeringTimeout, nginx's lingering_timeout, or cut
// at once when that is non-positive.
func (self *upstreamGroup) refuse(w http.ResponseWriter, r *http.Request, responseController *http.ResponseController, refusal *bodyRefusal) {
	forwardBodyRefusalsCounter.WithLabelValues(self.service.Name, refusal.reason).Inc()
	if refusal.status == 0 {
		panic(http.ErrAbortHandler)
	}
	if refusal.reason == bodyRefusalTimeout {
		if r.ProtoMajor != 3 {
			panic(http.ErrAbortHandler)
		}
	} else {
		lingeringDeadline := time.Unix(1, 0)
		if lingeringTimeout := self.settings.Http.LingeringTimeout; 0 < lingeringTimeout {
			lingeringDeadline = time.Now().Add(lingeringTimeout)
		}
		responseController.SetReadDeadline(lingeringDeadline)
	}
	writeForwardError(w, refusal.status)
}

// Answers the front's own 502 or 504 and counts it.
func (self *upstreamGroup) respondError(w http.ResponseWriter, status int) {
	forwardErrorsCounter.WithLabelValues(self.service.Name, strconv.Itoa(status)).Inc()
	writeForwardError(w, status)
}

// See serviceForwarder. Idempotent.
func (self *upstreamGroup) close() {
	tunnels := func() []*forwardTunnel {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return nil
		}
		self.closed = true
		tunnels := make([]*forwardTunnel, 0, len(self.tunnels))
		for tunnel := range self.tunnels {
			tunnels = append(tunnels, tunnel)
		}
		return tunnels
	}()
	self.cancel()
	for _, block := range self.blocks {
		block.pool.close()
	}
	for _, tunnel := range tunnels {
		tunnel.cut()
	}
}
