//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// What the dns and certificates case (dns_certs_concurrency_test.go) records of the certs worker's and the desktops'
// requests, each on a transport in front of the client's own, as a proxy in front of it would:
//
//	challenges  every POST /dns/internal/acme-challenge the certs worker's DNS client sends, certs' own DNS-01 and the
//	            delegated command's alike (one client, server.CertsDnsClient), with how it was answered: what the
//	            ledgers reconcile each challenge name's journal rows and its dns_challenges_per_name_hour with
//	holds       a hold a phase arms on the next GET /dns/internal/ready, or the next challenge write, the client sends:
//	            before the request goes out, its before step runs (the dns edge's restart, World.Restart, under a
//	            ready read; the dns API block's stop, World.Stop, under a write), and the request waits for it or for
//	            its own end; once the request is answered, its after step runs on a goroutine of its own (the API's
//	            start again, so the write meets the stopped block's 503 and certs' retries ride it until the block is
//	            back): chaos under an order's own request, deterministically; a gate on the writes holds every write
//	            that comes while its before step runs, not just the first (the certs worker's restart, which cuts
//	            each of them before it is sent)
//	nonces      every POST to Pebble, each a JWS (RFC 8555 §6.2), the anti-replay nonce its protected header carries,
//	            and Pebble's badNonce refusals (§22.9's 5%, which Pebble draws at random), each client's own: a nonce
//	            sent again after an answer to it came back is a replay, which a client must never send, since Pebble
//	            refuses it as a bad nonce and acmez's retry would hide it; a nonce sent again after a send of it got
//	            no answer at all is acmez's resend of the same request, which never reached the CA or whose answer was
//	            lost
//
// For a stalled phase's dump, each recorder also keeps the newest keepCount of what it saw that says why an order
// waits or fails: the ready reads' answers, and Pebble's problem documents.
//
// A challenge write's end is what its answer said: 200 with the serial it took, zero for a removal of nothing; 400 or
// 429, a refusal that wrote nothing; 503, the API's own or the lb's while the block is down, which wrote nothing; any
// other answer, the lb's 502 for an upstream that failed mid-request among them, or none at all, an unknown outcome.

package sidecare2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server/model"
)

// How one challenge write of the certs worker's client ended (see the file header).
type dnsCertsChallengeEnd int

const (
	// answered with its serial, zero for a removal of nothing
	dnsCertsChallengeAnswered dnsCertsChallengeEnd = iota
	// refused, 400 or 429: nothing written
	dnsCertsChallengeRefused
	// 503: nothing written
	dnsCertsChallengeUnavailable
	// any other answer, or none: the outcome is unknown
	dnsCertsChallengeUnknown
)

func (self dnsCertsChallengeEnd) String() string {
	switch self {
	case dnsCertsChallengeAnswered:
		return "answered"
	case dnsCertsChallengeRefused:
		return "refused"
	case dnsCertsChallengeUnavailable:
		return "unavailable"
	}
	return "unknown"
}

// One challenge write as the certs worker's client sent it and was answered.
type dnsCertsChallengeWrite struct {
	// The request's name, as sent, and its values, sorted; none for a removal.
	Name   string
	Values []string
	End    dnsCertsChallengeEnd
	// The answer's status, zero for none, and the serial a 200 named.
	Status int
	Serial uint64
}

// The TXT owner a challenge name writes, canonical: a custom domain's <acme_token>.acme.<root> is the owner itself
// (QD-175), and any other name's is _acme-challenge.<name>, a wildcard's at its base name (RFC 8555 §8.4).
func dnsCertsChallengeOwner(name string, root string) string {
	name = strings.TrimPrefix(dns.CanonicalName(name), "*.")
	if dnsCertsIsAcmeOwner(name, root) {
		return name
	}
	return dns.CanonicalName(model.DnsChallengeLabel + "." + name)
}

