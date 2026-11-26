package server

// The file clusters' wire (§19.6's deadline row, D-OPS-20): every request but a streamed PUT carries
// its deadline as x-ur-deadline-ms, the request's own deadline (WithBlobDeadline) or RequestTimeout
// after it is issued, whichever comes first, as Unix ms by this process's clock. The header rides
// this transport and not minio-go's request, because RemoveObject takes no custom header, and it is
// added after signing, so it is unsigned: the fork reads it, and SigV4 checks only the headers it
// signed.
//
// A DELETE that names its object (sidecar.WithBlobUnmodified) carries that object's LastModified as
// x-ur-if-unmodified-ms, in Unix ms, by the same route and for the same reason: the fork compares it,
// under the key's lock, with the object it finds, and answers 412 for one stored a second or more
// later (§19.6, DESIGN-DETAILS 2.2.2). Upstream ignores the header, which is why a cluster's deletes
// wait on SettlePassed.
//
// Two bounds cancel a request here, each only until its response starts, so a body then streams under
// the caller's ctx alone: http.Transport's ResponseHeaderTimeout, RequestTimeout after the request's
// last byte with no response (§19.6), and the request's own deadline when that comes first, by this
// process's clock. A cancel tells the store nothing, which is why the header exists.
//
// Each host endpoint of a cluster has its own client (fileClusterHost), and a request goes to a host
// not recently failed. A failed request is surfaced, never re-sent here, since that is the caller's
// unknown outcome (host_blobs.go); minio-go's own retries are off (MaxRetries 1).
//
// The clients of one host, in every cluster of a FileClusters naming it, send through one base
// transport, which opens at most MaxConnsPerHost connections to the host and keeps as many idle
// (§11.15's fan-out; owner decision 780). The capped client holds one of the host's MaxConnsPerHost
// slots, shared the same way, for each request in flight, from before the request is sent until its
// response's body ends or closes, so a request past the cap waits for a slot here rather than for a
// connection inside http.Transport, where no deadline reaches it: it waits under its ctx and until its
// deadline, on a Clock timer that runs only while it waits. A request whose deadline passes first is not
// sent, since the store would only refuse it (503 DeadlinePassed): it fails here with
// errFileClusterConnWait, as one whose ctx ends first does, and marks no host failed, the wait being
// this process's (noteFailure). Marking one would take a healthy host from the pool a burst queues for,
// and raise the pressure on the hosts left.
//
// A request with its slot can still wait a moment inside http.Transport, for a connection another
// request is closing or a dial another started, and http.Transport writes a request as soon as it hands
// it a connection, with no check between. So the deadline is checked again at the handover (httptrace's
// GotConn): a request whose deadline has passed by then has that connection closed under it before a
// byte of it is written. That needs HTTP/1, one request on a connection at a time, which the base
// transport keeps to. A dial still under way at the deadline is no answer from the host either; a dial
// that fails is, and marks it. A test's base that is no http.Transport keeps no connections, and has
// each request once the request has its slot.
//
// A request that waited for its slot reaches its host with only what the wait left of its deadline, and
// the fork refuses it 503 DeadlinePassed once that passes: on arrival, while it waits for its key's lock,
// as it takes the lock and before a write's commits (minio/cmd/settle-deadline.go), a HEAD so refused
// carrying its 503 alone, with no code. The store holds its clock, as this process holds its own, within
// §19.6's 50 ms of true time, so a request that arrives within 100 ms of its deadline may already read
// as past there. A request that reaches its connection with less than DeadlineHostMinimum left, 1 s, was
// cut short by this process's queue, not its host, so its 503, and the cancel at its own deadline, mark
// no host (fileClusterSend, noteFailure; owner decision 790). A request given that much or more still
// marks its host for either, and every request marks it for a reset, a failed dial, another 5xx answer
// or ResponseHeaderTimeout.
//
// Two kinds of request go through a second client of the host, whose base transport is the first's
// without the cap (QD-1402, owner decision 790):
//
//   - One whose body moves at a client's own pace, a GET, ranged GET or PutStream on a ctx
//     sidecar.WithBlobClientPaced marks (a segment upload's body, an object a read streams to its
//     client). It holds its connection for as long as its client takes, and on the capped transport a
//     few slow clients would hold every connection a burst's requests wait for; on its own it is bounded
//     as before the cap, by the lb's limits on each client's connections.
//   - A store check: the admin route, and a HEAD or LIST on a ctx sidecar.WithBlobProbe marks (each
//     cluster's sentinel, evs's and gkm's probes of place/). It is one request a cluster or an engine
//     every few seconds, never a burst, and in the cap it would wait in the block's own queue, so that the
//     queue read as the store failing: stale admin info refuses a cluster's writes and settle, and a
//     failed probe sets its engine not ready. Outside the cap a check measures the store, which still
//     fails closed when it does not answer, the check going stale at CheckStaleAfter.
//
// Both keep their deadlines, and every request but a streamed PUT or the admin route its deadline header.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/urnetwork/sidecar"
)

