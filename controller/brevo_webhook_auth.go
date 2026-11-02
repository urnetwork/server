// SPDX-License-Identifier: MPL-2.0

package controller

// How a Brevo webhook authenticates to POST /updates/brevo, and how
// `bringyourctl onboarding webhook-ensure` registers the transactional webhook
// so that it does.
//
// Brevo authenticates a webhook call in one of three ways
// (https://developers.brevo.com/docs/secured-webhooks): Basic credentials as
// the webhook url's userinfo ("https://username:password@host/path"), sent as
// "Authorization: Basic base64(username:password)"; a bearer token (`auth`
// {"type": "bearer", "token": ...}); or custom `headers`, a list of
// {key, value} sent with every call. Create (POST /v3/webhooks) and update
// (PUT /v3/webhooks/{webhookId}) take all three, and the list
// (GET /v3/webhooks) returns url, auth and headers. POST /updates/brevo
// accepts only Basic: a vault brevo.yml brevo.webhook_bearers user with an
// empty password (brevoWebhookExpectedAuths). A webhook registered without it
// is refused with 401 before any event is recorded, and Brevo discards an event
// refused with a 4xx other than 429 without a retry
// (https://developers.brevo.com/docs/retry-mechanism): the event is lost.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
)

// ----- the inbound check -----

// The results of POST /updates/brevo: accepted, or why it was refused before
// any event was recorded.
const (
	brevoWebhookAccepted = "accepted"
	// the request has no Authorization header
	brevoWebhookRefusedNoCredential = "no_credential"
	// no Authorization value is a brevo.webhook_bearers user with an empty
	// password
	brevoWebhookRefusedWrongCredential = "wrong_credential"
	// brevo.yml names no usable user, so nothing can be accepted
	brevoWebhookRefusedVerificationUnavailable = "verification_unavailable"
)

var brevoWebhookRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "brevo",
	Name:      "webhook_requests_total",
	Help:      "POST /updates/brevo requests by result: accepted, or refused before any event is recorded for no_credential (no Authorization header), wrong_credential (no Authorization value is a brevo.webhook_bearers user with an empty password) or verification_unavailable (brevo.yml names no usable user). Brevo does not retry a refused event.",
}, []string{"result"})

func init() {
	prometheus.MustRegister(brevoWebhookRequestsTotal)
	// each result exists from the start, so its first request is an increase
	for _, result := range []string{
		brevoWebhookAccepted,
		brevoWebhookRefusedNoCredential,
		brevoWebhookRefusedWrongCredential,
		brevoWebhookRefusedVerificationUnavailable,
	} {
		brevoWebhookRequestsTotal.WithLabelValues(result)
	}
}

// The refusal of a webhook without an accepted credential.
var errBrevoWebhookNotAuthorized = fmt.Errorf("%d Not authorized.", http.StatusUnauthorized)

// A misregistered webhook is refused on every event: one line a minute says so.
var brevoWebhookRefusalReports = newBoundedReport(time.Minute)

// verifyBrevoWebhookAuthorization is nil when one of the request's
// Authorization values is an accepted credential. Otherwise it is the refusal,
// counted by reason and logged at most once a minute. It fails closed: without
// a usable configured user nothing is accepted.
func verifyBrevoWebhookAuthorization(header map[string][]string) error {
	expectedAuths, err := brevoWebhookExpectedAuths()
	if err != nil {
		// the error names the path, never a value
		reportBrevoWebhookRefusal(
			brevoWebhookRefusedVerificationUnavailable,
			fmt.Sprintf("the verification users are unavailable (%s); fix vault brevo.yml brevo.webhook_bearers", err),
		)
		return errWebhookVerificationUnavailable
	}
	auths := header["Authorization"]
	for _, auth := range auths {
		if expectedAuths[strings.ToLower(auth)] {
			brevoWebhookRequestsTotal.WithLabelValues(brevoWebhookAccepted).Inc()
			return nil
		}
	}
	if !slices.ContainsFunc(auths, func(auth string) bool { return strings.TrimSpace(auth) != "" }) {
		reportBrevoWebhookRefusal(
			brevoWebhookRefusedNoCredential,
			"the request has no Authorization header; register the credential with `bringyourctl onboarding webhook-ensure`",
		)
	} else {
		reportBrevoWebhookRefusal(
			brevoWebhookRefusedWrongCredential,
			fmt.Sprintf(
				"its Authorization (%s) is no brevo.webhook_bearers user with an empty password; register the credential with `bringyourctl onboarding webhook-ensure`",
				brevoAuthorizationSchemes(auths),
			),
		)
	}
	return errBrevoWebhookNotAuthorized
}