// Whether name, canonical, is a custom domain's challenge target in root, <acme_token>.acme.<root>.
func dnsCertsIsAcmeOwner(name string, root string) bool {
	acmeToken, ok := strings.CutSuffix(name, ".acme."+dns.CanonicalName(root))
	return ok && acmeToken != "" && !strings.Contains(acmeToken, ".")
}

// The recorder in front of the certs worker's DNS transport (see the file header). Safe for concurrent use.
type dnsCertsDnsRecorder struct {
	transport http.RoundTripper
	keepCount int

	stateLock  sync.Mutex
	writes     []*dnsCertsChallengeWrite
	readyCount int
	// the newest ready reads' answers, oldest first
	readies []string
	// the holds on the next ready read and the next challenge write, nil while none is armed
	readyHold *dnsCertsHold
	writeHold *dnsCertsHold
}

func newDnsCertsDnsRecorder(transport http.RoundTripper, keepCount int) *dnsCertsDnsRecorder {
	return &dnsCertsDnsRecorder{transport: transport, keepCount: keepCount}
}

// Keeps line in lines, dropping the oldest past keepCount.
func dnsCertsKeep(lines []string, line string, keepCount int) []string {
	lines = append(lines, line)
	if keepCount < len(lines) {
		lines = slices.Delete(lines, 0, len(lines)-keepCount)
	}
	return lines
}

// response's body, read whole and handed back whole, as a short line for a dump; and the read's error.
func dnsCertsPeekBody(response *http.Response, maxByteCount int) (string, error) {
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(data))
	text := strings.Join(strings.Fields(string(data)), " ")
	if maxByteCount < len(text) {
		text = text[:maxByteCount]
	}
	return text, err
}

func (self *dnsCertsDnsRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	switch {
	case request.Method == http.MethodPost && request.URL.Path == dnsservice.DnsAcmeChallengePath:
		return self.challenge(request)
	case request.Method == http.MethodGet && request.URL.Path == dnsservice.DnsReadyPath:
		serial := request.URL.Query().Get("serial")
		hold := self.takeHold(&self.readyHold, true)
		if hold != nil {
			hold.hold(request.Context(), serial)
			defer hold.answered()
		}
		started := time.Now()
		response, err := self.transport.RoundTrip(request)
		line := fmt.Sprintf("%s ready?serial=%s after %s: ", started.Format(time.StampMilli), serial, time.Since(started).Round(time.Millisecond))
		if err != nil {
			line += err.Error()
		} else {
			body, _ := dnsCertsPeekBody(response, 200)
			line += fmt.Sprintf("%d %s", response.StatusCode, body)
		}
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.readies = dnsCertsKeep(self.readies, line, self.keepCount)
		}()
		return response, err
	}
	return self.transport.RoundTrip(request)
}

// Takes the hold in slot, nil when none is armed, counting a ready read when ready: a gate stays in its slot, and
// each request takes it, until its before step has ended.
func (self *dnsCertsDnsRecorder) takeHold(slot **dnsCertsHold, ready bool) *dnsCertsHold {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if ready {
		self.readyCount += 1
	}
	hold := *slot
	if hold == nil {
		return nil
	}
	if hold.gate {
		select {
		case <-hold.beforeDone:
			*slot = nil
			return nil
		default:
			return hold
		}
	}
	*slot = nil
	return hold
}

// One challenge write, sent as it came, its answer read and handed back whole.
func (self *dnsCertsDnsRecorder) challenge(request *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(request.Body)
	request.Body.Close()
	if err != nil {
		return nil, err
	}
	write := &dnsCertsChallengeWrite{End: dnsCertsChallengeUnknown}
	defer self.keep(write)
	challenge := &dnsservice.DnsChallengeRequest{}
	if err := json.Unmarshal(body, challenge); err == nil {
		write.Name = challenge.Name
		write.Values = slices.Sorted(slices.Values(challenge.Values))
	}
	if hold := self.takeHold(&self.writeHold, false); hold != nil {
		hold.hold(request.Context(), write.Name)
		defer hold.answered()
	}
	sent := request.Clone(request.Context())
	sent.Body = io.NopCloser(bytes.NewReader(body))
	sent.ContentLength = int64(len(body))
	response, err := self.transport.RoundTrip(sent)
	if err != nil {
		return nil, err
	}
	write.Status = response.StatusCode
	switch response.StatusCode {
	case http.StatusOK:
		data, readErr := io.ReadAll(response.Body)
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(data))
		answer := &dnsservice.DnsChallengeAnswer{}
		if readErr == nil && json.Unmarshal(data, answer) == nil {
			write.End = dnsCertsChallengeAnswered
			write.Serial = answer.Serial
		}
	case http.StatusBadRequest, http.StatusTooManyRequests:
		write.End = dnsCertsChallengeRefused
	case http.StatusServiceUnavailable:
		write.End = dnsCertsChallengeUnavailable
	}
	return response, nil
}

