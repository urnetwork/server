// The operator runs the existing scan directly; its child tasks retain ordinary
// queue ownership and deadlines without depending on a scanner task claim.
package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/taskworker/work"
)

// Keep the caller's environment and cancellation. A direct scan has no task
// runner timeout or enclosing database transaction; each child chunk commits
// through ScheduleOpenContractClosures itself. Success acknowledges scheduling.
func runScheduleOpenContractClosures(parent context.Context, writer io.Writer,
	scan func(*work.ScheduleOpenContractClosuresArgs, *session.ClientSession) (*work.ScheduleOpenContractClosuresResult, error),
) (exitCode int) {
	status := "scan_failed"
	defer func() {
		if recover() != nil {
			status = "scan_failed"
			exitCode = 1
		}
		if err := json.NewEncoder(writer).Encode(struct {
			Schema   int    `json:"schema"`
			Kind     string `json:"kind"`
			Status   string `json:"status"`
			PageSize int    `json:"page_size"`
		}{Schema: 1, Kind: "contract-open-closure-scan-v1", Status: status, PageSize: 1024}); err != nil {
			exitCode = 1
		}
	}()
	ctx, cancel := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if ctx.Err() != nil {
		status = "canceled"
		return 1
	}
	clientSession := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	result, err := scan(&work.ScheduleOpenContractClosuresArgs{
		PageSize: 1024, StartedAt: server.NowUtc().Truncate(time.Microsecond),
	}, clientSession)
	if ctx.Err() != nil {
		status = "canceled"
		return 1
	}
	if err != nil || result == nil {
		return 1
	}
	status = "scan_completed"
	return 0
}