// §19.6's deadline header (D-OPS-20), read by the fork's handlers.
const fileClusterDeadlineHeader = "X-Ur-Deadline-Ms"

// §19.6's header of a DELETE that names its object, read by the fork's handlers.
const fileClusterUnmodifiedHeader = "X-Ur-If-Unmodified-Ms"

// A request that ended waiting for a connection to its host, its deadline or its ctx coming first. It
// was not sent, so it says nothing of the host and marks none failed (the file header). A dial that
// fails is an error of its own, and marks its host.
var errFileClusterConnWait = errors.New("no connection to the host before the request ended, so it was not sent")

// Marks a request that carries the deadline header: every operation but PutStream sets it.
type fileClusterDeadlineKey struct{}

// The context key withFileClusterDeadline stores a request's fileClusterSend under.
type fileClusterSendKey struct{}

// ctx for a request that carries x-ur-deadline-ms, with a fileClusterSend the transport notes the
// request in, which noteFailure reads. requestTimeout is the cluster's, so the transport computes the
// deadline at the moment it sends.
func withFileClusterDeadline(ctx context.Context, requestTimeout time.Duration) context.Context {
	return context.WithValue(context.WithValue(ctx, fileClusterDeadlineKey{}, requestTimeout), fileClusterSendKey{}, &fileClusterSend{})
}

// What the transport notes of one request that carries x-ur-deadline-ms, for noteFailure: its deadline,
// when it reached its connection, and whether the cancel at its own deadline ended it after that, all
// by Clock. A LIST's pages share one, each page's request starting it again. Safe for concurrent use;
// a nil one notes nothing.
type fileClusterSend struct {
	stateLock sync.Mutex
	deadline  time.Time
	// zero while the request has no connection
	connectedTime time.Time
	cancelled     bool
}

func (self *fileClusterSend) start(deadline time.Time) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.deadline = deadline
	self.connectedTime = time.Time{}
	self.cancelled = false
}

func (self *fileClusterSend) connect(connectedTime time.Time) {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.connectedTime = connectedTime
}

