package main

// usagearchivectl review: the attribution review export (package usagearchive,
// review.go). Exit codes: 0 done (or a plan, verification or dry run that
// passed); 3 an export that a bound stopped with batches left; 4 a refusal,
// with nothing written; 1 any other failure; 2 a usage error.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/urnetwork/server/usagearchive"
)

const reviewUsage = `usagearchivectl review: export the contracts that payments under attribution review paid, then clear the review.

  plan          read only: the payments, their tied contracts by day, one sample batch's rows
  export        export batches of the tied contracts to --dir, resuming after the last batch
  verify        read every stored copy back, and check the coverage of the tied contracts on Main
  status        the run in --dir, without a database
  clear         plan the clear of the payments' attribution review flag; --commit --expect=<digest> makes it
  restore-flag  plan setting the flag again from a clear's before record; --commit --expect=<digest> makes it

Run "usagearchivectl review <command> -h" for a command's flags.
`

const (
	reviewExitPaused  = 3
	reviewExitRefused = 4
)

type reviewExit struct {
	code int
	err  error
}

func (self *reviewExit) Error() string {
	if self.err == nil {
		return fmt.Sprintf("exit %d", self.code)
	}
	return self.err.Error()
}

// Runs a review command; the error carries its exit code.
func runReview(ctx context.Context, args []string) error {
	if len(args) < 1 {
		fmt.Fprint(os.Stderr, reviewUsage)
		return &reviewExit{code: 2}
	}
	var err error
	switch args[0] {
	case "plan":
		err = runReviewPlan(ctx, args[1:])
	case "export":
		err = runReviewExport(ctx, args[1:])
	case "verify":
		err = runReviewVerify(ctx, args[1:])
	case "status":
		err = runReviewStatus(args[1:])
	case "clear":
		err = runReviewClear(ctx, args[1:])
	case "restore-flag":
		err = runReviewRestoreFlag(ctx, args[1:])
	default:
		fmt.Fprint(os.Stderr, reviewUsage)
		return &reviewExit{code: 2}
	}
	var exit *reviewExit
	if err != nil && !errors.As(err, &exit) && usagearchive.IsReviewRefusal(err) {
		return &reviewExit{code: reviewExitRefused, err: err}
	}
	return err
}

func reviewHost() string {
	host, _ := os.Hostname()
	return host
}

// dir must be inside mount, and mount a mount point of its own.
func requireMountedDir(dir string, mount string) error {
	if mount == "" {
		return nil
	}
	if err := requireOwnMount(mount); err != nil {
		return err
	}
	relative, err := filepath.Rel(filepath.Clean(mount), filepath.Clean(dir))
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, "../") {
		return fmt.Errorf("%s is not inside %s", dir, mount)
	}
	return nil
}

func runReviewPlan(ctx context.Context, args []string) error {
	settings := usagearchive.DefaultReviewSettings()
	flags := flag.NewFlagSet("review plan", flag.ContinueOnError)
	pgPath := flags.String("pg", "/etc/usagearchive/pg_maintenance.yml", "the PostgreSQL resource")
	payments := flags.String("payments", "", "the payments under attribution review, comma separated")
	dir := flags.String("dir", "", "the run's directory, to report on (optional)")
	flags.IntVar(&settings.SampleContracts, "sample-contracts", settings.SampleContracts, "tied contracts whose rows the sample batch counts")
	flags.IntVar(&settings.BatchContracts, "batch-contracts", settings.BatchContracts, "the export's batch size, for the projection")
	flags.Float64Var(&settings.MaxContractsPerSecond, "max-contracts-per-second", settings.MaxContractsPerSecond, "the export's pace, for the projection")
	flags.DurationVar(&settings.StatementTimeout, "statement-timeout", 25*time.Second, "bounds each statement")
	if err := flags.Parse(args); err != nil {
		return &reviewExit{code: 2, err: err}
	}
	paymentIds, err := usagearchive.ParseReviewPaymentIds(*payments)
	if err != nil {
		return &reviewExit{code: 2, err: err}
	}
	conn, err := connectReadOnly(ctx, *pgPath)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	report, err := usagearchive.PlanReview(ctx, conn, paymentIds, *dir, settings, os.Stdout)
	if err != nil {
		return err
	}
	fmt.Printf("REVIEW_PLAN %s\n", report.Json())
	if 0 < len(report.Blockers) {
		return usagearchive.ReviewPlanBlocked(report.Blockers)
	}
	return nil
}

