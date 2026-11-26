// `sidecarctl compute promo create` with its env, secrets and ledger call replaced (BAL-M12-13): the command line it
// takes and what it hands the ledger, the code printed once, and each line it refuses before an env is opened.
// cmd_compute_ledger_server_test.go runs it against the private test stack's compute_promo_code.

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	operator "github.com/urnetwork/operator-core"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"
)

// The command over a recording ledger call, opened only for env local.
func computePromoTestCommands(requests *[]*sidecarcontroller.ComputePromoCreate, opened *int, answer error) *sidecarctlCommandSet {
	settings := defaultComputeLedgerCommandSettings()
	settings.openEnv = func(ctx context.Context, env string) (operator.Env, error) {
		*opened += 1
		if env != "local" {
			return nil, errors.New("not this process's env")
		}
		return nil, nil
	}
	settings.secrets = func() sidecarcontroller.ComputeLedgerSecrets { return nil }
	settings.createPromo = func(env operator.Env, ctx context.Context, ledger *model.ComputeLedgerSettings, secrets sidecarcontroller.ComputeLedgerSecrets, request *sidecarcontroller.ComputePromoCreate) (*sidecarcontroller.ComputePromoCreated, error) {
		*requests = append(*requests, request)
		if answer != nil {
			return nil, answer
		}
		return &sidecarcontroller.ComputePromoCreated{CodeId: operator.Id{1}, Code: "WELCOME2026TEST"}, nil
	}
	commands := newSidecarctlCommandSet()
	registered, _ := sidecarctlCommands.find(strings.Fields("compute promo create"))
	command := *registered
	command.run = func(args []string, out io.Writer) error {
		return runComputePromoCreateWithSettings(args, settings, out)
	}
	commands.add(&command)
	return commands
}

// A line of every flag hands the ledger what it says, the value in exact nano-USD, and prints the code once; a code
// for a single network is drawn, with a cap of one.
func TestSidecarctlPromoCreateParses(t *testing.T) {
	requests := []*sidecarcontroller.ComputePromoCreate{}
	opened := 0
	commands := computePromoTestCommands(&requests, &opened, nil)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := commands.run([]string{"compute", "promo", "create", "local", "--campaign=launch", "--value=2.50", "--cap=500", "--last-day=2026-12-31",
		"--paid-lot", "--code=Welcome2026Test", "--by=operator.example", "--reason=launch week"}, stdout, stderr)
	if code != 0 || len(requests) != 1 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	request := requests[0]
	if request.Campaign != "launch" || request.AmountNanoUsd != 2_500_000_000 || request.RedeemMaxCount != 500 || !request.PaidLotRequired ||
		!request.LastDay.Equal(time.Date(2026, 12, 31, 0, 0, 0, 0, time.UTC)) || request.Code != "Welcome2026Test" || request.ForNetwork ||
		request.CreatedBy != "operator.example" || request.Reason != "launch week" {
		t.Fatalf("the request: %+v", request)
	}
	if !strings.Contains(stdout.String(), "code: WELCOME2026TEST\n") || !strings.Contains(stdout.String(), "value $2.50; cap 500") {
		t.Fatalf("the output: %s", stdout.String())
	}
	// a code for a single network: drawn, once
	requests = requests[:0]
	code = commands.run([]string{"compute", "promo", "create", "local", "--campaign=support", "--value=10", "--cap=1", "--last-day=2026-12-31",
		"--network=0190a8c1-0000-7000-8000-000000000001", "--by=operator.example", "--reason=a support case"}, stdout, stderr)
	if code != 0 || len(requests) != 1 || !requests[0].ForNetwork || requests[0].Code != "" || requests[0].AmountNanoUsd != 10_000_000_000 {
		t.Fatalf("a network's code: exit %d, %+v: %s", code, requests, stderr.String())
	}
}

// Each line refused before any env is opened, with the usage exit, 2.
func TestSidecarctlPromoCreateRefusesBadLines(t *testing.T) {
	base := map[string]string{
		"--campaign": "launch", "--value": "5", "--cap": "1", "--last-day": "2026-12-31", "--by": "operator.example", "--reason": "launch week",
	}
	line := func(changes map[string]string, extra ...string) []string {
		args := []string{"compute", "promo", "create", "local"}
		for name, value := range base {
			if changed, ok := changes[name]; ok {
				if changed == "" {
					continue
				}
				value = changed
			}
			args = append(args, name+"="+value)
		}
		return append(args, extra...)
	}
	for label, args := range map[string][]string{
		"no campaign":                     line(map[string]string{"--campaign": ""}),
		"no reason":                       line(map[string]string{"--reason": ""}),
		"no operator":                     line(map[string]string{"--by": ""}),
		"a negative value":                line(map[string]string{"--value": "-5"}),
		"a value in another form":         line(map[string]string{"--value": "5e2"}),
		"ten decimal places":              line(map[string]string{"--value": "1.0000000001"}),
		"no cap":                          line(map[string]string{"--cap": "0"}),
		"a cap above one, no paid lot":    line(map[string]string{"--cap": "2"}),
		"a day that is no day":            line(map[string]string{"--last-day": "2026-13-01"}),
		"a network that is no id":         line(nil, "--network=not-an-id"),
		"a network's code with a code":    line(nil, "--network=0190a8c1-0000-7000-8000-000000000001", "--code=ChosenCode"),
		"a network's code redeemed twice": line(map[string]string{"--cap": "2"}, "--paid-lot", "--network=0190a8c1-0000-7000-8000-000000000001"),
		"no env":                          {"compute", "promo", "create", "--campaign=launch"},
		"an env that is no name":          append([]string{"compute", "promo", "create", "../local"}, line(nil)[4:]...),
	} {
		requests := []*sidecarcontroller.ComputePromoCreate{}
		opened := 0
		commands := computePromoTestCommands(&requests, &opened, nil)
		stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
		if code := commands.run(args, stdout, stderr); code != 2 || opened != 0 || len(requests) != 0 {
			t.Errorf("%s: exit %d, opened %d: %s", label, code, opened, stderr.String())
		}
	}
	// the ledger's own refusal, a value past $10, fails the run with nothing printed of a code
	requests := []*sidecarcontroller.ComputePromoCreate{}
	opened := 0
	commands := computePromoTestCommands(&requests, &opened, sidecarcontroller.ErrComputePromoArgs)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	if code := commands.run(line(map[string]string{"--value": "11"}), stdout, stderr); code != 1 || strings.Contains(stdout.String(), "code:") {
		t.Errorf("past $10: exit %d: %s", code, stdout.String())
	}
	// another env than the process's is refused, nothing made
	requests = requests[:0]
	if code := commands.run(append([]string{"compute", "promo", "create", "production"}, line(nil)[4:]...), stdout, stderr); code != 1 || len(requests) != 0 {
		t.Errorf("another env: exit %d", code)
	}
}

// Dollars read into nano-USD with no float between, and print to the cent.
func TestComputeLedgerUsdParsesExactly(t *testing.T) {
	for text, want := range map[string]int64{"10": 10_000_000_000, "2.5": 2_500_000_000, "0.01": 10_000_000, "0.000000001": 1, "9.999999999": 9_999_999_999} {
		if got, err := parseComputeLedgerUsd(text); err != nil || got != want {
			t.Errorf("%s reads %d, %v", text, got, err)
		}
	}
	if formatComputeLedgerUsd(2_505_000_000) != "$2.50" || formatComputeLedgerUsd(10_000_000_000) != "$10.00" {
		t.Error("the output's dollars")
	}
}
