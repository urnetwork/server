// Malformed operator plans must produce no documents or host side effects.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/urnetwork/server/competitionagent"
)

// Reject unknown fields before any boundary rendering occurs.
func TestProvisioningCommandRejectsUnknownFields(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"--input=-"}, strings.NewReader(`{"unexpected":true}`), &output, &bytes.Buffer{}); err == nil || output.Len() != 0 {
		t.Fatal("unknown input produced documents", err)
	}
}

// A second JSON object cannot supply an alternative authority after validation.
func TestProvisioningCommandRejectsTrailingObjects(t *testing.T) {
	var output bytes.Buffer
	if err := run([]string{"--input=-"}, strings.NewReader(`{"boundary":{},"scope":{}} {}`), &output, &bytes.Buffer{}); err == nil || output.Len() != 0 {
		t.Fatal("multiple plans produced documents", err)
	}
}

// The CLI cannot allocate unbounded input just because the caller is an operator.
func TestProvisioningCommandEnforcesDocumentBudget(t *testing.T) {
	var output bytes.Buffer
	input := strings.NewReader(strings.Repeat("x", int(competitionagent.DefaultRuntimeResourceBoundarySettings().DocumentBytes)+1))
	if err := run([]string{"--input=-"}, input, &output, &bytes.Buffer{}); err == nil || output.Len() != 0 {
		t.Fatal("oversized input produced documents", err)
	}
}

// Input selection is explicit and cannot be replaced by positional shell text.
func TestProvisioningCommandRequiresExplicitInput(t *testing.T) {
	for _, args := range [][]string{nil, {"--input=-", "extra"}} {
		var output bytes.Buffer
		if err := run(args, strings.NewReader(`{}`), &output, &bytes.Buffer{}); err == nil || output.Len() != 0 {
			t.Fatal("ambiguous input produced documents", err)
		}
	}
}
