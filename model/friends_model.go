package model

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// Friends of the Network applications (POST /friends/apply).
//
// Volunteers apply to host a research Raspberry Pi node. The endpoint is
// public and works like the Services contact form (services_lead_model.go):
// the same per-address and global rate limit (the address is only a hash in
// redis, kept for the lookback), a honeypot that answers like a success, and
// validation refusals as 200 with `error.message`. A valid application is
// not stored: it is posted to the sales Slack channel through the sales
// webhook (vault `sales.yml`), which is where the team reviews it. Without
// that key the application is dropped, and the counter says so.

const (
	friendsMaxNameLength     = 256
	friendsMaxEmailLength    = 256
	friendsMaxNotesLength    = 4000
	friendsMaxShirtCount     = 2
	friendsMaxHoodieCount    = 2
	friendsAttemptsPerHour   = 5
	friendsAttemptsPerMinute = 60
)

var friendsRateLimitSettings = server.IpRateLimitAttemptSettings{
	KeyPrefix:       "friends_apply",
	AddressLookback: time.Hour,
	AddressLimit:    friendsAttemptsPerHour + 1,
	GlobalLookback:  time.Minute,
	GlobalLimit:     friendsAttemptsPerMinute + 1,
}

const friendsRateLimitHashTag = "friends_apply"

// The connection types the form offers.
var friendsConnectionTypes = map[string]string{
	"wifi":      "Home Wi-Fi",
	"ethernet":  "Ethernet",
	"cellular":  "Cellular (4G/5G)",
	"satellite": "Satellite",
	"other":     "Other",
}

var friendsGarmentFits = map[string]string{
	"m": "Men's",
	"f": "Women's",
}

var friendsGarmentSizes = map[string]bool{
	"XS": true, "S": true, "M": true, "L": true, "XL": true, "2XL": true, "3XL": true,
}

// Nodes ship under US export controls: no shipments to the comprehensively
// embargoed destinations, nor to Russia or Belarus. The form leaves these out;
// this is the server's copy of the same rule.
var friendsExcludedCountries = map[string]bool{
	"CU": true, "IR": true, "KP": true, "SY": true, "RU": true, "BY": true,
}

var friendsResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_friends_apply_total",
	Help: "Friends of the Network applications, by result.",
}, []string{"result"})

func init() {
	prometheus.MustRegister(friendsResults)
}

type FriendsGarment struct {
	// `m` or `f`
	Fit string `json:"fit"`
	// XS through 3XL
	Size string `json:"size"`
}

type FriendsApplyArgs struct {
	Name  string `json:"name"`
	Email string `json:"email"`
	// ISO 3166-1 alpha-2
	CountryCode    string `json:"country_code"`
	ConnectionType string `json:"connection_type"`
	// the volunteer agrees to do their best to keep the node online
	Consent bool `json:"consent"`
	// credit the volunteer in the research reports
	Credit     bool             `json:"credit,omitempty"`
	CreditName string           `json:"credit_name,omitempty"`
	TShirts    []FriendsGarment `json:"t_shirts,omitempty"`
	Hoodies    []FriendsGarment `json:"hoodies,omitempty"`
	Notes      string           `json:"notes,omitempty"`
	// honeypot: the form hides it, so a person leaves it empty
	Website string `json:"website,omitempty"`
}

type FriendsApplyError struct {
	Message string `json:"message"`
}

type FriendsApplyResult struct {
	RequestId *server.Id         `json:"request_id,omitempty"`
	Error     *FriendsApplyError `json:"error,omitempty"`
}

type friendsApplication struct {
	applicationId  server.Id
	name           string
	email          string
	countryCode    string
	connectionType string
	credit         bool
	creditName     string
	tShirts        []FriendsGarment
	hoodies        []FriendsGarment
	notes          string
}

func normalizeFriendsGarments(garments []FriendsGarment, max int, label string) ([]FriendsGarment, string) {
	if max < len(garments) {
		return nil, fmt.Sprintf("Please choose at most %d %s.", max, label)
	}
	out := []FriendsGarment{}
	for _, g := range garments {
		fit := strings.ToLower(strings.TrimSpace(g.Fit))
		size := strings.ToUpper(strings.TrimSpace(g.Size))
		if _, ok := friendsGarmentFits[fit]; !ok || !friendsGarmentSizes[size] {
			return nil, fmt.Sprintf("Please choose a fit and size for each of the %s.", label)
		}
		out = append(out, FriendsGarment{Fit: fit, Size: size})
	}
	return out, ""
}

