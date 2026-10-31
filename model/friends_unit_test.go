package model

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Pure tests: no database or redis.

func validFriendsApplyArgs() *FriendsApplyArgs {
	return &FriendsApplyArgs{
		Name:           "  Ada Lovelace ",
		Email:          " ada@example.com ",
		CountryCode:    " gb ",
		ConnectionType: "Cellular",
		Consent:        true,
		Credit:         true,
		CreditName:     " Ada L. ",
		TShirts:        []FriendsGarment{{Fit: "F", Size: "m"}, {Fit: "m", Size: "2xl"}},
		Hoodies:        []FriendsGarment{{Fit: "f", Size: "L"}},
		Notes:          "Rural LTE.\nPower cuts sometimes.",
	}
}

func TestNewFriendsApplicationValidation(t *testing.T) {
	applicationId := server.NewId()

	application, refusal := newFriendsApplication(validFriendsApplyArgs(), applicationId)
	connect.AssertEqual(t, refusal, "")
	connect.AssertEqual(t, application.name, "Ada Lovelace")
	connect.AssertEqual(t, application.email, "ada@example.com")
	connect.AssertEqual(t, application.countryCode, "GB")
	connect.AssertEqual(t, application.connectionType, "cellular")
	connect.AssertEqual(t, application.creditName, "Ada L.")
	connect.AssertEqual(t, friendsGarmentsText(application.tShirts), "Women's M, Men's 2XL")
	connect.AssertEqual(t, friendsGarmentsText(application.hoodies), "Women's L")

	// without credit, a credit name is dropped
	args := validFriendsApplyArgs()
	args.Credit = false
	application, refusal = newFriendsApplication(args, applicationId)
	connect.AssertEqual(t, refusal, "")
	connect.AssertEqual(t, application.creditName, "")

	refusals := map[string]func(*FriendsApplyArgs){
		"Please enter your name.":                                       func(a *FriendsApplyArgs) { a.Name = " " },
		"Please enter a valid email address.":                           func(a *FriendsApplyArgs) { a.Email = "ada" },
		"Please choose your country.":                                   func(a *FriendsApplyArgs) { a.CountryCode = "GBR" },
		"We can't ship nodes to that country under US export controls.": func(a *FriendsApplyArgs) { a.CountryCode = "ir" },
		"Please choose your network connection type.":                   func(a *FriendsApplyArgs) { a.ConnectionType = "dialup" },
		"Please agree to do your best to keep the node online.":         func(a *FriendsApplyArgs) { a.Consent = false },
		"Please choose at most 2 t-shirts.": func(a *FriendsApplyArgs) {
			a.TShirts = append(a.TShirts, FriendsGarment{Fit: "m", Size: "S"})
		},
		"Please choose a fit and size for each of the hoodies.": func(a *FriendsApplyArgs) {
			a.Hoodies = []FriendsGarment{{Fit: "x", Size: "M"}}
		},
		"Please keep the notes under 4000 characters.": func(a *FriendsApplyArgs) { a.Notes = strings.Repeat("a", 4001) },
	}
	for want, mutate := range refusals {
		args := validFriendsApplyArgs()
		mutate(args)
		_, refusal := newFriendsApplication(args, applicationId)
		connect.AssertEqual(t, refusal, want)
	}

	for _, code := range []string{"CU", "IR", "KP", "SY", "RU", "BY"} {
		args := validFriendsApplyArgs()
		args.CountryCode = code
		_, refusal := newFriendsApplication(args, applicationId)
		connect.AssertEqual(t, refusal, "We can't ship nodes to that country under US export controls.")
	}
}

func TestFriendsApplicationSlackTextEscapes(t *testing.T) {
	args := validFriendsApplyArgs()
	args.Name = "<!channel> Ada"
	args.Notes = ""
	application, refusal := newFriendsApplication(args, server.NewId())
	connect.AssertEqual(t, refusal, "")

	text := friendsApplicationSlackText(application)
	connect.AssertEqual(t, strings.Contains(text, "<!channel>"), false)
	connect.AssertEqual(t, strings.Contains(text, "*Name:* &lt;!channel&gt; Ada"), true)
	connect.AssertEqual(t, strings.Contains(text, "*Connection:* Cellular (4G/5G)"), true)
	connect.AssertEqual(t, strings.Contains(text, "*Credit in reports:* Yes, as \"Ada L.\""), true)
	connect.AssertEqual(t, strings.Contains(text, "*Notes:*\n(none)"), true)
}

// The whole path, without redis or a database: a request (from an address the
// rate limit cannot classify, so no redis call is made) is validated and
// posted to the sales webhook from vault `sales.yml`. A filled honeypot
// answers the same and posts nothing.
func TestFriendsApplyPostsToTheSalesWebhook(t *testing.T) {
	posts := make(chan map[string]any, 4)
	slack := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		posts <- body
	}))
	defer slack.Close()

	defer salesWebhookConfigCache.Store(nil)
	salesWebhookConfigCache.Store(nil)
	pop := server.Vault.PushSimpleResource(salesWebhookVaultResource, []byte("slack_webhook_url: "+slack.URL+"/services/T0/B0/x\n"))
	defer pop()

	previousFactory := friendsNotifierFactory
	friendsNotifierFactory = func() *salesWebhookNotifier {
		notifier := previousFactory()
		notifier.httpClient = slack.Client()
		return notifier
	}
	defer func() { friendsNotifierFactory = previousFactory }()

	ctx := context.Background()
	clientSession := session.Testing_CreateClientSession(ctx, nil)
	clientSession.ClientAddress = ""

	result, err := FriendsApply(validFriendsApplyArgs(), clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error, (*FriendsApplyError)(nil))
	connect.AssertNotEqual(t, result.RequestId, (*server.Id)(nil))

	select {
	case body := <-posts:
		text, _ := body["text"].(string)
		connect.AssertEqual(t, strings.HasPrefix(text, "*New Friends of the Network application*"), true)
		connect.AssertEqual(t, strings.Contains(text, "*Email:* ada@example.com"), true)
		connect.AssertEqual(t, strings.Contains(text, "*Country:* GB"), true)
		connect.AssertEqual(t, strings.Contains(text, result.RequestId.String()), true)
		connect.AssertEqual(t, body["unfurl_links"], false)
	case <-time.After(10 * time.Second):
		t.Fatal("the application was not posted")
	}

	// a refusal and a honeypot post nothing
	args := validFriendsApplyArgs()
	args.Consent = false
	result, err = FriendsApply(args, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Error.Message, "Please agree to do your best to keep the node online.")

	args = validFriendsApplyArgs()
	args.Website = "http://spam.example"
	result, err = FriendsApply(args, clientSession)
	connect.AssertEqual(t, err, nil)
	connect.AssertNotEqual(t, result.RequestId, (*server.Id)(nil))

	select {
	case <-posts:
		t.Fatal("a refused or honeypot application was posted")
	case <-time.After(500 * time.Millisecond):
	}
}
