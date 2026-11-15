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
// owner, 2026-11-05: "Trim networks over the cap"). --include_pro,
// --keep_described and --keep_used_since: owner, 2026-11-14, support item 8327.

const networkTrimProxiesDefaultRate = 200

func networkTrimProxiesCommand(opts docopt.Opts) int {
	networkIdsStr, _ := opts.String("--network_ids")
	keepStr, _ := opts.String("--keep")
	rateStr, _ := opts.String("--rate")
	includePro, _ := opts.Bool("--include_pro")
	keepDescribed, _ := opts.Bool("--keep_described")
	keepUsedSinceStr, _ := opts.String("--keep_used_since")
	list, _ := opts.Bool("--list")
	commit, _ := opts.Bool("--commit")
	expect, _ := opts.String("--expect")
	options, err := parseNetworkTrimProxiesOptions(networkIdsStr, keepStr, rateStr)
	if err == nil {
		err = parseNetworkTrimProxiesKeepOptions(&options, includePro, keepDescribed, keepUsedSinceStr)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "network trim-proxies: %s\n", err)
		return 1
	}
	// The dry run writes nothing to the database: its process is read only, so
	// PostgreSQL refuses any write on its path.
	if !commit {
		server.RequirePgReadOnly()
	}
	return runNetworkTrimProxies(context.Background(), os.Stdout, os.Stderr, options, list, commit, expect)
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

// The Pro opt-in and the keep rules beyond the newest. --include_pro needs the
// networks named: a trim never takes a Pro network the operator did not name.
// --keep_used_since is an RFC 3339 time, so the commit plans against the same
// bound the dry run printed.
func parseNetworkTrimProxiesKeepOptions(options *model.NetworkProxyTrimOptions, includePro bool, keepDescribed bool, keepUsedSinceStr string) error {
	if includePro && len(options.NetworkIds) == 0 {
		return errors.New("--include_pro needs --network_ids: it trims only the Pro networks named there")
	}
	options.IncludePro = includePro
	options.KeepDescribed = keepDescribed
	if keepUsedSinceStr != "" {
		since, err := time.Parse(time.RFC3339, keepUsedSinceStr)
		if err != nil {
			return fmt.Errorf("--keep_used_since: %q is not an RFC 3339 time", keepUsedSinceStr)
		}
		since = since.UTC()
		options.KeepUsedSince = &since
	}
	return nil
}

