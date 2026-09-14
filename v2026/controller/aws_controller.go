package controller

import (
	htmltemplate "html/template"
	texttemplate "text/template"

	// "net/url"
	"embed"
	"fmt"
	"io/fs"
	"regexp"
	"strings"
	"sync"

	// "time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/pinpointsmsvoicev2"
	"github.com/aws/aws-sdk-go/service/ses"

	// "github.com/aws/aws-sdk-go/aws/awserr"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// IMPORTANT this controller is for account messages only
// marketing messages are sent via a separate channel

type EmailConfig struct {
	CompanySenderEmail string `yaml:"company_sender_email"`
	// ReplyToEmail, when set, is the Reply-To of every account email. Empty
	// means replies go to the sender address, which is a monitored mailbox.
	ReplyToEmail string `yaml:"reply_to_email"`
	// ConfigurationSet names the SES configuration set that publishes send,
	// bounce, and complaint events (EMAIL1.md §5 phase A). Empty until it exists
	// in SES; the per-template message tag is attached only alongside it.
	ConfigurationSet string `yaml:"configuration_set"`
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

type AWSMessageSender struct{}

func (c *AWSMessageSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {

	normalUserAuth, userAuthType := model.NormalUserAuth(userAuth)

	switch userAuthType {
	case model.UserAuthTypeEmail:
		return SendAccountEmailTemplate(normalUserAuth, template, sendOpts...)
	case model.UserAuthTypePhone:
		return SendAccountSms(normalUserAuth, template)
	default:
		return fmt.Errorf("Unknown user auth: %s", userAuthType)
	}
}

func SendAccountEmailTemplate(emailAddress string, template Template, sendOpts ...any) error {
	subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
	if err != nil {
		return err
	}
	return sendAccountEmail(emailAddress, template.Name(), subject, bodyHtml, bodyText, sendOpts...)
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

func SendAccountSms(phoneNumber string, template Template) error {
	bodyText, err := RenderSmsTemplate(template)
	if err != nil {
		return err
	}
	return sendAccountSms(phoneNumber, bodyText)
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

// https://docs.aws.amazon.com/sdk-for-go/api/aws/session/
// https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/ses-example-send-email.html
// https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html
func sendAccountEmail(emailAddress string, templateName string, subject string, bodyHtml string, bodyText string, sendOpts ...any) error {
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

	awsSession, err := session.NewSession(&aws.Config{
		Region: aws.String(awsRegion),
	})
	if err != nil {
		return err
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

	// Attempt to send the email.
	_, err = sesService.SendEmail(input)

	// Display error messages if they occur.
	if err != nil {
		// if aerr, ok := err.(awserr.Error); ok {
		//     switch aerr.Code() {
		//     case ses.ErrCodeMessageRejected:
		//         fmt.Println(ses.ErrCodeMessageRejected, aerr.Error())
		//     case ses.ErrCodeMailFromDomainNotVerifiedException:
		//         fmt.Println(ses.ErrCodeMailFromDomainNotVerifiedException, aerr.Error())
		//     case ses.ErrCodeConfigurationSetDoesNotExistException:
		//         fmt.Println(ses.ErrCodeConfigurationSetDoesNotExistException, aerr.Error())
		//     default:
		//         fmt.Println(aerr.Error())
		//     }
		// } else {
		//     // Print the error, cast err to awserr.Error to get the Code and
		//     // Message from an error.
		//     fmt.Println(err.Error())
		// }
		return err

	}

	return nil
}

// https://docs.aws.amazon.com/sdk-for-go/api/service/sns/
// https://docs.aws.amazon.com/sdk-for-go/api/service/sns/#PublishInput
func sendAccountSms(phoneNumber string, bodyText string) error {
	awsRegion := "us-east-1"

	awsSession, err := session.NewSession(&aws.Config{
		Region: aws.String(awsRegion),
	})
	if err != nil {
		return err
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

	_, err = smsService.SendTextMessage(input)
	if err != nil {
		return err
	}

	return nil
}