// newFriendsApplication validates and normalizes a request. The message is
// the refusal shown to the person filling in the form.
func newFriendsApplication(args *FriendsApplyArgs, applicationId server.Id) (*friendsApplication, string) {
	name := strings.TrimSpace(args.Name)
	email := strings.TrimSpace(args.Email)
	countryCode := strings.ToUpper(strings.TrimSpace(args.CountryCode))
	connectionType := strings.ToLower(strings.TrimSpace(args.ConnectionType))
	creditName := strings.TrimSpace(args.CreditName)
	notes := strings.TrimSpace(args.Notes)

	if name == "" || friendsMaxNameLength < utf8.RuneCountInString(name) || !servicesLeadText(name, false) {
		return nil, "Please enter your name."
	}
	if friendsMaxEmailLength < utf8.RuneCountInString(email) || !servicesLeadText(email, false) {
		return nil, "Please enter a valid email address."
	}
	if parsed, err := mail.ParseAddress(email); err != nil || parsed.Address != email || !strings.Contains(email, "@") {
		return nil, "Please enter a valid email address."
	}
	if len(countryCode) != 2 || countryCode[0] < 'A' || 'Z' < countryCode[0] || countryCode[1] < 'A' || 'Z' < countryCode[1] {
		return nil, "Please choose your country."
	}
	if friendsExcludedCountries[countryCode] {
		return nil, "We can't ship nodes to that country under US export controls."
	}
	if _, ok := friendsConnectionTypes[connectionType]; !ok {
		return nil, "Please choose your network connection type."
	}
	if !args.Consent {
		return nil, "Please agree to do your best to keep the node online."
	}
	if !args.Credit {
		creditName = ""
	}
	if friendsMaxNameLength < utf8.RuneCountInString(creditName) || !servicesLeadText(creditName, false) {
		return nil, "Please shorten the name to credit."
	}
	tShirts, refusal := normalizeFriendsGarments(args.TShirts, friendsMaxShirtCount, "t-shirts")
	if refusal != "" {
		return nil, refusal
	}
	hoodies, refusal := normalizeFriendsGarments(args.Hoodies, friendsMaxHoodieCount, "hoodies")
	if refusal != "" {
		return nil, refusal
	}
	if friendsMaxNotesLength < utf8.RuneCountInString(notes) || !servicesLeadText(notes, true) {
		return nil, fmt.Sprintf("Please keep the notes under %d characters.", friendsMaxNotesLength)
	}

	return &friendsApplication{
		applicationId:  applicationId,
		name:           name,
		email:          email,
		countryCode:    countryCode,
		connectionType: connectionType,
		credit:         args.Credit,
		creditName:     creditName,
		tShirts:        tShirts,
		hoodies:        hoodies,
		notes:          notes,
	}, ""
}

// FriendsApply posts a Friends of the Network application to the team.
func FriendsApply(apply *FriendsApplyArgs, clientSession *session.ClientSession) (*FriendsApplyResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()

	if err := checkFriendsRateLimit(ctx, clientSession, now); err != nil {
		return nil, err
	}

	applicationId := server.NewId()
	if strings.TrimSpace(apply.Website) != "" {
		// answer like a success so a bot learns nothing
		friendsResults.WithLabelValues("honeypot").Inc()
		return &FriendsApplyResult{RequestId: &applicationId}, nil
	}

	application, refusal := newFriendsApplication(apply, applicationId)
	if refusal != "" {
		friendsResults.WithLabelValues("invalid").Inc()
		return &FriendsApplyResult{Error: &FriendsApplyError{Message: refusal}}, nil
	}
	friendsResults.WithLabelValues("accepted").Inc()

	go server.HandleError(func() {
		notifyFriendsApplication(application)
	})

	return &FriendsApplyResult{RequestId: &applicationId}, nil
}

// checkFriendsRateLimit counts every request. As with the Services form, a
// session without a classifiable address or a redis error is let through.
func checkFriendsRateLimit(ctx context.Context, clientSession *session.ClientSession, now time.Time) error {
	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		return nil
	}
	_, allowed, err := server.CheckIpRateLimitAttempt(
		ctx,
		rateLimitClient,
		friendsRateLimitHashTag,
		now,
		friendsRateLimitSettings,
	)
	if err != nil {
		friendsResults.WithLabelValues("rate_limit_error").Inc()
		glog.V(1).Infof("[friends]rate limit check failed: %s\n", err)
		return nil
	}
	if !allowed {
		friendsResults.WithLabelValues("rate_limited").Inc()
		return &rateLimitError{
			message: "429 Too many requests. Please try again later.",
		}
	}
	return nil
}

func friendsGarmentsText(garments []FriendsGarment) string {
	if len(garments) == 0 {
		return "(none)"
	}
	parts := []string{}
	for _, g := range garments {
		parts = append(parts, fmt.Sprintf("%s %s", friendsGarmentFits[g.Fit], g.Size))
	}
	return strings.Join(parts, ", ")
}

func friendsApplicationSlackText(application *friendsApplication) string {
	credit := "No"
	if application.credit {
		credit = "Yes"
		if application.creditName != "" {
			credit = fmt.Sprintf("Yes, as \"%s\"", slackEscape(application.creditName))
		}
	}
	notes := slackEscape(application.notes)
	if notes == "" {
		notes = "(none)"
	}
	return fmt.Sprintf(
		`*New Friends of the Network application*
*Name:* %s
*Email:* %s
*Country:* %s
*Connection:* %s
*Keep online:* Agreed
*Credit in reports:* %s
*T-shirts:* %s
*Hoodies:* %s
*Notes:*
%s
*Application:* %s`,
		slackEscape(application.name),
		slackEscape(application.email),
		application.countryCode,
		friendsConnectionTypes[application.connectionType],
		credit,
		friendsGarmentsText(application.tShirts),
		friendsGarmentsText(application.hoodies),
		notes,
		application.applicationId,
	)
}

// friendsNotifierFactory builds the notifier the application posts with.
// Tests replace it to reach a local webhook.
var friendsNotifierFactory = newSalesWebhookNotifier

func notifyFriendsApplication(application *friendsApplication) {
	webhookUrl := salesWebhookUrl(server.NowUtc())
	if webhookUrl == "" {
		friendsResults.WithLabelValues("unconfigured").Inc()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := friendsNotifierFactory().Notify(ctx, webhookUrl, friendsApplicationSlackText(application)); err != nil {
		friendsResults.WithLabelValues("notify_error").Inc()
		// logged without the url or the application
		glog.Infof("[friends]application %s slack notify failed: %s\n", application.applicationId, err)
		return
	}
	friendsResults.WithLabelValues("notified").Inc()
}
