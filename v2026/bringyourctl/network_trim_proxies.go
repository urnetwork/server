package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// `bringyourctl network trim-proxies` (model/network_client_proxy_trim.go;
// owner, 2026-11-05: "Trim networks over the cap").

const networkTrimProxiesDefaultRate = 200

func networkTrimProxiesCommand(opts docopt.Opts) int {
	networkIdsStr, _ := opts.String("--network_ids")
	keepStr, _ := opts.String("--keep")
	rateStr, _ := opts.String("--rate")
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	options, err := parseNetworkTrimProxiesOptions(networkIdsStr, keepStr, rateStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "network trim-proxies: %s\n", err)
		return 1
	}
	// The dry run writes nothing to the database: its process is read only, so
	// PostgreSQL refuses any write on its path.
	if !commit {
		server.RequirePgReadOnly()
	}
	return runNetworkTrimProxies(context.Background(), os.Stdout, os.Stderr, options, commit, expect)
}

func parseNetworkTrimProxiesOptions(networkIdsStr string, keepStr string, rateStr string) (model.NetworkProxyTrimOptions, error) {
	options := model.NetworkProxyTrimOptions{
		Keep:          model.DefaultNetworkProxyTrimKeep(),
		RatePerSecond: networkTrimProxiesDefaultRate,
	}
	for _, part := range strings.Split(networkIdsStr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		networkId, err := server.ParseId(part)
		if err != nil {
			return options, fmt.Errorf("--network_ids: %q: %s", part, err)
		}
		options.NetworkIds = append(options.NetworkIds, networkId)
	}
	if keepStr != "" {
		keep, err := strconv.Atoi(keepStr)
		if err != nil || keep < 0 {
			return options, fmt.Errorf("--keep: %q is not a count", keepStr)
		}
		options.Keep = keep
	}
	if rateStr != "" {
		rate, err := strconv.ParseFloat(rateStr, 64)
		if err != nil || rate <= 0 {
			return options, fmt.Errorf("--rate: %q is not a positive number of removals a second", rateStr)
		}
		options.RatePerSecond = rate
	}
	return options, nil
}

// Runs the dry run or the commit and returns the exit code.
func runNetworkTrimProxies(
	ctx context.Context,
	stdout io.Writer,
	stderr io.Writer,
	options model.NetworkProxyTrimOptions,
	commit bool,
	expect string,
) int {
	lastScanReport := time.Time{}
	options.Progress = func(progress model.NetworkProxyTrimProgress) {
		switch progress.Stage {
		case "scan":
			if time.Since(lastScanReport) < 10*time.Second {
				return
			}
			lastScanReport = time.Now()
			fmt.Fprintf(stderr, "network trim-proxies: scanned %d hosted proxies\n", progress.Scanned)
		case "remove":
			fmt.Fprintf(stdout, "  %s removed %d of %d (network %s)\n", time.Now().UTC().Format("15:04:05Z"), progress.Removed, progress.Total, progress.NetworkId)
		}
	}

	if !commit {
		plan, err := model.PlanNetworkProxyTrim(ctx, options)
		if err != nil {
			fmt.Fprintf(stderr, "%s\n", err)
			return 1
		}
		fmt.Fprintf(stdout, "network trim-proxies DRY RUN: nothing was changed\n")
		writeNetworkTrimProxiesPlan(stdout, plan)
		fmt.Fprintf(stdout, "to commit exactly this: bringyourctl network trim-proxies%s --commit --expect=%s\n", networkTrimProxiesFlags(options), plan.Digest)
		return 0
	}

	start := time.Now()
	result, err := model.CommitNetworkProxyTrim(ctx, options, expect)
	if err != nil {
		if errors.Is(err, model.ErrNetworkProxyTrimDigestMismatch) && result != nil && result.Plan != nil {
			fmt.Fprintf(stdout, "network trim-proxies COMMIT REFUSED: nothing was removed; the networks as they are now:\n")
			writeNetworkTrimProxiesPlan(stdout, result.Plan)
		} else if result != nil {
			fmt.Fprintf(stdout, "network trim-proxies STOPPED after removing %d clients\n", result.RemovedCount)
		}
		fmt.Fprintf(stderr, "%s\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "network trim-proxies COMMIT: removed %d hosted proxies in %s\n", result.RemovedCount, time.Since(start).Round(time.Second))
	for _, network := range result.Plan.Networks {
		if network.Skip != "" {
			continue
		}
		fmt.Fprintf(stdout, "  %s %q: removed %d of %d planned, kept %d\n", network.NetworkId, network.NetworkName, result.RemovedByNetwork[network.NetworkId], len(network.RemoveClientIds), len(network.KeepClientIds))
	}
	fmt.Fprintf(stdout, "[audit]network trim-proxies digest=%s keep=%d removed=%d networks=%d\n", expect, result.Plan.Keep, result.RemovedCount, len(result.RemovedByNetwork))
	return 0
}

func networkTrimProxiesFlags(options model.NetworkProxyTrimOptions) string {
	flags := ""
	if 0 < len(options.NetworkIds) {
		ids := make([]string, 0, len(options.NetworkIds))
		for _, networkId := range options.NetworkIds {
			ids = append(ids, networkId.String())
		}
		flags += " --network_ids=" + strings.Join(ids, ",")
	}
	if options.Keep != model.DefaultNetworkProxyTrimKeep() {
		flags += fmt.Sprintf(" --keep=%d", options.Keep)
	}
	if options.RatePerSecond != networkTrimProxiesDefaultRate {
		flags += " --rate=" + strconv.FormatFloat(options.RatePerSecond, 'f', -1, 64)
	}
	return flags
}

func writeNetworkTrimProxiesPlan(out io.Writer, plan *model.NetworkProxyTrimPlan) {
	if 0 < plan.ScannedProxies {
		fmt.Fprintf(out, "scanned %d hosted proxies\n", plan.ScannedProxies)
	}
	fmt.Fprintf(out, "keep the newest %d active hosted proxies of each free network\n", plan.Keep)
	trimmed := 0
	for _, network := range plan.Networks {
		switch network.Skip {
		case "":
			trimmed += 1
			fmt.Fprintf(out, "  trim %s %q: %d active, keep %d, remove %d\n", network.NetworkId, network.NetworkName, network.ActiveProxies, len(network.KeepClientIds), len(network.RemoveClientIds))
		case model.NetworkProxyTrimSkipPro:
			fmt.Fprintf(out, "  skip %s %q: Pro, %d active (over the Pro cap; never trimmed)\n", network.NetworkId, network.NetworkName, network.ActiveProxies)
		case model.NetworkProxyTrimSkipOverride:
			fmt.Fprintf(out, "  skip %s %q: client allowance %d, %d active (an Embed row; never trimmed)\n", network.NetworkId, network.NetworkName, network.OverrideLimit, network.ActiveProxies)
		}
	}
	fmt.Fprintf(out, "networks to trim: %d; hosted proxies to remove: %d\n", trimmed, plan.RemoveCount)
	fmt.Fprintf(out, "digest: %s\n", plan.Digest)
}
