package nginx_test

// The parity harness's recording upstreams (parity_test.go). One http.Server
// listens on the upstream address at each block's port, and the front's
// in-process connect handler is the same recorder under the block name
// "local". Each records every attempt it receives under the case key the
// client sent, "<target>/<case>", and answers with the case's scenario, so
// nginx's upstreams and the front's see the same scenario and the harness can
// compare what each received. Clients and upstreams share the process, so a
// case orders them with barriers rather than sleeps.

import (
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The headers every corpus request carries: its case key, and its sequence
// within the case. Both pass through nginx and the front untouched.
const (
	parityCaseHeader     = "X-Parity-Case"
	paritySequenceHeader = "X-Parity-Sequence"
)

// The block name the front's in-process connect handler records under.
const parityInProcessBlock = "local"

// One request an upstream received, and what it answered.
type parityAttempt struct {
	service string
	block   string
	// the request's X-Parity-Sequence
	sequence         int
	method           string
	requestUri       string
	proto            string
	host             string
	header           http.Header
	transferEncoding []string
	contentLength    int64
	body             []byte
	bodyError        string
	// the status the scenario answered; 0 when it answered none
	status int
}

// What an upstream does with one attempt.
type parityScenario func(exchange *parityUpstreamExchange)

// One attempt at an upstream, as its scenario sees it.
type parityUpstreamExchange struct {
	recorder *parityRecorder
	caseKey  string
	attempt  *parityAttempt
	// the case key's attempts so far, this one included
	attemptNumber int
	w             http.ResponseWriter
	r             *http.Request
}

// Reads the request body into the attempt.
func (self *parityUpstreamExchange) readBody() {
	body, err := io.ReadAll(io.LimitReader(self.r.Body, self.recorder.settings.UpstreamMaxBodyBytes))
	func() {
		self.recorder.stateLock.Lock()
		defer self.recorder.stateLock.Unlock()
		self.attempt.body = body
		if err != nil {
			self.attempt.bodyError = err.Error()
		}
	}()
}

// Records the status the scenario answers.
func (self *parityUpstreamExchange) answered(status int) {
	self.recorder.stateLock.Lock()
	defer self.recorder.stateLock.Unlock()
	self.attempt.status = status
}

// Reads the body, then answers with status, the header lines in order, and
// body.
func (self *parityUpstreamExchange) respond(status int, headerLines [][2]string, body string) {
	self.readBody()
	for _, headerLine := range headerLines {
		self.w.Header().Add(headerLine[0], headerLine[1])
	}
	self.answered(status)
	self.w.WriteHeader(status)
	io.WriteString(self.w, body)
}

// The barrier name of this attempt's case.
func (self *parityUpstreamExchange) barrier(name string) string {
	return self.caseKey + "/" + name
}

// The upstream's own body for an ordinary answer: it names the service only,
// so the block a balancer chose never shows in a compared body.
func parityUpstreamBody(service string) string {
	return "upstream " + service + "\n"
}

// Answers 200 with a short plain text body.
func parityDefaultScenario(exchange *parityUpstreamExchange) {
	exchange.respond(
		http.StatusOK,
		[][2]string{{"Content-Type", "text/plain; charset=utf-8"}},
		parityUpstreamBody(exchange.attempt.service),
	)
}

// Answers status with the header lines and body.
func parityRespondScenario(status int, headerLines [][2]string, body string) parityScenario {
	return func(exchange *parityUpstreamExchange) {
		exchange.respond(status, headerLines, body)
	}
}

// Answers each attempt with its status in order, the last repeating.
func parityStatusesScenario(statuses ...int) parityScenario {
	return func(exchange *parityUpstreamExchange) {
		status := statuses[min(exchange.attemptNumber, len(statuses))-1]
		exchange.respond(
			status,
			[][2]string{{"Content-Type", "text/plain; charset=utf-8"}},
			"upstream "+exchange.attempt.service+" "+strconv.Itoa(status)+"\n",
		)
	}
}

// Answers 200 with the request body as the response body.
func parityEchoScenario(exchange *parityUpstreamExchange) {
	exchange.readBody()
	body := func() []byte {
		exchange.recorder.stateLock.Lock()
		defer exchange.recorder.stateLock.Unlock()
		return slices.Clone(exchange.attempt.body)
	}()
	exchange.w.Header().Set("Content-Type", "application/octet-stream")
	exchange.answered(http.StatusOK)
	exchange.w.WriteHeader(http.StatusOK)
	exchange.w.Write(body)
}

// Writes a first line and flushes, waits at the client's "got-first"
// barrier, and ends with "second" when the client got the first line while
// the response was still open, or "late" when it didn't.
func parityStreamResponseScenario(exchange *parityUpstreamExchange) {
	exchange.readBody()
	exchange.w.Header().Set("Content-Type", "text/event-stream")
	exchange.answered(http.StatusOK)
	exchange.w.WriteHeader(http.StatusOK)
	io.WriteString(exchange.w, "first\n")
	http.NewResponseController(exchange.w).Flush()
	if exchange.recorder.wait(exchange.barrier("got-first"), exchange.recorder.settings.BarrierTimeout) {
		io.WriteString(exchange.w, "second\n")
	} else {
		io.WriteString(exchange.w, "late\n")
	}
}

// Reads the first parityStreamFirstBytes of the body, fires
// "upstream-got-first", reads the rest and echoes the whole body.
func parityStreamRequestScenario(exchange *parityUpstreamExchange) {
	first := make([]byte, parityStreamFirstBytes)
	_, err := io.ReadFull(exchange.r.Body, first)
	if err == nil {
		exchange.recorder.fire(exchange.barrier("upstream-got-first"))
	}
	rest, restErr := io.ReadAll(io.LimitReader(exchange.r.Body, exchange.recorder.settings.UpstreamMaxBodyBytes))
	body := append(first, rest...)
	func() {
		exchange.recorder.stateLock.Lock()
		defer exchange.recorder.stateLock.Unlock()
		exchange.attempt.body = body
		if err != nil {
			exchange.attempt.bodyError = err.Error()
		} else if restErr != nil {
			exchange.attempt.bodyError = restErr.Error()
		}
	}()
	exchange.w.Header().Set("Content-Type", "application/octet-stream")
	exchange.answered(http.StatusOK)
	exchange.w.WriteHeader(http.StatusOK)
	exchange.w.Write(body)
}

// The scratch buffer of a websocket upstream's echo, in bytes.
const parityEchoBufferBytes = 1024

// The bytes the request streaming cases send before they wait.
const parityStreamFirstBytes = 10

// Reads a body as parityStreamRequestScenario does, its first bytes before
// the client sends the rest, then answers each attempt with its status in
// order, the last repeating.
func parityStreamRequestStatusesScenario(statuses ...int) parityScenario {
	return func(exchange *parityUpstreamExchange) {
		first := make([]byte, parityStreamFirstBytes)
		_, err := io.ReadFull(exchange.r.Body, first)
		if err == nil {
			exchange.recorder.fire(exchange.barrier("upstream-got-first"))
		}
		rest, restErr := io.ReadAll(io.LimitReader(exchange.r.Body, exchange.recorder.settings.UpstreamMaxBodyBytes))
		func() {
			exchange.recorder.stateLock.Lock()
			defer exchange.recorder.stateLock.Unlock()
			exchange.attempt.body = append(first, rest...)
			if err != nil {
				exchange.attempt.bodyError = err.Error()
			} else if restErr != nil {
				exchange.attempt.bodyError = restErr.Error()
			}
		}()
		status := statuses[min(exchange.attemptNumber, len(statuses))-1]
		exchange.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		exchange.answered(status)
		exchange.w.WriteHeader(status)
		io.WriteString(exchange.w, "upstream "+exchange.attempt.service+" "+strconv.Itoa(status)+"\n")
	}
}

// Holds the attempt until the client releases it: it fires
// "arrived/<sequence>" and waits for "release/<sequence>" or "release", then
// answers as the default does. A hold that is never released answers once
// the barrier times out.
func parityHoldScenario(exchange *parityUpstreamExchange) {
	sequence := strconv.Itoa(exchange.attempt.sequence)
	exchange.recorder.fire(exchange.barrier("arrived/" + sequence))
	exchange.recorder.waitAny(
		exchange.recorder.settings.ExchangeTimeout,
		exchange.barrier("release/"+sequence),
		exchange.barrier("release"),
	)
	parityDefaultScenario(exchange)
}

// Answers with a response head larger than nginx's 4 KiB proxy buffer.
func parityLargeResponseHeadScenario(exchange *parityUpstreamExchange) {
	exchange.respond(
		http.StatusOK,
		[][2]string{
			{"Content-Type", "text/plain; charset=utf-8"},
			{"X-Parity-Large", strings.Repeat("a", 5*1024)},
		},
		parityUpstreamBody(exchange.attempt.service),
	)
}

// Sends a head promising 100 body bytes, 10 of them, and cuts the
// connection.
func parityPartialScenario(exchange *parityUpstreamExchange) {
	exchange.readBody()
	exchange.w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	exchange.w.Header().Set("Content-Length", "100")
	exchange.answered(http.StatusOK)
	exchange.w.WriteHeader(http.StatusOK)
	io.WriteString(exchange.w, "0123456789")
	http.NewResponseController(exchange.w).Flush()
	panic(http.ErrAbortHandler)
}

// Switches protocols when the request asks to, as a websocket upstream does,
// and echoes what follows; otherwise answers as the default does.
func parityWebsocketScenario(exchange *parityUpstreamExchange) {
	if !strings.EqualFold(exchange.r.Header.Get("Upgrade"), "websocket") {
		parityDefaultScenario(exchange)
		return
	}
	conn, readWriter, err := http.NewResponseController(exchange.w).Hijack()
	if err != nil {
		exchange.respond(http.StatusInternalServerError, nil, "no hijack: "+err.Error()+"\n")
		return
	}
	defer conn.Close()
	exchange.answered(http.StatusSwitchingProtocols)
	conn.SetDeadline(time.Now().Add(exchange.recorder.settings.ExchangeTimeout))
	readWriter.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nX-Parity-Upstream: " + exchange.attempt.service + "\r\n\r\n")
	readWriter.Flush()
	echoBytes := make([]byte, parityEchoBufferBytes)
	for {
		n, err := readWriter.Read(echoBytes)
		if 0 < n {
			readWriter.Write(echoBytes[:n])
			readWriter.Flush()
		}
		if err != nil {
			return
		}
	}
}

// Records attempts by case key, and the barriers clients and upstreams meet
// at. Safe for concurrent use.
type parityRecorder struct {
	settings *parityHarnessSettings

	stateLock           sync.Mutex
	caseKeyAttempts     map[string][]*parityAttempt
	caseNameScenarios   map[string]parityScenario
	barrierNameChannels map[string]chan struct{}
	firedBarrierNames   map[string]bool
}

// An empty recorder.
func newParityRecorder(settings *parityHarnessSettings) *parityRecorder {
	return &parityRecorder{
		settings:            settings,
		caseKeyAttempts:     map[string][]*parityAttempt{},
		caseNameScenarios:   map[string]parityScenario{},
		barrierNameChannels: map[string]chan struct{}{},
		firedBarrierNames:   map[string]bool{},
	}
}

// Sets the scenario of a case, by name, for both targets.
func (self *parityRecorder) setScenario(caseName string, scenario parityScenario) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if scenario == nil {
		delete(self.caseNameScenarios, caseName)
		return
	}
	self.caseNameScenarios[caseName] = scenario
}

