package main

import (
	"context"
	"encoding/json"
	"io"
	"os"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
)

func preparePayoutBoundaryAfterMigrate(ctx context.Context, opts docopt.Opts) error {
	return preparePayoutBoundaryAfterMigrateTo(ctx, opts, os.Stdout)
}

func preparePayoutBoundaryAfterMigrateTo(ctx context.Context, opts docopt.Opts, output io.Writer) error {
	expected, _ := opts.String("--sn-schedule-sha256")
	if expected == "" {
		return nil
	}
	binding, err := server.PrepareProviderPayoutBoundary(ctx, expected)
	if err != nil {
		return err
	}
	data, err := json.Marshal(struct {
		Boundary                 *server.ProviderEarningBoundary `json:"earning_boundary"`
		DeploymentVerified       bool                            `json:"deployment_verified"`
		ChainReadinessAuthorized bool                            `json:"chain_readiness_authorized"`
	}{Boundary: binding})
	if err != nil {
		return err
	}
	data = append(data, '\n')
	written, err := output.Write(data)
	if err == nil && written != len(data) {
		return io.ErrShortWrite
	}
	return err
}
