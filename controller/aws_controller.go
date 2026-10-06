package controller

import (
	htmltemplate "html/template"
	texttemplate "text/template"

	// "net/url"
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/pinpointsmsvoicev2"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// this controller is for account messages only
// marketing messages are sent via a separate channel

// Bounds every SES and SMS call when email.yml sets no send timeout. SES and
// Pinpoint answer in well under a second; the bound only has to outlast a slow
// network and the SDK's own retries, and keeps a hung call from holding an API
// request or the task worker that sends.
const DefaultAccountMessageSendTimeout = 15 * time.Second

// The largest send timeout email.yml can set; larger values are clamped. A call
// that needs longer will not succeed, whatever waits on a send (an API request,
// a task) should not wait longer, and an outbox claim must outlast a send
// (accountMessageClaimLease).
const maxAccountMessageSendTimeout = 2 * time.Minute

type EmailConfig struct {
	CompanySenderEmail string `yaml:"company_sender_email"`
	// ReplyToEmail, when set, is the Reply-To of every account email. Empty
	// means replies go to the sender address, which is a monitored mailbox.
	ReplyToEmail string `yaml:"reply_to_email"`
	// ConfigurationSet names the SES configuration set that publishes send,
	// bounce, and complaint events (EMAIL1.md §5 phase A). Empty until it exists
	// in SES; the per-template message tag is attached only alongside it.
	ConfigurationSet string `yaml:"configuration_set"`
	// SendTimeoutSeconds bounds each SES email and Pinpoint SMS call: the call's
	// context and its session's http client both stop there, and a call that
	// runs out is a failed send. 0 uses DefaultAccountMessageSendTimeout; more
	// than two minutes is clamped to two minutes.
	SendTimeoutSeconds int `yaml:"send_timeout_seconds"`
}

// The bound on one SES or SMS call.
func (self *EmailConfig) SendTimeout() time.Duration {
	if self.SendTimeoutSeconds <= 0 {
		return DefaultAccountMessageSendTimeout
	}
	return min(time.Duration(self.SendTimeoutSeconds)*time.Second, maxAccountMessageSendTimeout)
}

var EnvEmailConfig = sync.OnceValue(func() *EmailConfig {
	var email EmailConfig
	server.Config.RequireSimpleResource("email.yml").UnmarshalYaml(&email)
	return &email
})

//go:embed email_templates/*
var emailTemplates embed.FS

type Template interface {
	Name() string
	Funcs(texttemplate.FuncMap)
}

func TemplateFuncs(template Template) texttemplate.FuncMap {
	funcs := texttemplate.FuncMap{}
	template.Funcs(funcs)
	return funcs
}

type BaseTemplate struct {
}

func (self *BaseTemplate) Funcs(funcs texttemplate.FuncMap) {
	funcs["CopyrightYear"] = self.CopyrightYear
}

func (self *BaseTemplate) CopyrightYear() string {
	year, _, _ := server.NowUtc().Date()
	return fmt.Sprintf("%d", year)
}

type AuthPasswordResetTemplate struct {
	ResetCode string
	BaseTemplate
}

func (self *AuthPasswordResetTemplate) Name() string {
	return "auth_password_reset"
}

// func (self *AuthPasswordResetTemplate) Funcs(funcs texttemplate.FuncMap) {
//     self.BaseTemplate.Funcs(funcs)
//     funcs["ResetCodeUrlEncoded"] = self.ResetCodeUrlEncoded
// }

// func (self *AuthPasswordResetTemplate) ResetCodeUrlEncoded() string {
//     return url.QueryEscape(self.ResetCode)
// }

type AuthPasswordSetTemplate struct {
	BaseTemplate
}

func (self *AuthPasswordSetTemplate) Name() string {
	return "auth_password_set"
}

type AuthVerifyTemplate struct {
	VerifyCode string
	BaseTemplate
}

func (self *AuthVerifyTemplate) Name() string {
	return "auth_verify"
}

type NetworkWelcomeTemplate struct {
	BaseTemplate
}

func (self *NetworkWelcomeTemplate) Name() string {
	return "network_welcome"
}

// ProviderUpgradeNoticeTemplate is an email-only draft for an admin of a
// network with behaviorally confirmed provider compatibility failures. No
// sender is wired to this template until recipient selection is reviewed.
type ProviderUpgradeNoticeTemplate struct {
	NetworkName string
	BaseTemplate
}

func (self *ProviderUpgradeNoticeTemplate) Name() string {
	return "provider_upgrade_notice"
}

type SubscriptionTransferBalanceCodeTemplate struct {
	Secret           string
	BalanceByteCount model.ByteCount
	BaseTemplate
}

func (self *SubscriptionTransferBalanceCodeTemplate) Name() string {
	return "subscription_transfer_balance_code"
}

func (self *SubscriptionTransferBalanceCodeTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	funcs["Balance"] = self.Balance
}

func (self *SubscriptionTransferBalanceCodeTemplate) Balance() string {
	return model.ByteCountHumanReadable(self.BalanceByteCount)
}

// SubscriptionDataAppliedTemplate is the buy-data email when the purchase was made
// FOR a named network and the data has already been applied there: no code to
// redeem, just what landed where. The code is included for the customer's records.
type SubscriptionDataAppliedTemplate struct {
	Secret           string
	BalanceByteCount model.ByteCount
	NetworkName      string
	BaseTemplate
}

func (self *SubscriptionDataAppliedTemplate) Name() string {
	return "subscription_data_applied"
}

func (self *SubscriptionDataAppliedTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	funcs["Balance"] = self.Balance
}

func (self *SubscriptionDataAppliedTemplate) Balance() string {
	return model.ByteCountHumanReadable(self.BalanceByteCount)
}

// X402ReceiptTemplate is the receipt for a purchase an agent paid for inline over
// x402. Sent only when the caller supplied an email -- see x402_controller.go.
type X402ReceiptTemplate struct {
	Description      string
	PriceUsd         float64
	Asset            string
	Network          string
	Transaction      string
	Pro              bool
	BalanceByteCount model.ByteCount
	BaseTemplate
}

func (self *X402ReceiptTemplate) Name() string {
	return "x402_receipt"
}

func (self *X402ReceiptTemplate) Funcs(funcs texttemplate.FuncMap) {
	self.BaseTemplate.Funcs(funcs)
	funcs["Price"] = self.Price
	funcs["Balance"] = self.Balance
}

func (self *X402ReceiptTemplate) Price() string {
	return fmt.Sprintf("$%.2f", self.PriceUsd)
}

// Balance is empty for a Pro-month purchase with no separate data line, so the
// template can omit the row entirely.
func (self *X402ReceiptTemplate) Balance() string {
	if self.BalanceByteCount <= 0 {
		return ""
	}
	return model.ByteCountHumanReadable(self.BalanceByteCount)
}

type SubscriptionEndedTemplate struct {
	BaseTemplate
}

func (self *SubscriptionEndedTemplate) Name() string {
	return "subscription_ended"
}

type MissingWalletTemplate struct {
	PaymentId server.Id
	AmountUsd string
	BaseTemplate
}

func (self *MissingWalletTemplate) Name() string {
	return "subscription_missing_wallet"
}

// fixme - we can clean this up so all public functions are in the interface
type MessageSender interface {
	SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error
}

var messageSenderInstance MessageSender = &AWSMessageSender{}

func GetAWSMessageSender() MessageSender {
	return messageSenderInstance
}

// Used for testing
func SetMessageSender(messageSender MessageSender) {
	messageSenderInstance = messageSender
}

// The channels an account message goes out on, as the send metric labels them.
const (
	accountMessageChannelEmail = "email"
	accountMessageChannelSms   = "sms"
)

// Counts SES and SMS calls by channel and result (sent, timeout, failed).
var accountMessageSendCounter = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "account_message",
		Name:      "sends_total",
		Help:      "SES email and Pinpoint SMS calls for account messages, by channel and result",
	},
	[]string{"channel", "result"},
)

