// The compute ledger's operator commands (DESIGN-COMPUTE §9.11, §9.6, §14.10): `sidecarctl compute promo create`
// (BAL-M12-13), and after it `compute adjust` (BAL-M12-14) and `compute search-cap` (BAL-M13-02), each in this file.
//
// `compute promo create <env> --campaign --value --cap --last-day [--paid-lot] [--network] [--code] --by --reason`
// makes one of URnetwork's promotion codes (D-BAL-12): its value, at most $10, credited once to each network that
// redeems it and lapsing 30 days on; its cap on redemptions, a cap above one asking for an unreversed paid lot, so
// --paid-lot is required with it; its last UTC day; and who made it and why. The code is the operator's (--code, 8 to
// 32 letters and digits) or drawn, 16 characters, about 80 bits, and one made for a single network (--network) is
// always drawn and redeems once. The env keeps only a keyed hash of the code, its case folded, under the promotion key
// in its vault (compute-ledger.yml, ComputeServerSecrets): the code is printed this once and stored and logged nowhere.
// The network a code is made for is checked as an id and not kept, since compute_promo_code holds no network (§14.4).
//
// Each command reaches one env, the one WARP_ENV names, through server's Postgres as the env's services do, so it
// refuses an <env> that is not that one before reading or writing anything, and a binary that does not link MG-C1,
// which every release build is until M12's Gn-out registers the group.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	operator "github.com/urnetwork/operator-core"
	"github.com/urnetwork/sidecar"
	sidecarcontroller "github.com/urnetwork/sidecar/controller"
	"github.com/urnetwork/sidecar/model"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/core"
)

// The commands' registration, the only change to the binary they need.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute promo create",
		usage:   "<env> --campaign=<name> --value=<usd> --cap=<redemptions> --last-day=<YYYY-MM-DD> [--paid-lot] [--network=<network id>] [--code=<letters and digits>] --by=<operator> --reason=<text>",
		summary: "Make a promotion code in the env WARP_ENV names: at most $10, credited once to each network that redeems it and lapsing 30 days on (§9.11); the code is printed once and only its keyed hash is kept.",
		run:     runComputePromoCreate,
	})
}

// How a ledger command reaches its env. Tests replace the env, the secrets and the call.
type computeLedgerCommandSettings struct {
	// Bounds the whole run, so an env whose Postgres does not answer fails the runbook step rather than holding it.
	// Non-positive expires at once, so nothing is read or written.
	timeout time.Duration
	// The ledger's settings, the code's bounds among them.
	ledger *model.ComputeLedgerSettings
	// Opens env's Env, refusing an env this process does not run as and a binary without MG-C1's tables.
	openEnv func(ctx context.Context, env string) (operator.Env, error)
	// The env's ledger keys: its vault's.
	secrets func() sidecarcontroller.ComputeLedgerSecrets
	// Makes a code: sidecar/controller's.
	createPromo func(env operator.Env, ctx context.Context, settings *model.ComputeLedgerSettings, secrets sidecarcontroller.ComputeLedgerSecrets, request *sidecarcontroller.ComputePromoCreate) (*sidecarcontroller.ComputePromoCreated, error)
}

// The settings of a run. A caller changes its own copy.
func defaultComputeLedgerCommandSettings() *computeLedgerCommandSettings {
	return &computeLedgerCommandSettings{
		// one row's insert, well under a second on a healthy env, with room for a pool's first connect
		timeout:     30 * time.Second,
		ledger:      model.DefaultComputeLedgerSettings(),
		openEnv:     openServerComputeLedgerEnv,
		secrets:     func() sidecarcontroller.ComputeLedgerSecrets { return controller.NewComputeServerSecrets(nil) },
		createPromo: sidecarcontroller.ComputeCreatePromo,
	}
}

// The env as server reaches it, the one WARP_ENV names: any other env, no WARP_ENV, or a binary that does not link
// MG-C1 is refused before anything is read.
func openServerComputeLedgerEnv(ctx context.Context, env string) (operator.Env, error) {
	processEnv, err := sidecarctlProcessEnv()
	if err != nil {
		return nil, err
	}
	if processEnv != env {
		return nil, fmt.Errorf("this process runs as WARP_ENV=%s and was asked for %s; run it with WARP_ENV=%s", processEnv, env, env)
	}
	if !server.SidecarMgC1Linked() {
		return nil, errors.New("this binary does not link MG-C1, which holds the compute ledger: until M12's Gn-out registers the group, only a sidecar_pending build of a local or canary env has its tables")
	}
	return core.NewEnv(&sidecar.ServiceHost{}, &core.Grants{Db: true})
}

// An env's name: one directory under config/ and vault/, as warp lays them out.
var computeLedgerEnvRegex = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// A dollar amount as an operator writes one: whole dollars and up to nine decimal places, never negative or signed.
var computeLedgerUsdRegex = regexp.MustCompile(`^([0-9]{1,7})(?:\.([0-9]{1,9}))?$`)

