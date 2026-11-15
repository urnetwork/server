// bringyourctl account-points backfill-blocks: the operator block points the
// daily task did not award (controller/operator_block_points_backfill.go).
// Owner decision 2026-10-28.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/session"
)

// The dry run's award file when --file is omitted.
func operatorBlockPointsBackfillFile(digest string) string {
	return fmt.Sprintf("operator-block-points-%s.json", digest)
}

func accountPointsBackfillBlocksCommand(opts docopt.Opts) int {
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	file, _ := opts.String("--file")
	blocksValue, _ := opts.String("--blocks")
	// The dry run writes nothing to the database: its process is read only, so
	// PostgreSQL refuses any write on its path.
	if !commit {
		server.RequirePgReadOnly()
	}
	clientSession := session.NewLocalClientSession(context.Background(), "0.0.0.0:0", nil)
	defer clientSession.Cancel()
	if commit {
		return runAccountPointsBackfillCommit(os.Stdout, os.Stderr, file, expect, func(backfill *controller.OperatorBlockPointsBackfill) ([]int64, []int64, error) {
			return controller.CommitOperatorBlockPointsBackfill(clientSession, backfill, expect)
		})
	}
	blocks, err := controller.ParseOperatorBlocks(blocksValue)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return 1
	}
	return runAccountPointsBackfillDryRun(os.Stdout, os.Stderr, file, func() (*controller.OperatorBlockPointsBackfill, error) {
		return controller.PlanOperatorBlockPointsBackfill(clientSession.Ctx, blocks, server.NowUtc())
	})
}

// Prints the dry run's report, saves its awards and ends with the command that
// commits exactly them.
func runAccountPointsBackfillDryRun(
	stdout io.Writer,
	stderr io.Writer,
	file string,
	plan func() (*controller.OperatorBlockPointsBackfill, error),
) int {
	backfill, err := plan()
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	fmt.Fprint(stdout, backfill.Text())
	if len(backfill.Awards) == 0 {
		fmt.Fprintf(stdout, "nothing to commit\n")
		return 0
	}
	if file == "" {
		file = operatorBlockPointsBackfillFile(backfill.Digest)
	}
	data, err := json.MarshalIndent(backfill, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	if err := os.WriteFile(file, data, 0o600); err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s\n", file)
	fmt.Fprintf(stdout, "to commit exactly these: bringyourctl account-points backfill-blocks --commit --expect=%s --file=%s\n", backfill.Digest, file)
	return 0
}

// Commits the awards of the file the dry run saved, when its digest is the
// expected one.
func runAccountPointsBackfillCommit(
	stdout io.Writer,
	stderr io.Writer,
	file string,
	expect string,
	commit func(*controller.OperatorBlockPointsBackfill) ([]int64, []int64, error),
) int {
	if file == "" {
		file = operatorBlockPointsBackfillFile(expect)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	backfill := &controller.OperatorBlockPointsBackfill{}
	if err := json.Unmarshal(data, backfill); err != nil {
		fmt.Fprintf(stderr, "%s: %s\n", file, err)
		return 1
	}
	committed, skipped, err := commit(backfill)
	fmt.Fprintf(stdout, "awarded blocks %v; already awarded, skipped %v\n", committed, skipped)
	if err != nil {
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	return 0
}
