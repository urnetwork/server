package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl db scrub-network-create-secrets`
// (model/audit_network_create_scrub.go).

// Pure: a dry run unless --apply, and --apply needs the dry run's digest; the
// options other commands share keep their meaning.
func TestDbScrubNetworkCreateSecretsCommandLine(t *testing.T) {
	parse := func(args []string) (docopt.Opts, error) {
		return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, args, "test")
	}
	after, through := server.NewId().String(), server.NewId().String()
	for _, args := range [][]string{
		{"db", "scrub-network-create-secrets"},
		{"db", "scrub-network-create-secrets", "--through=" + through},
		{"db", "scrub-network-create-secrets", "--after=" + after, "--through=" + through, "--batch=1000", "--batch-pause=5s"},
		{"db", "scrub-network-create-secrets", "--through=" + through, "--apply", "--expect=digest"},
	} {
		opts, err := parse(args)
		if err != nil {
			t.Fatal(args, err)
		}
		db, _ := opts.Bool("db")
		scrub, _ := opts.Bool("scrub-network-create-secrets")
		apply, _ := opts.Bool("--apply")
		expect, _ := opts.String("--expect")
		if !db || !scrub || apply != strings.Contains(strings.Join(args, " "), "--apply") || (expect == "digest") != apply {
			t.Fatal("command line", args, opts)
		}
	}
	for _, args := range [][]string{
		{"db", "scrub-network-create-secrets", "--apply"},
		{"db", "scrub-network-create-secrets", "--expect=digest"},
		{"db", "scrub-network-create-secrets", "--pause"},
	} {
		if _, err := parse(args); err == nil {
			t.Fatal("accepted", args)
		}
	}
	// the flags other commands share
	opts, err := parse([]string{"onboarding", "experiments", "--pause"})
	if err != nil {
		t.Fatal(err)
	}
	if pause, _ := opts.Bool("--pause"); !pause {
		t.Fatal("--pause is no longer a flag", opts)
	}
	opts, err = parse([]string{"db", "backfill-sweep-destination-id", "--batch=7"})
	if err != nil {
		t.Fatal(err)
	}
	if batch, err := opts.Int("--batch"); err != nil || batch != 7 {
		t.Fatal("--batch changed", opts)
	}
}

// Pure: the scope and pace, with the defaults, and the command line that runs
// them again. A batch is never larger than the default.
func TestDbScrubNetworkCreateSecretsOptions(t *testing.T) {
	defaults := model.DefaultNetworkCreateAuditScrubSettings()
	options, err := parseDbScrubNetworkCreateSecretsOptions("", "", "", "", false)
	if err != nil || options.after != nil || options.through != nil || *options.settings != *defaults {
		t.Fatal("defaults", options, err)
	}
	if flags := dbScrubNetworkCreateSecretsFlags(options); flags != "" {
		t.Fatal("default flags", flags)
	}

	after, through := server.NewId(), server.NewId()
	options, err = parseDbScrubNetworkCreateSecretsOptions(after.String(), through.String(), "100", "500ms", true)
	if err != nil || *options.after != after || *options.through != through || options.settings.BatchSize != 100 || options.settings.BatchPause != 500*time.Millisecond {
		t.Fatal("parsed", options, err)
	}
	if flags := dbScrubNetworkCreateSecretsFlags(options); flags != " --after="+after.String()+" --through="+through.String()+" --batch=100 --batch-pause=500ms" {
		t.Fatal("flags", flags)
	}

	for _, bad := range []struct {
		after      string
		through    string
		batch      string
		batchPause string
		apply      bool
	}{
		{after: "not-an-id"},
		{through: "not-an-id"},
		// the apply runs the scope the dry run printed
		{apply: true},
		{batch: "0"},
		{batch: "5001"},
		{batch: "many"},
		{batchPause: "-1s"},
		{batchPause: "soon"},
	} {
		if _, err := parseDbScrubNetworkCreateSecretsOptions(bad.after, bad.through, bad.batch, bad.batchPause, bad.apply); err == nil {
			t.Fatal("accepted", bad)
		}
	}
}

// Pure: the census report prints the scope, a count per key, the rows to
// rewrite, the unreadable ids and the digest, and no event details.
func TestDbScrubNetworkCreateSecretsCensusReport(t *testing.T) {
	through := server.NewId()
	unreadable := []server.Id{}
	for range 22 {
		unreadable = append(unreadable, server.NewId())
	}
	census := &model.NetworkCreateAuditScrubCensus{
		Through:        through,
		CandidateCount: 12,
		KeyCounts: map[string]int{
			"network_create.password":                     7,
			"network_create.auth_jwt":                     3,
			"network_create.balance_code":                 1,
			"network_create.wallet_auth.wallet_signature": 2,
			"network_create.wallet_auth.wallet_message":   2,
		},
		EventIds:           []server.Id{server.NewId(), server.NewId()},
		UnreadableEventIds: unreadable,
		Digest:             "synthetic-digest",
	}
	var out bytes.Buffer
	writeDbScrubNetworkCreateSecretsCensus(&out, census)
	want := strings.Join([]string{
		"scope: audit_network_event ids after start through " + through.String(),
		"network_created rows that may hold a key: 12",
		"  rows holding network_create.password: 7",
		"  rows holding network_create.auth_jwt: 3",
		"  rows holding network_create.balance_code: 1",
		"  rows holding network_create.wallet_auth.wallet_signature: 2",
		"  rows holding network_create.wallet_auth.wallet_message: 2",
		"rows to rewrite: 2",
		"unreadable rows, left as they are: 22",
	}, "\n") + "\n"
	if !strings.HasPrefix(out.String(), want) {
		t.Fatalf("the report begins\n%s\nwant\n%s", out.String(), want)
	}
	if !strings.Contains(out.String(), "  "+unreadable[19].String()+"\n  ... and 2 more\ndigest: synthetic-digest\n") || strings.Contains(out.String(), unreadable[20].String()) {
		t.Fatalf("the report lists the unreadable ids as\n%s", out.String())
	}
}
