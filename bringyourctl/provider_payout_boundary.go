// A selected earning schedule is admitted before schema writes and checked
// again when its immutable database boundary is prepared after migration.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/docopt/docopt-go"
	"github.com/urnetwork/server"
)

// Only an omitted option preserves ordinary migration without a schedule.
// An explicitly empty, malformed or zero pin cannot silently omit preparation.
func payoutBoundaryMigrationPin(opts docopt.Opts) (string, error) {
	value, selected := opts["--sn-schedule-sha256"]
	if !selected || value == nil {
		return "", nil
	}
	expected, ok := value.(string)
	digest, err := hex.DecodeString(expected)
	if !ok || err != nil || len(digest) != 32 || expected != hex.EncodeToString(digest) || expected == strings.Repeat("0", 64) {
		return "", errors.New("migration schedule requires a nonzero lowercase SHA-256 digest")
	}
	return expected, nil
}

// The actual command uses this ordering. A rejected schedule never reaches
// migration; successful preflight does not replace the later reload/readback.
func migrateWithPayoutBoundary(ctx context.Context, opts docopt.Opts, output io.Writer, migrate func(context.Context)) error {
	if ctx == nil {
		return errors.New("database migration requires context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	expected, err := payoutBoundaryMigrationPin(opts)
	if err != nil {
		return err
	}
	if expected != "" {
		policy, err := server.LoadProviderPayoutTransition(ctx)
		if err != nil {
			return err
		}
		if policy == nil || policy.ConfigSha256 != expected {
			return errors.New("database migration requires the exact reviewed sn.yml digest")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	migrate(ctx)
	return preparePayoutBoundaryAfterMigrateTo(ctx, opts, output)
}

// Database preparation reloads the exact selected bytes after migrations and
// reports only the immutable boundary, never deployment or chain readiness.
func preparePayoutBoundaryAfterMigrateTo(ctx context.Context, opts docopt.Opts, output io.Writer) error {
	expected, err := payoutBoundaryMigrationPin(opts)
	if err != nil {
		return err
	}
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
