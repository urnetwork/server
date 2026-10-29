package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// The backfill is a dry run unless --commit names the dry run's digest.
func TestAccountPointsBackfillBlocksUsage(t *testing.T) {
	opts, err := docopt.ParseArgs(bringyourctlUsage, []string{"account-points", "backfill-blocks"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if commit, _ := opts.Bool("--commit"); commit {
		t.Fatal("the default is not a dry run")
	}
	opts, err = docopt.ParseArgs(bringyourctlUsage, []string{"account-points", "backfill-blocks", "--blocks=15,16,17", "--file=awards.json"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if blocks, _ := opts.String("--blocks"); blocks != "15,16,17" {
		t.Fatal("blocks", blocks)
	}
	opts, err = docopt.ParseArgs(bringyourctlUsage, []string{"account-points", "backfill-blocks", "--commit", "--expect=abc"}, "synthetic")
	if err != nil {
		t.Fatal(err)
	}
	if expect, _ := opts.String("--expect"); expect != "abc" {
		t.Fatal("expect", expect)
	}
	parser := &docopt.Parser{HelpHandler: docopt.NoHelpHandler}
	if _, err := parser.ParseArgs(bringyourctlUsage, []string{"account-points", "backfill-blocks", "--commit"}, "synthetic"); err == nil {
		t.Fatal("a commit without the dry run's digest parsed")
	}
	blocks, err := controller.ParseOperatorBlocks("17, 15,16,15")
	if err != nil || len(blocks) != 3 || blocks[0] != 15 || blocks[2] != 17 {
		t.Fatal("parse blocks", blocks, err)
	}
	if _, err := controller.ParseOperatorBlocks("15,x"); err == nil {
		t.Fatal("parsed an invalid block")
	}
}

// The dry run's process is read only before it opens a database session, as
// for close-parked; --commit runs in a writable process.
func TestAccountPointsBackfillDryRunProcessIsReadOnly(t *testing.T) {
	source, err := os.ReadFile("account_points_backfill.go")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	readOnly := strings.Index(text, "if !commit {\n\t\tserver.RequirePgReadOnly()\n\t}")
	session := strings.Index(text, "session.NewLocalClientSession(")
	plan := strings.Index(text, "controller.PlanOperatorBlockPointsBackfill(")
	if readOnly < 0 || session < readOnly || plan < readOnly {
		t.Fatal("the dry run opens a database session before its process is read only")
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(main), "os.Exit(accountPointsBackfillBlocksCommand(opts))") {
		t.Fatal("main does not run the backfill command")
	}
}

// The dry run saves exactly what it printed, under its digest, and a commit
// reads that file; a commit of another digest finds no file and writes nothing.
func TestAccountPointsBackfillDryRunFileIsTheCommitsInput(t *testing.T) {
	dir := t.TempDir()
	backfill := &controller.OperatorBlockPointsBackfill{
		Schema: controller.OperatorBlockPointsBackfillSchema,
		Awards: []*model.OperatorBlockPoints{{Schema: model.OperatorBlockPointsSchema, Block: 16, PaymentPlanId: model.OperatorBlockPointsPlanId(16)}},
	}
	backfill.Digest = model.OperatorBlockPointsDigest(backfill.Awards)
	file := filepath.Join(dir, "awards.json")
	var stdout, stderr bytes.Buffer
	if code := runAccountPointsBackfillDryRun(&stdout, &stderr, file, func() (*controller.OperatorBlockPointsBackfill, error) {
		return backfill, nil
	}); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "--commit --expect="+backfill.Digest) {
		t.Fatal("the dry run does not print the commit of exactly its awards", stdout.String())
	}

	var committed *controller.OperatorBlockPointsBackfill
	commit := func(parsed *controller.OperatorBlockPointsBackfill) ([]int64, []int64, error) {
		committed = parsed
		return []int64{16}, []int64{}, nil
	}
	stdout.Reset()
	if code := runAccountPointsBackfillCommit(&stdout, &stderr, file, backfill.Digest, commit); code != 0 {
		t.Fatal(code, stderr.String())
	}
	if committed == nil || committed.Digest != backfill.Digest || model.OperatorBlockPointsDigest(committed.Awards) != backfill.Digest {
		t.Fatal("the commit did not read the dry run's awards")
	}

	committed = nil
	if code := runAccountPointsBackfillCommit(&stdout, &stderr, "", strings.Repeat("0", 64), commit); code == 0 || committed != nil {
		t.Fatal("a commit of another digest ran")
	}
	refused := func(*controller.OperatorBlockPointsBackfill) ([]int64, []int64, error) {
		return nil, nil, errors.New("refused")
	}
	if code := runAccountPointsBackfillCommit(&stdout, &stderr, file, backfill.Digest, refused); code == 0 {
		t.Fatal("a refused commit exited 0")
	}
}
