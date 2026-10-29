// The security notices as their recipients read them, and the one channel they
// go out on: email.
package controller

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/model"
)

// A payout wallet change (owner decision 2026-10-29).
func securityWalletNoticeSample() *SecurityPayoutWalletChangedTemplate {
	changeTime := time.Date(2026, 10, 29, 14, 3, 7, 0, time.FixedZone("UTC-5", -5*60*60))
	return &SecurityPayoutWalletChangedTemplate{
		SecurityWalletNotice: model.SecurityWalletNotice{
			Changes: []*model.SecurityWalletChange{
				{
					ChangeTime:  changeTime,
					Destination: model.SecurityWalletDestinationUsdc,
					NewChain:    "SOL",
					NewWallet:   model.MaskWalletAddress("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"),
					Via:         model.SecurityNoticeViaAndroid,
				},
				{
					ChangeTime:     changeTime.Add(10 * time.Second),
					Destination:    model.SecurityWalletDestinationUsdc,
					PreviousChain:  "SOL",
					PreviousWallet: model.MaskWalletAddress("9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"),
					NewChain:       "MATIC",
					NewWallet:      model.MaskWalletAddress("0x52908400098527886E0F7030069857D2E4169EE7"),
					Via:            model.SecurityNoticeViaWeb,
				},
				{
					ChangeTime:   changeTime.Add(20 * time.Second),
					Destination:  model.SecurityWalletDestinationSubnetNetworkConsent,
					NewChain:     "TAO",
					NewWallet:    model.MaskWalletAddress("5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX"),
					FromEpoch:    12,
					ThroughEpoch: 20,
				},
				{
					ChangeTime:     changeTime.Add(30 * time.Second),
					Destination:    model.SecurityWalletDestinationUsdc,
					PreviousChain:  "MATIC",
					PreviousWallet: model.MaskWalletAddress("0x52908400098527886E0F7030069857D2E4169EE7"),
					Via:            model.SecurityNoticeViaApiKey,
				},
			},
		},
	}
}

// A sign-in method change of each action.
func securitySignInNoticeSample(actions ...string) *SecuritySignInChangedTemplate {
	changeTime := time.Date(2026, 10, 29, 14, 3, 7, 0, time.UTC)
	notice := &SecuritySignInChangedTemplate{}
	for i, action := range actions {
		notice.Changes = append(notice.Changes, &model.SecuritySignInChange{
			ChangeTime: changeTime.Add(time.Duration(i) * time.Second),
			Action:     action,
			Method:     model.SecuritySignInMethodGoogle,
			Identity:   "j…@gmail.com",
			Via:        model.SecurityNoticeViaIos,
		})
	}
	return notice
}

// The text part with its lines' trailing space removed.
func securityNoticeText(t *testing.T, template Template) (subject string, bodyHtml string, bodyText string) {
	t.Helper()
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		t.Fatalf("%s: render: %v", template.Name(), err)
	}
	return subject, bodyHtml, bodyText
}

