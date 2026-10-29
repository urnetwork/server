// The API key notice as its recipients read it (owner decision 2026-10-29,
// "Yes, API keys too").
package controller

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// An API key change of each action: a key created with the website, and a key
// deleted with another API key.
func securityApiKeyNoticeSample(actions ...string) *SecurityApiKeyChangedTemplate {
	changeTime := time.Date(2026, 10, 29, 14, 3, 7, 0, time.UTC)
	keyCreateTime := time.Date(2026, 9, 2, 8, 15, 0, 0, time.UTC)
	notice := &SecurityApiKeyChangedTemplate{}
	for i, action := range actions {
		change := &model.SecurityApiKeyChange{
			ChangeTime: changeTime.Add(time.Duration(i) * time.Second),
			Action:     action,
			Name:       "deploy-bot",
			Via:        model.SecurityNoticeViaWeb,
		}
		if action == model.SecurityApiKeyDeleted {
			change.KeyCreateTime = &keyCreateTime
			change.Via = model.SecurityNoticeViaApiKey
		}
		notice.Changes = append(notice.Changes, change)
	}
	return notice
}

// The API key notice says what happened to which key, when, and how, with the
// security line; a deleted key's creation time names it; a created key says
// what a key can do. Its subject and headline say what happened.
func TestSecurityApiKeyNoticeSaysWhatChanged(t *testing.T) {
	for _, c := range []struct {
		actions  []string
		subject  string
		headline string
	}{
		{[]string{model.SecurityApiKeyCreated}, "An API key was created for your URnetwork account", "API KEY CREATED"},
		{[]string{model.SecurityApiKeyDeleted}, "An API key was deleted from your URnetwork account", "API KEY DELETED"},
		{[]string{model.SecurityApiKeyCreated, model.SecurityApiKeyCreated}, "API keys were created for your URnetwork account", "API KEYS CREATED"},
		{[]string{model.SecurityApiKeyDeleted, model.SecurityApiKeyDeleted}, "API keys were deleted from your URnetwork account", "API KEYS DELETED"},
		{[]string{model.SecurityApiKeyCreated, model.SecurityApiKeyDeleted}, "Your URnetwork API keys were changed", "API KEYS CHANGED"},
	} {
		notice := securityApiKeyNoticeSample(c.actions...)
		subject, bodyHtml, bodyText := securityNoticeText(t, notice)
		if subject != c.subject {
			t.Errorf("%v: subject = %q, want %q", c.actions, subject, c.subject)
		}
		for _, want := range []string{
			c.headline,
			c.subject + ".",
			"When: October 29, 2026, 14:03:07 UTC",
			"API key: “deploy-bot”",
			securityNoticeContactLine,
			"To list or delete your API keys: " + securityNoticeApiKeysUrl,
			"To sign out every other device: " + securityNoticeOtherSessionsUrl,
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("%v: text part missing %q:\n%s", c.actions, want, bodyText)
			}
		}
		for _, want := range []string{
			`contact <a class="ur-link" href="mailto:security@ur.io"`,
			"right away.</p>",
			"October 29, 2026, 14:03:07 UTC",
			"“deploy-bot”",
			`href="` + securityNoticeApiKeysUrl + `"`,
			`href="` + securityNoticeOtherSessionsUrl + `"`,
		} {
			if !strings.Contains(bodyHtml, want) {
				t.Errorf("%v: html part missing %q", c.actions, want)
			}
		}

		created, deleted := 0, 0
		for _, action := range c.actions {
			switch action {
			case model.SecurityApiKeyCreated:
				created += 1
			case model.SecurityApiKeyDeleted:
				deleted += 1
			}
		}
		for _, count := range []struct {
			text string
			want int
		}{
			{"Change: Created\nMade with: The ur.io website\n", created},
			{"Change: Deleted\nMade with: An API key\n", deleted},
			// only a deleted key names when it was created
			{"Key created: September 2, 2026, 08:15:00 UTC\n", deleted},
		} {
			if got := strings.Count(bodyText, count.text); got != count.want {
				t.Errorf("%v: %q appears %d times, want %d:\n%s", c.actions, count.text, got, count.want, bodyText)
			}
		}
		// what a key can do, only when one was created
		const keyPower = "Anyone with an API key can use your URnetwork account until the key is deleted."
		if strings.Contains(bodyText, keyPower) != (0 < created) || strings.Contains(bodyHtml, keyPower) != (0 < created) {
			t.Errorf("%v: the line on what a key can do is shown %v, want %v", c.actions, strings.Contains(bodyText, keyPower), 0 < created)
		}
	}

	// changes past the listed ones are counted
	more := securityApiKeyNoticeSample(model.SecurityApiKeyCreated, model.SecurityApiKeyDeleted)
	more.MoreCount = 1
	_, _, moreText := securityNoticeText(t, more)
	if !strings.Contains(moreText, "1 more change was made between these and is not listed. The last change listed is the latest.") {
		t.Errorf("the notice does not count the changes it leaves out:\n%s", moreText)
	}
}

// A key's name is shown in quotes on one line of the text part, escaped in the
// html part, and as "(no name)" when it has none. The notice carries no key and
// no IP address.
func TestSecurityApiKeyNoticeShowsTheNameSafely(t *testing.T) {
	notice := securityApiKeyNoticeSample(model.SecurityApiKeyCreated)
	notice.Changes[0].Name = "Ops <b>bot</b> & co\nChange: Deleted\u202e"
	subject, bodyHtml, bodyText := securityNoticeText(t, notice)
	if !strings.Contains(bodyText, "API key: “Ops <b>bot</b> & co Change: Deleted”\n") {
		t.Errorf("the text part does not show the name on its own line:\n%s", bodyText)
	}
	if strings.Contains(bodyText, "\nChange: Deleted") || strings.Contains(bodyText, "\u202e") {
		t.Errorf("the name broke the text part's lines or kept a direction override:\n%s", bodyText)
	}
	if !strings.Contains(bodyHtml, "Ops &lt;b&gt;bot&lt;/b&gt; &amp; co Change: Deleted") || strings.Contains(bodyHtml, "<b>bot</b>") {
		t.Error("the html part does not escape the name")
	}
	if strings.Contains(subject, "Ops") {
		t.Errorf("the subject carries the name: %q", subject)
	}

	unnamed := securityApiKeyNoticeSample(model.SecurityApiKeyDeleted)
	unnamed.Changes[0].Name = ""
	_, _, unnamedText := securityNoticeText(t, unnamed)
	if !strings.Contains(unnamedText, "API key: (no name)\n") {
		t.Errorf("a key without a name is not shown as such:\n%s", unnamedText)
	}

	ipAddress := regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	for _, part := range []string{subject, bodyHtml, bodyText} {
		if ipAddress.MatchString(part) {
			t.Errorf("the notice carries an IP address: %q", ipAddress.FindString(part))
		}
		if strings.Contains(part, "urn_") {
			t.Error("the notice carries an API key")
		}
	}
}
