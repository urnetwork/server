package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Services contact-sales leads (POST /services/contact-sales, EMBED1.md).
//
// The endpoint is public: a prospect has no account. Every request counts
// against a per-address and a global rate limit before anything else (429 with
// the standard refusal); a filled honeypot field answers like a success and
// stores nothing; a validation failure answers 200 with `error.message`, the
// house style of auth-client. A valid lead is stored, then posted to the
// sales Slack channel through the incoming webhook in vault `sales.yml`
// (`slack_webhook_url`) without holding up the response. Without that key the
// lead is only stored.

const servicesSalesVaultResource = "sales.yml"

const servicesSalesConfigRefresh = 60 * time.Second

const (
	servicesLeadMaxNameLength    = 256
	servicesLeadMaxEmailLength   = 256
	servicesLeadMaxCompanyLength = 256
	servicesLeadMaxMessageLength = 4000

	servicesLeadMaxMonthlyActiveUsers         = 1_000_000_000
	servicesLeadMaxMonthlyDataBudgetByteCount = ByteCount(1) << 60
)

const (
	servicesLeadAddressAttemptsPerHour  = 5
	servicesLeadGlobalAttemptsPerMinute = 60
)

// The attempt script admits an attempt while the count including it stays
// below the limit, so each limit is one more than the attempts it admits.
var servicesLeadRateLimitSettings = server.IpRateLimitAttemptSettings{
	KeyPrefix:       "services_contact_sales",
	AddressLookback: time.Hour,
	AddressLimit:    servicesLeadAddressAttemptsPerHour + 1,
	GlobalLookback:  time.Minute,
	GlobalLimit:     servicesLeadGlobalAttemptsPerMinute + 1,
}

// address and global histories share this tag, so the attempt script runs on
// one cluster slot
const servicesLeadRateLimitHashTag = "services_contact_sales"

const (
	servicesLeadSlackAttemptCount   = 3
	servicesLeadSlackAttemptTimeout = 10 * time.Second
)

var servicesLeadSlackRetryDelays = []time.Duration{2 * time.Second, 6 * time.Second}

var servicesLeadResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_services_lead_total",
	Help: "Services contact-sales requests, by result.",
}, []string{"result"})

var servicesLeadNotifyResults = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_services_lead_notify_total",
	Help: "Services lead Slack notifications, by result.",
}, []string{"result"})

func init() {
	prometheus.MustRegister(servicesLeadResults, servicesLeadNotifyResults)
}

type ServicesContactSalesArgs struct {
	Name                       string    `json:"name"`
	Email                      string    `json:"email"`
	Company                    string    `json:"company"`
	MonthlyActiveUsers         int64     `json:"monthly_active_users"`
	MonthlyDataBudgetByteCount ByteCount `json:"monthly_data_budget_byte_count"`
	// the use case
	Message string `json:"message,omitempty"`
	// honeypot: the form hides it, so a person leaves it empty
	Website string `json:"website,omitempty"`
}

type ServicesContactSalesError struct {
	Message string `json:"message"`
}

type ServicesContactSalesResult struct {
	RequestId *server.Id                 `json:"request_id,omitempty"`
	Error     *ServicesContactSalesError `json:"error,omitempty"`
}

type servicesLead struct {
	leadId                     server.Id
	createTime                 time.Time
	name                       string
	email                      string
	company                    string
	monthlyActiveUsers         int64
	monthlyDataBudgetByteCount ByteCount
	message                    string
}

// servicesLeadText reports whether a field is free of control characters. A
// multi-line field may also hold line breaks and tabs.
func servicesLeadText(value string, multiline bool) bool {
	for _, r := range value {
		if multiline && (r == '\n' || r == '\r' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return false
		}
	}
	return utf8.ValidString(value)
}

// newServicesLead validates and normalizes a request. The message is the
// refusal shown to the person filling in the form.
func newServicesLead(args *ServicesContactSalesArgs, leadId server.Id, createTime time.Time) (*servicesLead, string) {
	name := strings.TrimSpace(args.Name)
	email := strings.TrimSpace(args.Email)
	company := strings.TrimSpace(args.Company)
	message := strings.TrimSpace(args.Message)

	if name == "" || servicesLeadMaxNameLength < utf8.RuneCountInString(name) || !servicesLeadText(name, false) {
		return nil, "Please enter your name."
	}
	if servicesLeadMaxEmailLength < utf8.RuneCountInString(email) || !servicesLeadText(email, false) {
		return nil, "Please enter a valid email address."
	}
	if parsed, err := mail.ParseAddress(email); err != nil || parsed.Address != email || !strings.Contains(email, "@") {
		return nil, "Please enter a valid email address."
	}
	if company == "" || servicesLeadMaxCompanyLength < utf8.RuneCountInString(company) || !servicesLeadText(company, false) {
		return nil, "Please enter your company."
	}
	if args.MonthlyActiveUsers < 1 || servicesLeadMaxMonthlyActiveUsers < args.MonthlyActiveUsers {
		return nil, fmt.Sprintf("Monthly active users must be between 1 and %d.", servicesLeadMaxMonthlyActiveUsers)
	}
	if args.MonthlyDataBudgetByteCount < 0 || servicesLeadMaxMonthlyDataBudgetByteCount < args.MonthlyDataBudgetByteCount {
		return nil, "Please enter a monthly data budget of zero or more."
	}
	if servicesLeadMaxMessageLength < utf8.RuneCountInString(message) || !servicesLeadText(message, true) {
		return nil, fmt.Sprintf("Please keep the use case under %d characters.", servicesLeadMaxMessageLength)
	}

	return &servicesLead{
		leadId:                     leadId,
		createTime:                 createTime,
		name:                       name,
		email:                      email,
		company:                    company,
		monthlyActiveUsers:         args.MonthlyActiveUsers,
		monthlyDataBudgetByteCount: args.MonthlyDataBudgetByteCount,
		message:                    message,
	}, ""
}

