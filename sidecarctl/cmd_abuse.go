// `sidecarctl abuse <kind>`: the abuse role's client of POST /abuse/action (§21.10, §21.16; ABUSE-M5-12; QD-126,
// QD-896, owner decision 532), one subcommand per kind connect/api/bringyour.yml documents for the route: lookup_case,
// lookup_join, action, lift, review, decide_appeal and clear_restore_flag. Each builds {kind, ...} from its flags, one
// flag per field of the kind's body, posts it to <api>/abuse/action with the staff bearer, and prints the answer's
// result as JSON. There is no web UI (owner rule 6). The route decides what a body asks and refuses what it cannot
// do; this checks only what building a body needs: each required flag given, each id in its 8-4-4-4-12 form, each
// decision one the route takes, and which of review's flags its decision takes. Kinds added later, holds and limit
// rows among them, take a subcommand each in their own task, as `abuse production` and `abuse transparency-report`
// did (cmd_abuse_production.go, whose client helpers these share).
//
// The bearer is read from the file -bearer-file or UR_ABUSE_STAFF_BEARER_FILE names, never from an argument: -bearer
// is refused by name, as is any positional. It is never printed or logged: a refusal prints the route's status and
// fixed message with any copy of the bearer struck out, an answer that holds it is not printed, and a bearer file
// that cannot be read is named by its flag rather than its path, since a bearer typed in place of the path would
// otherwise be printed with it.
//
// The api is -api's URL, or else https://api.<domain> of the env WARP_ENV names: the origin the env's own services
// take for the api (session.DpopApiOrigin), under the domain warpctl runs them with, read from the env's services.yml
// where server's resolver finds its vault (server_env.go). Without -api, WARP_ENV or a domain the command refuses
// rather than guessing an address. The bearer never crosses a network in the clear: -api is https, or http only to
// this machine, as a test's route is.
//
// action, lift and a review that lifts carry the caller's own id for what they record, action_id or lift_id, so that
// a retry records it once (the route answers "done already by the same ids"). When its flag is not given the command
// draws one, and a failure names it, so a retry after a route that did not answer passes the same id back.

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
	"net"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// review's decisions, as the route takes them (controller's abuseStaffReview values).
const (
	abuseStaffReviewUphold    = "uphold"
	abuseStaffReviewLift      = "lift"
	abuseStaffReviewLiftWrong = "lift_wrong"
	abuseStaffReviewNoAction  = "no_action"
)

// What every kind's usage line ends with: the client's own flags.
const abuseStaffKindClientUsage = "[-api=<url>] [-bearer-file=<file>]"

// Tunables of the kinds' commands.
type abuseStaffKindSettings struct {
	// The route client's: abuseStaffClientSettings' defaults, with RequestTimeout 2 minutes, past api's 30 s write
	// bound so that the client never gives up before the route does, and ResponseMaxByteCount 16 MiB, past a case
	// file of the route's 100 rows, reasons and all, many times over.
	ClientSettings *abuseStaffClientSettings
	// The most bytes read of a -reason-file: 64 KiB. The route's own bound on a reason (model.AbuseSettings'
	// ReviewReasonMaxByteCount, 2 KiB) refuses a longer reason; only a file past this is refused here, unread.
	ReasonFileMaxByteCount int64
}

// The defaults, each with its field's reason.
func defaultAbuseStaffKindSettings() *abuseStaffKindSettings {
	clientSettings := defaultAbuseStaffClientSettings()
	clientSettings.RequestTimeout = 2 * time.Minute
	clientSettings.ResponseMaxByteCount = 16 * 1024 * 1024
	return &abuseStaffKindSettings{
		ClientSettings:         clientSettings,
		ReasonFileMaxByteCount: 64 * 1024,
	}
}

// What the kinds' commands run on. A test replaces each.
type abuseStaffKindDeps struct {
	// draws the id a body carries for its retries: server.NewId
	newId func() server.Id
	// the api's origin when -api names none: abuseStaffEnvApiOrigin
	envApiOrigin func() (string, error)
}

