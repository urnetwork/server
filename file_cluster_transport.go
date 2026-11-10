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

import (
	"context"
	"io"
	"net"
	"net/http"
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

// Marks a request that carries the deadline header: every operation but PutStream sets it.
type fileClusterDeadlineKey struct{}

// ctx for a request that carries x-ur-deadline-ms. requestTimeout is the cluster's, so the transport
// computes the deadline at the moment it sends.
func withFileClusterDeadline(ctx context.Context, requestTimeout time.Duration) context.Context {
	return context.WithValue(ctx, fileClusterDeadlineKey{}, requestTimeout)
}

// An http.RoundTripper over one host's base transport that adds the deadline header and cancels at a
// request's own deadline, as the file header says. Safe for concurrent use.
type fileClusterTransport struct {
	base  http.RoundTripper
	clock sidecar.Clock
}

func (self *fileClusterTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	requestTimeout, ok := request.Context().Value(fileClusterDeadlineKey{}).(time.Duration)
	if !ok {
		// a streamed PUT, or the admin api: no deadline but the base transport's
		return self.base.RoundTrip(request)
	}
	issued := self.clock.Now()
	deadline := sidecar.BlobRequestDeadline(request.Context(), issued, requestTimeout)
	// RoundTrip must not change the request it was given
	request = request.Clone(request.Context())
	request.Header.Set(fileClusterDeadlineHeader, strconv.FormatInt(deadline.UnixMilli(), 10))
	if request.Method == http.MethodDelete {
		if lastModified, ok := sidecar.BlobUnmodified(request.Context()); ok {
			request.Header.Set(fileClusterUnmodifiedHeader, strconv.FormatInt(lastModified.UnixMilli(), 10))
		}
	}
	if !deadline.Before(issued.Add(requestTimeout)) {
		// ResponseHeaderTimeout is the bound
		return self.base.RoundTrip(request)
	}
	// the request's own deadline comes first: cancel at it unless the response has started
	requestCtx, cancel := context.WithCancel(request.Context())
	stop := self.clock.AfterFunc(deadline.Sub(issued), cancel)
	response, err := self.base.RoundTrip(request.WithContext(requestCtx))
	stop()
	if err != nil {
		cancel()
		return nil, err
	}
	response.Body = &fileClusterResponseBody{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

// A response body that releases its request's context when closed.
type fileClusterResponseBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (self *fileClusterResponseBody) Close() error {
	err := self.ReadCloser.Close()
	self.cancel()
	return err
}

// What a host's base transport is: an http.Transport, or a test's fake.
type fileClusterBaseTransportFunction func(tls bool) (http.RoundTripper, error)

// The base transport of every host: dials within dialTimeout and gives up on a response
// requestTimeout after the request's last byte.
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
		return transport, nil
	}
}

// One MinIO host of a cluster, with its own client. Safe for concurrent use.
type fileClusterHost struct {
	endpoint string
	core     *minio.Core
	// what core sends through, which the admin route uses too
	transport http.RoundTripper
	// Unix ns of the last request that failed here, zero for none.
	failedAtNanos atomic.Int64
}

// A client for one host. The SDK reads MaxRetries 1 as one attempt, and Region set means no bucket
// location lookup, so every operation is the one request it names.
func newFileClusterHost(
	endpoint string,
	config *FileClusterConfig,
	settings *FileClusterSettings,
	clock sidecar.Clock,
	newBaseTransport fileClusterBaseTransportFunction,
) (*fileClusterHost, error) {
	base, err := newBaseTransport(config.Tls)
	if err != nil {
		return nil, err
	}
	transport := &fileClusterTransport{base: base, clock: clock}
	core, err := minio.NewCore(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(config.AccessKey, config.SecretKey, ""),
		Secure:       config.Tls,
		Transport:    transport,
		Region:       config.Region,
		BucketLookup: minio.BucketLookupPath,
		MaxRetries:   1,
	})
	if err != nil {
		return nil, err
	}
	return &fileClusterHost{endpoint: endpoint, core: core, transport: transport}, nil
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