func runReviewExport(ctx context.Context, args []string) error {
	settings := usagearchive.DefaultReviewSettings()
	flags := flag.NewFlagSet("review export", flag.ContinueOnError)
	pgPath := flags.String("pg", "/etc/usagearchive/pg_maintenance.yml", "the PostgreSQL resource")
	payments := flags.String("payments", "", "the payments under attribution review, comma separated; required for a new run, checked against an existing one")
	dir := flags.String("dir", "", "the run's directory")
	mount := flags.String("require-mount", "", "refuse unless this is a mount point of its own and --dir is inside it")
	flags.IntVar(&settings.BatchContracts, "batch-contracts", settings.BatchContracts, "tied contracts per batch, at most")
	flags.Float64Var(&settings.MaxContractsPerSecond, "max-contracts-per-second", settings.MaxContractsPerSecond, "pace; non-positive is unpaced")
	flags.IntVar(&settings.MaxBatches, "max-batches", 0, "batches this run commits at most; 0 is no bound")
	maxSeconds := flags.Int("max-seconds", 0, "no batch starts once this run has used this many seconds; 0 is no bound")
	flags.Uint64Var(&settings.MinFreeBytes, "min-free-bytes", settings.MinFreeBytes, "no batch starts while --dir's filesystem has less free")
	flags.IntVar(&settings.Concurrency, "threads", settings.Concurrency, "zstd threads")
	flags.DurationVar(&settings.StatementTimeout, "statement-timeout", settings.StatementTimeout, "bounds each statement of a batch")
	if err := flags.Parse(args); err != nil {
		return &reviewExit{code: 2, err: err}
	}
	settings.MaxDuration = time.Duration(*maxSeconds) * time.Second
	if *dir == "" {
		return &reviewExit{code: 2, err: errors.New("--dir is required")}
	}
	var paymentIds []string
	if *payments != "" {
		var err error
		if paymentIds, err = usagearchive.ParseReviewPaymentIds(*payments); err != nil {
			return &reviewExit{code: 2, err: err}
		}
	}
	if err := requireMountedDir(*dir, *mount); err != nil {
		return err
	}
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		return err
	}
	unlock, err := lockDir(*dir)
	if err != nil {
		return err
	}
	defer unlock()
	conn, err := connectReadOnly(ctx, *pgPath)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	start := time.Now()
	report, err := usagearchive.ExportReview(ctx, conn, *dir, paymentIds, reviewHost(), settings)
	if report != nil {
		fmt.Printf("REVIEW_EXPORT stop=%s done=%t batches=%d contracts=%d rows=%v total_batches=%d total_contracts=%d last=%s seconds=%.0f\n",
			report.Stop, report.Done, report.Batches, report.Contracts, report.TableRows, report.TotalBatches, report.TotalContracts, report.LastContractId, time.Since(start).Seconds())
	}
	if err != nil {
		return err
	}
	if !report.Done {
		return &reviewExit{code: reviewExitPaused, err: fmt.Errorf("stopped by %s with batches left", report.Stop)}
	}
	return nil
}

func runReviewVerify(ctx context.Context, args []string) error {
	settings := usagearchive.DefaultReviewSettings()
	flags := flag.NewFlagSet("review verify", flag.ContinueOnError)
	pgPath := flags.String("pg", "/etc/usagearchive/pg_maintenance.yml", "the PostgreSQL resource")
	dir := flags.String("dir", "", "the run's directory")
	mount := flags.String("require-mount", "", "refuse unless this is a mount point of its own and --dir is inside it")
	filesOnly := flags.Bool("files-only", false, "read the stored copies back only; no Main read")
	mainRows := flags.Bool("main-rows", false, "also read every batch's rows on Main again and match them")
	flags.DurationVar(&settings.StatementTimeout, "statement-timeout", settings.StatementTimeout, "bounds each statement")
	if err := flags.Parse(args); err != nil {
		return &reviewExit{code: 2, err: err}
	}
	if *dir == "" {
		return &reviewExit{code: 2, err: errors.New("--dir is required")}
	}
	if err := requireMountedDir(*dir, *mount); err != nil {
		return err
	}
	unlock, err := lockDir(*dir)
	if err != nil {
		return err
	}
	defer unlock()
	conn, err := connectReadOnly(ctx, *pgPath)
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	verification, path, err := usagearchive.VerifyReview(ctx, conn, *dir, reviewHost(), *filesOnly, *mainRows, settings)
	if err != nil {
		return err
	}
	fmt.Printf("REVIEW_VERIFY %s %s\n", path, verification.Json())
	if !verification.Ok {
		return &reviewExit{code: 1, err: fmt.Errorf("the verification failed: %v", verification.Failures)}
	}
	return nil
}

func runReviewStatus(args []string) error {
	flags := flag.NewFlagSet("review status", flag.ContinueOnError)
	dir := flags.String("dir", "", "the run's directory")
	if err := flags.Parse(args); err != nil {
		return &reviewExit{code: 2, err: err}
	}
	if *dir == "" {
		return &reviewExit{code: 2, err: errors.New("--dir is required")}
	}
	run, batches, unfinished, err := usagearchive.ReviewStatus(*dir)
	if err != nil {
		return err
	}
	var contracts, fileBytes int64
	rows := map[string]int64{}
	for _, batch := range batches {
		contracts += batch.Contracts
		for _, copied := range batch.Tables {
			rows[copied.Table] += copied.Rows
			fileBytes += copied.FileBytes
		}
	}
	last := ""
	if 0 < len(batches) {
		last = batches[len(batches)-1].UpperContractId
	}
	fmt.Printf("REVIEW_STATUS run=%s payments=%v batches=%d contracts=%d rows=%v file_bytes=%d last=%s unfinished=%d\n",
		run.RunId, run.PaymentIds(), len(batches), contracts, rows, fileBytes, last, len(unfinished))
	return nil
}

