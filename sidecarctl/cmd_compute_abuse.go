// `sidecarctl compute suspend` and `sidecarctl compute resume` (DESIGN-COMPUTE §17.2, D-CABUSE-1, D-CABUSE-2;
// CARCH-M12-01): an operator's fast suspension of a network's compute, and its end, as the abuse role's client of POST
// /abuse/action's kinds compute_suspend and compute_resume (server/controller/abuse_compute_hold_controller.go), the
// staff route `sidecarctl abuse` posts (cmd_abuse.go, whose client these share).
//
//	compute suspend <network> --reason=<class>
//	    holds the network's compute for one class (mining, scanning, spam, fraud, provider_complaint,
//	    harmful_content, terms, takedown): every broker admission, remote calls, searches and enclave creates and
//	    starts, answers it `suspended`, and its enclaves stop; it keeps its balance and its records
//	compute resume <network>
//	    ends the hold; nothing restarts on its own, its enclaves waiting for their keepers or sponsors
//
// Each prints the route's answer as JSON: the hold as it stands, whether this changed it, whether every broker reads it
// at once (`projected`) and what the enclaves' stop came to. Both are safe to repeat: a network held for that class,
// or not held, is answered as it stands. `resume --search --review=<reference>` is how question
// compute-plan-search-suspension-clear's answer (QD-1374) clears a network's web search suspension once reviewed; it
// waits for SRCH's compute_search_flag (SRCH-M13-08), so this build refuses it and sends nothing.
//
// The bearer follows cmd_abuse.go's rules: it is read from the file -bearer-file or UR_ABUSE_STAFF_BEARER_FILE names,
// never an argument, -bearer refused by name, and it is never printed. The one positional is the network's id in its
// 8-4-4-4-12 form, and anything else there is refused without being repeated, since it may be a bearer typed in the
// wrong place. The api is -api's URL, or https://api.<domain> of the env WARP_ENV names, as for `sidecarctl abuse`.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/controller"

	sidecarmodel "github.com/urnetwork/sidecar/model"
)

// What both commands' usage lines end with: the client's own flags.
const computeAbuseClientUsage = "[--api=<url>] [--bearer-file=<file>]"

// The commands' registrations, the only change to the binary they need.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute suspend",
		usage:   "<network id> --reason=<" + strings.Join(sidecarmodel.ComputeAbuseHoldReasonWords(), "|") + "> " + computeAbuseClientUsage,
		summary: "Suspend a network's compute (§17.2): every remote call, search and enclave create and start refused, its enclaves stopped, its balance and records kept; through POST /abuse/action's compute_suspend, as a reviewer.",
		run: func(args []string, out io.Writer) error {
			return runComputeAbuseCommand(context.Background(), controller.AbuseStaffKindComputeSuspend, args, out, defaultAbuseStaffKindSettings(), defaultAbuseStaffKindDeps())
		},
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "compute resume",
		usage:   "<network id> [--search --review=<reference>] " + computeAbuseClientUsage,
		summary: "End a network's compute suspension; nothing restarts on its own (§17.2). --search, a web search suspension's clear, waits for SRCH-M13-08 and is refused.",
		run: func(args []string, out io.Writer) error {
			return runComputeAbuseCommand(context.Background(), controller.AbuseStaffKindComputeResume, args, out, defaultAbuseStaffKindSettings(), defaultAbuseStaffKindDeps())
		},
	})
}

