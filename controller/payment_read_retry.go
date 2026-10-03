// Only read-only payment observations use this operation budget. Processor
// submissions retain their existing single-call/idempotent reconciliation path.
package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/urnetwork/server"
)

const DefaultPaymentReadBudget = 300 * time.Second
const maximumPaymentReadBytes = 1024 * 1024

// Budget belongs to one complete GET, while AttemptTimeout bounds one wire
// attempt. A caller's earlier cancellation/deadline always takes precedence.
type PaymentReadSettings struct {
	Budget         time.Duration
	AttemptTimeout time.Duration
}

func (self PaymentReadSettings) normalized() (PaymentReadSettings, error) {
	if self.Budget == 0 {
		self.Budget = DefaultPaymentReadBudget
	}
	if self.AttemptTimeout == 0 {
		self.AttemptTimeout = server.DefaultHttpTimeout
	}
	if self.Budget < 60*time.Second || self.Budget > 15*time.Minute || self.AttemptTimeout <= 0 || self.AttemptTimeout > server.DefaultHttpTimeout {
		return self, fmt.Errorf("payment GET budget must be 60s..15m and attempt timeout in (0,60s]")
	}
	return self, nil
}

// Immutable per-client test seams; production uses the real clock and HTTP
// transport. There is no process-global clock, retry policy, or transport hook.
type paymentReadHooks struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
	do   func(*http.Request) (*http.Response, error)
}

type paymentReadTransportError struct{ cause error }

func (self *paymentReadTransportError) Error() string { return self.cause.Error() }
func (self *paymentReadTransportError) Unwrap() error { return self.cause }

type paymentReadCloseError struct{ cause error }

func (self *paymentReadCloseError) Error() string { return self.cause.Error() }
func (self *paymentReadCloseError) Unwrap() error { return self.cause }

type paymentReadStatusError struct{ cause *server.HttpStatusError }

func (self *paymentReadStatusError) Error() string { return self.cause.Error() }
func (self *paymentReadStatusError) Unwrap() error { return self.cause }

func paymentHttpGet[R any](ctx context.Context, settings PaymentReadSettings, hooks paymentReadHooks, uri string, headers server.HeaderCallback, decode server.ResponseCallback[R]) (R, error) {
	var empty R
	if ctx == nil {
		return empty, fmt.Errorf("payment GET requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	settings, err := settings.normalized()
	if err != nil {
		return empty, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Budget)
	defer cancel()
	now := hooks.now
	if now == nil {
		now = time.Now
	}
	deadline := now().Add(settings.Budget)
	do := hooks.do
	if do == nil {
		client := server.DefaultHttpClient()
		defer client.CloseIdleConnections()
		do = client.Do
	}
	wait := hooks.wait
	if wait == nil {
		wait = func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		}
	}
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return empty, errors.Join(lastErr, err)
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return empty, errors.Join(lastErr, context.DeadlineExceeded)
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, min(settings.AttemptTimeout, remaining))
		value, err := paymentHttpGetAttempt(attemptCtx, do, uri, headers, decode)
		attemptCancel()
		if ownerErr := ctx.Err(); ownerErr != nil {
			return empty, errors.Join(err, ownerErr)
		}
		if !now().Before(deadline) {
			return empty, errors.Join(err, context.DeadlineExceeded)
		}
		if err == nil {
			return value, nil
		}
		lastErr = err
		if !retryablePaymentReadError(err, false) {
			return empty, err
		}
		delay := time.Second + time.Duration(rand.Int64N(int64(time.Second)))
		delay = min(delay, deadline.Sub(now()))
		if delay <= 0 {
			return empty, errors.Join(lastErr, context.DeadlineExceeded)
		}
		if err := wait(ctx, delay); err != nil {
			return empty, errors.Join(lastErr, err)
		}
	}
}

// Every body is bounded and closed before a retry; cancellation refuses even a
// full successfully read response. Decode errors never acquire transport origin.
func paymentHttpGetAttempt[R any](ctx context.Context, do func(*http.Request) (*http.Response, error), uri string, headers server.HeaderCallback, decode server.ResponseCallback[R]) (R, error) {
	var empty R
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return empty, err
	}
	headers(request.Header)
	response, err := do(request)
	if err != nil {
		var closeErr error
		if response != nil && response.Body != nil {
			if e := response.Body.Close(); e != nil {
				closeErr = &paymentReadCloseError{cause: e}
			}
		}
		return empty, errors.Join(&paymentReadTransportError{cause: err}, closeErr)
	}
	if response == nil || response.Body == nil {
		return empty, fmt.Errorf("payment GET returned no response body")
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumPaymentReadBytes+1))
	closeErr := response.Body.Close()
	if closeErr != nil {
		closeErr = &paymentReadCloseError{cause: closeErr}
	}
	if readErr != nil {
		readErr = &paymentReadTransportError{cause: readErr}
	}
	var result R
	var resultErr error
	if len(body) > maximumPaymentReadBytes {
		resultErr = fmt.Errorf("payment GET response exceeds %d bytes", maximumPaymentReadBytes)
	} else if response.StatusCode != http.StatusOK {
		resultErr = &paymentReadStatusError{cause: &server.HttpStatusError{StatusCode: response.StatusCode, Status: response.Status, ResponseBody: string(body)}}
	} else if readErr == nil && ctx.Err() == nil {
		result, resultErr = decode(response, body)
	}
	var contextErr error
	if err := ctx.Err(); err != nil {
		contextErr = &paymentReadTransportError{cause: err}
	}
	if err := errors.Join(resultErr, readErr, closeErr, contextErr); err != nil {
		return empty, err
	}
	return result, nil
}

// All joined causes must permit retry. Authentication, malformed payloads,
// identity conflicts and close failures dominate a coincident timeout/503.
func retryablePaymentReadError(err error, transport bool) bool {
	if err == nil || err == context.Canceled {
		return false
	}
	switch value := err.(type) {
	case *paymentReadCloseError:
		return false
	case *paymentReadStatusError:
		switch value.cause.StatusCode {
		case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		}
		return false
	case *paymentReadTransportError:
		return retryablePaymentReadError(value.cause, true)
	case *os.PathError:
		return false
	case *url.Error:
		return retryablePaymentReadError(value.Err, transport)
	case *net.OpError:
		return retryablePaymentReadError(value.Err, transport)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, cause := range joined.Unwrap() {
			if cause != nil {
				found = true
				if !retryablePaymentReadError(cause, transport) {
					return false
				}
			}
		}
		return found
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return retryablePaymentReadError(wrapped.Unwrap(), transport)
	}
	if !transport {
		return false
	}
	if err == context.DeadlineExceeded || err == io.EOF || err == io.ErrUnexpectedEOF || err == net.ErrClosed || err == syscall.ECONNRESET || err == syscall.ECONNREFUSED || err == syscall.EPIPE || err == syscall.ETIMEDOUT || err == syscall.ENETUNREACH || err == syscall.EHOSTUNREACH {
		return true
	}
	if network, ok := err.(net.Error); ok {
		return network.Timeout() || network.Temporary()
	}
	return false
}
