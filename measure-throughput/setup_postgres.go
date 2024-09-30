package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"bringyour.com/bringyour"
	"github.com/pterm/pterm"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupPostgres(ctx context.Context, tempDir string, w io.Writer) (fn func() error, err error) {

	// bringyour.ApplyDbMigrations can panic
	defer func() {
		r := recover()
		if r != nil {
			stackBuffer := make([]byte, 4096)
			n := runtime.Stack(stackBuffer, false)
			rerr, ok := r.(error)
			if !ok {
				err = errors.Join(err, fmt.Errorf("panic: %v\n%s", r, string(stackBuffer[:n])))
			}
			err = errors.Join(err, fmt.Errorf("panic: %w\n%s", rerr, string(stackBuffer[:n])))
		}
	}()

	spinner, err := pterm.DefaultSpinner.
		WithWriter(w).
		Start("Database")

	if err != nil {
		return nil, fmt.Errorf("failed to create postgres spinner: %w", err)
	}

	defer func() {
		if err != nil {
			spinner.Fail(fmt.Sprintf("failed: %v", err))
		}
		spinner.Stop()
	}()

	dbName := "bringyour"
	dbUser := "bringyour"
	dbPassword := "thisisatest"

	spinner.UpdateText("Starting Postgres")
	postgresContainer, err := postgres.Run(
		ctx,
		"docker.io/postgres:16-alpine",
		postgres.WithDatabase(dbName),
		postgres.WithUsername(dbUser),
		postgres.WithPassword(dbPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(5*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start postgres container: %w", err)
	}

	defer func() {
		if err != nil {
			spinner.Fail("failed: %v", err)
			postgresContainer.Terminate(context.Background())
		}
	}()

	ep, err := postgresContainer.Endpoint(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get postgres endpoint: %w", err)
	}

	pgAuth := map[string]string{
		"authority": ep,
		"user":      dbUser,
		"password":  dbPassword,
		"db":        dbName,
	}

	d, err := json.Marshal(pgAuth)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal pg auth: %w", err)
	}

	err = os.WriteFile(filepath.Join(tempDir, "pg.yml"), d, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to write pg.yml: %w", err)
	}

	spinner.UpdateText("applying migrations")
	bringyour.ApplyDbMigrations(ctx)

	spinner.Success("Postgres ready")

	return func() error {
		return postgresContainer.Terminate(context.Background())
	}, nil

}