// Runs the dry run or the commit and returns the exit code. `list` also prints
// each kept and removed proxy of a trimmed network.
func runNetworkTrimProxies(
	ctx context.Context,
	stdout io.Writer,
	stderr io.Writer,
	options model.NetworkProxyTrimOptions,
	list bool,
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
		writeNetworkTrimProxiesPlan(stdout, plan, list)
		fmt.Fprintf(stdout, "to commit exactly this: bringyourctl network trim-proxies%s --commit --expect=%s\n", networkTrimProxiesFlags(options), plan.Digest)
		return 0
	}

	start := time.Now()
	result, err := model.CommitNetworkProxyTrim(ctx, options, expect)
	if err != nil {
		if errors.Is(err, model.ErrNetworkProxyTrimDigestMismatch) && result != nil && result.Plan != nil {
			fmt.Fprintf(stdout, "network trim-proxies COMMIT REFUSED: nothing was removed; the networks as they are now:\n")
			writeNetworkTrimProxiesPlan(stdout, result.Plan, list)
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
	fmt.Fprintf(stdout, "[audit]network trim-proxies digest=%s keep=%d removed=%d networks=%d%s\n", expect, result.Plan.Keep, result.RemovedCount, len(result.RemovedByNetwork), networkTrimProxiesAuditKeepOptions(result.Plan))
	return 0
}

// The audit line's fields for the options added on 2026-11-14, only those set.
func networkTrimProxiesAuditKeepOptions(plan *model.NetworkProxyTrimPlan) string {
	fields := ""
	if plan.IncludePro {
		fields += " include_pro=true"
	}
	if plan.KeepDescribed {
		fields += " keep_described=true"
	}
	if plan.KeepUsedSince != nil {
		fields += " keep_used_since=" + plan.KeepUsedSince.UTC().Format(time.RFC3339Nano)
	}
	return fields
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
	if options.IncludePro {
		flags += " --include_pro"
	}
	if options.Keep != model.DefaultNetworkProxyTrimKeep() {
		flags += fmt.Sprintf(" --keep=%d", options.Keep)
	}
	if options.KeepDescribed {
		flags += " --keep_described"
	}
	if options.KeepUsedSince != nil {
		flags += " --keep_used_since=" + options.KeepUsedSince.UTC().Format(time.RFC3339Nano)
	}
	if options.RatePerSecond != networkTrimProxiesDefaultRate {
		flags += " --rate=" + strconv.FormatFloat(options.RatePerSecond, 'f', -1, 64)
	}
	return flags
}

func writeNetworkTrimProxiesPlan(out io.Writer, plan *model.NetworkProxyTrimPlan, list bool) {
	if 0 < plan.ScannedProxies {
		fmt.Fprintf(out, "scanned %d hosted proxies\n", plan.ScannedProxies)
	}
	if plan.IncludePro {
		fmt.Fprintf(out, "keep the newest %d active hosted proxies of each free network and of each Pro network named in --network_ids\n", plan.Keep)
	} else {
		fmt.Fprintf(out, "keep the newest %d active hosted proxies of each free network\n", plan.Keep)
	}
	if plan.KeepDescribed {
		fmt.Fprintf(out, "also keep every hosted proxy with a description\n")
	}
	if plan.KeepUsedSince != nil {
		fmt.Fprintf(out, "also keep every hosted proxy used since %s (its last contract or auth)\n", networkTrimProxiesTime(*plan.KeepUsedSince))
	}
	keepRules := plan.KeepDescribed || plan.KeepUsedSince != nil
	trimmed := 0
	for _, network := range plan.Networks {
		switch network.Skip {
		case "":
			trimmed += 1
			pro := ""
			if network.Pro {
				pro = " (Pro, named with --include_pro)"
			}
			fmt.Fprintf(out, "  trim %s %q%s: %d active, keep %d, remove %d\n", network.NetworkId, network.NetworkName, pro, network.ActiveProxies, len(network.KeepClientIds), len(network.RemoveClientIds))
			if keepRules || network.Pro {
				writeNetworkTrimProxiesSummary(out, network)
			}
			if list {
				for _, proxy := range network.KeepProxies {
					writeNetworkTrimProxiesProxy(out, "keep", proxy)
				}
				for _, proxy := range network.RemoveProxies {
					writeNetworkTrimProxiesProxy(out, "remove", proxy)
				}
			}
		case model.NetworkProxyTrimSkipPro:
			fmt.Fprintf(out, "  skip %s %q: Pro, %d active (over the Pro cap; trimmed only when named with --include_pro)\n", network.NetworkId, network.NetworkName, network.ActiveProxies)
		case model.NetworkProxyTrimSkipOverride:
			fmt.Fprintf(out, "  skip %s %q: client allowance %d, %d active (an Embed row; never trimmed)\n", network.NetworkId, network.NetworkName, network.OverrideLimit, network.ActiveProxies)
		}
	}
	fmt.Fprintf(out, "networks to trim: %d; hosted proxies to remove: %d\n", trimmed, plan.RemoveCount)
	fmt.Fprintf(out, "digest: %s\n", plan.Digest)
}

// Why the trimmed network keeps what it keeps, and what the removed proxies
// have in common: when they were created and last used, and how many have a
// description.
func writeNetworkTrimProxiesSummary(out io.Writer, network *model.NetworkProxyTrimNetwork) {
	reasons := map[string]int{}
	for _, proxy := range network.KeepProxies {
		reasons[proxy.KeepReason] += 1
	}
	fmt.Fprintf(
		out,
		"    keep %d: %d newest, %d described, %d used\n",
		len(network.KeepProxies),
		reasons[model.NetworkProxyTrimKeepNewest],
		reasons[model.NetworkProxyTrimKeepDescribed],
		reasons[model.NetworkProxyTrimKeepUsed],
	)
	if len(network.RemoveProxies) == 0 {
		return
	}
	first := network.RemoveProxies[0]
	minCreate, maxCreate := first.CreateTime, first.CreateTime
	minUse, maxUse := first.LastUseTime(), first.LastUseTime()
	described := 0
	for _, proxy := range network.RemoveProxies {
		if proxy.CreateTime.Before(minCreate) {
			minCreate = proxy.CreateTime
		}
		if maxCreate.Before(proxy.CreateTime) {
			maxCreate = proxy.CreateTime
		}
		if lastUse := proxy.LastUseTime(); lastUse.Before(minUse) {
			minUse = lastUse
		} else if maxUse.Before(lastUse) {
			maxUse = lastUse
		}
		if proxy.Description != "" {
			described += 1
		}
	}
	fmt.Fprintf(
		out,
		"    remove %d: created %s to %s, last used %s to %s, %d described\n",
		len(network.RemoveProxies),
		networkTrimProxiesTime(minCreate),
		networkTrimProxiesTime(maxCreate),
		networkTrimProxiesTime(minUse),
		networkTrimProxiesTime(maxUse),
		described,
	)
}

func writeNetworkTrimProxiesProxy(out io.Writer, action string, proxy *model.NetworkProxyTrimProxy) {
	reason := proxy.KeepReason
	if reason == "" {
		reason = "-"
	}
	fmt.Fprintf(
		out,
		"    %s %s %s created %s last used %s description %q\n",
		action,
		proxy.ClientId,
		reason,
		networkTrimProxiesTime(proxy.CreateTime),
		networkTrimProxiesTime(proxy.LastUseTime()),
		proxy.Description,
	)
}

func networkTrimProxiesTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}