// The handler of one block of a service.
func (self *parityRecorder) handler(service string, block string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		caseKey := r.Header.Get(parityCaseHeader)
		sequence, _ := strconv.Atoi(r.Header.Get(paritySequenceHeader))
		attempt := &parityAttempt{
			service:          service,
			block:            block,
			sequence:         sequence,
			method:           r.Method,
			requestUri:       r.RequestURI,
			proto:            r.Proto,
			host:             r.Host,
			header:           r.Header.Clone(),
			transferEncoding: slices.Clone(r.TransferEncoding),
			contentLength:    r.ContentLength,
		}
		caseName := caseKey
		if _, name, ok := strings.Cut(caseKey, "/"); ok {
			caseName = name
		}
		scenario, attemptNumber := func() (parityScenario, int) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.caseKeyAttempts[caseKey] = append(self.caseKeyAttempts[caseKey], attempt)
			return self.caseNameScenarios[caseName], len(self.caseKeyAttempts[caseKey])
		}()
		if scenario == nil {
			scenario = parityDefaultScenario
		}
		scenario(&parityUpstreamExchange{
			recorder:      self,
			caseKey:       caseKey,
			attempt:       attempt,
			attemptNumber: attemptNumber,
			w:             w,
			r:             r,
		})
	})
}

// A copy of a case key's attempts, in the order they arrived.
func (self *parityRecorder) attempts(caseKey string) []parityAttempt {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	attempts := []parityAttempt{}
	for _, attempt := range self.caseKeyAttempts[caseKey] {
		attemptCopy := *attempt
		attemptCopy.header = attempt.header.Clone()
		attemptCopy.body = slices.Clone(attempt.body)
		attempts = append(attempts, attemptCopy)
	}
	return attempts
}

