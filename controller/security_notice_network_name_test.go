// The security notices that can reach a team name the network they are about
// (QD-1216; §7.8's notices row, R-TEAM-12): the auth key, API key, network
// authority and payout wallet notices reach each Owner and Admin, or each Owner
// and Billing admin, of a team at their own sign-in addresses (QD-1215,
// QD-1217), so a person in several networks must be told which one changed.
// The name is read at delivery and never stored with the notice.
package controller

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/urnetwork/server/model"
)

// The network the samples' notices are about.
const securityNoticeSampleNetworkName = "synthetic-sample-network"

// The notices about a network, each named for the sample network, by template.
func securityNetworkNoticeSamples() map[string]Template {
	return map[string]Template{
		model.SecurityPayoutWalletTemplateName:     securityWalletNoticeSample(),
		model.SecurityApiKeyTemplateName:           securityApiKeyNoticeSample(model.SecurityApiKeyCreated),
		model.SecurityAuthKeyTemplateName:          securityAuthKeyNoticeSample(model.SecurityAuthKeyCreated),
		model.SecurityNetworkAuthorityTemplateName: securityNetworkAuthorityNoticeSample(),
	}
}

// Each notice about a network says "your URnetwork network <name>" in its
// subject, its first line and its footer, and only those notices name one: the
// sign-in method and password notices are about the person's own account.
func TestSecurityNoticesAboutANetworkNameIt(t *testing.T) {
	const network = "your URnetwork network " + securityNoticeSampleNetworkName
	for templateName, notice := range securityNetworkNoticeSamples() {
		if _, ok := notice.(accountMessageNetworkNamer); !ok {
			t.Fatalf("%s does not name its network", templateName)
		}
		subject, bodyHtml, bodyText := securityNoticeText(t, notice)
		if !strings.Contains(subject, network) {
			t.Errorf("%s: the subject %q does not name the network", templateName, subject)
		}
		// the first line and the footer
		for part, body := range map[string]string{"html": bodyHtml, "text": bodyText} {
			if count := strings.Count(strings.ReplaceAll(body, "\n", " "), network); count < 2 {
				t.Errorf("%s: the %s part names the network %d times, want its first line and its footer:\n%s", templateName, part, count, body)
			}
		}
	}
	for _, notice := range []Template{securitySignInNoticeSample(model.SecuritySignInAdded), &AuthPasswordSetTemplate{}} {
		if _, ok := notice.(accountMessageNetworkNamer); ok {
			t.Errorf("%s names a network; it is about the person's own account", notice.Name())
		}
	}
}

// A notice whose network has no name to show (none was read: the network is
// gone) reads as before the names, "your URnetwork network", with no stray
// space.
func TestSecurityNoticeWithoutANameSaysYourNetwork(t *testing.T) {
	for _, c := range []struct {
		notice  Template
		subject string
	}{
		{&SecurityPayoutWalletChangedTemplate{}, "The payout wallet of your URnetwork network was changed"},
		{&SecurityApiKeyChangedTemplate{}, "The API keys of your URnetwork network were changed"},
		{&SecurityAuthKeyChangedTemplate{}, "The auth keys of your URnetwork network were changed"},
		{&SecurityNetworkAuthorityChangedTemplate{}, "Who can admit devices to your URnetwork network changed"},
	} {
		subject, bodyHtml, bodyText := securityNoticeText(t, c.notice)
		if subject != c.subject {
			t.Errorf("%s: subject = %q, want %q", c.notice.Name(), subject, c.subject)
		}
		for _, part := range []string{subject, bodyHtml, bodyText} {
			if strings.Contains(part, "network  ") || strings.Contains(part, "network .") {
				t.Errorf("%s: the unnamed network leaves a stray space: %q", c.notice.Name(), part)
			}
		}
	}
	unnamed := securityAuthKeyNoticeSample(model.SecurityAuthKeyCreated)
	unnamed.nameNetwork("")
	if subject, _, _ := securityNoticeText(t, unnamed); subject != "An auth key was created for your URnetwork network" {
		t.Errorf("a network with no name to show: subject = %q", subject)
	}
}

// A network's name is shown on one line in reading order, escaped in the html
// part: a line break, a tab or a direction override in a stored name, which
// ValidateNetworkName refuses but an older name may hold, neither breaks the
// subject's one line nor reorders the words around it.
func TestSecurityNoticeShowsTheNetworkNameSafely(t *testing.T) {
	notice := securityAuthKeyNoticeSample(model.SecurityAuthKeyCreated)
	notice.nameNetwork("  Ops <b>net</b> &\r\nSubject: x\t‮gro.example​ ")
	subject, bodyHtml, bodyText := securityNoticeText(t, notice)
	const shown = "Ops <b>net</b> & Subject: x gro.example"
	if subject != "An auth key was created for your URnetwork network "+shown {
		t.Errorf("subject = %q", subject)
	}
	if !strings.Contains(bodyText, "\nAn auth key was created for your URnetwork network "+shown+".\n") {
		t.Errorf("the text part does not show the name on its line:\n%s", bodyText)
	}
	for _, part := range []string{subject, bodyHtml, bodyText} {
		if strings.ContainsAny(part, "‮​\t") {
			t.Errorf("the name kept a format character or a tab: %q", part)
		}
	}
	if !strings.Contains(bodyHtml, "Ops &lt;b&gt;net&lt;/b&gt; &amp; Subject: x gro.example") || strings.Contains(bodyHtml, "<b>net</b>") {
		t.Error("the html part does not escape the name")
	}
	if got := securityNoticeNetworkName(securityNoticeNetworkName(" a \n b ")); got != "a b" {
		t.Errorf("a shown name shown again = %q, want it unchanged", got)
	}
}

// The name is read at delivery and never stored: a named notice's fields, as
// the outbox stores them, hold no name, and a notice decoded from them is
// unnamed until its delivery names it.
func TestSecurityNoticeNetworkNameIsNeverStored(t *testing.T) {
	for templateName, notice := range securityNetworkNoticeSamples() {
		templateJson, err := json.Marshal(notice)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(templateJson), securityNoticeSampleNetworkName) || strings.Contains(string(templateJson), "network_name") {
			t.Errorf("%s: the stored fields hold the network's name: %s", templateName, templateJson)
		}
		decoded, err := decodeAccountMessageTemplate(templateName, `{"changes":[],"networkName":"stored-name","network_name":"stored-name"}`)
		if err != nil {
			t.Fatal(err)
		}
		subject, _, _ := securityNoticeText(t, decoded)
		if strings.Contains(subject, "stored-name") {
			t.Errorf("%s: a name in the stored fields reached the subject %q", templateName, subject)
		}
	}
}