// The deps of a run.
func defaultAbuseStaffKindDeps() *abuseStaffKindDeps {
	return &abuseStaffKindDeps{
		newId:        server.NewId,
		envApiOrigin: abuseStaffEnvApiOrigin,
	}
}

// One kind's subcommand.
type abuseStaffKindSpec struct {
	// the body's kind, and the command's word after abuse
	kind string
	// the kind's own flags, which abuseStaffKindClientUsage follows in the usage line
	usage   string
	summary string
	// adds the kind's flags to flags, answering what fills a body from them once they are parsed
	register func(flags *flag.FlagSet) func(body *abuseStaffKindBody)
}

// -reason and -reason-file, of every kind that records a reason.
type abuseStaffReasonFlags struct {
	reason     *string
	reasonFile *string
}

// Adds -reason and -reason-file to flags.
func registerAbuseStaffReasonFlags(flags *flag.FlagSet) *abuseStaffReasonFlags {
	return &abuseStaffReasonFlags{
		reason:     flags.String("reason", "", "the reasons, as text"),
		reasonFile: flags.String("reason-file", "", "a file holding the reasons, in place of -reason"),
	}
}

// The kinds this file builds, in the order bringyour.yml documents them.
func abuseStaffKindSpecs() []*abuseStaffKindSpec {
	reviewDecisions := []string{abuseStaffReviewUphold, abuseStaffReviewLift, abuseStaffReviewLiftWrong, abuseStaffReviewNoAction}
	appealDecisions := []string{string(model.AbuseAppealStateGranted), string(model.AbuseAppealStateDenied)}
	return []*abuseStaffKindSpec{
		{
			kind:    "lookup_case",
			usage:   "-case-id=<case_id>",
			summary: "Read a case by its id, with its actions and appeals (reviewer or legal). The route logs each read before it answers.",
			register: func(flags *flag.FlagSet) func(body *abuseStaffKindBody) {
				caseId := flags.String("case-id", "", "the case's id")
				return func(body *abuseStaffKindBody) {
					body.id("case_id", "case-id", *caseId)
				}
			},
		},
		{
			kind:    "lookup_join",
			usage:   "-join-kind=<host|address|headers|connection|network> -join-key=<key>",
			summary: "Read the cases one join key names, at most the route's row limit and never a list (reviewer or legal). The route logs each read before it answers.",
			register: func(flags *flag.FlagSet) func(body *abuseStaffKindBody) {
				joinKind := flags.String("join-kind", "", "how the key ties a case to a network")
				joinKey := flags.String("join-key", "", "the key")
				return func(body *abuseStaffKindBody) {
					body.text("join_kind", "join-kind", *joinKind)
					body.text("join_key", "join-key", *joinKey)
				}
			},
		},
		{
			kind:    "action",
			usage:   "-case-id=<case_id> -action-kind=<kind> -target=<name> (-reason=<text> | -reason-file=<file>) [-until=<RFC 3339 time>] [-action-id=<id>]",
			summary: "Take a person's action under a case, upheld as it is taken (reviewer); never on a root or a platform name. -until ends it by itself; -action-id, drawn when unset, makes a retry record it once.",
			register: func(flags *flag.FlagSet) func(body *abuseStaffKindBody) {
				caseId := flags.String("case-id", "", "the case's id")
				actionKind := flags.String("action-kind", "", "what the action does, such as host_kill")
				target := flags.String("target", "", "the host, label, sending domain or network it acts on")
				until := flags.String("until", "", "when it ends by itself, RFC 3339; only a lift ends it when unset")
				actionId := flags.String("action-id", "", "the action's own id, drawn when unset")
				reasonFlags := registerAbuseStaffReasonFlags(flags)
				return func(body *abuseStaffKindBody) {
					body.id("case_id", "case-id", *caseId)
					body.text("action_kind", "action-kind", *actionKind)
					body.text("target", "target", *target)
					body.timeMs("until_time_ms", "until", *until)
					body.reason(reasonFlags)
					body.recordId("action_id", "action-id", *actionId)
				}
			},
		},
		{
			kind:    "lift",
			usage:   "-case-id=<case_id> -action-id=<action_id> (-reason=<text> | -reason-file=<file>) [-wrong] [-lift-id=<id>]",
			summary: "Lift an action through its case (reviewer), -wrong when review found it wrong, which takes it out of every count. -lift-id, drawn when unset, makes a retry record the lift once.",
			register: func(flags *flag.FlagSet) func(body *abuseStaffKindBody) {
				caseId := flags.String("case-id", "", "the case's id")
				actionId := flags.String("action-id", "", "the action lifted")
				wrong := flags.Bool("wrong", false, "review found the action wrong")
				liftId := flags.String("lift-id", "", "the lift's own id, drawn when unset")
				reasonFlags := registerAbuseStaffReasonFlags(flags)
				return func(body *abuseStaffKindBody) {
					body.id("case_id", "case-id", *caseId)
					body.id("action_id", "action-id", *actionId)
					body.set("wrong", *wrong)
					body.reason(reasonFlags)
					body.recordId("lift_id", "lift-id", *liftId)
				}
			},
		},
		{
			kind:    "review",
			usage:   "-case-id=<case_id> -decision=<uphold|lift|lift_wrong|no_action> [-action-id=<action_id>] (-reason=<text> | -reason-file=<file>) [-lift-id=<id>]",
			summary: "Review a machine's action, which -action-id names: uphold, lift or lift_wrong it; or no_action, naming none, for a case with no action, which closes it (reviewer). A lift's -lift-id, drawn when unset, makes a retry record it once.",
			register: func(flags *flag.FlagSet) func(body *abuseStaffKindBody) {
				caseId := flags.String("case-id", "", "the case's id")
				decision := flags.String("decision", "", "uphold, lift, lift_wrong or no_action")
				actionId := flags.String("action-id", "", "the action reviewed; none for no_action")
				liftId := flags.String("lift-id", "", "a lift's own id, drawn when unset")
				reasonFlags := registerAbuseStaffReasonFlags(flags)
				return func(body *abuseStaffKindBody) {
					body.id("case_id", "case-id", *caseId)
					body.choice("decision", "decision", *decision, reviewDecisions)
					switch *decision {
					case abuseStaffReviewNoAction:
						body.refuseGiven("action-id", *actionId, "no_action reviews a case with no action, so it names none")
						body.refuseGiven("lift-id", *liftId, "no_action lifts nothing")
					case abuseStaffReviewUphold:
						body.id("action_id", "action-id", *actionId)
						body.refuseGiven("lift-id", *liftId, "an uphold lifts nothing")
					case abuseStaffReviewLift, abuseStaffReviewLiftWrong:
						body.id("action_id", "action-id", *actionId)
					}
					body.reason(reasonFlags)
					if *decision == abuseStaffReviewLift || *decision == abuseStaffReviewLiftWrong {
						body.recordId("lift_id", "lift-id", *liftId)
					}
				}
			},
		},
		{
			kind:    "decide_appeal",
			usage:   "-appeal-id=<appeal_id> -decision=<granted|denied> (-reason=<text> | -reason-file=<file>)",
			summary: "Decide an appeal as its second reviewer, never one who reviewed or took an action of its case (reviewer): granted lifts the case's standing actions as wrong, denied upholds them.",
			register: func(flags *flag.FlagSet) func(body *abuseStaffKindBody) {
				appealId := flags.String("appeal-id", "", "the appeal's id")
				decision := flags.String("decision", "", "granted or denied")
				reasonFlags := registerAbuseStaffReasonFlags(flags)
				return func(body *abuseStaffKindBody) {
					body.id("appeal_id", "appeal-id", *appealId)
					body.choice("decision", "decision", *decision, appealDecisions)
					body.reason(reasonFlags)
				}
			},
		},
		{
			kind:    "clear_restore_flag",
			usage:   "-root=<root> -incident-id=<incident_id> -network-id=<network_id>",
			summary: "Clear one network's restore flags in a dns restore incident (reviewer; §12.6).",
			register: func(flags *flag.FlagSet) func(body *abuseStaffKindBody) {
				root := flags.String("root", "", "the dns root of the incident")
				incidentId := flags.String("incident-id", "", "the restore incident's id")
				networkId := flags.String("network-id", "", "the network whose flags clear")
				return func(body *abuseStaffKindBody) {
					body.text("root", "root", *root)
					body.id("incident_id", "incident-id", *incidentId)
					body.id("network_id", "network-id", *networkId)
				}
			},
		},
	}
}

