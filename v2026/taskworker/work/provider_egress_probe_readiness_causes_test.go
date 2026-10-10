// Public task admission keeps malformed failures out of funding-only cadence.
package work

import (
	"context"
	"errors"
	"testing"

	"github.com/urnetwork/server/v2026/session"
)

// Text remains safe to persist while Unwrap deliberately cycles or terminates nil.
type egressCauseTestOne struct{ cause error }

func (self *egressCauseTestOne) Error() string { return "synthetic egress cause" }
func (self *egressCauseTestOne) Unwrap() error { return self.cause }

// Retain nil branches exactly as received from a custom joined transport.
type egressCauseTestMany struct{ causes []error }

func (self *egressCauseTestMany) Error() string   { return "synthetic egress joined cause" }
func (self *egressCauseTestMany) Unwrap() []error { return self.causes }

// No database, network, or replacement task is needed to exercise the target.
func TestProviderEgressPublicTaskBoundsIncompleteFundingCauses(t *testing.T) {
	settings := testProviderEgressProbeSettings(1)
	withProviderEgressProbeSettings(t, settings)
	previous := executeProviderEgressProbe
	t.Cleanup(func() { executeProviderEgressProbe = previous })
	cycle := &egressCauseTestOne{}
	cycle.cause = cycle
	var deep error = errProviderEgressProbeUnfunded
	for range 40 {
		deep = &egressCauseTestOne{cause: deep}
	}
	wide := make([]error, 256)
	for index := range wide {
		wide[index] = errProviderEgressProbeUnfunded
	}
	for index, cause := range []error{nil, cycle, deep, &egressCauseTestMany{causes: wide},
		&egressCauseTestMany{}, &egressCauseTestOne{}, &egressCauseTestMany{causes: []error{nil, nil}},
		&egressCauseTestMany{causes: []error{nil, errProviderEgressProbeUnfunded}},
		errors.Join(errProviderEgressProbeUnfunded, context.Canceled)} {
		calls := 0
		executeProviderEgressProbe = func(ctx context.Context, _ *ProviderEgressProbeArgs) (*ProviderEgressProbeResult, error) {
			calls++
			if _, bounded := ctx.Deadline(); !bounded {
				t.Fatal("public task lost its original owner deadline")
			}
			return nil, cause
		}
		result, err := ProviderEgressProbe(providerEgressProbeArgs(settings, 0), &session.ClientSession{Ctx: t.Context()})
		if result != nil || err != cause || calls != 1 || providerEgressProbeUnfundedOnly(cause) {
			t.Fatal("incomplete public task failure gained funding cadence or lost original cause", index, calls)
		}
	}
	for _, cause := range []error{errProviderEgressProbeUnfunded, &egressCauseTestOne{cause: errProviderEgressProbeUnfunded},
		errors.Join(errProviderEgressProbeUnfunded, errProviderEgressProbeUnfunded)} {
		if !providerEgressProbeUnfundedOnly(cause) {
			t.Fatal("complete unfunded task lost existing cadence permission")
		}
	}
}