func printReviewClearPlan(plan *usagearchive.ReviewClearPlan) {
	fmt.Printf("change: %s\n", plan.Change)
	fmt.Printf("run %s: %d batches, %d tied contracts %v, manifests %s\n", plan.Run.RunId, plan.Batches, plan.Contracts, plan.PaymentContracts, plan.ManifestsSha256)
	fmt.Printf("verified by %s (sha256 %s)\n", filepath.Base(plan.VerificationFile), plan.VerificationSha256)
	for _, payment := range plan.Payments {
		fmt.Printf("payment %s now: %s\n", payment.PaymentId, payment.Row)
	}
	fmt.Printf("REVIEW_PLAN_DIGEST %s\n", plan.Digest)
}

func runReviewClear(ctx context.Context, args []string) error {
	settings := usagearchive.DefaultReviewSettings()
	flags := flag.NewFlagSet("review clear", flag.ContinueOnError)
	pgPath := flags.String("pg", "/etc/usagearchive/pg_maintenance.yml", "the PostgreSQL resource")
	dir := flags.String("dir", "", "the run's directory")
	mount := flags.String("require-mount", "", "refuse unless this is a mount point of its own and --dir is inside it")
	commit := flags.Bool("commit", false, "make the change; without it, only the plan")
	expect := flags.String("expect", "", "the plan's digest, with --commit")
	flags.DurationVar(&settings.MaxVerificationAge, "max-verification-age", settings.MaxVerificationAge, "the newest verification is at most this old")
	flags.DurationVar(&settings.StatementTimeout, "statement-timeout", 25*time.Second, "bounds each statement")
	if err := flags.Parse(args); err != nil {
		return &reviewExit{code: 2, err: err}
	}
	if *dir == "" || (*commit && *expect == "") {
		return &reviewExit{code: 2, err: errors.New("--dir is required, and --commit needs --expect")}
	}
	if err := requireMountedDir(*dir, *mount); err != nil {
		return err
	}
	unlock, err := lockDir(*dir)
	if err != nil {
		return err
	}
	defer unlock()
	var conn *pgxConn
	if *commit {
		conn, err = connect(ctx, *pgPath)
	} else {
		conn, err = connectReadOnly(ctx, *pgPath)
	}
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	plan, record, err := usagearchive.ClearReview(ctx, conn, *dir, reviewHost(), *commit, *expect, settings)
	if plan != nil {
		printReviewClearPlan(plan)
	}
	if err != nil {
		return err
	}
	if record == nil {
		fmt.Printf("dry run: nothing written. To clear: usagearchivectl review clear --dir=%s --commit --expect=%s\n", *dir, plan.Digest)
		return nil
	}
	for i, payment := range record.Payments {
		fmt.Printf("CLEARED payment %s now: %s (journal event %s)\n", payment.PaymentId, payment.Row, record.EventIds[i])
	}
	return nil
}

func runReviewRestoreFlag(ctx context.Context, args []string) error {
	settings := usagearchive.DefaultReviewSettings()
	flags := flag.NewFlagSet("review restore-flag", flag.ContinueOnError)
	pgPath := flags.String("pg", "/etc/usagearchive/pg_maintenance.yml", "the PostgreSQL resource")
	dir := flags.String("dir", "", "the run's directory")
	prior := flags.String("prior", "", "the record the clear wrote before its change (clear-...-before.json)")
	commit := flags.Bool("commit", false, "make the change; without it, only the plan")
	expect := flags.String("expect", "", "the plan's digest, with --commit")
	flags.DurationVar(&settings.StatementTimeout, "statement-timeout", 25*time.Second, "bounds each statement")
	if err := flags.Parse(args); err != nil {
		return &reviewExit{code: 2, err: err}
	}
	if *dir == "" || *prior == "" || (*commit && *expect == "") {
		return &reviewExit{code: 2, err: errors.New("--dir and --prior are required, and --commit needs --expect")}
	}
	unlock, err := lockDir(*dir)
	if err != nil {
		return err
	}
	defer unlock()
	var conn *pgxConn
	if *commit {
		conn, err = connect(ctx, *pgPath)
	} else {
		conn, err = connectReadOnly(ctx, *pgPath)
	}
	if err != nil {
		return err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	plan, record, err := usagearchive.RestoreReviewFlag(ctx, conn, *dir, *prior, reviewHost(), *commit, *expect, settings)
	if plan != nil {
		printReviewClearPlan(plan)
	}
	if err != nil {
		return err
	}
	if record == nil {
		fmt.Printf("dry run: nothing written. To restore: usagearchivectl review restore-flag --dir=%s --prior=%s --commit --expect=%s\n", *dir, *prior, plan.Digest)
		return nil
	}
	for i, payment := range record.Payments {
		fmt.Printf("RESTORED payment %s now: %s (journal event %s)\n", payment.PaymentId, payment.Row, record.EventIds[i])
	}
	return nil
}