// The payout wallet notice names the time in UTC, each wallet masked with its
// chain, how the change was made when known, and the security line, and it
// carries no IP address.
func TestSecurityPayoutWalletNoticeSaysWhatChanged(t *testing.T) {
	subject, bodyHtml, bodyText := securityNoticeText(t, securityWalletNoticeSample())
	if subject != "Your URnetwork payout wallet was changed" {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{
		securityNoticeContactLine,
		"The wallets your URnetwork payouts go to were changed.",
		"When: October 29, 2026, 19:03:07 UTC",
		"Wallet: USDC payout wallet",
		"Before: None",
		"After: Solana 9WzD…AWWM",
		"Before: Solana 9WzD…AWWM",
		"After: Polygon 0x52…9EE7",
		"Wallet: Bittensor wallet approved for your network's subnet earnings",
		"After: Bittensor 5F3s…YjQX",
		"Epochs: 12 to 20",
		"Made with: The URnetwork app for Android",
		"Made with: The ur.io website",
		"Made with: An API key",
		"Before: Polygon 0x52…9EE7\nAfter: None",
		securityNoticePayoutWalletsUrl,
		securityNoticeOtherSessionsUrl,
	} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("text part missing %q:\n%s", want, bodyText)
		}
	}
	for _, want := range []string{
		`contact <a class="ur-link" href="mailto:security@ur.io"`,
		"right away.</p>",
		"October 29, 2026, 19:03:07 UTC",
		"Solana 9WzD…AWWM",
		"Polygon 0x52…9EE7",
		"12 to 20",
		`href="` + securityNoticePayoutWalletsUrl + `"`,
	} {
		if !strings.Contains(bodyHtml, want) {
			t.Errorf("html part missing %q", want)
		}
	}
	// the epoch row only for a consent: three of four changes have none
	if count := strings.Count(bodyText, "Epochs:"); count != 1 {
		t.Errorf("listed %d epoch rows, want only the consent's", count)
	}
	// the made-with row only when known: the consent's is unknown
	if count := strings.Count(bodyText, "Made with:"); count != 3 {
		t.Errorf("listed %d made-with rows, want the three known", count)
	}
	ipAddress := regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	for _, part := range []string{subject, bodyHtml, bodyText} {
		if ipAddress.MatchString(part) {
			t.Errorf("the notice carries an IP address: %q", ipAddress.FindString(part))
		}
		for _, address := range []string{
			"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM",
			"0x52908400098527886E0F7030069857D2E4169EE7",
			"5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX",
		} {
			if strings.Contains(part, address) {
				t.Errorf("the notice carries a full wallet address %s", address)
			}
		}
	}

	// one change reads as one
	single := &SecurityPayoutWalletChangedTemplate{SecurityWalletNotice: model.SecurityWalletNotice{
		Changes: securityWalletNoticeSample().Changes[:1],
	}}
	_, _, singleText := securityNoticeText(t, single)
	if !strings.Contains(singleText, "The wallet your URnetwork payouts go to was changed.") {
		t.Errorf("a single change reads as several:\n%s", singleText)
	}
	// changes past the listed ones are counted
	more := securityWalletNoticeSample()
	more.MoreCount = 3
	_, _, moreText := securityNoticeText(t, more)
	if !strings.Contains(moreText, "3 more changes were made between these and are not listed. The last change listed is the latest.") {
		t.Errorf("the notice does not count the changes it leaves out:\n%s", moreText)
	}
}

// The sign-in method notice says which method, added or removed, when, and how,
// with the security line; its subject says what happened.
func TestSecuritySignInNoticeSaysWhatChanged(t *testing.T) {
	for _, c := range []struct {
		actions  []string
		subject  string
		headline string
	}{
		{[]string{model.SecuritySignInAdded}, "A sign-in method was added to your URnetwork account", "SIGN-IN METHOD ADDED"},
		{[]string{model.SecuritySignInRemoved}, "A sign-in method was removed from your URnetwork account", "SIGN-IN METHOD REMOVED"},
		{[]string{model.SecuritySignInAdded, model.SecuritySignInAdded}, "Sign-in methods were added to your URnetwork account", "SIGN-IN METHOD ADDED"},
		{[]string{model.SecuritySignInRemoved, model.SecuritySignInAdded}, "Your URnetwork sign-in methods were changed", "SIGN-IN METHODS CHANGED"},
		{[]string{model.SecuritySignInReplaced}, "Your URnetwork sign-in methods were changed", "SIGN-IN METHODS CHANGED"},
	} {
		subject, bodyHtml, bodyText := securityNoticeText(t, securitySignInNoticeSample(c.actions...))
		if subject != c.subject {
			t.Errorf("%v: subject = %q, want %q", c.actions, subject, c.subject)
		}
		for _, want := range []string{
			c.headline,
			c.subject + ".",
			"When: October 29, 2026, 14:03:07 UTC",
			"Sign-in method: Google (j…@gmail.com)",
			"Change: " + map[string]string{
				model.SecuritySignInAdded:    "Added",
				model.SecuritySignInRemoved:  "Removed",
				model.SecuritySignInReplaced: "Replaced",
			}[c.actions[0]],
			"Made with: The URnetwork app for iOS",
			securityNoticeContactLine,
			securityNoticeSignInMethodsUrl,
		} {
			if !strings.Contains(bodyText, want) {
				t.Errorf("%v: text part missing %q:\n%s", c.actions, want, bodyText)
			}
		}
		if !strings.Contains(bodyHtml, `contact <a class="ur-link" href="mailto:security@ur.io"`) {
			t.Errorf("%v: html part lacks the security contact", c.actions)
		}
	}
	// every method by its name in the apps
	for method, name := range map[string]string{
		model.SecuritySignInMethodEmail:      "Email",
		model.SecuritySignInMethodPhone:      "Phone",
		model.SecuritySignInMethodApple:      "Apple",
		model.SecuritySignInMethodGoogle:     "Google",
		model.SecuritySignInMethodSolana:     "Solana wallet",
		model.SecuritySignInMethodBittensor:  "Bittensor wallet",
		model.SecuritySignInMethodSeedphrase: "Seedphrase",
	} {
		_, _, bodyText := securityNoticeText(t, &SecuritySignInChangedTemplate{SecuritySignInNotice: model.SecuritySignInNotice{
			Changes: []*model.SecuritySignInChange{{ChangeTime: time.Now(), Action: model.SecuritySignInAdded, Method: method}},
		}})
		if !strings.Contains(bodyText, "Sign-in method: "+name+"\n") {
			t.Errorf("%s: the notice does not name the method %q", method, name)
		}
	}
}

