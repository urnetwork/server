// The network authority notice as its recipients read it (R-TEAM-12, §7.8's
// notices; TEAM-M2-16, TEAM-M6-05).
package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// A Delegation, an AdminSet change and an IdP: the notice says what kind of
// change each was and when, how an IdP was added, with the security lines, and
// names no key, member or IdP.
func TestSecurityNetworkAuthorityNoticeSaysWhatChanged(t *testing.T) {
	changeTime := time.Date(2026, 11, 10, 9, 30, 0, 0, time.UTC)
	notice := &SecurityNetworkAuthorityChangedTemplate{}
	notice.Changes = []*model.SecurityNetworkAuthorityChange{
		{ChangeTime: changeTime, Action: model.SecurityNetworkAuthorityDelegation},
		{ChangeTime: changeTime.Add(time.Second), Action: model.SecurityNetworkAuthorityAdminSet},
		{ChangeTime: changeTime.Add(2 * time.Second), Action: model.SecurityNetworkAuthorityIdp, Via: model.SecurityNoticeViaWeb},
	}
	subject, bodyHtml, bodyText := securityNoticeText(t, notice)
	if subject != "Who can admit devices to your URnetwork network changed" {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{
		"When: November 10, 2026, 09:30:00 UTC\nChange: A delegation that admits devices was added\n",
		"When: November 10, 2026, 09:30:01 UTC\nChange: The network's admins changed\n",
		"Change: A sign-in provider was added\nMade with: The ur.io website\n",
		securityNoticeContactLine,
		"To sign out every other device: " + securityNoticeOtherSessionsUrl,
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("text part missing %q:\n%s", want, bodyText)
		}
	}
	for _, want := range []string{
		"A delegation that admits devices was added",
		"A sign-in provider was added",
		`contact <a class="ur-link" href="mailto:security@ur.io"`,
	} {
		if !strings.Contains(bodyHtml, want) {
			t.Errorf("html part missing %q", want)
		}
	}
}

// The network authority notice is email only, and goes through the outbox.
func TestSecurityNetworkAuthorityNoticeIsNeverSentBySms(t *testing.T) {
	notice := &SecurityNetworkAuthorityChangedTemplate{}
	if SmsAllowed(notice) {
		t.Fatalf("%s is allowed by SMS", notice.Name())
	}
	if _, ok := accountMessageTemplates[notice.Name()]; !ok {
		t.Fatalf("%s does not go through the outbox", notice.Name())
	}
}