func (self *dnsCertsDnsRecorder) keep(write *dnsCertsChallengeWrite) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writes = append(self.writes, write)
}

// Every challenge write so far, in the order their answers came.
func (self *dnsCertsDnsRecorder) Writes() []*dnsCertsChallengeWrite {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.writes)
}

// How many ready reads went out.
func (self *dnsCertsDnsRecorder) ReadyCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.readyCount
}

// For a stalled phase's dump: each challenge owner's writes by end, with its newest few, and the newest ready answers.
func (self *dnsCertsDnsRecorder) Describe(root string) string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	owners := map[string][]*dnsCertsChallengeWrite{}
	for _, write := range self.writes {
		owner := dnsCertsChallengeOwner(write.Name, root)
		owners[owner] = append(owners[owner], write)
	}
	lines := []string{fmt.Sprintf("certs' DNS client: %d challenge writes, %d ready reads", len(self.writes), self.readyCount)}
	for _, owner := range slices.Sorted(maps.Keys(owners)) {
		writes := owners[owner]
		ends := map[dnsCertsChallengeEnd]int{}
		for _, write := range writes {
			ends[write.End] += 1
		}
		newest := []string{}
		for _, write := range writes[max(0, len(writes)-4):] {
			newest = append(newest, fmt.Sprintf("%d values %s %d at %d", len(write.Values), write.End, write.Status, write.Serial))
		}
		lines = append(lines, fmt.Sprintf("  %s: %v; newest %s", owner, ends, strings.Join(newest, ", ")))
	}
	for _, ready := range self.readies {
		lines = append(lines, "  "+ready)
	}
	return strings.Join(lines, "\n")
}

// Arms a hold on the next ready read, named name: before runs before that read is sent (see the file header). The
// hold, which the arming phase waits on before it ends.
func (self *dnsCertsDnsRecorder) HoldNextReady(name string, before func() error) *dnsCertsHold {
	hold := newDnsCertsHold(name, before, nil)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.readyHold = hold
	return hold
}

// Arms a hold on the next challenge write, named name: before runs before that write is sent, and after once it is
// answered (see the file header).
func (self *dnsCertsDnsRecorder) HoldNextWrite(name string, before func() error, after func() error) *dnsCertsHold {
	hold := newDnsCertsHold(name, before, after)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeHold = hold
	return hold
}

// Arms a gate on the next challenge writes, named name: the first starts before, and it and every write that comes
// until before ends wait for it, or for their own end (see the file header).
func (self *dnsCertsDnsRecorder) GateWrites(name string, before func() error) *dnsCertsHold {
	hold := newDnsCertsHold(name, before, nil)
	hold.gate = true
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writeHold = hold
	return hold
}

// Takes back an armed hold no request took, so no later request takes it.
func (self *dnsCertsDnsRecorder) Disarm(hold *dnsCertsHold) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.readyHold == hold {
		self.readyHold = nil
	}
	if self.writeHold == hold {
		self.writeHold = nil
	}
}

// One hold on a request (see the file header). Safe for concurrent use.
type dnsCertsHold struct {
	name   string
	before func() error
	after  func() error
	// a gate: every request that comes while before runs waits for it
	gate bool
	// closed once before ended, and once before and after both ended
	beforeDone chan struct{}
	done       chan struct{}
	// before and after each start once
	beforeOnce sync.Once
	afterOnce  sync.Once

	stateLock sync.Mutex
	started   bool
	// the requests held
	heldCount int
	// what the held request named: a ready read's serial, a write's name
	what string
	errs []error
}

