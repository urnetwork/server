// A non-reusing probe request owns its tunnel dial and TLS handshake. net/http
// detaches dial cancellation for connection reuse even with keep-alives off;
// restore the request boundary without changing transport or TLS policy.
package providertunnel

import (
	"context"
	"net/http"
	"sync"
)

// The embedded transport retains CloseIdleConnections and HTTP/1 behavior.
// Each RoundTrip creates an independent owner; no cross-request budget or lock.
type providerHttpTransport struct {
	*http.Transport
}

type providerHttpRequestKey struct{}

// Serializes admission with close so a delayed net/http dial cannot enter after
// RoundTrip has joined its work. The original request remains the response-body
// owner: closing this separate dial context must not cancel a successful body.
type providerHttpRequestOwner struct {
	ctx       context.Context
	cancel    context.CancelCauseFunc
	stateLock sync.Mutex
	closed    bool
	dials     sync.WaitGroup
}

// Cancel and join the custom dial work before the caller releases its fetch
// slot. These owned tunnel dials and TLS handshakes honor context cancellation.
func (self *providerHttpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancelCause(req.Context())
	owner := &providerHttpRequestOwner{ctx: ctx, cancel: cancel}
	defer owner.close()
	return self.Transport.RoundTrip(req.WithContext(context.WithValue(req.Context(), providerHttpRequestKey{}, owner)))
}

func (self *providerHttpRequestOwner) close() {
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.closed = true
	}()
	self.cancel(nil)
	self.dials.Wait()
}

// Preserve the transport's values/cancellation, and restore the originating
// request's exact deadline and cancellation stripped by context.WithoutCancel.
// Direct diagnostic calls without RoundTrip retain their supplied context.
func beginProviderHttpDial(ctx context.Context) (context.Context, func(), error) {
	owner, ok := ctx.Value(providerHttpRequestKey{}).(*providerHttpRequestOwner)
	if !ok {
		return ctx, func() {}, nil
	}
	admitted := func() bool {
		owner.stateLock.Lock()
		defer owner.stateLock.Unlock()
		if owner.closed || owner.ctx.Err() != nil {
			return false
		}
		owner.dials.Add(1)
		return true
	}()
	if !admitted {
		err := context.Cause(owner.ctx)
		if err == nil {
			err = context.Canceled
		}
		return nil, nil, err
	}
	dialCtx, cancel := context.WithCancelCause(ctx)
	var deadlineCancel context.CancelFunc = func() {}
	if deadline, ok := owner.ctx.Deadline(); ok {
		dialCtx, deadlineCancel = context.WithDeadline(dialCtx, deadline)
	}
	stop := context.AfterFunc(owner.ctx, func() { cancel(context.Cause(owner.ctx)) })
	if owner.ctx.Err() != nil {
		cancel(context.Cause(owner.ctx))
	}
	return dialCtx, func() {
		stop()
		cancel(nil)
		deadlineCancel()
		owner.dials.Done()
	}, nil
}