// Parses one command's arguments into the client's flags and the route's body, or the usage mistake that refuses them,
// before anything is read from the env or posted.
func parseComputeAbuseArgs(kind string, args []string) (*abuseStaffClientFlags, map[string]any, error) {
	flags := flag.NewFlagSet(kind, flag.ContinueOnError)
	client := &abuseStaffClientFlags{}
	client.register(flags)
	// a bearer typed as an argument is refused as one, rather than as an unknown flag; its value is never read
	flags.String("bearer", "", "refused: the bearer is read from its file")
	reason := flags.String("reason", "", "the class the hold is for")
	search := flags.Bool("search", false, "clear the network's web search suspension after review (refused in this build)")
	review := flags.String("review", "", "the review's reference, with --search")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return nil, nil, err
	}
	given := map[string]bool{}
	flags.Visit(func(visited *flag.Flag) {
		given[visited.Name] = true
	})
	if given["bearer"] {
		return nil, nil, sidecarctlUsageErrorf("a bearer is never an argument: name its file with --bearer-file or %s", abuseStaffBearerFileEnv)
	}
	if len(positionals) != 1 {
		// never repeated, since one may be a bearer
		return nil, nil, sidecarctlUsageErrorf("takes one positional argument, the network's id")
	}
	networkId, err := server.ParseId(positionals[0])
	if err != nil || !strings.EqualFold(networkId.String(), positionals[0]) || networkId == (server.Id{}) {
		return nil, nil, sidecarctlUsageErrorf("the network must be its id in the 8-4-4-4-12 form")
	}
	body := map[string]any{"kind": kind, "network_id": networkId.String()}
	switch kind {
	case controller.AbuseStaffKindComputeSuspend:
		if given["search"] || given["review"] {
			return nil, nil, sidecarctlUsageErrorf("--search and --review are resume's")
		}
		if _, err := sidecarmodel.ParseComputeAbuseHoldReason(*reason); err != nil {
			return nil, nil, sidecarctlUsageErrorf("--reason must be one of %s", strings.Join(sidecarmodel.ComputeAbuseHoldReasonWords(), ", "))
		}
		body["reason"] = *reason
	case controller.AbuseStaffKindComputeResume:
		if given["reason"] {
			return nil, nil, sidecarctlUsageErrorf("--reason is suspend's: a resume ends the hold whatever it was for")
		}
		if given["review"] && !*search {
			return nil, nil, sidecarctlUsageErrorf("--review goes with --search")
		}
		if *search && strings.TrimSpace(*review) == "" {
			return nil, nil, sidecarctlUsageErrorf("--search takes --review=<reference>, the review that cleared the network")
		}
		if *search {
			// QD-1374's clear writes compute_search_flag's review fields, which SRCH-M13-08 builds; until then this
			// refuses, so no operator believes a network's search was cleared
			return nil, nil, errors.New("clearing a web search suspension (--search, QD-1374) waits for SRCH-M13-08's compute_search_flag: nothing was sent")
		}
	default:
		return nil, nil, fmt.Errorf("no compute abuse command is %q", kind)
	}
	return client, body, nil
}

// Runs one command line (see the file header): parses it, posts its body with the bearer, and prints the answer's
// result as indented JSON to out.
func runComputeAbuseCommand(ctx context.Context, kind string, args []string, out io.Writer, settings *abuseStaffKindSettings, deps *abuseStaffKindDeps) error {
	client, body, err := parseComputeAbuseArgs(kind, args)
	if err != nil {
		return err
	}
	routeUrl, err := abuseStaffKindRouteUrl(client.apiUrl, deps)
	if err != nil {
		return err
	}
	bearer, err := client.bearer(settings.ClientSettings)
	if err != nil {
		// a file that cannot be read is named by its flag, since a bearer typed in place of its path would be printed
		pathErr := &fs.PathError{}
		if errors.As(err, &pathErr) {
			return fmt.Errorf("the bearer file --bearer-file or %s names cannot be read: %w", abuseStaffBearerFileEnv, pathErr.Err)
		}
		return err
	}
	result, err := postAbuseStaffAction(ctx, routeUrl, bearer, body, settings.ClientSettings)
	if err != nil {
		// the route's message is fixed, yet a route that echoed the bearer would print it here; a retry with the same
		// arguments changes nothing twice
		return errors.New(strings.ReplaceAll(err.Error(), bearer, "<bearer>") + " (the same command again acts at most once)")
	}
	if bytes.Contains(result, []byte(bearer)) {
		return errors.New("the route's answer holds the bearer, so it is not printed")
	}
	indented := &bytes.Buffer{}
	if err := json.Indent(indented, result, "", "  "); err != nil {
		return err
	}
	indented.WriteString("\n")
	_, err = out.Write(indented.Bytes())
	return err
}