// text in nano-USD, exactly, with no float between: "10" is 10^10, "2.50" 2.5 × 10^9.
func parseComputeLedgerUsd(text string) (int64, error) {
	match := computeLedgerUsdRegex.FindStringSubmatch(text)
	if match == nil {
		return 0, fmt.Errorf("%q is no dollar amount, such as 10 or 2.50", text)
	}
	dollars, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, err
	}
	fraction := int64(0)
	if match[2] != "" {
		digits := match[2] + strings.Repeat("0", 9-len(match[2]))
		if fraction, err = strconv.ParseInt(digits, 10, 64); err != nil {
			return 0, err
		}
	}
	return dollars*model.ComputeCreditNanoUsd + fraction, nil
}

// nano-USD as dollars to the cent, for a command's output.
func formatComputeLedgerUsd(nanoUsd int64) string {
	cents := nanoUsd / (model.ComputeCreditNanoUsd / 100)
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// `sidecarctl compute promo create`, against the env as server reaches it.
func runComputePromoCreate(args []string, out io.Writer) error {
	return runComputePromoCreateWithSettings(args, defaultComputeLedgerCommandSettings(), out)
}

// Parses the command line and makes the code, printing it once.
func runComputePromoCreateWithSettings(args []string, settings *computeLedgerCommandSettings, out io.Writer) error {
	flags := flag.NewFlagSet("compute promo create", flag.ContinueOnError)
	campaign := flags.String("campaign", "", "")
	value := flags.String("value", "", "")
	redeemMax := flags.String("cap", "", "")
	lastDayText := flags.String("last-day", "", "")
	paidLot := flags.Bool("paid-lot", false, "")
	networkText := flags.String("network", "", "")
	code := flags.String("code", "", "")
	by := flags.String("by", "", "")
	reason := flags.String("reason", "", "")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if len(positionals) != 1 {
		return sidecarctlUsageErrorf("want the env alone, got %q", positionals)
	}
	envName := positionals[0]
	if !computeLedgerEnvRegex.MatchString(envName) {
		return sidecarctlUsageErrorf("%q is not an env's name, one directory under config/", envName)
	}
	for name, given := range map[string]string{"--campaign": *campaign, "--value": *value, "--cap": *redeemMax, "--last-day": *lastDayText, "--by": *by, "--reason": *reason} {
		if strings.TrimSpace(given) == "" {
			return sidecarctlUsageErrorf("%s is required", name)
		}
	}
	amount, err := parseComputeLedgerUsd(*value)
	if err != nil {
		return sidecarctlUsageErrorf("--value: %v", err)
	}
	redeemMaxCount, err := strconv.ParseInt(*redeemMax, 10, 64)
	if err != nil || redeemMaxCount <= 0 {
		return sidecarctlUsageErrorf("--cap is a positive count of redemptions, not %q", *redeemMax)
	}
	if 1 < redeemMaxCount && !*paidLot {
		return sidecarctlUsageErrorf("a code with a cap above one asks for an unreversed paid lot: give --paid-lot")
	}
	lastDay, err := time.ParseInLocation(time.DateOnly, *lastDayText, time.UTC)
	if err != nil {
		return sidecarctlUsageErrorf("--last-day is a UTC day, YYYY-MM-DD, not %q", *lastDayText)
	}
	forNetwork := *networkText != ""
	if forNetwork {
		if _, err := server.ParseId(*networkText); err != nil {
			return sidecarctlUsageErrorf("--network is a network's id, not %q", *networkText)
		}
		if *code != "" || redeemMaxCount != 1 {
			return sidecarctlUsageErrorf("a code made for a single network is drawn and redeems once: no --code, and --cap=1")
		}
	}
	request := &sidecarcontroller.ComputePromoCreate{
		Campaign:        *campaign,
		AmountNanoUsd:   amount,
		RedeemMaxCount:  redeemMaxCount,
		LastDay:         lastDay,
		PaidLotRequired: *paidLot,
		Code:            *code,
		ForNetwork:      forNetwork,
		CreatedBy:       *by,
		Reason:          *reason,
	}
	ctx, cancel := context.WithTimeout(context.Background(), settings.timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("nothing was written in %s: the run's %v bound passed: %w", envName, settings.timeout, err)
	}
	env, err := settings.openEnv(ctx, envName)
	if err != nil {
		return fmt.Errorf("nothing was written in %s: %w", envName, err)
	}
	created, err := settings.createPromo(env, ctx, settings.ledger, settings.secrets(), request)
	if errors.Is(err, model.ErrComputePromoCodeTaken) {
		return fmt.Errorf("nothing was written in %s: a code of that hash stands; give another --code, or none to draw one", envName)
	}
	if err != nil {
		return fmt.Errorf("nothing was written in %s: %w", envName, err)
	}
	paid := "no"
	if request.PaidLotRequired {
		paid = "yes"
	}
	fmt.Fprintf(out, "code: %s\n", created.Code)
	fmt.Fprintf(out, "code id: %s\n", created.CodeId)
	fmt.Fprintf(out, "campaign: %s; value %s; cap %d; last day %s; paid lot required: %s\n",
		request.Campaign, formatComputeLedgerUsd(request.AmountNanoUsd), request.RedeemMaxCount, lastDay.Format(time.DateOnly), paid)
	if forNetwork {
		fmt.Fprintf(out, "made for network %s, which is not kept with it\n", *networkText)
	}
	fmt.Fprintln(out, "the code is shown this once: the env keeps only its keyed hash")
	return nil
}