func (self *fileClusterSend) cancelAtDeadline() {
	if self == nil {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.cancelled = true
}

// Whether err, the request's failure, came too late in its time to say anything of its host: the
// request reached its connection with less than minimum of its deadline left, the rest spent before it
// was sent, and was answered 503, as the fork refuses one past its deadline, or was cancelled at its own
// deadline (DeadlineHostMinimum). A non-positive minimum finds none too late.
func (self *fileClusterSend) tooLate(minimum time.Duration, err error) bool {
	if self == nil {
		return false
	}
	late, cancelled := func() (bool, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return !self.connectedTime.IsZero() && self.deadline.Sub(self.connectedTime) < minimum, self.cancelled
	}()
	if !late {
		return false
	}
	response, ok := fileClusterErrorResponse(err)
	return cancelled || (ok && response.StatusCode == http.StatusServiceUnavailable)
}

// The context key WithFileClusterStoreTime stores under.
type fileClusterStoreTimeKey struct{}

// ctx for requests to a store that keeps another clock than the clusters' Clock: storeTime gives the
// store's own time for a deadline on Clock, and x-ur-deadline-ms carries that. T3's store clock
// (sidecart3/clock.go) sets it, since a schedule's fake clock and the store's real one part as a test
// runs, and the fork refuses a deadline already past by its own clock. The cancel at a request's deadline
// still keeps Clock. Production never sets it, and sends Clock's time as it is.
func WithFileClusterStoreTime(ctx context.Context, storeTime func(deadline time.Time) time.Time) context.Context {
	return context.WithValue(ctx, fileClusterStoreTimeKey{}, storeTime)
}

// What x-ur-deadline-ms carries for deadline: the store's time for it when ctx says the store keeps
// another clock, else deadline itself.
func fileClusterHeaderDeadline(ctx context.Context, deadline time.Time) time.Time {
	if storeTime, ok := ctx.Value(fileClusterStoreTimeKey{}).(func(deadline time.Time) time.Time); ok && storeTime != nil {
		return storeTime(deadline)
	}
	return deadline
}

// An http.RoundTripper over one host's base transport that adds the deadline header, holds a slot of
// the host's for each request in flight, ends a request's wait for a slot or a connection at its
// deadline, and cancels at a request's own deadline, as the file header says. Safe for concurrent use.
type fileClusterTransport struct {
	base  http.RoundTripper
	clock sidecar.Clock
	// whether base says when a request reaches its connection: an http.Transport does, through
	// httptrace's GotConn
	traced bool
	// one value for each request in flight through base, MaxConnsPerHost at most, shared by the capped
	// clients of every cluster naming the host; nil for the paced client, or with no cap
	slots chan struct{}
}

func (self *fileClusterTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	ctx := request.Context()
	issued := self.clock.Now()
	wait := &fileClusterConnWait{clock: self.clock, issued: issued}
	// a streamed PUT, or the admin api, carries no deadline: its ctx alone bounds its wait, and
	// ResponseHeaderTimeout its answer
	requestTimeout, timed := ctx.Value(fileClusterDeadlineKey{}).(time.Duration)
	if timed {
		wait.deadline = sidecar.BlobRequestDeadline(ctx, issued, requestTimeout)
		if !issued.Before(wait.deadline) {
			return nil, fmt.Errorf("%w: its own deadline had passed when it was issued", errFileClusterConnWait)
		}
		wait.own = wait.deadline.Before(issued.Add(requestTimeout))
		wait.send, _ = ctx.Value(fileClusterSendKey{}).(*fileClusterSend)
		wait.send.start(wait.deadline)
	}
	requestCtx, cancel := context.WithCancel(ctx)
	wait.cancel = cancel
	// an own deadline's timer runs from issue: it ends the request's wait, and cancels the request at the
	// deadline unless its response has started; RequestTimeout's runs only while the request waits for a
	// slot, ResponseHeaderTimeout bounding the rest
	stop := func() bool { return false }
	if wait.own {
		stop = self.clock.AfterFunc(wait.deadline.Sub(issued), wait.deadlineReached)
	}
	release := func() {}
	fail := func(err error) (*http.Response, error) {
		release()
		stop()
		cancel()
		return nil, wait.failed(ctx, err)
	}
	if self.slots != nil {
		taken, err := self.takeSlot(requestCtx, wait, timed && !wait.own)
		if taken == nil {
			return fail(err)
		}
		release = taken
	}
	// RoundTrip must not change the request it was given
	request = request.Clone(ctx)
	if timed {
		request.Header.Set(fileClusterDeadlineHeader, strconv.FormatInt(fileClusterHeaderDeadline(ctx, wait.deadline).UnixMilli(), 10))
		if request.Method == http.MethodDelete {
			if lastModified, ok := sidecar.BlobUnmodified(ctx); ok {
				request.Header.Set(fileClusterUnmodifiedHeader, strconv.FormatInt(lastModified.UnixMilli(), 10))
			}
		}
	}
	if self.traced {
		requestCtx = httptrace.WithClientTrace(requestCtx, &httptrace.ClientTrace{GotConn: wait.gotConn})
	} else if !wait.connect(self.clock.Now()) {
		// a base that keeps no connections has the request once it has its slot
		return fail(nil)
	}
	response, err := self.base.RoundTrip(request.WithContext(requestCtx))
	stop()
	if err != nil {
		return fail(err)
	}
	response.Body = &fileClusterResponseBody{ReadCloser: response.Body, cancel: cancel, release: release}
	return response, nil
}

// Takes one of the host's slots for the request, waiting while every one is taken until one frees, the
// request ends, or, when timed, its deadline passes, on a Clock timer that runs only while it waits. A
// request whose deadline has passed by the time it has its slot gives the slot back unsent. Returns what
// gives the slot back, once however often it is called, or nil for a request not to be sent, with its
// ctx's error when that ended the wait.
func (self *fileClusterTransport) takeSlot(ctx context.Context, wait *fileClusterConnWait, timed bool) (func(), error) {
	release := sync.OnceFunc(func() {
		<-self.slots
	})
	select {
	case self.slots <- struct{}{}:
		return release, nil
	default:
	}
	if timed {
		stop := self.clock.AfterFunc(wait.deadline.Sub(self.clock.Now()), wait.deadlineReached)
		defer stop()
	}
	select {
	case self.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if !wait.inTime(self.clock.Now()) {
		release()
		return nil, nil
	}
	return release, nil
}

// One request's way to its connection, which fileClusterTransport ends at the request's deadline: it
// waits, then has its connection or, its deadline or ctx coming first, is never sent. Safe for
// concurrent use.
type fileClusterConnWait struct {
	clock  sidecar.Clock
	issued time.Time
	// zero for a request that carries none
	deadline time.Time
	// the deadline is the request's own, before RequestTimeout's, so its cancel holds once the request
	// has its connection too
	own bool
	// the operation's note of the request, nil for none
	send   *fileClusterSend
	cancel context.CancelFunc

	stateLock sync.Mutex
	connected bool
	// the deadline passed before the request had its connection, so it was not sent
	passed bool
}

// Whether the request is still in time at now, its deadline not yet passed. One that is not is never to
// be sent.
func (self *fileClusterConnWait) inTime(now time.Time) bool {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.inTimeWithLock(now)
}

func (self *fileClusterConnWait) inTimeWithLock(now time.Time) bool {
	if self.passed || (!self.deadline.IsZero() && !now.Before(self.deadline)) {
		self.passed = true
		return false
	}
	return true
}

// The request has its connection at now: true, unless its deadline has passed, when it is not to be
// sent.
func (self *fileClusterConnWait) connect(now time.Time) bool {
	connected := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.inTimeWithLock(now) {
			return false
		}
		self.connected = true
		return true
	}()
	if connected {
		self.send.connect(now)
	}
	return connected
}

// httptrace's GotConn, which http.Transport calls just before it writes the request on info's
// connection: a request whose deadline has passed has that connection closed under it, so no byte of it
// is written, and its ctx cancelled, so http.Transport sends it on no other.
func (self *fileClusterConnWait) gotConn(info httptrace.GotConnInfo) {
	if self.connect(self.clock.Now()) {
		return
	}
	self.cancel()
	info.Conn.Close()
}

// The deadline's timer: a request with no connection yet, waiting for a slot or a connection, is not
// sent, and one with its connection is cancelled at its own deadline.
func (self *fileClusterConnWait) deadlineReached() {
	connected := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if !self.connected {
			self.passed = true
		}
		return self.connected
	}()
	switch {
	case !connected:
		self.cancel()
	case self.own:
		self.send.cancelAtDeadline()
		self.cancel()
	}
}