// The commands' registrations, one per kind. They are the only change to the binary these commands need.
func init() {
	for _, spec := range abuseStaffKindSpecs() {
		registerSidecarctlCommand(&sidecarctlCommand{
			words:   "abuse " + spec.kind,
			usage:   spec.usage + " " + abuseStaffKindClientUsage,
			summary: spec.summary,
			run: func(args []string, out io.Writer) error {
				return runAbuseStaffKind(context.Background(), spec, args, out, defaultAbuseStaffKindSettings(), defaultAbuseStaffKindDeps())
			},
		})
	}
}

// A kind's body as its flags fill it, and the first mistake, after which nothing more is set.
type abuseStaffKindBody struct {
	settings *abuseStaffKindSettings
	deps     *abuseStaffKindDeps
	// the body's fields by name, kind included
	nameValues map[string]any
	// the flag that passes back the id the body carries for its retries, without its dash, and the id; "" for a
	// kind that carries none
	retryFlagName string
	retryId       string
	err           error
}

// A body of kind with nothing else set.
func newAbuseStaffKindBody(kind string, settings *abuseStaffKindSettings, deps *abuseStaffKindDeps) *abuseStaffKindBody {
	return &abuseStaffKindBody{
		settings:   settings,
		deps:       deps,
		nameValues: map[string]any{"kind": kind},
	}
}

