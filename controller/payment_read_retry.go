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
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server"
)

const DefaultPaymentReadBudget = 300 * time.Second
const maximumPaymentReadBytes = 1024 * 1024

// Budget belongs to one logical read (including its pages/related balances),
// while AttemptTimeout bounds one wire attempt. A caller's earlier deadline wins.
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

type paymentReadStatusError struct {
	cause      *server.HttpStatusError
	retryAfter string
}

func (self *paymentReadStatusError) Error() string { return self.cause.Error() }
func (self *paymentReadStatusError) Unwrap() error { return self.cause }

type paymentReadScopeKey struct{}

// A logical list/pages/balances operation owns one deadline and one transport.
// Child GETs borrow this immutable scope; none can restart the budget.
type paymentReadScope struct {
	deadline time.Time
	settings PaymentReadSettings
	hooks    paymentReadHooks
}

// Authenticated observations never forward user/API credentials via redirects.
// The fixed endpoint or admitted pagination Link must return the response itself.
func paymentReadHttpClient() *http.Client {
	client := server.DefaultHttpClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func beginPaymentReads(ctx context.Context, settings PaymentReadSettings, hooks paymentReadHooks) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("payment GET requires a context")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if scope, ok := ctx.Value(paymentReadScopeKey{}).(*paymentReadScope); ok {
		if !scope.hooks.now().Before(scope.deadline) {
			return nil, nil, context.DeadlineExceeded
		}
		return ctx, func() {}, nil
	}
	settings, err := settings.normalized()
	if err != nil {
		return nil, nil, err
	}
	ownerBudget := settings.Budget
	if deadline, ok := ctx.Deadline(); ok {
		ownerBudget = min(ownerBudget, time.Until(deadline))
	}
	ctx, cancel := context.WithTimeout(ctx, settings.Budget)
	if hooks.now == nil {
		hooks.now = time.Now
	}
	closeIdle := func() {}
	if hooks.do == nil {
		client := paymentReadHttpClient()
		closeIdle = client.CloseIdleConnections
		hooks.do = client.Do
	}
	if hooks.wait == nil {
		hooks.wait = func(ctx context.Context, delay time.Duration) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
				return nil
			}
		}
	}
	scope := &paymentReadScope{deadline: hooks.now().Add(ownerBudget), settings: settings, hooks: hooks}
	return context.WithValue(ctx, paymentReadScopeKey{}, scope), func() { cancel(); closeIdle() }, nil
}

func paymentHttpGet[R any](ctx context.Context, settings PaymentReadSettings, hooks paymentReadHooks, uri string, headers server.HeaderCallback, decode server.ResponseCallback[R]) (R, error) {
	var empty R
	ctx, cancel, err := beginPaymentReads(ctx, settings, hooks)
	if err != nil {
		return empty, err
	}
	defer cancel()
	scope := ctx.Value(paymentReadScopeKey{}).(*paymentReadScope)
	now, deadline := scope.hooks.now, scope.deadline
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return empty, errors.Join(lastErr, err)
		}
		remaining := deadline.Sub(now())
		if remaining <= 0 {
			return empty, errors.Join(lastErr, context.DeadlineExceeded)
		}
		attemptCtx, attemptCancel := context.WithTimeout(ctx, min(scope.settings.AttemptTimeout, remaining))
		value, err := paymentHttpGetAttempt(attemptCtx, scope.hooks.do, uri, headers, decode)
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
		delay = max(delay, paymentReadRetryAfter(err, now(), deadline.Sub(now())))
		delay = min(delay, deadline.Sub(now()))
		if delay <= 0 {
			return empty, errors.Join(lastErr, context.DeadlineExceeded)
		}
		if err := scope.hooks.wait(ctx, delay); err != nil {
			return empty, errors.Join(lastErr, err)
		}
	}
}

// Retry-After cannot extend the owner deadline or overflow a duration. Invalid
// headers retain the normal short pause, and past dates never spin immediately.
func paymentReadRetryAfter(err error, now time.Time, remaining time.Duration) time.Duration {
	var status *paymentReadStatusError
	causes := server.InspectErrorCauses(err)
	if !causes.Complete {
		return 0
	}
	for _, node := range causes.Nodes {
		if candidate, ok := node.Err.(*paymentReadStatusError); ok && candidate != nil {
			status = candidate
			break
		}
	}
	if status == nil {
		return 0
	}
	value := strings.TrimSpace(status.retryAfter)
	if value == "" || len(value) > 128 {
		return 0
	}
	digits := true
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			digits = false
			break
		}
	}
	if digits {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(max(remaining, 0)/time.Second) {
			return max(remaining, 0)
		}
		return min(time.Duration(seconds)*time.Second, max(remaining, 0))
	}
	if after, err := http.ParseTime(value); err == nil {
		return min(max(after.Sub(now), 0), max(remaining, 0))
	}
	return 0
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
		resultErr = &paymentReadStatusError{cause: &server.HttpStatusError{StatusCode: response.StatusCode, Status: response.Status, ResponseBody: string(body)}, retryAfter: response.Header.Get("Retry-After")}
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
	causes := server.InspectErrorCauses(err)
	if !causes.Complete {
		return false
	}
	transportOrigins := make([]bool, len(causes.Nodes))
	statusOrigins := make([]bool, len(causes.Nodes))
	for index, node := range causes.Nodes {
		transportOrigins[index] = transport
		if node.Parent >= 0 {
			transportOrigins[index] = transportOrigins[node.Parent]
			statusOrigins[index] = statusOrigins[node.Parent]
		}
		switch node.Err.(type) {
		case *paymentReadCloseError, *os.PathError:
			return false
		case *paymentReadTransportError:
			transportOrigins[index] = true
		case *paymentReadStatusError:
			statusOrigins[index] = true
		}
		if !node.Leaf {
			continue
		}
		if value, ok := node.Err.(*server.HttpStatusError); ok && statusOrigins[index] && value != nil {
			switch value.StatusCode {
			case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				continue
			}
			return false
		}
		if !transportOrigins[index] || node.Err == context.Canceled {
			return false
		}
		if node.Err == context.DeadlineExceeded || node.Err == io.EOF || node.Err == io.ErrUnexpectedEOF || node.Err == net.ErrClosed || node.Err == syscall.ECONNRESET || node.Err == syscall.ECONNREFUSED || node.Err == syscall.EPIPE || node.Err == syscall.ETIMEDOUT || node.Err == syscall.ENETUNREACH || node.Err == syscall.EHOSTUNREACH {
			continue
		}
		if network, ok := node.Err.(net.Error); ok && (network.Timeout() || network.Temporary()) {
			continue
		}
		return false
	}
	return true
}
