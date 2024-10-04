package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"bringyour.com/bringyour"
	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

func setupPostgres(ctx context.Context, tempDir string, pw progress.Writer) (fn func() error, err error) {

	tracker := &progress.Tracker{
		Message: "Postgres",
		Total:   2,
		// Units:   *units,
	}

	pw.AppendTracker(tracker)
	tracker.Start()

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

	defer func() {
		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("Starting Postgres failed: %v", err))
			tracker.MarkAsErrored()
			return
		}

		tracker.UpdateMessage("Postgres is ready")
		tracker.MarkAsDone()
	}()

	dbName := "bringyour"
	dbUser := "bringyour"
	dbPassword := "thisisatest"

	tracker.UpdateMessage("Postgres: Starting Container")
	postgresContainer, err := postgres.Run(
		ctx,
		"docker.io/postgres:16-alpine",
		postgres.WithDatabase(dbName),
		postgres.WithUsername(dbUser),
		postgres.WithPassword(dbPassword),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(10*time.Second),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to start postgres container: %w", err)
	}
	tracker.Increment(1)

	defer func() {
		if err != nil {
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

	tracker.UpdateMessage("Postgres: applying migrations")
	bringyour.ApplyDbMigrations(ctx)
	tracker.Increment(1)

	return func() error {
		return postgresContainer.Terminate(context.Background())
	}, nil

}