func reportBrevoWebhookRefusal(reason string, detail string) {
	brevoWebhookRequestsTotal.WithLabelValues(reason).Inc()
	if suppressedCount, ok := brevoWebhookRefusalReports.Allow(server.NowUtc()); ok {
		glog.Warningf(
			"[product_updates]brevo webhook refused (%s): %s. Brevo does not retry a refused event. %d more refusals since the last report (urnetwork_brevo_webhook_requests_total).\n",
			reason,
			detail,
			suppressedCount,
		)
	}
}

// The schemes of Authorization values as fixed labels: a refused value is
// never logged.
func brevoAuthorizationSchemes(auths []string) string {
	schemes := []string{}
	for _, auth := range auths {
		scheme, _, _ := strings.Cut(strings.TrimSpace(auth), " ")
		label := "other"
		switch strings.ToLower(scheme) {
		case "basic":
			label = "Basic"
		case "bearer":
			label = "Bearer"
		}
		if !slices.Contains(schemes, label) {
			schemes = append(schemes, label)
		}
	}
	return strings.Join(schemes, ", ")
}

// ----- the registration -----

// How the registered webhook carries the credential.
type brevoWebhookCredentialCarrier string

const (
	// Brevo's username and password authentication: the user, with an empty
	// password, as the url's userinfo ("https://<user>:@host/path"). The
	// default.
	brevoWebhookCredentialUrl brevoWebhookCredentialCarrier = "url"
	// a custom Authorization header (`headers`) with exactly the Basic value
	// POST /updates/brevo accepts; the url carries no credential
	brevoWebhookCredentialHeader brevoWebhookCredentialCarrier = "header"
)

// "" is the url.
func parseBrevoWebhookCredentialCarrier(value string) (brevoWebhookCredentialCarrier, error) {
	switch carrier := brevoWebhookCredentialCarrier(strings.ToLower(strings.TrimSpace(value))); carrier {
	case "":
		return brevoWebhookCredentialUrl, nil
	case brevoWebhookCredentialUrl, brevoWebhookCredentialHeader:
		return carrier, nil
	}
	return "", fmt.Errorf("unknown credential carrier %q: use url or header", value)
}

// The credential the registered webhook carries.
type brevoWebhookCredential struct {
	// the first usable brevo.webhook_bearers user, verbatim
	user string
	// "Basic base64(<user>:)", one of expectedAuths
	authorization string
	expectedAuths map[string]bool
}

// The first usable brevo.webhook_bearers user, as a Basic value that
// brevoWebhookExpectedAuths accepts exactly.
func loadBrevoWebhookCredential() (*brevoWebhookCredential, error) {
	expectedAuths, err := brevoWebhookExpectedAuths()
	if err != nil {
		return nil, err
	}
	brevo, err := server.Vault.SimpleResource("brevo.yml")
	if err != nil {
		return nil, err
	}
	// brevoWebhookExpectedAuths parsed the resource
	for _, user := range brevo.String("brevo", "webhook_bearers") {
		if strings.TrimSpace(user) == "" {
			continue
		}
		authorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(user+":"))
		if expectedAuths[strings.ToLower(authorization)] {
			return &brevoWebhookCredential{
				user:          user,
				authorization: authorization,
				expectedAuths: expectedAuths,
			}, nil
		}
	}
	return nil, errors.New("brevo.yml: brevo.webhook_bearers names no user")
}

// The user, masked for output.
func (self *brevoWebhookCredential) maskedUser() string {
	return server.MaskValue(self.user)
}

// Removes the credential, and the userinfo of any url, from text Brevo
// returned, and bounds it.
func (self *brevoWebhookCredential) redact(text string) string {
	text = brevoUserinfoPattern.ReplaceAllString(text, "${1}***@")
	for _, secret := range []string{self.authorization[len("Basic "):], self.user} {
		text = strings.ReplaceAll(text, secret, "***")
	}
	if runes := []rune(text); 200 < len(runes) {
		text = string(runes[:200]) + "..."
	}
	return text
}

var brevoUserinfoPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@]*@`)

// A url credential that every HTTP client sends as the same Basic value: a
// user of RFC 3986 unreserved characters, which no client escapes or decodes.
func brevoWebhookUrlSafeUser(user string) bool {
	for _, c := range user {
		switch {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9', c == '-', c == '.', c == '_', c == '~':
		default:
			return false
		}
	}
	return user != ""
}

// A webhook as GET /v3/webhooks and GET /v3/webhooks/{webhookId} return it.
// auth and headers are read leniently (brevoWebhookAuthType,
// brevoWebhookHeaders): their item schemas are undocumented.
type brevoWebhook struct {
	Id          int64           `json:"id"`
	Url         string          `json:"url"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	Events      []string        `json:"events"`
	Batched     bool            `json:"batched"`
	Auth        json.RawMessage `json:"auth"`
	Headers     json.RawMessage `json:"headers"`
}

type brevoWebhookListResult struct {
	Webhooks []*brevoWebhook `json:"webhooks"`
	Code     string          `json:"code"`
	Message  string          `json:"message"`
}

type brevoWebhookGetResult struct {
	brevoWebhook
	Code    string `json:"code"`
	Message string `json:"message"`
}

type brevoWebhookHeader struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type brevoWebhookCreateArgs struct {
	Url         string               `json:"url"`
	Description string               `json:"description"`
	Events      []string             `json:"events"`
	Type        string               `json:"type"`
	Batched     bool                 `json:"batched"`
	Headers     []brevoWebhookHeader `json:"headers,omitempty"`
}

// The fields an update sets. Description and headers are sent back as they
// should stay, so an update that replaces the webhook rather than merging
// keeps them.
type brevoWebhookUpdateArgs struct {
	Url         string                `json:"url"`
	Description string                `json:"description,omitempty"`
	Events      []string              `json:"events"`
	Batched     bool                  `json:"batched"`
	Headers     *[]brevoWebhookHeader `json:"headers,omitempty"`
}

type brevoWebhookIdResult struct {
	Id      int64  `json:"id"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

const brevoWebhookDescription = "URnetwork onboarding campaign (server-registered)"

// The event names create and update accept, to send back an existing event in
// the spelling they take.
var brevoWebhookEventNames = []string{
	"sent", "request", "delivered", "hardBounce", "softBounce", "blocked", "spam", "invalid", "deferred",
	"click", "opened", "uniqueOpened", "unsubscribed", "listAddition", "contactUpdated", "contactDeleted",
	"inboundEmailProcessed", "reply",
}

// Brevo's event names compared without case or underscores (hardBounce,
// hard_bounce).
func brevoWebhookEventKey(event string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(event), "_", ""))
}

func brevoWebhookEventName(event string) string {
	for _, name := range brevoWebhookEventNames {
		if brevoWebhookEventKey(name) == brevoWebhookEventKey(event) {
			return name
		}
	}
	return event
}

// The custom headers of a listed webhook: the documented list of
// {key, value}, or an object of key: value. ok is false for anything else.
func brevoWebhookHeaders(raw json.RawMessage) (headers []brevoWebhookHeader, ok bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, true
	}
	if err := json.Unmarshal(trimmed, &headers); err == nil {
		return headers, true
	}
	var object map[string]string
	if err := json.Unmarshal(trimmed, &object); err == nil {
		headers = nil
		for _, key := range slices.Sorted(maps.Keys(object)) {
			headers = append(headers, brevoWebhookHeader{Key: key, Value: object[key]})
		}
		return headers, true
	}
	return nil, false
}

// Whether a listed webhook has `auth` set, and its type for output.
func brevoWebhookAuthType(raw json.RawMessage) (authType string, set bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return "", false
	}
	var auth struct {
		Type  string `json:"type"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal(trimmed, &auth); err != nil {
		return "unreadable", true
	}
	if auth.Type == "" && auth.Token == "" {
		return "", false
	}
	return brevoSafeLabel(auth.Type), true
}

var brevoSafeLabelPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_ .-]{0,63}$`)

// A provider-controlled label, printed only when it is a plain word.
func brevoSafeLabel(label string) string {
	if brevoSafeLabelPattern.MatchString(label) {
		return label
	}
	return "unclassified"
}

func brevoWebhookIsAuthorizationHeader(header brevoWebhookHeader) bool {
	return strings.EqualFold(strings.TrimSpace(header.Key), "Authorization")
}

// The configured webhook url: https, without credentials. The credential
// comes from the vault, never from config.
func parseBrevoWebhookTarget(webhookUrl string) (*url.URL, error) {
	target, err := url.Parse(strings.TrimSpace(webhookUrl))
	// the url is never printed: it could carry a credential
	if err != nil {
		return nil, errors.New("onboarding.brevo.webhook_url is not a url")
	}
	if !strings.EqualFold(target.Scheme, "https") || target.Host == "" {
		return nil, errors.New("onboarding.brevo.webhook_url must be an https url")
	}
	if target.User != nil {
		return nil, errors.New("onboarding.brevo.webhook_url must not carry credentials: the credential is vault brevo.yml brevo.webhook_bearers")
	}
	target.Scheme = "https"
	target.Host = strings.ToLower(target.Host)
	return target, nil
}

// The host with the scheme's default port removed.
func brevoWebhookHost(u *url.URL) string {
	host := strings.ToLower(u.Hostname())
	switch port := u.Port(); {
	case port == "",
		port == "443" && strings.EqualFold(u.Scheme, "https"),
		port == "80" && strings.EqualFold(u.Scheme, "http"):
		return host
	default:
		return host + ":" + port
	}
}

// Whether a listed webhook posts to the target endpoint: the same host and
// path, whatever credentials, scheme, case or trailing slash its url carries.
func brevoWebhookSameEndpoint(listed *url.URL, target *url.URL) bool {
	return brevoWebhookHost(listed) == brevoWebhookHost(target) &&
		strings.EqualFold(strings.TrimRight(listed.Path, "/"), strings.TrimRight(target.Path, "/"))
}

// A webhook url for output: its userinfo masked, so no credential is printed.
func brevoWebhookRedactedUrl(u *url.URL) string {
	withoutUser := *u
	withoutUser.User = nil
	text := withoutUser.String()
	if u.User == nil {
		return text
	}
	userinfo := server.MaskValue(u.User.Username())
	if password, set := u.User.Password(); set {
		if password == "" {
			userinfo += ":"
		} else {
			userinfo += ":***"
		}
	}
	scheme, rest, ok := strings.Cut(text, "://")
	if !ok {
		return "(a url with credentials)"
	}
	return scheme + "://" + userinfo + "@" + rest
}

type brevoCredentialState int

const (
	brevoCredentialNone brevoCredentialState = iota
	brevoCredentialAccepted
	brevoCredentialOther
)

// The credential a url's userinfo carries is what a client sends for it,
// "Basic base64(<user>:<password>)": a user with an empty or absent password.
func brevoWebhookUrlCredential(listed *url.URL, expectedAuths map[string]bool) brevoCredentialState {
	if listed.User == nil {
		return brevoCredentialNone
	}
	password, _ := listed.User.Password()
	authorization := "Basic " + base64.StdEncoding.EncodeToString([]byte(listed.User.Username()+":"+password))
	if expectedAuths[strings.ToLower(authorization)] {
		return brevoCredentialAccepted
	}
	return brevoCredentialOther
}

// The credential the custom Authorization headers carry: accepted only when
// every one of them is.
func brevoWebhookHeaderCredential(headers []brevoWebhookHeader, expectedAuths map[string]bool) brevoCredentialState {
	state := brevoCredentialNone
	for _, header := range headers {
		if !brevoWebhookIsAuthorizationHeader(header) {
			continue
		}
		if !expectedAuths[strings.ToLower(strings.TrimSpace(header.Value))] {
			return brevoCredentialOther
		}
		state = brevoCredentialAccepted
	}
	return state
}

func describeBrevoWebhookCredentials(urlCredential brevoCredentialState, headerCredential brevoCredentialState) string {
	parts := []string{}
	switch urlCredential {
	case brevoCredentialAccepted:
		parts = append(parts, "an accepted credential in the url")
	case brevoCredentialOther:
		parts = append(parts, "a credential in the url that this server does not accept")
	}
	switch headerCredential {
	case brevoCredentialAccepted:
		parts = append(parts, "an accepted Authorization header")
	case brevoCredentialOther:
		parts = append(parts, "an Authorization header that this server does not accept")
	}
	if len(parts) == 0 {
		return "no credential"
	}
	return strings.Join(parts, " and ")
}

// One listed webhook at the endpoint and the state the carrier wants it in:
// exactly one accepted credential, every wanted event, and no batching, since
// POST /updates/brevo reads one event per request.
type brevoWebhookPlan struct {
	webhook          *brevoWebhook
	listedUrl        *url.URL
	listedHeaders    []brevoWebhookHeader
	urlCredential    brevoCredentialState
	headerCredential brevoCredentialState

	url           *url.URL
	headers       []brevoWebhookHeader
	events        []string
	missingEvents []string
}

func planBrevoWebhook(
	webhook *brevoWebhook,
	listedUrl *url.URL,
	listedHeaders []brevoWebhookHeader,
	target *url.URL,
	carrier brevoWebhookCredentialCarrier,
	credential *brevoWebhookCredential,
	events []string,
) *brevoWebhookPlan {
	plan := &brevoWebhookPlan{
		webhook:          webhook,
		listedUrl:        listedUrl,
		listedHeaders:    listedHeaders,
		urlCredential:    brevoWebhookUrlCredential(listedUrl, credential.expectedAuths),
		headerCredential: brevoWebhookHeaderCredential(listedHeaders, credential.expectedAuths),
	}

	desiredUrl := *target
	headers := []brevoWebhookHeader{}
	for _, header := range listedHeaders {
		if !brevoWebhookIsAuthorizationHeader(header) {
			headers = append(headers, header)
		}
	}
	switch carrier {
	case brevoWebhookCredentialHeader:
		if plan.headerCredential == brevoCredentialAccepted {
			headers = listedHeaders
		} else {
			headers = append(headers, brevoWebhookHeader{Key: "Authorization", Value: credential.authorization})
		}
	default:
		if plan.urlCredential == brevoCredentialAccepted {
			// another accepted user is as good as the first
			desiredUrl.User = listedUrl.User
		} else {
			desiredUrl.User = url.UserPassword(credential.user, "")
		}
	}
	plan.url = &desiredUrl
	plan.headers = headers

	present := map[string]bool{}
	for _, event := range webhook.Events {
		present[brevoWebhookEventKey(event)] = true
		plan.events = append(plan.events, brevoWebhookEventName(event))
	}
	for _, event := range events {
		if !present[brevoWebhookEventKey(event)] {
			present[brevoWebhookEventKey(event)] = true
			plan.missingEvents = append(plan.missingEvents, event)
			plan.events = append(plan.events, event)
		}
	}
	return plan
}

func (self *brevoWebhookPlan) urlChanged() bool {
	return self.url.String() != self.listedUrl.String()
}

func (self *brevoWebhookPlan) headersChanged() bool {
	return !slices.Equal(self.headers, self.listedHeaders)
}

func (self *brevoWebhookPlan) done() bool {
	return !self.urlChanged() && !self.headersChanged() && len(self.missingEvents) == 0 && !self.webhook.Batched
}

// What still differs from the wanted state, for output.
func (self *brevoWebhookPlan) gaps() []string {
	gaps := []string{}
	if self.urlChanged() || self.headersChanged() {
		gaps = append(gaps, fmt.Sprintf("it shows %s", describeBrevoWebhookCredentials(self.urlCredential, self.headerCredential)))
	}
	if 0 < len(self.missingEvents) {
		gaps = append(gaps, fmt.Sprintf("it lacks the events %s", strings.Join(self.missingEvents, ", ")))
	}
	if self.webhook.Batched {
		gaps = append(gaps, "it is batched")
	}
	return gaps
}

// The masked user of the credential the plan leaves on the webhook, which may
// be another accepted user than the first.
func (self *brevoWebhookPlan) maskedUser(carrier brevoWebhookCredentialCarrier) string {
	if carrier == brevoWebhookCredentialHeader {
		for _, header := range self.headers {
			if !brevoWebhookIsAuthorizationHeader(header) {
				continue
			}
			scheme, encoded, _ := strings.Cut(strings.TrimSpace(header.Value), " ")
			if decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded)); err == nil && strings.EqualFold(scheme, "Basic") {
				user, _, _ := strings.Cut(string(decoded), ":")
				return server.MaskValue(user)
			}
		}
		return "***"
	}
	if self.url.User == nil {
		return "***"
	}
	return server.MaskValue(self.url.User.Username())
}

func describeBrevoWebhookCarrier(carrier brevoWebhookCredentialCarrier, maskedUser string) string {
	if carrier == brevoWebhookCredentialHeader {
		return fmt.Sprintf("Basic, user %s with an empty password, in a custom Authorization header", maskedUser)
	}
	return fmt.Sprintf("Basic, user %s with an empty password, in the url", maskedUser)
}

// ensureBrevoTransactionalWebhook makes one transactional webhook post `events`
// to webhookUrl with a credential POST /updates/brevo accepts, carried as
// `carrier` says. It recognizes the webhook by host and path whatever
// credentials its url carries, updates it in place when it lacks the
// credential, an event or has batching on, and creates it when there is none.
// It changes nothing else: a webhook of another type at the endpoint, a
// duplicate, or `auth` set is reported, not touched. Returns what it did; the
// credential is never printed.
func ensureBrevoTransactionalWebhook(
	ctx context.Context,
	webhookUrl string,
	events []string,
	carrier brevoWebhookCredentialCarrier,
) (string, error) {
	target, err := parseBrevoWebhookTarget(webhookUrl)
	if err != nil {
		return "", err
	}
	credential, err := loadBrevoWebhookCredential()
	if err != nil {
		// names the path, never a value
		return "", fmt.Errorf("no webhook credential: %w", err)
	}
	if carrier == brevoWebhookCredentialUrl && !brevoWebhookUrlSafeUser(credential.user) {
		return "", fmt.Errorf("the first brevo.webhook_bearers user has characters a url credential would escape: rerun with --credential=header")
	}

	status, list, err := server.HttpGetWithStatus[brevoWebhookListResult](
		ctx,
		brevoApiUrl("webhooks?type=transactional"),
		brevoHeader,
		brevoResponseJsonObject[brevoWebhookListResult],
	)
	if err != nil {
		return "", fmt.Errorf("list webhooks: %s", brevoWebhookFailure(status, "", "", credential, err))
	}
	if status != nil && status.Code != http.StatusNotFound && (status.Code < 200 || 300 <= status.Code) {
		return "", fmt.Errorf("list webhooks: %s", brevoWebhookFailure(status, list.Code, list.Message, credential, nil))
	}

	notes := []string{}
	plans := []*brevoWebhookPlan{}
	for _, webhook := range list.Webhooks {
		if webhook == nil {
			continue
		}
		listedUrl, err := url.Parse(strings.TrimSpace(webhook.Url))
		if err != nil || !brevoWebhookSameEndpoint(listedUrl, target) {
			continue
		}
		// the list asks for transactional webhooks; one without a type is one
		if webhookType := strings.TrimSpace(webhook.Type); webhookType != "" && !strings.EqualFold(webhookType, "transactional") {
			notes = append(notes, fmt.Sprintf(
				"note: webhook %d (%s) also posts to this endpoint; it is not the transactional webhook and is left as it is",
				webhook.Id,
				brevoSafeLabel(webhookType),
			))
			continue
		}
		listedHeaders, ok := brevoWebhookHeaders(webhook.Headers)
		if !ok {
			return "", fmt.Errorf("webhook %d: Brevo lists headers this command cannot read; fix the webhook in the Brevo console", webhook.Id)
		}
		plans = append(plans, planBrevoWebhook(webhook, listedUrl, listedHeaders, target, carrier, credential, events))
	}

	if len(plans) == 0 {
		return createBrevoTransactionalWebhook(ctx, target, events, carrier, credential, notes)
	}

	// the webhook that already has everything, else the first listed
	primary := plans[0]
	if index := slices.IndexFunc(plans, (*brevoWebhookPlan).done); 0 <= index {
		primary = plans[index]
	}
	for _, plan := range plans {
		if plan == primary {
			continue
		}
		refused := ""
		if plan.urlCredential != brevoCredentialAccepted && plan.headerCredential != brevoCredentialAccepted {
			refused = ", and it carries no accepted credential, so each of its copies is refused (urnetwork_brevo_webhook_requests_total)"
		}
		notes = append(notes, fmt.Sprintf(
			"warning: webhook %d %s also posts transactional events to this endpoint, so each event arrives twice%s; it is left as it is: delete it in the Brevo console unless it is wanted",
			plan.webhook.Id,
			brevoWebhookRedactedUrl(plan.listedUrl),
			refused,
		))
	}
	if authType, set := brevoWebhookAuthType(primary.webhook.Auth); set {
		notes = append(notes, fmt.Sprintf(
			"warning: webhook %d also has auth (type %s): Brevo may send it as the Authorization header in place of the credential; remove it in the Brevo console",
			primary.webhook.Id,
			authType,
		))
	}

	if primary.done() {
		lines := []string{
			fmt.Sprintf("exists: transactional webhook %d %s", primary.webhook.Id, brevoWebhookRedactedUrl(primary.listedUrl)),
			fmt.Sprintf("  credential: %s", describeBrevoWebhookCarrier(carrier, primary.maskedUser(carrier))),
			fmt.Sprintf("  events: all %d present", len(events)),
		}
		return strings.Join(append(lines, notes...), "\n"), nil
	}
	return updateBrevoTransactionalWebhook(ctx, primary, target, events, carrier, credential, notes)
}

func createBrevoTransactionalWebhook(
	ctx context.Context,
	target *url.URL,
	events []string,
	carrier brevoWebhookCredentialCarrier,
	credential *brevoWebhookCredential,
	notes []string,
) (string, error) {
	webhookUrl := *target
	created := &brevoWebhookCreateArgs{
		Description: brevoWebhookDescription,
		Events:      events,
		Type:        "transactional",
		Batched:     false,
	}
	if carrier == brevoWebhookCredentialHeader {
		created.Headers = []brevoWebhookHeader{{Key: "Authorization", Value: credential.authorization}}
	} else {
		webhookUrl.User = url.UserPassword(credential.user, "")
	}
	created.Url = webhookUrl.String()

	status, r, err := server.HttpPostWithStatus[brevoWebhookIdResult](
		ctx,
		brevoApiUrl("webhooks"),
		created,
		brevoHeader,
		brevoResponseJsonObject[brevoWebhookIdResult],
	)
	if err != nil {
		return "", fmt.Errorf("create webhook: %s", brevoWebhookFailure(status, "", "", credential, err))
	}
	if status == nil || status.Code < 200 || 300 <= status.Code {
		return "", fmt.Errorf("create webhook: %s", brevoWebhookFailure(status, r.Code, r.Message, credential, nil))
	}
	lines := []string{
		fmt.Sprintf("created: transactional webhook %d %s", r.Id, brevoWebhookRedactedUrl(&webhookUrl)),
		fmt.Sprintf("  credential: %s", describeBrevoWebhookCarrier(carrier, credential.maskedUser())),
		fmt.Sprintf("  events: %s", strings.Join(events, ", ")),
		checkBrevoTransactionalWebhook(ctx, r.Id, target, events, carrier, credential),
	}
	return strings.Join(append(lines, notes...), "\n"), nil
}

func updateBrevoTransactionalWebhook(
	ctx context.Context,
	plan *brevoWebhookPlan,
	target *url.URL,
	events []string,
	carrier brevoWebhookCredentialCarrier,
	credential *brevoWebhookCredential,
	notes []string,
) (string, error) {
	updated := &brevoWebhookUpdateArgs{
		Url:         plan.url.String(),
		Description: plan.webhook.Description,
		Events:      plan.events,
		Batched:     false,
	}
	if updated.Description == "" {
		updated.Description = brevoWebhookDescription
	}
	if plan.headersChanged() || 0 < len(plan.headers) {
		updated.Headers = &plan.headers
	}
	status, r, err := brevoWebhookPut(ctx, plan.webhook.Id, updated)
	if err != nil {
		return "", fmt.Errorf("update webhook %d: %s", plan.webhook.Id, brevoWebhookFailure(status, "", "", credential, err))
	}
	if status == nil || status.Code < 200 || 300 <= status.Code {
		return "", fmt.Errorf("update webhook %d: %s", plan.webhook.Id, brevoWebhookFailure(status, r.Code, r.Message, credential, nil))
	}

	lines := []string{
		fmt.Sprintf("updated: transactional webhook %d %s", plan.webhook.Id, brevoWebhookRedactedUrl(plan.url)),
	}
	if plan.urlChanged() || plan.headersChanged() {
		lines = append(lines, fmt.Sprintf(
			"  credential: now %s (it had %s)",
			describeBrevoWebhookCarrier(carrier, plan.maskedUser(carrier)),
			describeBrevoWebhookCredentials(plan.urlCredential, plan.headerCredential),
		))
	} else {
		lines = append(lines, fmt.Sprintf("  credential: %s, unchanged", describeBrevoWebhookCarrier(carrier, plan.maskedUser(carrier))))
	}
	if 0 < len(plan.missingEvents) {
		lines = append(lines, fmt.Sprintf("  events: added %s", strings.Join(plan.missingEvents, ", ")))
	} else {
		lines = append(lines, fmt.Sprintf("  events: all %d present", len(events)))
	}
	if plan.webhook.Batched {
		lines = append(lines, "  batching: turned off (POST /updates/brevo reads one event per request)")
	}
	lines = append(lines, checkBrevoTransactionalWebhook(ctx, plan.webhook.Id, target, events, carrier, credential))
	return strings.Join(append(lines, notes...), "\n"), nil
}

// Reads the webhook back and says whether Brevo's copy is in the wanted state.
// A difference is a warning, not a failure: the change was made, and a test
// send settles whether the credential arrives.
func checkBrevoTransactionalWebhook(
	ctx context.Context,
	id int64,
	target *url.URL,
	events []string,
	carrier brevoWebhookCredentialCarrier,
	credential *brevoWebhookCredential,
) string {
	status, r, err := server.HttpGetWithStatus[brevoWebhookGetResult](
		ctx,
		brevoApiUrl(fmt.Sprintf("webhooks/%d", id)),
		brevoHeader,
		brevoResponseJsonObject[brevoWebhookGetResult],
	)
	if err != nil {
		return fmt.Sprintf("  warning: could not read webhook %d back: %s; confirm with a test send", id, brevoWebhookFailure(status, "", "", credential, err))
	}
	if status == nil || status.Code < 200 || 300 <= status.Code {
		return fmt.Sprintf("  warning: could not read webhook %d back: %s; confirm with a test send", id, brevoWebhookFailure(status, r.Code, r.Message, credential, nil))
	}
	listedUrl, err := url.Parse(strings.TrimSpace(r.Url))
	listedHeaders, ok := brevoWebhookHeaders(r.Headers)
	if err != nil || !ok || !brevoWebhookSameEndpoint(listedUrl, target) {
		return fmt.Sprintf("  warning: Brevo's copy of webhook %d does not read as this endpoint; confirm with a test send", id)
	}
	plan := planBrevoWebhook(&r.brevoWebhook, listedUrl, listedHeaders, target, carrier, credential, events)
	if plan.done() {
		return "  check: Brevo's copy carries the credential and every event, unbatched"
	}
	return fmt.Sprintf(
		"  warning: Brevo's copy of webhook %d differs (%s); Brevo may hide a url credential, so confirm with a test send, and if its events are refused rerun with the other --credential",
		id,
		strings.Join(plan.gaps(), "; "),
	)
}

// PUT /v3/webhooks/{webhookId}: 204 with no body on success. The server
// package has no PUT helper.
func brevoWebhookPut(ctx context.Context, id int64, args *brevoWebhookUpdateArgs) (*server.Status, brevoWebhookIdResult, error) {
	var empty brevoWebhookIdResult
	body, err := json.Marshal(args)
	if err != nil {
		return nil, empty, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, brevoApiUrl(fmt.Sprintf("webhooks/%d", id)), bytes.NewReader(body))
	if err != nil {
		return nil, empty, err
	}
	request.Header.Add("Content-Type", "application/json")
	brevoHeader(request.Header)
	response, err := server.DefaultHttpClient().Do(request)
	if err != nil {
		return nil, empty, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		return server.NewStatusFromResponse(response), empty, err
	}
	r, err := brevoResponseJsonObject[brevoWebhookIdResult](response, responseBody)
	return server.NewStatusFromResponse(response), r, err
}

// A failed Brevo webhook call for output: the status, a plain provider code
// and the provider message with any credential removed. A response that does
// not decode is named, not quoted; a transport error keeps its text (the api
// url, which carries no credential), redacted all the same.
func brevoWebhookFailure(status *server.Status, code string, message string, credential *brevoWebhookCredential, err error) string {
	parts := []string{}
	if status != nil {
		parts = append(parts, fmt.Sprintf("status %d", status.Code))
	} else {
		parts = append(parts, "no response")
	}
	if code != "" {
		parts = append(parts, fmt.Sprintf("code %s", brevoSafeLabel(code)))
	}
	if message != "" {
		parts = append(parts, fmt.Sprintf("message %q", credential.redact(message)))
	}
	switch {
	case err == nil:
	case errors.Is(err, context.Canceled):
		parts = append(parts, "canceled")
	case errors.Is(err, context.DeadlineExceeded):
		parts = append(parts, "timed out")
	case status != nil:
		parts = append(parts, "unreadable response")
	default:
		parts = append(parts, fmt.Sprintf("transport error (%s)", credential.redact(err.Error())))
	}
	return strings.Join(parts, ", ")
}
