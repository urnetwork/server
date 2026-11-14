package main

// `bringyourctl health`: the public network health status that GET /health
// reports and ur.io shows in its status pill and, while the network is
// degraded, in a banner on every page (model/network_health_model.go).
//
// The reason is public text, shown verbatim to everyone who opens the site.
// It is checked before it is stored: no host, tool, person, address, file,
// identifier, credential or amount. Write it for the people reading the site,
// as what they may notice, not what broke inside.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/docopt/docopt-go"

	"github.com/urnetwork/server/v2026/model"
)

// healthCommand runs one health subcommand and returns the exit code.
func healthCommand(opts docopt.Opts, out io.Writer) int {
	ctx := context.Background()
	now := time.Now()

	if degrade, _ := opts.Bool("degrade"); degrade {
		level := model.NetworkHealthDegraded
		if text, _ := opts.String("--level"); text != "" {
			parsed, err := model.ParseNetworkHealthLevel(text)
			if err != nil {
				fmt.Fprintf(out, "invalid --level: %s\n", err)
				return 1
			}
			level = parsed
		}
		reason, _ := opts.String("--reason")
		if err := model.ValidateNetworkHealthReason(reason); err != nil {
			fmt.Fprintf(out, "invalid --reason: %s\n", err)
			return 1
		}
		record, err := model.SetNetworkHealth(ctx, level, reason, model.NetworkHealthSourceOperator)
		if err != nil {
			fmt.Fprintf(out, "the status was not set: %s\n", err)
			return 1
		}
		printNetworkHealth(out, record, now)
		return 0
	}

	if recover_, _ := opts.Bool("recover"); recover_ {
		previous, cleared, err := model.ClearNetworkHealth(ctx)
		if err != nil {
			fmt.Fprintf(out, "the status was not cleared: %s\n", err)
			return 1
		}
		switch {
		case !cleared:
			fmt.Fprintf(out, "healthy (no status was set)\n")
		case previous == nil:
			fmt.Fprintf(out, "healthy (cleared a malformed status)\n")
		default:
			fmt.Fprintf(out, "healthy (was %s for %s)\n", previous.Status, formatHealthAge(now.Sub(previous.Since)))
		}
		printNetworkHealthPublic(out, model.NetworkHealthResultOf(nil))
		return 0
	}

	record, err := model.ReadNetworkHealth(ctx)
	if errors.Is(err, model.ErrNetworkHealthMalformed) {
		fmt.Fprintf(out, "unknown: %s\n", err)
		fmt.Fprintf(out, "GET /health reports unknown until the status is set again or cleared with `bringyourctl health recover`.\n")
		return 1
	}
	if err != nil {
		fmt.Fprintf(out, "the status could not be read: %s\n", err)
		return 1
	}
	if record == nil {
		fmt.Fprintf(out, "healthy\n")
		printNetworkHealthPublic(out, model.NetworkHealthResultOf(nil))
		return 0
	}
	printNetworkHealth(out, record, now)
	return 0
}

func printNetworkHealth(out io.Writer, record *model.NetworkHealthRecord, now time.Time) {
	fmt.Fprintf(
		out,
		"%s since %s (%s), updated %s, set by %s\n",
		record.Status,
		record.Since.UTC().Format(time.RFC3339),
		formatHealthAge(now.Sub(record.Since)),
		record.Updated.UTC().Format(time.RFC3339),
		record.Source,
	)
	fmt.Fprintf(out, "reason: %s\n", record.Reason)
	printNetworkHealthPublic(out, model.NetworkHealthResultOf(record))
}

// What the site will read: each API process rereads the status within 10
// seconds, and a browser may keep the previous answer for 30.
func printNetworkHealthPublic(out io.Writer, result *model.NetworkHealthResult) {
	public, err := json.Marshal(result)
	if err != nil {
		return
	}
	fmt.Fprintf(out, "GET /health within 40 seconds: %s\n", public)
}

// An age to the minute, as 1h12m.
func formatHealthAge(age time.Duration) string {
	if age < time.Minute {
		return "under a minute"
	}
	return strings.TrimSuffix(age.Round(time.Minute).String(), "0s")
}
