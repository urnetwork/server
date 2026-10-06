// A test-only seam between the attempts of one transaction. Tx reruns its
// callback after an attempt that did not commit; a test that needs a competing
// change committed exactly between two attempts attaches a hook to the
// transaction's context. Production contexts never carry one, and a context
// without a hook costs one lookup per rerun.
package server

import (
	"context"
)

// Keys the hook in a context.
type txRerunHookKey struct{}

// Returns a context whose Tx and MaintenanceTx call hook after each attempt
// that ends without committing, once the retry decision and its backoff are
// done and before the attempt that reruns the callback. The hook runs on the
// caller's goroutine with no connection of the transaction held, so it may
// commit changes of its own through other contexts. Tests only.
func Testing_WithTxRerunHook(ctx context.Context, hook func()) context.Context {
	return context.WithValue(ctx, txRerunHookKey{}, hook)
}

// Calls the context's hook, when it carries one, between two attempts.
func runTxRerunHook(ctx context.Context) {
	if hook, ok := ctx.Value(txRerunHookKey{}).(func()); ok {
		hook()
	}
}
