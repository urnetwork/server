// One bounded owner retries complete read-only observations. Transaction sends
// keep their existing single-endpoint, same-nonce reconciliation ownership.
package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/stabi"
	"github.com/urnetwork/server"
)

const stReadOperationBudget = 300 * time.Second
const stReadAttemptBudget = 60 * time.Second

// Client-key authority reads gate interactive registrations and validator
// observations. A healthy read is four small batched requests, so one stalled
// endpoint may hold an attempt for at most this long before the next configured
// endpoint is tried. The general settlement attempt budget above is unchanged.
const stClientKeyAuthorityAttemptBudget = 10 * time.Second

// Private per-client seams allow deterministic deadline/recovery tests without
// process-global clocks or changing production timing based on test execution.
type stRpcReadHooks struct {
	now  func() time.Time
	wait func(context.Context, time.Duration) error
}

type stRpcReadScopeKey struct{}
type stRpcReadScope struct {
	deadline time.Time
	hooks    stRpcReadHooks
}

func beginStRpcRead(ctx context.Context, hooks stRpcReadHooks) (context.Context, context.CancelFunc, error) {
	if ctx == nil {
		return nil, nil, errors.New("st: read requires an owner context")
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if scope, ok := ctx.Value(stRpcReadScopeKey{}).(*stRpcReadScope); ok {
		if !scope.hooks.now().Before(scope.deadline) {
			return nil, nil, context.DeadlineExceeded
		}
		return ctx, func() {}, nil
	}
	if hooks.now == nil {
		hooks.now = time.Now
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
	budget := stReadOperationBudget
	if deadline, ok := ctx.Deadline(); ok {
		budget = min(budget, time.Until(deadline))
	}
	owner, cancel := context.WithTimeout(ctx, stReadOperationBudget)
	scope := &stRpcReadScope{deadline: hooks.now().Add(budget), hooks: hooks}
	return context.WithValue(owner, stRpcReadScopeKey{}, scope), cancel, nil
}

// Bounded cause inspection refuses typed-nil/cyclic/unknown errors and gives a
// contradictory sibling precedence over a coincident transient transport cause.
func retryableStRpcRead(err error) bool {
	causes := server.InspectErrorCauses(err)
	if !causes.Complete {
		return false
	}
	for _, node := range causes.Nodes {
		if !node.Leaf {
			continue
		}
		if status, ok := node.Err.(rpc.HTTPError); ok {
			switch status.StatusCode {
			case http.StatusRequestTimeout, http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
				continue
			}
			return false
		}
		if !retryablePaymentReadError(node.Err, true) {
			return false
		}
	}
	return true
}

// Each retry re-executes the entire original read at one endpoint. Results from
// a failed snapshot never become a partial success or a new transaction send.
func (self *CoreStClient) eachRpcUrls(ctx context.Context, urls []string, op func(context.Context, *ethclient.Client) error) error {
	return self.eachRpcUrlsWithin(ctx, urls, stReadAttemptBudget, op)
}

// The attempt budget bounds one endpoint's complete read, including its dial
// and chain identity probe, so a stalled endpoint cannot consume the time the
// next configured endpoint needs. Every endpoint is still tried in order.
func (self *CoreStClient) eachRpcUrlsWithin(ctx context.Context, urls []string, attemptBudget time.Duration, op func(context.Context, *ethclient.Client) error) error {
	if self == nil || self.cfg == nil || len(urls) == 0 || op == nil || attemptBudget <= 0 {
		return errors.New("st: read endpoint or operation is absent")
	}
	ctx, cancel, err := beginStRpcRead(ctx, self.readHooks)
	if err != nil {
		return err
	}
	defer cancel()
	scope := ctx.Value(stRpcReadScopeKey{}).(*stRpcReadScope)
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(last, err)
		}
		if !scope.hooks.now().Before(scope.deadline) {
			return errors.Join(last, context.DeadlineExceeded)
		}
		failures := make([]error, 0, len(urls))
		for _, url := range urls {
			remaining := scope.deadline.Sub(scope.hooks.now())
			if remaining <= 0 {
				return errors.Join(errors.Join(failures...), context.DeadlineExceeded)
			}
			attempt, stop := context.WithTimeout(ctx, min(attemptBudget, remaining))
			client, err := self.client(attempt, url)
			if err == nil {
				err = op(attempt, client)
			}
			if err == nil {
				err = attempt.Err()
			}
			stop()
			if !scope.hooks.now().Before(scope.deadline) {
				return errors.Join(err, context.DeadlineExceeded)
			}
			if ownerErr := ctx.Err(); ownerErr != nil {
				return errors.Join(err, ownerErr)
			}
			if err == nil {
				return nil
			}
			if client != nil && retryableStRpcRead(err) {
				self.dropClient(url, client)
			}
			failures = append(failures, fmt.Errorf("%s: %w", url, err))
		}
		last = errors.Join(failures...)
		if !retryableStRpcRead(last) {
			return fmt.Errorf("st: no complete RPC observation: %w", last)
		}
		remaining := scope.deadline.Sub(scope.hooks.now())
		if remaining <= 0 {
			return errors.Join(last, context.DeadlineExceeded)
		}
		if err := scope.hooks.wait(ctx, min(time.Second, remaining)); err != nil {
			return errors.Join(last, err)
		}
	}
}

