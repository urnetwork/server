package mcp

// mcp receiving middleware: panic recovery and request logging.
//
// The sdk runs method handlers on jsonrpc connection goroutines with no
// recover of its own, so an unrecovered handler panic exits the whole
// process (the http router's recover only covers the http goroutine).
// Recovery must be the outermost middleware; `AddReceivingMiddleware(m1, m2)`
// applies as m1(m2(handler)), so pass it first.

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

// Converts handler panics into jsonrpc errors instead of process exits.
// Context-done raises (the standard model raise pattern) are passed through
// quietly as cancellation.
func createRecoveryMiddleware() mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (result mcpsdk.Result, err error) {
			defer func() {
				if r := recover(); r != nil {
					if server.IsDoneError(r) {
						// standard pattern to raise on context done. ignore
						result = nil
						err = context.Canceled
					} else {
						glog.Infof(
							"[mcp][recover]%s: %s\n",
							method,
							server.ErrorJson(r, debug.Stack()),
						)
						result = nil
						err = fmt.Errorf("internal error handling %s", method)
					}
				}
			}()
			return next(ctx, method, req)
		}
	}
}

// Logs one line per handled request with method, tool name, status, and
// duration. The stateless transport uses a throwaway session per request, so
// there is no session id worth correlating on. The request line is verbose
// (V(1)) to keep per-request log volume low.
func createLoggingMiddleware() mcpsdk.Middleware {
	return func(next mcpsdk.MethodHandler) mcpsdk.MethodHandler {
		return func(ctx context.Context, method string, req mcpsdk.Request) (mcpsdk.Result, error) {
			target := method
			if callToolParams, ok := req.GetParams().(*mcpsdk.CallToolParamsRaw); ok && callToolParams != nil {
				target = fmt.Sprintf("%s %s", method, callToolParams.Name)
			}

			if glog.V(1) {
				glog.Infof("[mcp][request]%s\n", target)
			}

			startTime := time.Now()
			result, err := next(ctx, method, req)
			duration := time.Since(startTime)

			if err != nil {
				glog.Infof("[mcp][response]%s error after %v: %s\n", target, duration, err)
			} else if callToolResult, ok := result.(*mcpsdk.CallToolResult); ok && callToolResult.IsError {
				// tool execution errors are embedded in the result, not
				// returned as errors from the method handler
				glog.Infof("[mcp][response]%s tool error after %v\n", target, duration)
			} else {
				glog.Infof("[mcp][response]%s ok in %v\n", target, duration)
			}

			return result, err
		}
	}
}