// Refuses the body with a usage mistake, unless an earlier mistake stands.
func (self *abuseStaffKindBody) refuse(format string, args ...any) {
	if self.err == nil {
		self.err = sidecarctlUsageErrorf(format, args...)
	}
}

// The id flagName gives, in its 8-4-4-4-12 form, the one the route reads; false, with the body refused, when it
// gives none or another form. The refusal never repeats the text, which may not be an id at all.
func (self *abuseStaffKindBody) parseId(flagName string, text string) (server.Id, bool) {
	if text == "" {
		self.refuse("-%s is required", flagName)
		return server.Id{}, false
	}
	id, err := server.ParseId(text)
	if err != nil || !strings.EqualFold(id.String(), text) {
		self.refuse("-%s must be an id in its 8-4-4-4-12 form", flagName)
		return server.Id{}, false
	}
	return id, true
}

// Sets name to the id flagName gives, which is required.
func (self *abuseStaffKindBody) id(name string, flagName string, text string) {
	if self.err != nil {
		return
	}
	if id, ok := self.parseId(flagName, text); ok {
		self.nameValues[name] = id.String()
	}
}

// Sets name to the id flagName gives, or to one drawn when it gives none: the caller's id for what the kind records,
// which a retry passes back so that it is recorded once.
func (self *abuseStaffKindBody) recordId(name string, flagName string, text string) {
	if self.err != nil {
		return
	}
	var id server.Id
	if text == "" {
		id = self.deps.newId()
	} else {
		parsedId, ok := self.parseId(flagName, text)
		if !ok {
			return
		}
		id = parsedId
	}
	self.nameValues[name] = id.String()
	self.retryFlagName = flagName
	self.retryId = id.String()
}

// Sets name to flagName's text, trimmed, which is required.
func (self *abuseStaffKindBody) text(name string, flagName string, text string) {
	if self.err != nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		self.refuse("-%s is required", flagName)
		return
	}
	self.nameValues[name] = text
}