// The per-endpoint bound for client-key authority reads. A test override only
// shortens how long one endpoint may hold an attempt.
func (self *CoreStClient) clientKeyAuthorityAttemptBudget() time.Duration {
	if self != nil && 0 < self.clientKeyAttemptBudget {
		return self.clientKeyAttemptBudget
	}
	return stClientKeyAuthorityAttemptBudget
}

// Ordered failover for the validator batch observation. An Http dial performs
// no I/O, so a dead endpoint is found only by the read itself. Each attempt
// runs the complete read, including its final witness, at one endpoint with a
// fresh client that is closed afterwards; a failed endpoint's values are never
// kept or combined with another endpoint's. One pass, no retry. A large batch
// is sized for paced public endpoints, so no per-endpoint bound shorter than
// the operation's own deadline is imposed here: refusals, transport errors and
// failed verification move to the next endpoint, a stall still holds the batch.
// Every attempt spends the same operation allowance, including failed reads.
func readStClientKeyAuthoritiesFailover(ctx context.Context, urls []string, queries []stabi.ClientKeyAuthorityQuery, limits stabi.ClientKeyAuthorityRpcLimits) ([]stabi.ClientKeyAuthorityObservation, stabi.ClientKeyAuthorityRpcWork, error) {
	if ctx == nil || len(urls) == 0 {
		return nil, stabi.ClientKeyAuthorityRpcWork{}, errors.New("client-key batch has no actual chain transport")
	}
	failures := make([]error, 0, len(urls))
	work := stabi.ClientKeyAuthorityRpcWork{}
	for _, endpoint := range urls {
		if err := ctx.Err(); err != nil {
			return nil, work, errors.Join(errors.Join(failures...), err)
		}
		if work.Requests >= limits.MaximumRequests || work.Methods >= limits.MaximumMethods || work.Bytes >= limits.MaximumBytes {
			return nil, work, errors.Join(errors.Join(failures...), stabi.ErrClientKeyAuthorityRpcWork)
		}
		remaining := stabi.ClientKeyAuthorityRpcLimits{
			MaximumRequests: limits.MaximumRequests - work.Requests,
			MaximumMethods:  limits.MaximumMethods - work.Methods,
			MaximumBytes:    limits.MaximumBytes - work.Bytes,
		}
		authorities, attemptWork, err := func() ([]stabi.ClientKeyAuthorityObservation, stabi.ClientKeyAuthorityRpcWork, error) {
			client, err := ethclient.DialContext(ctx, endpoint)
			if err != nil {
				return nil, stabi.ClientKeyAuthorityRpcWork{}, err
			}
			defer client.Close()
			return stabi.ReadClientKeyAuthoritiesContext(ctx, client, queries, remaining)
		}()
		work.Requests += attemptWork.Requests
		work.Methods += attemptWork.Methods
		work.Bytes += attemptWork.Bytes
		if err == nil {
			return authorities, work, nil
		}
		failures = append(failures, fmt.Errorf("%s: %w", endpoint, err))
		if errors.Is(err, stabi.ErrClientKeyAuthorityRpcWork) {
			break
		}
	}
	return nil, work, errors.Join(failures...)
}
