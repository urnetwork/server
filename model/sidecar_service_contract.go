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
	"math"
	"time"
)

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

// The latest end of a contract inserted on ctx, in UTC to the millisecond, which the insert caps the row's
// expiration_time at beside its database-clock hour; nil for none. The insert takes no time of the application's:
// the deadline is the pair's grant expiry, an absolute time, and CreateContract checked it against its own clock
// before it got this far.
func contractDeadlineFor(ctx context.Context) *time.Time {
	deadline, ok := ctx.Value(contractDeadlineKey{}).(time.Time)
	if !ok {
		return nil
	}
	deadline = deadline.UTC().Truncate(time.Millisecond)
	return &deadline
}