// Registers the send metric with the default registry.
func init() {
	prometheus.MustRegister(accountMessageSendCounter)
}

// The minimum time between two default-level reports of failed account
// message calls.
const accountMessageSendReportInterval = 1 * time.Minute

// The default-level report of failed SES and SMS calls. A client can make the
// server send (a verification code), so a line per failed call would let an
// outage, or a client during one, fill the logs.
var accountMessageSendFailures = newBoundedReport(accountMessageSendReportInterval)

// Sends account messages through SES (email) and Pinpoint SMS (phone). Every
// AWS call runs under the send timeout: the call's context and its session's
// http client both stop there, so a hung endpoint fails the send instead of
// holding the caller. Each call is counted (urnetwork_account_message_sends_total)
// and failures are reported at most once per interval; a call that ran out of
// time returns an error that is `context.DeadlineExceeded`.
// The zero value is the production sender. Safe for concurrent use.
type AWSMessageSender struct {
	// zero uses email.yml's send timeout
	sendTimeout time.Duration
	// empty uses the AWS endpoints; tests point both services at a local server
	endpoint string
	// nil uses the default credential chain
	credentials *credentials.Credentials
}

func (self *AWSMessageSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {

	normalUserAuth, userAuthType := model.NormalUserAuth(userAuth)

	switch userAuthType {
	case model.UserAuthTypeEmail:
		return self.sendEmailTemplate(normalUserAuth, template, sendOpts...)
	case model.UserAuthTypePhone:
		return self.sendSmsTemplate(normalUserAuth, template)
	default:
		return fmt.Errorf("Unknown user auth: %s", userAuthType)
	}
}

// The bound on one call.
func (self *AWSMessageSender) timeout() time.Duration {
	if 0 < self.sendTimeout {
		return self.sendTimeout
	}
	return EnvEmailConfig().SendTimeout()
}

// A session for one call in the region, whose http client stops at the send
// timeout as a backstop for any request made without the call's context.
func (self *AWSMessageSender) newSession(region string, sendTimeout time.Duration) (*session.Session, error) {
	config := &aws.Config{
		Region: aws.String(region),
		HTTPClient: &http.Client{
			Timeout: sendTimeout,
		},
	}
	if self.endpoint != "" {
		config.Endpoint = aws.String(self.endpoint)
	}
	if self.credentials != nil {
		config.Credentials = self.credentials
	}
	return session.NewSession(config)
}

// Counts and reports the outcome of one call. A call whose context or http
// client ran out of time returns an error that is `context.DeadlineExceeded`.
func accountMessageSendResult(sendCtx context.Context, channel string, sendTimeout time.Duration, err error) error {
	if err == nil {
		accountMessageSendCounter.WithLabelValues(channel, "sent").Inc()
		return nil
	}
	result := "failed"
	timedOut := errors.Is(sendCtx.Err(), context.DeadlineExceeded)
	if !timedOut {
		// the http client's backstop timeout surfaces as a net timeout inside
		// the SDK's error chain
		for cause := err; cause != nil; {
			var netErr net.Error
			if errors.As(cause, &netErr) && netErr.Timeout() {
				timedOut = true
				break
			}
			awsErr, ok := cause.(awserr.Error)
			if !ok {
				break
			}
			cause = awsErr.OrigErr()
		}
	}
	if timedOut {
		result = "timeout"
		err = fmt.Errorf("%s send timed out after %s: %w: %w", channel, sendTimeout, context.DeadlineExceeded, err)
	}
	accountMessageSendCounter.WithLabelValues(channel, result).Inc()
	// the line names the channel and the SDK's error code only: SES messages can
	// carry an address
	code := "?"
	var awsErr awserr.Error
	if errors.As(err, &awsErr) {
		code = awsErr.Code()
	}
	if suppressedCount, ok := accountMessageSendFailures.Allow(time.Now()); ok {
		glog.Infof(
			"[aws]account %s send %s (%s); %d more failed sends since the last report\n",
			channel,
			result,
			code,
			suppressedCount,
		)
	}
	if glog.V(1) {
		glog.Infof("[aws]account %s send %s: %s\n", channel, result, err)
	}
	return err
}

// Sends an account email with the production sender.
func SendAccountEmailTemplate(emailAddress string, template Template, sendOpts ...any) error {
	return (&AWSMessageSender{}).sendEmailTemplate(emailAddress, template, sendOpts...)
}

// Renders the template and sends it as one email.
func (self *AWSMessageSender) sendEmailTemplate(emailAddress string, template Template, sendOpts ...any) error {
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		return err
	}
	return self.sendEmail(emailAddress, template.Name(), subject, bodyHtml, bodyText, sendOpts...)
}

