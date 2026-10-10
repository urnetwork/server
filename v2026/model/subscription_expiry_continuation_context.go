package model

import (
	"context"

	"github.com/urnetwork/server/v2026"
)

// A private fixture seam supplies a child context only after expiry commits
// its original report proof. Ordinary callers keep their exact existing context
// and budgets. Tests use real PostgreSQL operations, not substituted errors.
type forceCloseContinuationContextKey struct{}

func forceCloseContinuationContext(ctx context.Context, contractId server.Id) context.Context {
	if forTest, ok := ctx.Value(forceCloseContinuationContextKey{}).(func(context.Context, server.Id) context.Context); ok {
		return forTest(ctx, contractId)
	}
	return ctx
}