// The password-changed notice says when and how the password changed when it
// knows, and reads as before for a notice owed before it recorded them.
func TestPasswordChangedNoticeSaysWhenAndHow(t *testing.T) {
	changeTime := time.Date(2026, 10, 29, 14, 3, 7, 0, time.UTC)
	_, bodyHtml, bodyText := securityNoticeText(t, &AuthPasswordSetTemplate{PasswordSetNotice: model.PasswordSetNotice{
		ChangeTime: &changeTime,
		Via:        model.SecurityNoticeViaWeb,
	}})
	for _, want := range []string{"When: October 29, 2026, 14:03:07 UTC", "Made with: The ur.io website", "contact security@ur.io right away"} {
		if !strings.Contains(bodyText, want) {
			t.Errorf("text part missing %q:\n%s", want, bodyText)
		}
	}
	if !strings.Contains(bodyHtml, "October 29, 2026, 14:03:07 UTC") {
		t.Error("html part lacks the time")
	}
	_, _, legacyText := securityNoticeText(t, &AuthPasswordSetTemplate{})
	if strings.Contains(legacyText, "When:") || strings.Contains(legacyText, "Made with:") {
		t.Errorf("a notice without a time shows an empty one:\n%s", legacyText)
	}
	if !strings.Contains(legacyText, "you're all set.\n\nIf you didn't make this change") {
		t.Errorf("the notice without a time lost its paragraph break:\n%s", legacyText)
	}
}

// The security notices go to email only: none is allowed by SMS, a phone number
// never reaches the SMS provider with one, and an email address gets the email.
func TestSecurityNoticesAreNeverSentBySms(t *testing.T) {
	smsEndpoint := newAwsEndpoint(t, "application/x-amz-json-1.0", `{"MessageId":"synthetic-message-id"}`)
	smsSender := smsEndpoint.sender(5 * time.Second)
	emailEndpoint := newAwsEndpoint(t, "text/xml", `<SendEmailResponse xmlns="http://ses.amazonaws.com/doc/2010-12-01/">
  <SendEmailResult>
    <MessageId>synthetic-message-id</MessageId>
  </SendEmailResult>
  <ResponseMetadata>
    <RequestId>synthetic-request-id</RequestId>
  </ResponseMetadata>
</SendEmailResponse>`)
	emailSender := emailEndpoint.sender(5 * time.Second)

	for _, template := range []Template{
		securityWalletNoticeSample(),
		securitySignInNoticeSample(model.SecuritySignInAdded),
		securityApiKeyNoticeSample(model.SecurityApiKeyCreated),
	} {
		if SmsAllowed(template) {
			t.Fatalf("%s is allowed by SMS; AWS approved SMS for verification, password reset and password changed only", template.Name())
		}
		err := sendWithGuard(t, func() error {
			return smsSender.SendAccountMessageTemplate("+1 555 555 0100", template)
		})
		if err != nil {
			t.Fatalf("%s: phone send error = %v, want none", template.Name(), err)
		}
		if request := smsEndpoint.receivedRequest(); request != nil {
			t.Fatalf("%s reached the SMS provider (%s)", template.Name(), request.Header.Get("X-Amz-Target"))
		}

		err = sendWithGuard(t, func() error {
			return emailSender.SendAccountMessageTemplate("owner@synthetic.example", template, SenderEmail("sender@synthetic.example"))
		})
		if err != nil {
			t.Fatalf("%s: email send error = %v, want none", template.Name(), err)
		}
		request := emailEndpoint.receivedRequest()
		if request == nil {
			t.Fatalf("%s: the email address got no email", template.Name())
		}
		if err := request.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if request.PostForm.Get("Action") != "SendEmail" || request.PostForm.Get("Destination.ToAddresses.member.1") != "owner@synthetic.example" {
			t.Fatalf("%s: SES call = %v, want SendEmail to the owner", template.Name(), request.PostForm)
		}
	}
}