// What a request that failed returns: errFileClusterConnWait for one that never had its connection,
// its deadline or ctx, the caller's, having come first, else err as the base transport gave it.
func (self *fileClusterConnWait) failed(ctx context.Context, err error) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch {
	case self.passed:
		return fmt.Errorf("%w: its deadline passed %v after it was issued", errFileClusterConnWait, self.deadline.Sub(self.issued))
	case !self.connected && ctx.Err() != nil:
		return fmt.Errorf("%w: %w", errFileClusterConnWait, ctx.Err())
	}
	return err
}

// A response body that gives its request's slot back at its end, at a read that fails or at its close,
// whichever comes first, and releases its request's context at its close.
type fileClusterResponseBody struct {
	io.ReadCloser
	cancel  context.CancelFunc
	release func()
}

func (self *fileClusterResponseBody) Read(buffer []byte) (int, error) {
	count, err := self.ReadCloser.Read(buffer)
	if err != nil {
		self.release()
	}
	return count, err
}

func (self *fileClusterResponseBody) Close() error {
	err := self.ReadCloser.Close()
	self.release()
	self.cancel()
	return err
}

// What a host's base transport is: an http.Transport, or a test's fake.
type fileClusterBaseTransportFunction func(tls bool) (http.RoundTripper, error)

