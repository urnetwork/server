// CLI diagnostics accept only the concrete transport type's closed stage.
package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/controller"
)

// Even an empty typed error produces a fixed unknown stage, not its wrapper.
func TestRecoveryTransportFailureShowsOnlyClosedStage(t *testing.T) {
	private := fmt.Errorf("synthetic-private-token-path: %w", &controller.CompetitionAgentTransportError{})
	text := recoveryReevaluationFailure(private).Error()
	if !strings.Contains(text, "transport_stage=unknown") || strings.Contains(text, "synthetic") || strings.Contains(text, "private-token") {
		t.Fatal("CLI exposed a private wrapper instead of the closed stage", text)
	}
}

// Ordinary store failures retain the existing generic refusal text exactly.
func TestRecoveryNonTransportFailureRemainsGeneric(t *testing.T) {
	text := recoveryReevaluationFailure(errors.New("synthetic-private-stderr-sentinel")).Error()
	if text != "audited reevaluation refused; inspect restricted operator evidence" {
		t.Fatal("non-transport failure disclosed private detail", text)
	}
}