// Sets name to flagName's text, which must be one of choices.
func (self *abuseStaffKindBody) choice(name string, flagName string, text string, choices []string) {
	if self.err != nil {
		return
	}
	if !slices.Contains(choices, text) {
		self.refuse("-%s must be one of %s", flagName, strings.Join(choices, ", "))
		return
	}
	self.nameValues[name] = text
}

// Sets name to flagName's RFC 3339 time in milliseconds, when it gives one. The route reads 0 as no time, so a time
// at or before 1970 is refused rather than sent as none.
func (self *abuseStaffKindBody) timeMs(name string, flagName string, text string) {
	if self.err != nil || text == "" {
		return
	}
	parsedTime, err := time.Parse(time.RFC3339, text)
	if err != nil || parsedTime.UnixMilli() <= 0 {
		self.refuse("-%s must be an RFC 3339 time after 1970", flagName)
		return
	}
	self.nameValues[name] = parsedTime.UnixMilli()
}

// Sets name to value.
func (self *abuseStaffKindBody) set(name string, value any) {
	if self.err != nil {
		return
	}
	self.nameValues[name] = value
}

// Refuses flagName when it gives anything, for why: a flag the kind takes only with another choice.
func (self *abuseStaffKindBody) refuseGiven(flagName string, text string, why string) {
	if self.err != nil || text == "" {
		return
	}
	self.refuse("-%s is not taken here: %s", flagName, why)
}

// Sets reason from -reason, or from the file -reason-file names, exactly one of which is required. Either is trimmed;
// the route bounds and checks the text.
func (self *abuseStaffKindBody) reason(reasonFlags *abuseStaffReasonFlags) {
	if self.err != nil {
		return
	}
	reason := *reasonFlags.reason
	switch {
	case reason != "" && *reasonFlags.reasonFile != "":
		self.refuse("give -reason or -reason-file, not both")
		return
	case *reasonFlags.reasonFile != "":
		file, err := os.Open(*reasonFlags.reasonFile)
		if err != nil {
			self.err = fmt.Errorf("the reason file: %w", err)
			return
		}
		defer file.Close()
		reasonBytes, err := io.ReadAll(io.LimitReader(file, self.settings.ReasonFileMaxByteCount+1))
		if err != nil {
			self.err = fmt.Errorf("the reason file: %w", err)
			return
		}
		if self.settings.ReasonFileMaxByteCount < int64(len(reasonBytes)) {
			self.refuse("the reason file is past %d bytes", self.settings.ReasonFileMaxByteCount)
			return
		}
		reason = string(reasonBytes)
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		self.refuse("-reason or -reason-file is required, and holds the reasons")
		return
	}
	self.nameValues["reason"] = reason
}

// Parses one kind's arguments into the client's flags and the body, or the usage mistake that refuses them, before
// anything is read from the env or posted.
func parseAbuseStaffKindArgs(spec *abuseStaffKindSpec, args []string, settings *abuseStaffKindSettings, deps *abuseStaffKindDeps) (*abuseStaffClientFlags, *abuseStaffKindBody, error) {
	flags := flag.NewFlagSet("abuse "+spec.kind, flag.ContinueOnError)
	client := &abuseStaffClientFlags{}
	client.register(flags)
	// a bearer typed as an argument is refused as one, rather than as an unknown flag; its value is never read
	flags.String("bearer", "", "refused: the bearer is read from its file")
	fill := spec.register(flags)
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return nil, nil, err
	}
	bearerGiven := false
	flags.Visit(func(visited *flag.Flag) {
		if visited.Name == "bearer" {
			bearerGiven = true
		}
	})
	if bearerGiven {
		return nil, nil, sidecarctlUsageErrorf("a bearer is never an argument: name its file with -bearer-file or %s", abuseStaffBearerFileEnv)
	}
	if 0 < len(positionals) {
		// never repeated, since one may be a bearer
		return nil, nil, sidecarctlUsageErrorf("takes no positional arguments")
	}
	body := newAbuseStaffKindBody(spec.kind, settings, deps)
	fill(body)
	if body.err != nil {
		return nil, nil, body.err
	}
	return client, body, nil
}