func newDnsCertsHold(name string, before func() error, after func() error) *dnsCertsHold {
	return &dnsCertsHold{name: name, before: before, after: after, beforeDone: make(chan struct{}), done: make(chan struct{})}
}

func (self *dnsCertsHold) fail(err error) {
	if err == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.errs = append(self.errs, err)
}

// before on a goroutine of its own, started by the first request held, waited for until it ends or ctx does: a request
// whose order the certs worker's drain cuts goes on without it, and the step still ends.
func (self *dnsCertsHold) hold(ctx context.Context, what string) {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.started {
			self.started = true
			self.what = what
		}
		self.heldCount += 1
	}()
	self.beforeOnce.Do(func() {
		go func() {
			self.fail(self.before())
			close(self.beforeDone)
		}()
	})
	select {
	case <-self.beforeDone:
	case <-ctx.Done():
	}
}

// A held request was answered: after runs once, once before has ended, on a goroutine of its own.
func (self *dnsCertsHold) answered() {
	self.afterOnce.Do(func() {
		go func() {
			<-self.beforeDone
			if self.after != nil {
				self.fail(self.after())
			}
			close(self.done)
		}()
	})
}

// Waits until the steps a request started have ended, or ctx does.
func (self *dnsCertsHold) AwaitDone(ctx context.Context) error {
	select {
	case <-self.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%s never came and ended: %w", self.name, ctx.Err())
	}
}

// How many requests the hold held.
func (self *dnsCertsHold) HeldCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.heldCount
}

// Whether a request took the hold, and what it named.
func (self *dnsCertsHold) Started() (bool, string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.started, self.what
}

// Waits until the steps a request started have ended, within timeout: their errors, or nil when no request took the
// hold.
func (self *dnsCertsHold) Wait(timeout time.Duration) error {
	if started, _ := self.Started(); !started {
		return nil
	}
	select {
	case <-self.done:
	case <-time.After(timeout):
		return fmt.Errorf("%s did not end within %s", self.name, timeout)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if 0 < len(self.errs) {
		return fmt.Errorf("%s: %w", self.name, errors.Join(self.errs...))
	}
	return nil
}

// The recorder in front of one client's transport to Pebble (see the file header). Safe for concurrent use.
type dnsCertsAcmeRecorder struct {
	name      string
	transport http.RoundTripper
	keepCount int

	stateLock sync.Mutex
	// every POST, and those that carried no nonce
	postCount    int
	nonceless    int
	refusedCount int
	nonceStates  map[string]*dnsCertsNonceState
	replays      []string
	// the newest problem documents Pebble answered, oldest first, and every one's count by type
	problems     []string
	problemTypes map[string]int
}

// One nonce's sends.
type dnsCertsNonceState struct {
	sendCount int
	// a send of it got an answer back
	answered bool
}

// A client like base whose requests go through a recorder named name in front of base's transport, keeping the newest
// keepCount problem documents.
func newDnsCertsAcmeRecorder(name string, base *http.Client, keepCount int) (*dnsCertsAcmeRecorder, *http.Client) {
	transport := base.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	recorder := &dnsCertsAcmeRecorder{
		name:         name,
		transport:    transport,
		keepCount:    keepCount,
		nonceStates:  map[string]*dnsCertsNonceState{},
		problemTypes: map[string]int{},
	}
	client := *base
	client.Transport = recorder
	return recorder, &client
}

func (self *dnsCertsAcmeRecorder) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.Body == nil {
		response, err := self.transport.RoundTrip(request)
		if err == nil && 400 <= response.StatusCode {
			self.problem(request, response)
		}
		return response, err
	}
	body, err := io.ReadAll(request.Body)
	request.Body.Close()
	if err != nil {
		return nil, err
	}
	nonce := dnsCertsJwsNonce(body)
	self.sending(nonce)
	sent := request.Clone(request.Context())
	sent.Body = io.NopCloser(bytes.NewReader(body))
	sent.ContentLength = int64(len(body))
	response, err := self.transport.RoundTrip(sent)
	if err != nil {
		return nil, err
	}
	refused := false
	if 400 <= response.StatusCode {
		refused = self.problem(request, response) == dnsCertsAcmeProblemPrefix+"badNonce"
	}
	self.answered(nonce, refused)
	return response, nil
}

