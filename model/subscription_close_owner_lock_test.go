package model

import (
	"context"
	"testing"

	"github.com/urnetwork/server"
)

// The blocker owns its connection in a separate actor. The caller can run
// registration and its read assertions without retaining a PostgreSQL handle.
func holdContractCloseTestRow(t testing.TB, ctx context.Context, query string, args ...any) func() {
	t.Helper()
	ready := make(chan error, 1)
	release := make(chan struct{})
	done := runPaymentModelTest(func() error {
		conn, err := server.AcquireMaintenanceDbConn(ctx)
		if err != nil {
			ready <- err
			return err
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, `SET default_transaction_read_only=off`); err != nil {
			ready <- err
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			ready <- err
			return err
		}
		defer tx.Rollback(context.Background())
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			ready <- err
			return err
		}
		ready <- nil
		select {
		case <-release:
			return tx.Rollback(ctx)
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	joined := false
	finish := func() {
		if !joined {
			joined = true
			close(release)
			if err := <-done; err != nil {
				t.Error("close owner blocker cleanup", err)
			}
		}
	}
	t.Cleanup(finish)
	select {
	case err := <-ready:
		server.Raise(err)
	case err := <-done:
		joined = true
		close(release)
		if err == nil {
			t.Fatal("close owner blocker ended before its ready barrier")
		}
		server.Raise(err)
	}
	return finish
}
