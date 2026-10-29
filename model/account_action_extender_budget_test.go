package model

import (
	"strings"
	"testing"
	"time"
)

// An extender activation is budgeted per address family (support inbox 8083),
// and each family's budget has a display name of its own, so a refusal says
// which family's budget is spent rather than "this action". The actions are
// written out as the strings the budgets are recorded under, since those are
// what the refusal is looked up by.
func TestExtenderActivateBudgetsHaveFamilyDisplayNames(t *testing.T) {
	cases := []struct {
		action   string
		wantName string
	}{
		{
			action:   "extender_activate_v4",
			wantName: "for activating an extender over IPv4 in the last hour",
		},
		{
			action:   "extender_activate_v6",
			wantName: "for activating an extender over IPv6 in the last hour",
		},
		{
			action:   "extender_activate_account_v4",
			wantName: "for activating your account's extenders over IPv4 in the last hour",
		},
		{
			action:   "extender_activate_account_v6",
			wantName: "for activating your account's extenders over IPv6 in the last hour",
		},
	}
	for _, c := range cases {
		message := maxAccountActionAttemptsError(c.action, time.Hour).Error()
		if !strings.Contains(message, c.wantName) || strings.Contains(message, "this action") {
			t.Errorf("%s: the refusal %q does not name %q", c.action, message, c.wantName)
		}
	}
}
