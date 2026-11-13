package main

import (
	"bytes"
	"slices"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
)

func parsePayoutsCloseAttemptedArgs(args []string) (docopt.Opts, error) {
	return (&docopt.Parser{HelpHandler: docopt.NoHelpHandler}).ParseArgs(bringyourctlUsage, append([]string{"payouts", "close-attempted"}, args...), "test")
}

// `--forfeit` names the payments, with or without the commit pair; the pair
// stays all or none, and without `--forfeit` the run is the Circle pass.
func TestPayoutsCloseAttemptedForfeitArgs(t *testing.T) {
	first, second := server.NewId(), server.NewId()
	opts, err := parsePayoutsCloseAttemptedArgs([]string{"--forfeit=" + first.String() + "," + second.String()})
	if err != nil {
		t.Fatal(err)
	}
	if forfeit, _ := opts.String("--forfeit"); forfeit != first.String()+","+second.String() {
		t.Fatal("forfeit", forfeit)
	}
	if commit, _ := opts.Bool("--commit"); commit {
		t.Fatal("a forfeit without --commit commits")
	}
	opts, err = parsePayoutsCloseAttemptedArgs([]string{"--forfeit=" + first.String(), "--commit", "--expect=synthetic-digest"})
	if err != nil {
		t.Fatal(err)
	}
	if expect, _ := opts.String("--expect"); expect != "synthetic-digest" {
		t.Fatal("expect", expect)
	}
	if _, err := parsePayoutsCloseAttemptedArgs([]string{"--forfeit=" + first.String(), "--commit"}); err == nil {
		t.Fatal("--commit without --expect parsed")
	}
	opts, err = parsePayoutsCloseAttemptedArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := opts["--forfeit"].(string); ok {
		t.Fatal("the Circle pass names a forfeit")
	}
}

// The ids parse in their order. No id, an empty or malformed one and a
// repeated one refuse the whole list.
func TestParsePayoutsForfeitIdsRefusesAnythingButDistinctIds(t *testing.T) {
	first, second := server.NewId(), server.NewId()
	paymentIds, err := parsePayoutsForfeitIds(second.String() + ", " + first.String())
	if err != nil || !slices.Equal(paymentIds, []server.Id{second, first}) {
		t.Fatal(paymentIds, err)
	}
	for _, forfeit := range []string{
		"",
		first.String() + ",",
		first.String() + ",," + second.String(),
		first.String() + ",synthetic-not-an-id",
		first.String() + "," + second.String() + "," + first.String(),
	} {
		if paymentIds, err := parsePayoutsForfeitIds(forfeit); err == nil {
			t.Fatalf("%q parsed as %v", forfeit, paymentIds)
		}
	}
}

// A `--forfeit` present but empty refuses before any read, rather than running
// the Circle pass.
func TestPayoutsCloseAttemptedEmptyForfeitRefuses(t *testing.T) {
	opts, err := parsePayoutsCloseAttemptedArgs([]string{"--forfeit="})
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if exit := payoutsCloseAttemptedCommand(opts, stdout, stderr); exit != 1 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "--forfeit") {
		t.Fatalf("exit %d, stdout %q, stderr %q", exit, stdout.String(), stderr.String())
	}
}