// The base transport of a host: dials within DialTimeout, gives up on a response RequestTimeout after
// the request's last byte, and opens at most MaxConnsPerHost connections to the host, keeping as many
// idle. It speaks HTTP/1 alone, one request on a connection at a time, as the check at a connection's
// handover needs (the file header). newFileClusters makes one for each host and shares it among the
// clusters naming the host.
func newFileClusterBaseTransport(settings *FileClusterSettings) fileClusterBaseTransportFunction {
	return func(tls bool) (http.RoundTripper, error) {
		transport, err := minio.DefaultTransport(tls)
		if err != nil {
			return nil, err
		}
		dialer := &net.Dialer{Timeout: settings.DialTimeout}
		transport.DialContext = dialer.DialContext
		transport.ResponseHeaderTimeout = settings.RequestTimeout
		transport.DisableCompression = true
		transport.Protocols = &http.Protocols{}
		transport.Protocols.SetHTTP1(true)
		if 0 < settings.MaxConnsPerHost {
			transport.MaxConnsPerHost = settings.MaxConnsPerHost
			// a burst's connections stay for the next, rather than all but minio-go's 16 closing
			transport.MaxIdleConnsPerHost = settings.MaxConnsPerHost
			transport.MaxIdleConns = max(transport.MaxIdleConns, settings.MaxConnsPerHost)
		}
		return transport, nil
	}
}

// One MinIO host of a cluster, with its own clients. Safe for concurrent use.
type fileClusterHost struct {
	endpoint string
	core     *minio.Core
	// what core sends through
	transport http.RoundTripper
	// the client of a request outside the cap, one whose body moves at a client's own pace or a store
	// check's, and what it and the admin route send through: the host's base transport without
	// MaxConnsPerHost (see the file header)
	pacedCore      *minio.Core
	pacedTransport http.RoundTripper
	// Unix ns of the last request that failed here, zero for none.
	failedAtNanos atomic.Int64
}

