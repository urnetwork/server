// Artifact staging permission is only the authenticated API account's bounded
// storage budget. Historical VPK/hotkey and complete replay remain independent.
package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The server chooses the deployment; no request may select quota namespaces,
// another operator's credentials, storage paths or a validator identity.
func StReserveAttemptUpload(ctx context.Context, accountId server.Id, size uint64) error {
	if ctx == nil || accountId == (server.Id{}) || size == 0 {
		return errors.New("503 Attempt upload quota owner is invalid.")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cfg := stConfig()
	if cfg == nil || !cfg.Enabled || cfg.DeploymentKey() == "" || cfg.AttemptUploadBudget.Validate() != nil {
		return errors.New("503 Attempt upload is not configured.")
	}
	if err := model.ReserveStAttemptUpload(ctx, cfg.DeploymentKey(), accountId, size, cfg.AttemptUploadBudget); err != nil {
		var rateLimit interface{ RetryAfterSeconds() int }
		if errors.As(err, &rateLimit) {
			return err
		}
		return fmt.Errorf("503 Attempt upload quota unavailable: %w", err)
	}
	return nil
}