// The channel of a barrier, made on first use.
func (self *parityRecorder) barrierChannelWithLock(name string) chan struct{} {
	channel, ok := self.barrierNameChannels[name]
	if !ok {
		channel = make(chan struct{})
		self.barrierNameChannels[name] = channel
	}
	return channel
}

// Fires a barrier, once.
func (self *parityRecorder) fire(name string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	channel := self.barrierChannelWithLock(name)
	if !self.firedBarrierNames[name] {
		self.firedBarrierNames[name] = true
		close(channel)
	}
}

// Waits up to timeout for a barrier to fire, and reports whether it did.
func (self *parityRecorder) wait(name string, timeout time.Duration) bool {
	return self.waitAny(timeout, name)
}

// Waits up to timeout for any of the barriers to fire, and reports whether
// one did.
func (self *parityRecorder) waitAny(timeout time.Duration, names ...string) bool {
	channels := func() []chan struct{} {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		channels := []chan struct{}{}
		for _, name := range names {
			channels = append(channels, self.barrierChannelWithLock(name))
		}
		return channels
	}()
	deadline := time.After(timeout)
	fired := make(chan struct{})
	stop := make(chan struct{})
	defer close(stop)
	for _, channel := range channels {
		go func() {
			select {
			case <-channel:
				select {
				case fired <- struct{}{}:
				case <-stop:
				}
			case <-stop:
			}
		}()
	}
	select {
	case <-fired:
		return true
	case <-deadline:
		return false
	}
}