// The route's URL: <api>/abuse/action, the api from -api, or else from the env WARP_ENV names. A URL that would carry
// the bearer across a network in the clear is refused: http only to this machine.
func abuseStaffKindRouteUrl(apiUrl string, deps *abuseStaffKindDeps) (string, error) {
	if apiUrl == "" {
		envApiOrigin, err := deps.envApiOrigin()
		if err != nil {
			return "", err
		}
		apiUrl = envApiOrigin
	}
	base, err := url.Parse(strings.TrimSpace(apiUrl))
	if err != nil || base.Host == "" || base.Opaque != "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return "", sidecarctlUsageErrorf("-api must be the api's absolute https URL")
	}
	switch base.Scheme {
	case "https":
	case "http":
		hostIp := net.ParseIP(base.Hostname())
		if base.Hostname() != "localhost" && (hostIp == nil || !hostIp.IsLoopback()) {
			return "", sidecarctlUsageErrorf("-api is http to another machine, which would carry the bearer in the clear; give its https URL")
		}
	default:
		return "", sidecarctlUsageErrorf("-api must be the api's absolute https URL")
	}
	return strings.TrimRight(base.String(), "/") + "/abuse/action", nil
}

// The api of the env WARP_ENV names, the file header's: https://api.<domain>, the domain from the env's services.yml
// in the vault server's resolver reads. Refused, never guessed, without WARP_ENV or a domain.
func abuseStaffEnvApiOrigin() (string, error) {
	if _, err := sidecarctlProcessEnv(); err != nil {
		return "", sidecarctlUsageErrorf("name the api with -api, or set WARP_ENV to the env whose api it is")
	}
	services := &struct {
		Domain string `yaml:"domain"`
	}{}
	servicesResource, err := server.Vault.SimpleResource("services.yml")
	if err != nil {
		return "", fmt.Errorf("no -api names the api, and the env's services.yml, which gives its domain, cannot be found: %w", err)
	}
	if err := servicesResource.UnmarshalYamlE(services); err != nil {
		// the file holds the env's addresses, and a parse error can quote it, so only the failure is told
		return "", errors.New("no -api names the api, and the env's services.yml, which gives its domain, does not read as yaml")
	}
	domain := strings.TrimSpace(services.Domain)
	domainValid := domain != "" && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".") && !strings.Contains(domain, "..")
	for _, domainRune := range domain {
		if !('a' <= domainRune && domainRune <= 'z' || 'A' <= domainRune && domainRune <= 'Z' || '0' <= domainRune && domainRune <= '9' || domainRune == '-' || domainRune == '.') {
			domainValid = false
		}
	}
	if !domainValid {
		return "", errors.New("no -api names the api, and the env's services.yml gives no domain to find it under")
	}
	return "https://api." + domain, nil
}

// Runs one kind's command line, the file header's: parses it, posts its body with the bearer, and prints the answer's
// result as indented JSON to out.
func runAbuseStaffKind(ctx context.Context, spec *abuseStaffKindSpec, args []string, out io.Writer, settings *abuseStaffKindSettings, deps *abuseStaffKindDeps) error {
	client, body, err := parseAbuseStaffKindArgs(spec, args, settings, deps)
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
			return fmt.Errorf("the bearer file -bearer-file or %s names cannot be read: %w", abuseStaffBearerFileEnv, pathErr.Err)
		}
		return err
	}
	result, err := postAbuseStaffAction(ctx, routeUrl, bearer, body.nameValues, settings.ClientSettings)
	if err != nil {
		// the route's message is fixed, yet a route that echoed the bearer would print it here
		message := strings.ReplaceAll(err.Error(), bearer, "<bearer>")
		if body.retryFlagName != "" {
			message += fmt.Sprintf(" (a retry passing -%s=%s records it at most once)", body.retryFlagName, body.retryId)
		}
		return errors.New(message)
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
