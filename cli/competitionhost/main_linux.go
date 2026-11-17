//go:build linux

// Render protected host documents without installing files, running commands,
// loading main configuration or accepting any readiness/qualification claim.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/urnetwork/server/competitionagent"
)

// The operator supplies prospective boundary metadata; native admission still
// verifies every actual installed pin, kernel fact, namespace and quota.
type provisioningRequest struct {
	Boundary competitionagent.RuntimeResourceBoundary `json:"boundary"`
	Scope    competitionagent.RuntimeResourceScope    `json:"scope"`
}

// The renderer never writes the host; stdout carries an explicitly unqualified plan.
func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// Input bytes use the existing owner-local configuration ceiling.
func run(args []string, input io.Reader, output, diagnostic io.Writer) error {
	flags := flag.NewFlagSet("competitionhost", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	path := flags.String("input", "", "boundary/scope JSON file, or - for stdin")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *path == "" {
		return errors.New("usage: competitionhost --input=<boundary-and-scope.json|->")
	}
	if *path != "-" {
		file, err := os.Open(*path)
		if err != nil {
			return err
		}
		defer file.Close()
		input = file
	}
	settings := competitionagent.DefaultRuntimeResourceBoundarySettings()
	content, err := io.ReadAll(io.LimitReader(input, settings.DocumentBytes+1))
	if err != nil || int64(len(content)) > settings.DocumentBytes {
		return errors.New("provisioning input exceeds its bounded document budget")
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	var request provisioningRequest
	if err := decoder.Decode(&request); err != nil {
		return errors.New("invalid provisioning request")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("provisioning input must contain exactly one JSON object")
	}
	plan, err := competitionagent.RenderRuntimeProvisioningDocuments(request.Boundary, request.Scope, settings)
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(plan)
}