// Keeps the problem document response answered request with, read whole and handed back whole: its type.
func (self *dnsCertsAcmeRecorder) problem(request *http.Request, response *http.Response) string {
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	response.Body = io.NopCloser(bytes.NewReader(data))
	problem := struct {
		Type   string `json:"type"`
		Detail string `json:"detail"`
	}{}
	if err != nil || json.Unmarshal(data, &problem) != nil {
		problem.Type = "unreadable"
	}
	detail := problem.Detail
	if 200 < len(detail) {
		detail = detail[:200]
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.problemTypes[strings.TrimPrefix(problem.Type, dnsCertsAcmeProblemPrefix)] += 1
	if problem.Type != dnsCertsAcmeProblemPrefix+"badNonce" {
		line := fmt.Sprintf("%s %s %s %d %s: %s", time.Now().Format(time.StampMilli), request.Method, request.URL.Path, response.StatusCode, strings.TrimPrefix(problem.Type, dnsCertsAcmeProblemPrefix), detail)
		self.problems = dnsCertsKeep(self.problems, line, self.keepCount)
	}
	return problem.Type
}

// For a stalled phase's dump: Pebble's problem documents by type, and the newest that were not a bad nonce.
func (self *dnsCertsAcmeRecorder) Describe() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	lines := []string{fmt.Sprintf("%s at Pebble: %d POSTs, problems %v", self.name, self.postCount, self.problemTypes)}
	for _, problem := range self.problems {
		lines = append(lines, "  "+problem)
	}
	return strings.Join(lines, "\n")
}

// A POST's nonce as it goes out: a send of a nonce whose earlier send was answered is a replay.
func (self *dnsCertsAcmeRecorder) sending(nonce string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.postCount += 1
	if nonce == "" {
		self.nonceless += 1
		return
	}
	state := self.nonceStates[nonce]
	if state == nil {
		state = &dnsCertsNonceState{}
		self.nonceStates[nonce] = state
	}
	state.sendCount += 1
	if state.answered {
		self.replays = append(self.replays, nonce)
	}
}

func (self *dnsCertsAcmeRecorder) answered(nonce string, refused bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if refused {
		self.refusedCount += 1
	}
	if state := self.nonceStates[nonce]; state != nil {
		state.answered = true
	}
}

// What the recorder counted: POSTs, those Pebble refused for their nonce, those that carried none, nonces sent more
// than once, and the replays among them, a nonce sent again after an answer to it.
func (self *dnsCertsAcmeRecorder) Counts() (postCount int, refusedCount int, nonceless int, resentCount int, replays []string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	for _, state := range self.nonceStates {
		if 1 < state.sendCount {
			resentCount += 1
		}
	}
	return self.postCount, self.refusedCount, self.nonceless, resentCount, slices.Clone(self.replays)
}

// The anti-replay nonce of a JWS in its flattened JSON serialization (RFC 8555 §6.2): the protected header's, empty
// when the body is no JWS or carries none.
func dnsCertsJwsNonce(body []byte) string {
	jws := struct {
		Protected string `json:"protected"`
	}{}
	if json.Unmarshal(body, &jws) != nil || jws.Protected == "" {
		return ""
	}
	header, err := base64.RawURLEncoding.DecodeString(jws.Protected)
	if err != nil {
		return ""
	}
	protected := struct {
		Nonce string `json:"nonce"`
	}{}
	if json.Unmarshal(header, &protected) != nil {
		return ""
	}
	return protected.Nonce
}
