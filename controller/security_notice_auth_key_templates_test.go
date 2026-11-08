// The auth key notice as its recipients read it (R-TEAM-12, §7.8's notices).
package controller

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// An auth key change of each action: a single-use key created with the website,
// and a pre-approved, ephemeral, tagged key revoked with an API key.
func securityAuthKeyNoticeSample(actions ...string) *SecurityAuthKeyChangedTemplate {
	changeTime := time.Date(2026, 11, 7, 14, 3, 7, 0, time.UTC)
	notice := &SecurityAuthKeyChangedTemplate{}
	for i, action := range actions {
		change := &model.SecurityAuthKeyChange{
			ChangeTime: changeTime.Add(time.Duration(i) * time.Second),
			Action:     action,
			ExpireTime: changeTime.Add(5 * time.Minute),
			MaxUses:    1,
			Via:        model.SecurityNoticeViaWeb,
		}
		if action == model.SecurityAuthKeyRevoked {
			change.ExpireTime = changeTime.Add(30 * 24 * time.Hour)
			change.MaxUses = 25
			change.UseCount = 3
			change.Ephemeral = true
			change.PreApproved = true
			change.TagCount = 2
			change.Via = model.SecurityNoticeViaApiKey
		}
		notice.Changes = append(notice.Changes, change)
	}
	return notice
}

// The notice says what happened to which kind of key, when, and how, with the
// security line; a created key says what a key can do, and a revoked
// pre-approved key that its bundle still admits.
func TestSecurityAuthKeyNoticeSaysWhatChanged(t *testing.T) {
	for _, c := range []struct {
		actions []string
		subject string
	}{
		{[]string{model.SecurityAuthKeyCreated}, "An auth key was created for your URnetwork network"},
		{[]string{model.SecurityAuthKeyRevoked}, "An auth key was revoked from your URnetwork network"},
		{[]string{model.SecurityAuthKeyCreated, model.SecurityAuthKeyCreated}, "Auth keys were created for your URnetwork network"},
		{[]string{model.SecurityAuthKeyCreated, model.SecurityAuthKeyRevoked}, "Your URnetwork auth keys were changed"},
	} {
		notice := securityAuthKeyNoticeSample(c.actions...)
		subject, bodyHtml, bodyText := securityNoticeText(t, notice)
		if subject != c.subject {
			t.Errorf("%v: subject = %q, want %q", c.actions, subject, c.subject)
		}
		for _, want := range []string{
			c.subject + ".",
			"When: November 7, 2026, 14:03:07 UTC",
			securityNoticeContactLine,
			"To sign out every other device: " + securityNoticeOtherSessionsUrl,
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("%v: text part missing %q:\n%s", c.actions, want, bodyText)
			}
		}
		for _, want := range []string{
			`contact <a class="ur-link" href="mailto:security@ur.io"`,
			"November 7, 2026, 14:03:07 UTC",
			`href="` + securityNoticeOtherSessionsUrl + `"`,
		} {
			if !strings.Contains(bodyHtml, want) {
				t.Errorf("%v: html part missing %q", c.actions, want)
			}
		}

		created, revoked := 0, 0
		for _, action := range c.actions {
			switch action {
			case model.SecurityAuthKeyCreated:
				created += 1
			case model.SecurityAuthKeyRevoked:
				revoked += 1
			}
		}
		for _, count := range []struct {
			text string
			want int
		}{
			{"Change: Created\nExpires: November 7, 2026, 14:08:07 UTC\nUses: 1 use\nMade with: The ur.io website\n", created},
			{"Uses: 3 of 25 uses\nKind: Ephemeral, pre-approved\nTags: 2 tags\nMade with: An API key\n", revoked},
		} {
			if got := strings.Count(bodyText, count.text); got != count.want {
				t.Errorf("%v: %q appears %d times, want %d:\n%s", c.actions, count.text, got, count.want, bodyText)
			}
		}
		const keyPower = "Anyone with an auth key can add a device to your network"
		if strings.Contains(bodyText, keyPower) != (0 < created) || strings.Contains(bodyHtml, keyPower) != (0 < created) {
			t.Errorf("%v: the line on what a key can do is shown %v, want %v", c.actions, strings.Contains(bodyText, keyPower), 0 < created)
		}
		const bundleLine = "a copy of its bundle still admits devices until its Delegation is revoked"
		if strings.Contains(bodyText, bundleLine) != (0 < revoked) || strings.Contains(bodyHtml, bundleLine) != (0 < revoked) {
			t.Errorf("%v: the bundle line is shown %v, want %v", c.actions, strings.Contains(bodyText, bundleLine), 0 < revoked)
		}
	}
}

// The auth key notice is email only.
func TestSecurityAuthKeyNoticeIsNeverSentBySms(t *testing.T) {
	notice := securityAuthKeyNoticeSample(model.SecurityAuthKeyCreated)
	if SmsAllowed(notice) {
		t.Fatalf("%s is allowed by SMS", notice.Name())
	}
	if _, ok := accountMessageTemplates[notice.Name()]; !ok {
		t.Fatalf("%s does not go through the outbox", notice.Name())
	}
}