// The clients of one host of config's cluster: one sending through base, the host's base transport,
// each request holding one of slots while in flight (nil for no cap), and one through pacedBase, the
// same without its cap. The SDK reads MaxRetries 1 as one attempt, and Region set means no bucket
// location lookup, so every operation is the one request it names.
func newFileClusterHost(
	endpoint string,
	config *FileClusterConfig,
	base http.RoundTripper,
	slots chan struct{},
	pacedBase http.RoundTripper,
	clock sidecar.Clock,
) (*fileClusterHost, error) {
	newClient := func(base http.RoundTripper, slots chan struct{}) (*minio.Core, http.RoundTripper, error) {
		_, traced := base.(*http.Transport)
		transport := &fileClusterTransport{base: base, clock: clock, traced: traced, slots: slots}
		core, err := minio.NewCore(endpoint, &minio.Options{
			Creds:        credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
			Secure:       config.Tls,
			Transport:    transport,
			Region:       config.Region,
			BucketLookup: minio.BucketLookupPath,
			MaxRetries:   1,
		})
		return core, transport, err
	}
	core, transport, err := newClient(base, slots)
	if err != nil {
		return nil, err
	}
	pacedCore, pacedTransport, err := newClient(pacedBase, nil)
	if err != nil {
		return nil, err
	}
	return &fileClusterHost{
		endpoint:       endpoint,
		core:           core,
		transport:      transport,
		pacedCore:      pacedCore,
		pacedTransport: pacedTransport,
	}, nil
}

// What a request is, as the client it goes through depends on it (fileClusterHost.client).
type fileClusterRequestKind int

const (
	// a PUT of a body in hand, or a DELETE
	fileClusterRequestWrite fileClusterRequestKind = iota
	// a GET, ranged GET or PutStream, whose body may move at a client's own pace
	fileClusterRequestBody
	// a HEAD or LIST, which a store check may send
	fileClusterRequestHeadOrList
)

// The client a request of kind on ctx goes through: pacedCore, outside the cap, for a GET, ranged GET
// or PutStream whose body ctx says moves at a client's own pace (sidecar.WithBlobClientPaced), and for a
// HEAD or LIST ctx says a store check sends (sidecar.WithBlobProbe); else core, the capped one. A write
// is capped whatever ctx says, and so is a GET on a probe's ctx or a HEAD on a paced one.
func (self *fileClusterHost) client(ctx context.Context, kind fileClusterRequestKind) *minio.Core {
	switch {
	case kind == fileClusterRequestBody && sidecar.BlobClientPaced(ctx):
		return self.pacedCore
	case kind == fileClusterRequestHeadOrList && sidecar.BlobProbe(ctx):
		return self.pacedCore
	}
	return self.core
}

// A host's base transport for requests outside the cap, at a client's own pace or a store check's: base
// without its cap on the host's connections, its other settings kept. A test's base that is no
// http.Transport keeps no connections and serves as it is.
func fileClusterPacedTransport(base http.RoundTripper) http.RoundTripper {
	transport, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	paced := transport.Clone()
	paced.MaxConnsPerHost = 0
	return paced
}

// Picks the host a request goes to: the next in turn not failed within holdoff, else the one that
// failed longest ago. Safe for concurrent use.
type fileClusterHostPicker struct {
	hosts   []*fileClusterHost
	holdoff time.Duration
	clock   sidecar.Clock

	stateLock sync.Mutex
	next      int
}

func (self *fileClusterHostPicker) pick() *fileClusterHost {
	now := self.clock.Now().UnixNano()
	start := func() int {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		start := self.next
		self.next = (self.next + 1) % len(self.hosts)
		return start
	}()
	var oldestFailed *fileClusterHost
	for i := range self.hosts {
		host := self.hosts[(start+i)%len(self.hosts)]
		failedAt := host.failedAtNanos.Load()
		if failedAt == 0 || self.holdoff.Nanoseconds() <= now-failedAt {
			return host
		}
		if oldestFailed == nil || failedAt < oldestFailed.failedAtNanos.Load() {
			oldestFailed = host
		}
	}
	return oldestFailed
}

// Records a request that failed at host, so the next goes elsewhere.
func (self *fileClusterHostPicker) failed(host *fileClusterHost) {
	host.failedAtNanos.Store(max(1, self.clock.Now().UnixNano()))
}
