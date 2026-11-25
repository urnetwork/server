package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// `bringyourctl db contract-legacy-settle` settles the unsettled escrows the
// contract tables' legacy partition keeps (model/contract_legacy_settlement.go;
// CONTRACT-PARTITION-CUTOVER-NOW.md 6, decision 9). Without --apply it is a
// read-only dry run: its process is read only, it prints each category's
// contracts and amounts, the token of those totals and the command that applies
// exactly that plan. With --apply --expect=<token> it plans again, refuses
// unless that plan's token is the one given, and settles the planned contracts
// at most --max-contracts-per-second. It prints counts and amounts only, never
// a contract, grant or network.

// Runs the command line and returns the exit code.
func dbContractLegacySettleCommand(opts docopt.Opts) int {
	apply, _ := opts.Bool("--apply")
	expect, _ := opts.String("--expect")
	settings, err := contractLegacySettleOptions(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "db contract-legacy-settle: %s\n", err)
		return 1
	}
	if !apply {
		server.RequirePgReadOnly()
	}
	return runDbContractLegacySettle(context.Background(), os.Stdout, os.Stderr, settings, apply, expect)
}

// The settings the options ask for.
func contractLegacySettleOptions(opts docopt.Opts) (settings *model.ContractLegacySettlementSettings, err error) {
	settings = model.DefaultContractLegacySettlementSettings()
	settings.ChargeLiveGrants, _ = opts.Bool("--charge-live-grants")
	if value, _ := opts.String("--min-close-age"); value != "" {
		if settings.MinCloseAge, err = time.ParseDuration(value); err != nil {
			return nil, fmt.Errorf("--min-close-age: %w", err)
		}
	}
	if value, _ := opts.String("--max-contract-id"); value != "" {
		id, err := server.ParseId(value)
		if err != nil {
			return nil, fmt.Errorf("--max-contract-id: %q is not a contract id", value)
		}
		settings.MaxContractId = &id
	}
	if value, _ := opts.String("--max-contracts-per-second"); value != "" {
		if settings.MaxContractsPerSecond, err = strconv.ParseFloat(value, 64); err != nil {
			return nil, fmt.Errorf("--max-contracts-per-second: %w", err)
		}
	}
	if value, _ := opts.String("--max-contracts"); value != "" {
		if settings.MaxContracts, err = strconv.Atoi(value); err != nil {
			return nil, fmt.Errorf("--max-contracts: %w", err)
		}
	}
	if err = settings.Validate(); err != nil {
		return nil, err
	}
	return settings, nil
}

// The flags of the command that plans with the same settings.
func contractLegacySettleFlags(settings *model.ContractLegacySettlementSettings) string {
	defaults := model.DefaultContractLegacySettlementSettings()
	flags := ""
	if settings.MinCloseAge != defaults.MinCloseAge {
		flags += " --min-close-age=" + settings.MinCloseAge.String()
	}
	if settings.MaxContractId != nil {
		flags += " --max-contract-id=" + settings.MaxContractId.String()
	}
	if settings.ChargeLiveGrants {
		flags += " --charge-live-grants"
	}
	if settings.MaxContractsPerSecond != defaults.MaxContractsPerSecond {
		flags += " --max-contracts-per-second=" + strconv.FormatFloat(settings.MaxContractsPerSecond, 'f', -1, 64)
	}
	if settings.MaxContracts != defaults.MaxContracts {
		flags += " --max-contracts=" + strconv.Itoa(settings.MaxContracts)
	}
	return flags
}

// Prints a report's scope and every category's totals.
func printContractLegacySettlementReport(stdout io.Writer, label string, report *model.ContractLegacySettlementReport) {
	bound := "none (the contract tables are not partitioned)"
	if report.MaxContractId != nil {
		bound = report.MaxContractId.String()
	}
	fmt.Fprintf(stdout, "%s: contract ids below %s, closed before %s\n", label, bound, report.CloseBefore.Format(time.RFC3339))
	for _, category := range model.ContractLegacySettlementCategories {
		totals := report.Categories[category]
		if totals.Contracts == 0 {
			continue
		}
		fmt.Fprintf(stdout, "%s %s contracts %d escrow_rows %d escrow_bytes %d charge_bytes %d sweep_bytes %d sweep_usd %.9f provider_networks %d payer_networks %d\n",
			label, category, totals.Contracts, totals.EscrowRows, totals.EscrowByteCount, totals.ChargeByteCount,
			totals.SweepByteCount, model.NanoCentsToUsd(totals.SweepNanoCents), totals.ProviderNetworks, totals.PayerNetworks)
	}
}

// Runs the dry run or the settle and returns the exit code.
func runDbContractLegacySettle(
	ctx context.Context,
	stdout io.Writer,
	stderr io.Writer,
	settings *model.ContractLegacySettlementSettings,
	apply bool,
	expect string,
) int {
	logf := func(format string, args ...any) {
		fmt.Fprintf(stderr, format+"\n", args...)
	}
	if !apply {
		plan, err := model.PlanContractLegacySettlement(ctx, settings, logf)
		if err != nil {
			fmt.Fprintf(stderr, "db contract-legacy-settle: %s\n", err)
			return 1
		}
		printContractLegacySettlementReport(stdout, "plan", plan)
		fmt.Fprintf(stdout, "plan writes %d contracts; token %s\n", plan.WriteContracts(), plan.Token)
		fmt.Fprintf(stdout, "settle with: bringyourctl db contract-legacy-settle%s --apply --expect=%s\n", contractLegacySettleFlags(settings), plan.Token)
		return 0
	}
	plan, settled, err := model.SettleContractLegacyEscrows(ctx, settings, expect, logf)
	if plan != nil {
		printContractLegacySettlementReport(stdout, "plan", plan)
		fmt.Fprintf(stdout, "plan token %s\n", plan.Token)
	}
	if settled != nil {
		printContractLegacySettlementReport(stdout, "settled", settled)
	}
	if err != nil {
		fmt.Fprintf(stderr, "db contract-legacy-settle: %s\n", err)
		return 1
	}
	return 0
}
