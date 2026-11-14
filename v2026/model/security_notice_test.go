// The security notices' pure rules: how wallets, email addresses and phone
// numbers are masked, how a change says it was made, how a full notice keeps its
// latest change, and which sign-ins are recipients.
package model

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026/session"
)

// A notice shows a wallet as its first four and last four characters around an
// ellipsis, on every chain, and never more of it.
func TestMaskWalletAddressShowsFirstFourAndLastFour(t *testing.T) {
	for _, c := range []struct {
		name    string
		address string
		want    string
	}{
		{"solana", "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM", "9WzD…AWWM"},
		{"polygon", "0x52908400098527886E0F7030069857D2E4169EE7", "0x52…9EE7"},
		{"bittensor", "5F3sa2TJAWMqDhXG6jhV4N8ko9SxwGy8TpaNS1repo5EYjQX", "5F3s…YjQX"},
		{"surrounding space", "  9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM\n", "9WzD…AWWM"},
		{"twelve characters", "abcdefghijkl", "abcd…ijkl"},
		{"eleven characters hide everything", "abcdefghijk", "…"},
		{"short", "abc", "…"},
		{"empty", "", ""},
	} {
		got := MaskWalletAddress(c.address)
		if got != c.want {
			t.Errorf("%s: MaskWalletAddress(%q) = %q, want %q", c.name, c.address, got, c.want)
		}
		address := strings.TrimSpace(c.address)
		if 12 <= len(address) && strings.Contains(got, address[4:len(address)-4]) {
			t.Errorf("%s: the mask shows the middle of the address: %q", c.name, got)
		}
	}
}

// An email address keeps the first character of its mailbox and its domain; a
// phone number keeps its last two digits.
func TestMaskUserAuthHidesTheAddress(t *testing.T) {
	for _, c := range []struct {
		userAuth string
		want     string
	}{
		{"Jane.Doe@Example.com", "j…@example.com"},
		{"x@privaterelay.appleid.com", "x…@privaterelay.appleid.com"},
		{"+1 415-555-0123", "…23"},
		{"not an address", ""},
	} {
		if got := maskUserAuth(c.userAuth); got != c.want {
			t.Errorf("maskUserAuth(%q) = %q, want %q", c.userAuth, got, c.want)
		}
	}
	if got := maskPhoneNumber("12345"); got != "…" {
		t.Errorf("a short number = %q, want nothing of it shown", got)
	}
}

// How a change was made: an API key first, then the device type the client
// declared, then a browser call from the operator's website; anything else is
// unknown and the notice leaves it out.
func TestSecurityNoticeViaNamesAppWebsiteOrApiKey(t *testing.T) {
	for _, c := range []struct {
		name    string
		channel session.RequestChannel
		want    string
	}{
		{"android app", session.RequestChannel{DeviceType: "android"}, SecurityNoticeViaAndroid},
		{"ios app", session.RequestChannel{DeviceType: "ios"}, SecurityNoticeViaIos},
		{"declared web", session.RequestChannel{DeviceType: "web"}, SecurityNoticeViaWeb},
		{"ur.io browser call", session.RequestChannel{DeviceType: "unknown", BrowserOrigin: "https://ur.io"}, SecurityNoticeViaWeb},
		{"bringyour.com subdomain", session.RequestChannel{BrowserOrigin: "https://app.bringyour.com"}, SecurityNoticeViaWeb},
		{"api key beats the device", session.RequestChannel{DeviceType: "android", ApiKey: true}, SecurityNoticeViaApiKey},
		{"another site", session.RequestChannel{BrowserOrigin: "https://ur.io.example.com"}, ""},
		{"plain http", session.RequestChannel{BrowserOrigin: "http://ur.io"}, ""},
		{"server device", session.RequestChannel{DeviceType: "server"}, ""},
		{"nothing known", session.RequestChannel{DeviceType: "unknown"}, ""},
	} {
		if got := securityNoticeViaOf(c.channel); got != c.want {
			t.Errorf("%s: via = %q, want %q", c.name, got, c.want)
		}
	}
}

// A notice lists at most securityNoticeMaxChanges changes; past that the last
// slot takes the latest change and the rest are counted, so the notice always
// ends with the current state.
func TestSecurityNoticeKeepsTheLatestChangeWhenFull(t *testing.T) {
	notice := &SecurityWalletNotice{}
	base := time.Date(2026, 10, 29, 14, 3, 0, 0, time.UTC)
	total := securityNoticeMaxChanges + 3
	for i := range total {
		notice.add(&SecurityWalletChange{ChangeTime: base.Add(time.Duration(i) * time.Second), NewWallet: string(rune('a' + i))})
	}
	if len(notice.Changes) != securityNoticeMaxChanges || notice.MoreCount != 3 {
		t.Fatalf("listed %d with %d more, want %d with 3 more", len(notice.Changes), notice.MoreCount, securityNoticeMaxChanges)
	}
	if notice.Changes[0].NewWallet != "a" || notice.Changes[len(notice.Changes)-1].NewWallet != string(rune('a'+total-1)) {
		t.Fatalf("the list must keep the first change and end with the latest, got %q ... %q", notice.Changes[0].NewWallet, notice.Changes[len(notice.Changes)-1].NewWallet)
	}
}

// Recipients are the sign-in email addresses, normalized and distinct; a phone
// number or anything else is never one.
func TestSecurityNoticeRecipientsAreEmailAddressesOnly(t *testing.T) {
	got := securityNoticeEmails([]string{
		"B@Example.com",
		"a@example.com",
		" b@example.com ",
		"+1 415-555-0123",
		"9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM",
	})
	want := []string{"a@example.com", "b@example.com"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("recipients = %v, want %v", got, want)
	}
	many := []string{}
	for i := range securityNoticeMaxRecipients + 4 {
		many = append(many, string(rune('a'+i))+"@example.com")
	}
	if got := securityNoticeEmails(many); len(got) != securityNoticeMaxRecipients {
		t.Fatalf("%d recipients, want at most %d", len(got), securityNoticeMaxRecipients)
	}
}
