package main

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
)

// `bringyourctl reengagement suppress` (controller/reengagement_suppress.go;
// owner, 2026-11-13).

// Pure: one network or an ids file, always a reason; a dry run unless
// --apply, and --apply needs --expect.
func TestReengagementSuppressCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	a := server.NewId().String()
	for _, args := range [][]string{
		{"reengagement", "suppress", "--network_id=" + a, "--reason=sanctions"},
		{"reengagement", "suppress", "--network_ids_file=ids.txt", "--reason=automated_signup"},
		{"reengagement", "suppress", "--network_ids_file=ids.txt", "--reason=automated_signup", "--apply", "--expect=digest"},
		{"reengagement", "suppress", "--network_id=" + a, "--reason=sanctions", "--apply", "--expect=digest"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		reengagement, _ := opts.Bool("reengagement")
		suppress, _ := opts.Bool("suppress")
		apply, _ := opts.Bool("--apply")
		expect, _ := opts.String("--expect")
		reason, _ := opts.String("--reason")
		if !reengagement || !suppress || apply != slices.Contains(args, "--apply") || (expect == "digest") != apply || reason == "" {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"reengagement", "suppress", "--reason=sanctions"},
		{"reengagement", "suppress", "--network_id=" + a},
		{"reengagement", "suppress", "--network_id=" + a, "--network_ids_file=ids.txt", "--reason=sanctions"},
		{"reengagement", "suppress", "--network_id=" + a, "--reason=sanctions", "--apply"},
		{"reengagement", "suppress", "--network_id=" + a, "--reason=sanctions", "--expect=digest"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
}

// Pure: the ids file skips blank lines and comments and trims space; a line
// that is not a network id is refused by its number, never its text; an
// empty or oversized file is refused.
func TestReengagementSuppressNetworkIds(t *testing.T) {
	settings := controller.DefaultMarketingSuppressionSettings()
	a, b := server.NewId(), server.NewId()
	files := map[string]string{
		"ids.txt":     "# the openclaw cohort\n" + a.String() + "\n\n  " + b.String() + "  \r\n" + a.String() + "\n",
		"bad.txt":     a.String() + "\nsomeone@example.com\n",
		"empty.txt":   "# nothing\n\n",
		"comment.txt": "#" + a.String() + "\n",
	}
	readFile := func(path string) ([]byte, error) {
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, errors.New("no such file")
	}
	networkIds, err := readReengagementSuppressNetworkIds(reengagementSuppressArgs{networkIdsFile: "ids.txt"}, settings, readFile)
	if err != nil || !slices.Equal(networkIds, []server.Id{a, b, a}) {
		t.Fatal("ids file", networkIds, err)
	}
	networkIds, err = readReengagementSuppressNetworkIds(reengagementSuppressArgs{networkId: " " + a.String() + " "}, settings, readFile)
	if err != nil || !slices.Equal(networkIds, []server.Id{a}) {
		t.Fatal("--network_id", networkIds, err)
	}
	_, err = readReengagementSuppressNetworkIds(reengagementSuppressArgs{networkIdsFile: "bad.txt"}, settings, readFile)
	if err == nil || !strings.Contains(err.Error(), "line 2") || strings.Contains(err.Error(), "example.com") {
		t.Fatal("a bad line", err)
	}
	if _, err := readReengagementSuppressNetworkIds(reengagementSuppressArgs{networkId: "someone@example.com"}, settings, readFile); err == nil || strings.Contains(err.Error(), "example.com") {
		t.Fatal("a bad --network_id", err)
	}
	for _, path := range []string{"empty.txt", "comment.txt", "missing.txt"} {
		if _, err := readReengagementSuppressNetworkIds(reengagementSuppressArgs{networkIdsFile: path}, settings, readFile); err == nil {
			t.Fatal("accepted", path)
		}
	}
	small := controller.DefaultMarketingSuppressionSettings()
	small.MaxNetworkIds = 1
	if _, err := readReengagementSuppressNetworkIds(reengagementSuppressArgs{networkIdsFile: "ids.txt"}, small, readFile); err == nil {
		t.Fatal("an oversized file accepted")
	}
}

// Pure: the dry run prints the counts, the digest and the command that
// applies exactly it, and never applies; the apply prints what it wrote; a
// refused apply says nothing was written and exits non-zero; an apply that
// stopped prints what it wrote and exits non-zero.
func TestRunReengagementSuppress(t *testing.T) {
	settings := controller.DefaultMarketingSuppressionSettings()
	a := server.NewId()
	readFile := func(path string) ([]byte, error) {
		return []byte(a.String() + "\n"), nil
	}
	counts := controller.MarketingSuppressionCounts{Found: 1, ToSuppress: 1}
	plan := func(networkIds []server.Id) (*controller.MarketingSuppressionPlan, error) {
		return &controller.MarketingSuppressionPlan{Reason: "automated_signup", NetworkIds: networkIds, Counts: counts, Digest: "d1"}, nil
	}
	noApply := func(networkIds []server.Id) (*controller.MarketingSuppressionResult, error) {
		t.Fatal("the dry run applied")
		return nil, nil
	}
	noPlan := func(networkIds []server.Id) (*controller.MarketingSuppressionPlan, error) {
		t.Fatal("the apply planned")
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	args := reengagementSuppressArgs{networkIdsFile: "/tmp/surge ids.txt", reason: "automated_signup"}
	if code := runReengagementSuppress(&stdout, &stderr, args, settings, readFile, plan, noApply); code != 0 {
		t.Fatal("dry run exit", code, stderr.String())
	}
	for _, want := range []string{
		"DRY RUN: nothing was written",
		"found: 1",
		"missing: 0",
		"already suppressed for good: 0",
		"to suppress: 1",
		"digest: d1",
		"to apply exactly this: bringyourctl reengagement suppress --network_ids_file='/tmp/surge ids.txt' --reason=automated_signup --apply --expect=d1",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("dry run output lacks %q:\n%s", want, stdout.String())
		}
	}

	stdout.Reset()
	stderr.Reset()
	args.apply, args.expect = true, "d1"
	applied := func(networkIds []server.Id) (*controller.MarketingSuppressionResult, error) {
		return &controller.MarketingSuppressionResult{Reason: "automated_signup", Requested: 1, Counts: counts, Suppressed: 1, Batches: 1}, nil
	}
	if code := runReengagementSuppress(&stdout, &stderr, args, settings, readFile, noPlan, applied); code != 0 {
		t.Fatal("apply exit", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "APPLY: suppressed 1 networks in 1 batches") ||
		!strings.Contains(stdout.String(), "[audit]reengagement suppress digest=d1 reason=automated_signup requested=1 suppressed=1 already=0 missing=0 batches=1") {
		t.Fatal("apply output", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	refused := func(networkIds []server.Id) (*controller.MarketingSuppressionResult, error) {
		return nil, controller.ErrMarketingSuppressionDigestMismatch
	}
	if code := runReengagementSuppress(&stdout, &stderr, args, settings, readFile, noPlan, refused); code == 0 ||
		!strings.Contains(stdout.String(), "APPLY REFUSED: nothing was written") || !strings.Contains(stderr.String(), "--expect does not match") {
		t.Fatal("refused apply", code, stdout.String(), stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	stopped := func(networkIds []server.Id) (*controller.MarketingSuppressionResult, error) {
		return &controller.MarketingSuppressionResult{Reason: "automated_signup", Requested: 1, Suppressed: 0, Batches: 0}, errors.New("synthetic stop")
	}
	if code := runReengagementSuppress(&stdout, &stderr, args, settings, readFile, noPlan, stopped); code == 0 ||
		!strings.Contains(stdout.String(), "APPLY: suppressed 0 networks in 0 batches") || !strings.Contains(stderr.String(), "synthetic stop") {
		t.Fatal("stopped apply", code, stdout.String(), stderr.String())
	}

	// --network_id prints its own apply command
	stdout.Reset()
	stderr.Reset()
	single := reengagementSuppressArgs{networkId: a.String(), reason: "sanctions"}
	if code := runReengagementSuppress(&stdout, &stderr, single, settings, readFile, plan, noApply); code != 0 ||
		!strings.Contains(stdout.String(), "bringyourctl reengagement suppress --network_id="+a.String()+" --reason=sanctions --apply --expect=d1") {
		t.Fatal("single network dry run", code, stdout.String())
	}
}
