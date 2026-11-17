// `sidecarctl abuse production` and `sidecarctl abuse transparency-report`: the legal role's client for §21.9's
// productions and §21.11's counts (ABUSE-M5-17; QD-126, QD-896), thin callers of api's staff route, POST
// /abuse/action, which runs the kinds produce and transparency_counts (server/controller/abuse_legal_controller.go).
// The abuse role has a command-line client and no web UI (owner rule 6).
//
// A production is the file counsel keeps, so it goes to -out, created 0600 and never over a file that is there, and
// the command prints only how many sections it wrote; a report goes to -out, or to the terminal as JSON. The staff
// bearer is read from the file -bearer-file or UR_ABUSE_STAFF_BEARER_FILE names, never an argument, and is never
// printed or logged; a refusal prints the route's status and its fixed message alone. The api is -api's URL, which
// the command needs: it does not guess an env's address. Neither command reads a store itself: what is exported, and
// the access log row each read writes, are the route's.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// The environment variable that names the bearer file when -bearer-file does not.
const abuseStaffBearerFileEnv = "UR_ABUSE_STAFF_BEARER_FILE"

// Tunables of the staff client.
type abuseStaffClientSettings struct {
	// The longest one call to the route may take: 10 minutes, since a production reads every exported row of each
	// account it names.
	RequestTimeout time.Duration
	// The most bytes of an answer read: 1 GiB, past any production the route's section bounds allow.
	ResponseMaxByteCount int64
	// The most bytes of a bearer file read: 4 KiB, past any bearer.
	BearerFileMaxByteCount int64
}

// The defaults, each with its field's reason.
func defaultAbuseStaffClientSettings() *abuseStaffClientSettings {
	return &abuseStaffClientSettings{
		RequestTimeout:         10 * time.Minute,
		ResponseMaxByteCount:   1 << 30,
		BearerFileMaxByteCount: 4096,
	}
}

// The commands' registrations.
func init() {
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "abuse production",
		usage:   "-api=<url> -request=<request_id> -out=<file> [-bearer-file=<file>]",
		summary: "Export a signed-off legal request's production to a new file for counsel (§21.9), through POST /abuse/action's produce kind.",
		run:     runAbuseProduction,
	})
	registerSidecarctlCommand(&sidecarctlCommand{
		words:   "abuse transparency-report",
		usage:   "-api=<url> -from=<RFC 3339 time> -to=<RFC 3339 time> [-out=<file>] [-bearer-file=<file>]",
		summary: "Count §21.11's transparency numbers over [from, to), through POST /abuse/action's transparency_counts kind, as JSON to -out or the terminal.",
		run:     runAbuseTransparencyReport,
	})
}

// What both commands take: the api and the bearer.
type abuseStaffClientFlags struct {
	apiUrl     string
	bearerFile string
}

// Adds -api and -bearer-file to flags.
func (self *abuseStaffClientFlags) register(flags *flag.FlagSet) {
	flags.StringVar(&self.apiUrl, "api", "", "the api's base URL")
	flags.StringVar(&self.bearerFile, "bearer-file", "", "the file holding the staff bearer; "+abuseStaffBearerFileEnv+" when unset")
}

// The route's URL from the api's base, refused unless it is an absolute http or https URL.
func (self *abuseStaffClientFlags) routeUrl() (string, error) {
	base, err := url.Parse(strings.TrimSpace(self.apiUrl))
	if self.apiUrl == "" || err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return "", sidecarctlUsageErrorf("-api must be the api's absolute http or https URL")
	}
	return strings.TrimRight(base.String(), "/") + "/abuse/action", nil
}

// The bearer from its file, trimmed, refused when no file is named or the file holds none.
func (self *abuseStaffClientFlags) bearer(settings *abuseStaffClientSettings) (string, error) {
	path := self.bearerFile
	if path == "" {
		path = os.Getenv(abuseStaffBearerFileEnv)
	}
	if path == "" {
		return "", sidecarctlUsageErrorf("name the staff bearer's file with -bearer-file or %s; a bearer is never an argument", abuseStaffBearerFileEnv)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("the bearer file: %w", err)
	}
	defer file.Close()
	bearerBytes, err := io.ReadAll(io.LimitReader(file, settings.BearerFileMaxByteCount+1))
	if err != nil {
		return "", fmt.Errorf("the bearer file: %w", err)
	}
	bearer := strings.TrimSpace(string(bearerBytes))
	if bearer == "" || settings.BearerFileMaxByteCount < int64(len(bearerBytes)) || strings.ContainsAny(bearer, " \t\r\n") {
		return "", errors.New("the bearer file holds no one bearer")
	}
	return bearer, nil
}

// The staff route's answer to one request: {kind, result}.
type abuseStaffClientAnswer struct {
	Kind   string          `json:"kind"`
	Result json.RawMessage `json:"result"`
}

