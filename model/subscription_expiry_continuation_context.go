package model

import (
	"context"

	"github.com/urnetwork/server"
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

// Task-level tests reach the same seam through the real worker context.
// Production contexts never carry it.
func Testing_WithForceCloseContinuationContext(ctx context.Context, continuation func(context.Context, server.Id) context.Context) context.Context {
	return context.WithValue(ctx, forceCloseContinuationContextKey{}, continuation)
}
