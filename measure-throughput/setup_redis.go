package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jedib0t/go-pretty/v6/progress"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

func setupRedis(ctx context.Context, vaultDir string, pw progress.Writer) (fn func() error, err error) {

	tracker := &progress.Tracker{
		Message: "Redis",
		Total:   1,
		// Units:   *units,
	}

	pw.AppendTracker(tracker)

	tracker.Start()

	defer func() {
		if err != nil {
			tracker.UpdateMessage(fmt.Sprintf("Starting Redis failed: %v", err))
			tracker.MarkAsErrored()
			return
		}
		tracker.UpdateMessage("Redis is ready")
		tracker.MarkAsDone()
	}()

	redisContainer, err := redis.Run(ctx, "redis:7.4.0")
	if err != nil {
		return nil, fmt.Errorf("failed to start redis container: %w", err)
	}

	defer func() {
		if err != nil {
			redisContainer.Terminate(context.Background())
		}
	}()

	ep, err := redisContainer.Endpoint(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("failed to get redis endpoint: %w", err)
	}

	redisConfig := map[string]any{
		"authority": ep,
		"password":  "",
		"db":        0,
	}

	d, err := json.Marshal(redisConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal redis config: %w", err)
	}

	err = os.WriteFile(filepath.Join(vaultDir, "redis.yml"), d, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to write redis config: %w", err)
	}

	tracker.Increment(1)

	return func() error {
		return redisContainer.Terminate(context.Background())
	}, nil

}