// Posts body to the route with the bearer, answering the result's JSON. A refusal is an error naming the status
// and the route's message, never the bearer.
func postAbuseStaffAction(ctx context.Context, routeUrl string, bearer string, body any, settings *abuseStaffClientSettings) (json.RawMessage, error) {
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, settings.RequestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, routeUrl, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("the route did not answer: %w", err)
	}
	defer response.Body.Close()
	answerBytes, err := io.ReadAll(io.LimitReader(response.Body, settings.ResponseMaxByteCount+1))
	if err != nil {
		return nil, fmt.Errorf("the route's answer: %w", err)
	}
	if settings.ResponseMaxByteCount < int64(len(answerBytes)) {
		return nil, errors.New("the route's answer is past the client's bound")
	}
	if response.StatusCode != http.StatusOK {
		message := strings.TrimSpace(string(answerBytes))
		if 512 < len(message) {
			message = message[:512]
		}
		return nil, fmt.Errorf("the route refused with %d: %s", response.StatusCode, message)
	}
	answer := &abuseStaffClientAnswer{}
	if err := json.Unmarshal(answerBytes, answer); err != nil || len(answer.Result) == 0 {
		return nil, errors.New("the route's answer does not read as {kind, result}")
	}
	return answer.Result, nil
}

// Writes data to a new file at path, 0600, refusing a path that holds a file already.
func writeAbuseStaffFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	return file.Close()
}

// See the file header.
func runAbuseProduction(args []string, out io.Writer) error {
	return runAbuseProductionWith(context.Background(), args, out, defaultAbuseStaffClientSettings())
}

// runAbuseProduction with its context and settings, which a test sets.
func runAbuseProductionWith(ctx context.Context, args []string, out io.Writer, settings *abuseStaffClientSettings) error {
	flags := flag.NewFlagSet("abuse production", flag.ContinueOnError)
	client := &abuseStaffClientFlags{}
	client.register(flags)
	requestId := flags.String("request", "", "the legal request's id")
	outPath := flags.String("out", "", "the new file the production goes to")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if 0 < len(positionals) {
		return sidecarctlUsageErrorf("takes no positional arguments")
	}
	if *requestId == "" || *outPath == "" {
		return sidecarctlUsageErrorf("-request and -out are required")
	}
	if _, err := os.Lstat(*outPath); err == nil {
		return fmt.Errorf("%s exists; a production goes to a new file", *outPath)
	}
	routeUrl, err := client.routeUrl()
	if err != nil {
		return err
	}
	bearer, err := client.bearer(settings)
	if err != nil {
		return err
	}
	result, err := postAbuseStaffAction(ctx, routeUrl, bearer, map[string]any{"kind": "produce", "request_id": *requestId}, settings)
	if err != nil {
		return err
	}
	production := &struct {
		Sections []json.RawMessage `json:"sections"`
	}{}
	if err := json.Unmarshal(result, production); err != nil {
		return errors.New("the production does not read as one")
	}
	indented := &bytes.Buffer{}
	if err := json.Indent(indented, result, "", "  "); err != nil {
		return err
	}
	indented.WriteString("\n")
	if err := writeAbuseStaffFile(*outPath, indented.Bytes()); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote a production of %d sections to %s\n", len(production.Sections), *outPath)
	return nil
}

// See the file header.
func runAbuseTransparencyReport(args []string, out io.Writer) error {
	return runAbuseTransparencyReportWith(context.Background(), args, out, defaultAbuseStaffClientSettings())
}

// runAbuseTransparencyReport with its context and settings, which a test sets.
func runAbuseTransparencyReportWith(ctx context.Context, args []string, out io.Writer, settings *abuseStaffClientSettings) error {
	flags := flag.NewFlagSet("abuse transparency-report", flag.ContinueOnError)
	client := &abuseStaffClientFlags{}
	client.register(flags)
	fromText := flags.String("from", "", "the window's start, RFC 3339")
	toText := flags.String("to", "", "the window's end, RFC 3339, not counted")
	outPath := flags.String("out", "", "a new file the report goes to; the terminal when unset")
	positionals, err := parseSidecarctlArgs(flags, args)
	if err != nil {
		return err
	}
	if 0 < len(positionals) {
		return sidecarctlUsageErrorf("takes no positional arguments")
	}
	fromTime, fromErr := time.Parse(time.RFC3339, *fromText)
	toTime, toErr := time.Parse(time.RFC3339, *toText)
	if fromErr != nil || toErr != nil || !fromTime.Before(toTime) {
		return sidecarctlUsageErrorf("-from and -to must be RFC 3339 times, -from before -to")
	}
	if *outPath != "" {
		if _, err := os.Lstat(*outPath); err == nil {
			return fmt.Errorf("%s exists; a report goes to a new file", *outPath)
		}
	}
	routeUrl, err := client.routeUrl()
	if err != nil {
		return err
	}
	bearer, err := client.bearer(settings)
	if err != nil {
		return err
	}
	result, err := postAbuseStaffAction(ctx, routeUrl, bearer, map[string]any{
		"kind":         "transparency_counts",
		"from_time_ms": fromTime.UnixMilli(),
		"to_time_ms":   toTime.UnixMilli(),
	}, settings)
	if err != nil {
		return err
	}
	indented := &bytes.Buffer{}
	if err := json.Indent(indented, result, "", "  "); err != nil {
		return err
	}
	indented.WriteString("\n")
	if *outPath == "" {
		_, err := out.Write(indented.Bytes())
		return err
	}
	if err := writeAbuseStaffFile(*outPath, indented.Bytes()); err != nil {
		return err
	}
	fmt.Fprintf(out, "wrote the report to %s\n", *outPath)
	return nil
}