const emailTemplateDir = "email_templates"

// Every email is the shared shell plus one body. `_layout.html` and `_layout.txt`
// carry the header, footer, preheader slot, and dark-mode css; `<name>.html` and
// `<name>.txt` supply the `{{define}}` blocks the layout invokes (title, preheader,
// eyebrow, accent, headline, why, content). Executing the layout renders the whole
// message, so a change to the shell changes every template at once. See EMAIL1.md.
func RenderEmailTemplate(template Template) (subject string, bodyHtml string, bodyText string, returnErr error) {
	subject, returnErr = renderEmailSubject(template)
	if returnErr != nil {
		return
	}
	bodyHtml, returnErr = renderEmailHtml(template)
	if returnErr != nil {
		return
	}
	bodyText, returnErr = renderEmailText(template)
	return
}

func renderEmailSubject(template Template) (string, error) {
	out, err := renderEmailTextFile(template, "subject", fmt.Sprintf("%s/%s.subject.txt", emailTemplateDir, template.Name()))
	if err != nil {
		return "", err
	}
	// a subject is one header line; a stray newline in the file would otherwise
	// become a header injection or an SES rejection
	subject := strings.TrimSpace(out)
	if subject == "" {
		return "", fmt.Errorf("email template %s: empty subject", template.Name())
	}
	if strings.ContainsAny(subject, "\r\n") {
		return "", fmt.Errorf("email template %s: subject must be a single line", template.Name())
	}
	return subject, nil
}

