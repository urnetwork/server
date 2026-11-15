// Error preservation is internal: source text and partial command output must
// not become alert evidence, even when they resemble useful diagnostics.
package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMimirAdmissionObservationFailureOmitsPrivateSourceCanaries(t *testing.T) {
	canaries := []string{
		"password=synthetic-password-canary",
		"https://observer.example/query?token=synthetic-token-canary",
		"SELECT synthetic_private_column FROM synthetic_private_table",
		`{tenant="synthetic-tenant-canary",stream="synthetic-stream-canary"}`,
	}
	privateText := strings.Join(canaries, " ")
	admissionErr := &sshAdmissionUnavailableError{err: errors.New(privateText)}
	sourceErr := errors.Join(
		fmt.Errorf("%s: %w", privateText, context.DeadlineExceeded),
		admissionErr,
	)
	alerts := runMimirAdmissionSynthetic(t, NewMimirAdmissionSignal(),
		time.Date(2033, 7, 8, 9, 10, 0, 0, time.UTC),
		map[string]mimirAdmissionSyntheticResponse{
			"observer-source.example": {output: privateText, err: sourceErr},
		},
	)
	alert := requireMimirAdmissionObservationFailure(t, alerts, "observer-source.example/mimir-admission")
	classField, _, _ := strings.Cut(alert.Observed, " ")
	if classField != "error_class="+observationErrorClassTimeout {
		t.Fatal("private source text displaced the authoritative lifecycle class")
	}
	for _, value := range canaries {
		if strings.Contains(alert.Markdown(), value) {
			t.Fatal("private source canary reached Alert Markdown")
		}
	}
	if len(alerts) != 1 {
		t.Fatal("failed source output created a service finding")
	}
}