// ServicesContactSales stores a contact-sales lead and notifies sales.
func ServicesContactSales(contactSales *ServicesContactSalesArgs, clientSession *session.ClientSession) (*ServicesContactSalesResult, error) {
	ctx := clientSession.Ctx
	now := server.NowUtc()

	if err := checkServicesLeadRateLimit(ctx, clientSession, now); err != nil {
		return nil, err
	}

	leadId := server.NewId()
	if strings.TrimSpace(contactSales.Website) != "" {
		// answer like a success so a bot learns nothing
		servicesLeadResults.WithLabelValues("honeypot").Inc()
		return &ServicesContactSalesResult{RequestId: &leadId}, nil
	}

	lead, refusal := newServicesLead(contactSales, leadId, now)
	if refusal != "" {
		servicesLeadResults.WithLabelValues("invalid").Inc()
		return &ServicesContactSalesResult{Error: &ServicesContactSalesError{Message: refusal}}, nil
	}

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO services_lead (
					lead_id,
					create_time,
					name,
					email,
					company,
					monthly_active_users,
					monthly_data_budget_byte_count,
					message
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			`,
			lead.leadId,
			lead.createTime,
			lead.name,
			lead.email,
			lead.company,
			lead.monthlyActiveUsers,
			lead.monthlyDataBudgetByteCount,
			lead.message,
		))
	})
	servicesLeadResults.WithLabelValues("stored").Inc()

	go server.HandleError(func() {
		notifyServicesLead(lead)
	})

	return &ServicesContactSalesResult{RequestId: &leadId}, nil
}

// checkServicesLeadRateLimit counts every request. A session without a
// classifiable address, or a rate limit store error, is let through: the lead
// still faces the honeypot and validation, and the form must not fail because
// redis did.
func checkServicesLeadRateLimit(ctx context.Context, clientSession *session.ClientSession, now time.Time) error {
	rateLimitClient, err := server.NewRateLimitClient(clientSession.ClientAddress)
	if err != nil {
		return nil
	}
	_, allowed, err := server.CheckIpRateLimitAttempt(
		ctx,
		rateLimitClient,
		servicesLeadRateLimitHashTag,
		now,
		servicesLeadRateLimitSettings,
	)
	if err != nil {
		servicesLeadResults.WithLabelValues("rate_limit_error").Inc()
		glog.V(1).Infof("[sl]rate limit check failed: %s\n", err)
		return nil
	}
	if !allowed {
		servicesLeadResults.WithLabelValues("rate_limited").Inc()
		return &rateLimitError{
			message: "429 Too many requests. Please try again later.",
		}
	}
	return nil
}

type servicesSalesConfig struct {
	SlackWebhookUrl string `yaml:"slack_webhook_url"`
}

// parseServicesSalesConfig returns the webhook url, or "" when the key is
// absent or is not an https url. The url is a credential: it is never logged.
func parseServicesSalesConfig(data []byte) string {
	var config servicesSalesConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	webhookUrl := strings.TrimSpace(config.SlackWebhookUrl)
	parsed, err := url.Parse(webhookUrl)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	return webhookUrl
}

type servicesSalesConfigSnapshot struct {
	loadTime        time.Time
	slackWebhookUrl string
}

var servicesSalesConfigCache atomic.Pointer[servicesSalesConfigSnapshot]

// servicesSalesSlackWebhookUrl re-reads vault every
// `servicesSalesConfigRefresh`, so ops can add or rotate the webhook without
// a restart.
func servicesSalesSlackWebhookUrl(now time.Time) string {
	if snapshot := servicesSalesConfigCache.Load(); snapshot != nil && now.Sub(snapshot.loadTime) < servicesSalesConfigRefresh && !now.Before(snapshot.loadTime) {
		return snapshot.slackWebhookUrl
	}
	slackWebhookUrl := ""
	if resource, err := server.Vault.SimpleResource(servicesSalesVaultResource); err == nil {
		if data, err := resource.BytesE(); err == nil {
			slackWebhookUrl = parseServicesSalesConfig(data)
		}
	}
	servicesSalesConfigCache.Store(&servicesSalesConfigSnapshot{
		loadTime:        now,
		slackWebhookUrl: slackWebhookUrl,
	})
	return slackWebhookUrl
}

// slackEscape escapes the three characters Slack treats as control
// characters, so a field cannot mention a channel (<!channel>), a user or
// render a disguised link.
func slackEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	return value
}

func formatServicesLeadByteCount(byteCount ByteCount) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB", "EB"}
	value := float64(byteCount)
	unit := 0
	for 1000 <= value && unit < len(units)-1 {
		value /= 1000
		unit += 1
	}
	if unit == 0 {
		return fmt.Sprintf("%d B", byteCount)
	}
	return fmt.Sprintf("%s %s", strconv.FormatFloat(value, 'f', -1, 64), units[unit])
}

func servicesLeadSlackText(lead *servicesLead) string {
	useCase := slackEscape(lead.message)
	if useCase == "" {
		useCase = "(none)"
	}
	return fmt.Sprintf(
		`*New Services lead*
*Name:* %s
*Email:* %s
*Company:* %s
*Monthly active users:* %d
*Monthly data budget:* %s (%d bytes)
*Use case:*
%s
*Lead:* %s`,
		slackEscape(lead.name),
		slackEscape(lead.email),
		slackEscape(lead.company),
		lead.monthlyActiveUsers,
		formatServicesLeadByteCount(lead.monthlyDataBudgetByteCount),
		lead.monthlyDataBudgetByteCount,
		useCase,
		lead.leadId,
	)
}

// errServicesLeadSlackStatus is a non-2xx webhook answer.
type errServicesLeadSlackStatus struct {
	statusCode int
}

func (self *errServicesLeadSlackStatus) Error() string {
	return fmt.Sprintf("slack webhook answered %d", self.statusCode)
}

// postServicesLeadToSlack posts one message. The returned error never contains
// the webhook url: a *url.Error, whose text includes the url, is unwrapped to
// its cause.
func postServicesLeadToSlack(ctx context.Context, httpClient *http.Client, webhookUrl string, text string) error {
	body, err := json.Marshal(map[string]any{
		"text":         text,
		"unfurl_links": false,
		"unfurl_media": false,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookUrl, bytes.NewReader(body))
	if err != nil {
		return errors.New("slack webhook request could not be built")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return urlErr.Err
		}
		return errors.New("slack webhook request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		return &errServicesLeadSlackStatus{statusCode: response.StatusCode}
	}
	return nil
}

// servicesLeadNotifier posts a lead with bounded retries. Replaceable in tests.
type servicesLeadNotifier struct {
	httpClient  *http.Client
	retryDelays []time.Duration
	sleep       func(context.Context, time.Duration) bool
}

// servicesLeadNotifierFactory builds the notifier notifyServicesLead posts
// with. Tests replace it to reach a local webhook.
var servicesLeadNotifierFactory = newServicesLeadNotifier

func newServicesLeadNotifier() *servicesLeadNotifier {
	return &servicesLeadNotifier{
		httpClient:  server.NewHttpClient(servicesLeadSlackAttemptTimeout),
		retryDelays: servicesLeadSlackRetryDelays,
		sleep:       sleepServicesLeadRetry,
	}
}

func sleepServicesLeadRetry(ctx context.Context, delay time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// Notify returns the last error after the final attempt.
func (self *servicesLeadNotifier) Notify(ctx context.Context, webhookUrl string, text string) error {
	var err error
	for attempt := 0; attempt < servicesLeadSlackAttemptCount; attempt += 1 {
		if 0 < attempt {
			delay := self.retryDelays[min(attempt-1, len(self.retryDelays)-1)]
			if !self.sleep(ctx, delay) {
				return ctx.Err()
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, servicesLeadSlackAttemptTimeout)
		err = postServicesLeadToSlack(attemptCtx, self.httpClient, webhookUrl, text)
		cancel()
		if err == nil {
			return nil
		}
		// a 4xx other than a rate limit will not change on retry
		var statusErr *errServicesLeadSlackStatus
		if errors.As(err, &statusErr) && statusErr.statusCode/100 == 4 && statusErr.statusCode != http.StatusTooManyRequests {
			return err
		}
	}
	return err
}

func notifyServicesLead(lead *servicesLead) {
	webhookUrl := servicesSalesSlackWebhookUrl(server.NowUtc())
	if webhookUrl == "" {
		servicesLeadNotifyResults.WithLabelValues("unconfigured").Inc()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := servicesLeadNotifierFactory().Notify(ctx, webhookUrl, servicesLeadSlackText(lead)); err != nil {
		servicesLeadNotifyResults.WithLabelValues("error").Inc()
		// an ops problem, not the prospect's: logged without the url or the lead
		glog.Infof("[sl]services lead %s slack notify failed: %s\n", lead.leadId, err)
		return
	}
	servicesLeadNotifyResults.WithLabelValues("ok").Inc()

	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE services_lead SET notify_time = $2 WHERE lead_id = $1`,
			lead.leadId,
			server.NowUtc(),
		))
	})
}