func renderEmailHtml(template Template) (string, error) {
	layoutTemplate, err := htmltemplate.New("_layout.html").
		Funcs(TemplateFuncs(template)).
		ParseFS(
			emailTemplates,
			fmt.Sprintf("%s/_layout.html", emailTemplateDir),
			fmt.Sprintf("%s/%s.html", emailTemplateDir, template.Name()),
		)
	if err != nil {
		return "", err
	}
	out := &strings.Builder{}
	if err := layoutTemplate.ExecuteTemplate(out, "_layout.html", template); err != nil {
		return "", err
	}
	return out.String(), nil
}

func renderEmailText(template Template) (string, error) {
	layoutTemplate, err := texttemplate.New("_layout.txt").
		Funcs(TemplateFuncs(template)).
		ParseFS(
			emailTemplates,
			fmt.Sprintf("%s/_layout.txt", emailTemplateDir),
			fmt.Sprintf("%s/%s.txt", emailTemplateDir, template.Name()),
		)
	if err != nil {
		return "", err
	}
	out := &strings.Builder{}
	if err := layoutTemplate.ExecuteTemplate(out, "_layout.txt", template); err != nil {
		return "", err
	}
	return out.String(), nil
}

// renderEmailTextFile renders one standalone text file (a subject or an SMS body)
// with no layout around it.
func renderEmailTextFile(template Template, name string, path string) (string, error) {
	contents, err := emailTemplates.ReadFile(path)
	if err != nil {
		return "", err
	}
	fileTemplate, err := texttemplate.New(name).Funcs(TemplateFuncs(template)).Parse(string(contents))
	if err != nil {
		return "", err
	}
	out := &strings.Builder{}
	if err := fileTemplate.Execute(out, template); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Sends an account SMS with the production sender.
func SendAccountSms(phoneNumber string, template Template) error {
	return (&AWSMessageSender{}).sendSmsTemplate(phoneNumber, template)
}

// Renders the template's SMS body and sends it as one text.
func (self *AWSMessageSender) sendSmsTemplate(phoneNumber string, template Template) error {
	bodyText, err := RenderSmsTemplate(template)
	if err != nil {
		return err
	}
	return self.sendSms(phoneNumber, bodyText)
}

// RenderSmsTemplate renders `<name>.sms.txt`, the short body a phone account
// receives instead of the email. A template without one (the email-only
// purchase receipts) falls back to its full plain-text email body.
func RenderSmsTemplate(template Template) (string, error) {
	path := fmt.Sprintf("%s/%s.sms.txt", emailTemplateDir, template.Name())
	if _, err := fs.Stat(emailTemplates, path); err == nil {
		out, err := renderEmailTextFile(template, "sms", path)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(out), nil
	}
	return renderEmailText(template)
}

type SendAccountEmailSenderEmail struct {
	SenderEmail string
}

func SenderEmail(senderEmail string) *SendAccountEmailSenderEmail {
	return &SendAccountEmailSenderEmail{
		SenderEmail: senderEmail,
	}
}

// One SES SendEmail call, bounded by the send timeout.
// https://docs.aws.amazon.com/sdk-for-go/api/aws/session/
// https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/ses-example-send-email.html
// https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html
func (self *AWSMessageSender) sendEmail(emailAddress string, templateName string, subject string, bodyHtml string, bodyText string, sendOpts ...any) error {
	awsRegion := "us-west-1"
	charSet := "UTF-8"

	// note any sender email domain will need to be registed as an identity in SES
	senderEmail := EnvEmailConfig().CompanySenderEmail
	for _, sendOpt := range sendOpts {
		switch v := sendOpt.(type) {
		case SendAccountEmailSenderEmail:
			senderEmail = v.SenderEmail
		case *SendAccountEmailSenderEmail:
			senderEmail = v.SenderEmail
		}
	}

	sendTimeout := self.timeout()
	sendCtx, sendCancel := context.WithTimeout(context.Background(), sendTimeout)
	defer sendCancel()

	awsSession, err := self.newSession(awsRegion, sendTimeout)
	if err != nil {
		return accountMessageSendResult(sendCtx, accountMessageChannelEmail, sendTimeout, err)
	}

	sesService := ses.New(awsSession)

	input := &ses.SendEmailInput{
		Destination: &ses.Destination{
			CcAddresses: []*string{},
			ToAddresses: []*string{
				aws.String(emailAddress),
			},
		},
		Message: &ses.Message{
			Body: &ses.Body{
				Html: &ses.Content{
					Charset: aws.String(charSet),
					Data:    aws.String(bodyHtml),
				},
				Text: &ses.Content{
					Charset: aws.String(charSet),
					Data:    aws.String(bodyText),
				},
			},
			Subject: &ses.Content{
				Charset: aws.String(charSet),
				Data:    aws.String(subject),
			},
		},
		Source: aws.String(senderEmail),
	}
	emailConfig := EnvEmailConfig()
	if replyTo := strings.TrimSpace(emailConfig.ReplyToEmail); replyTo != "" {
		input.ReplyToAddresses = []*string{aws.String(replyTo)}
	}
	if configurationSet := strings.TrimSpace(emailConfig.ConfigurationSet); configurationSet != "" {
		// the configuration set publishes send/bounce/complaint events, and the
		// template tag splits every metric by template (EMAIL1.md §5)
		input.ConfigurationSetName = aws.String(configurationSet)
		input.Tags = []*ses.MessageTag{{
			Name:  aws.String("template"),
			Value: aws.String(templateName),
		}}
	}

	// the call and every SDK retry inside it stop at the send timeout
	_, err = sesService.SendEmailWithContext(sendCtx, input)
	return accountMessageSendResult(sendCtx, accountMessageChannelEmail, sendTimeout, err)
}

// One Pinpoint SMS SendTextMessage call, bounded by the send timeout.
// https://docs.aws.amazon.com/sdk-for-go/api/service/pinpointsmsvoicev2/
// https://docs.aws.amazon.com/pinpoint/latest/apireference_smsvoicev2/API_SendTextMessage.html
func (self *AWSMessageSender) sendSms(phoneNumber string, bodyText string) error {
	awsRegion := "us-east-1"

	sendTimeout := self.timeout()
	sendCtx, sendCancel := context.WithTimeout(context.Background(), sendTimeout)
	defer sendCancel()

	awsSession, err := self.newSession(awsRegion, sendTimeout)
	if err != nil {
		return accountMessageSendResult(sendCtx, accountMessageChannelSms, sendTimeout, err)
	}

	// pinpoint requires +CCXXXXXXX format with no spaces and no dashes
	smsStrip := regexp.MustCompile("[\\-\\s]+")
	strippedPhoneNumber := smsStrip.ReplaceAllString(phoneNumber, "")

	smsService := pinpointsmsvoicev2.New(awsSession)

	input := &pinpointsmsvoicev2.SendTextMessageInput{
		DestinationPhoneNumber: aws.String(strippedPhoneNumber),
		MessageBody:            aws.String(bodyText),
		MessageType:            aws.String(pinpointsmsvoicev2.MessageTypeTransactional),
	}

	// the call and every SDK retry inside it stop at the send timeout
	_, err = smsService.SendTextMessageWithContext(sendCtx, input)
	return accountMessageSendResult(sendCtx, accountMessageChannelSms, sendTimeout, err)
}
