package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/pterm/pterm"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

func setupRedis(ctx context.Context, tempDir string, w io.Writer) (func() error, error) {

	spinner, err := pterm.DefaultSpinner.
		WithWriter(w).
		WithText("Starting Redis").
		Start("Redis")

	if err != nil {
		return nil, fmt.Errorf("failed to create redis spinner: %w", err)
	}

	defer func() {
		if err != nil {
			spinner.Fail("failed: %v", err)
		}
		spinner.Stop()
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

	err = os.WriteFile(filepath.Join(tempDir, "redis.yml"), d, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to write redis config: %w", err)
	}

	spinner.Success("Redis ready")

	return func() error {
		return redisContainer.Terminate(context.Background())
	}, nil

}
