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
	"github.com/urnetwork/server"
)

const stReadOperationBudget = 300 * time.Second
const stReadAttemptBudget = 60 * time.Second

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
	if self == nil || self.cfg == nil || len(urls) == 0 || op == nil {
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
			attempt, stop := context.WithTimeout(ctx, min(stReadAttemptBudget, remaining))
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
