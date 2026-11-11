package model

// §6.10.1's Service contracts (C9, D-PROTO-9, D-PROTO-15; PROTO-M5-04): what CreateContract mints between an
// urnetwork-edge platform client and a sidecar, for a pair a Find recorded (socket_service_pair.go). The decision is
// the controller's, on the relationship's one query and one Redis read of the pair; this file holds the pair's
// deadline and carries it to the contract's insert.
//
// A Service contract has no escrow or payout: §20's leases bill its bytes. It is created on the no-escrow path, as a
// Network contract is, and ends at the earlier of its pair's grant expiry and the contract's own hour, so a grant that
// ends ends the contract with it. Contract-hole forwarding is keyed on the client pair and needs nothing more.

import (
	"context"
	"errors"
	"math"
	"time"
)

// A deadline that passed before the contract's insert: the pair's grant ended under the request.
var ErrContractDeadlinePassed = errors.New("Contract deadline passed before creation.")

// CreateContract's Service rule's tunables.
type ServiceContractSettings struct {
	// The longest CreateContract waits for its one read of the pair, where the request's own context sets an
	// earlier deadline or none: 2 s. The read is one HGET, so this bounds only a Redis that hangs, and a read
	// that times out refuses the contract, as an unavailable pair does. Must be positive.
	PairReadTimeout time.Duration
}

func DefaultServiceContractSettings() *ServiceContractSettings {
	return &ServiceContractSettings{
		PairReadTimeout: 2 * time.Second,
	}
}

// When a Service contract created at now ends, for a pair whose grant expires at grantExpireMillis (0: none): the
// earlier of that and now plus the contract's own hour, DefaultContractExpiration. False when the grant has ended.
func ServiceContractDeadline(now time.Time, grantExpireMillis uint64) (time.Time, bool) {
	deadline := now.Add(DefaultContractExpiration)
	if grantExpireMillis == 0 {
		return deadline, true
	}
	if uint64(math.MaxInt64) < grantExpireMillis {
		// past any time a contract could reach
		return deadline, true
	}
	grantExpire := time.UnixMilli(int64(grantExpireMillis)).UTC()
	if !now.Before(grantExpire) {
		return time.Time{}, false
	}
	if grantExpire.Before(deadline) {
		return grantExpire, true
	}
	return deadline, true
}

// Carries a contract's latest end to its insert.
type contractDeadlineKey struct{}

// ctx with the latest time a contract created on it may end: a Service contract's ServiceContractDeadline. A
// deadline past DefaultContractExpiration from the insert changes nothing.
func WithContractDeadline(ctx context.Context, deadline time.Time) context.Context {
	return context.WithValue(ctx, contractDeadlineKey{}, deadline)
}

// How long a contract inserted at now on ctx lives: DefaultContractExpiration, or less to end by ctx's deadline, in
// whole milliseconds as the insert takes it, with that deadline in UTC to the millisecond, which the insert caps the
// row's expiration_time at, since the database's clock runs on from now; nil for none. ErrContractDeadlinePassed when
// the deadline is not a millisecond away.
func contractExpirationFor(ctx context.Context, now time.Time) (time.Duration, *time.Time, error) {
	expiration := DefaultContractExpiration
	deadline, ok := ctx.Value(contractDeadlineKey{}).(time.Time)
	if !ok {
		return expiration, nil, nil
	}
	deadline = deadline.UTC().Truncate(time.Millisecond)
	remaining := deadline.Sub(now).Truncate(time.Millisecond)
	if remaining < time.Millisecond {
		return 0, nil, ErrContractDeadlinePassed
	}
	return min(expiration, remaining), &deadline, nil
}
