package controller


import (
    texttemplate "text/template"
    htmltemplate "html/template"
    // "net/url"
    "fmt"
    "embed"
    "strings"
    // "time"

    "github.com/aws/aws-sdk-go/aws"
    "github.com/aws/aws-sdk-go/aws/session"
    "github.com/aws/aws-sdk-go/service/ses"
    "github.com/aws/aws-sdk-go/service/sns"
    // "github.com/aws/aws-sdk-go/aws/awserr"

	"bringyour.com/bringyour"
    "bringyour.com/bringyour/model"
)


// IMPORTANT this controller is for account messages only
// marketing messages are sent via a separate channel


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
    year, _, _ := bringyour.NowUtc().Date()
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
    Secret string
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


type SubscriptionTransferBalanceCompanyTemplate struct {
    BalanceByteCount model.ByteCount
    BaseTemplate
}

func (self *SubscriptionTransferBalanceCompanyTemplate) Name() string {
    return "subscription_transfer_balance_company"
}

func (self *SubscriptionTransferBalanceCompanyTemplate) Funcs(funcs texttemplate.FuncMap) {
    self.BaseTemplate.Funcs(funcs)
    funcs["Balance"] = self.Balance
}

func (self *SubscriptionTransferBalanceCompanyTemplate) Balance() string {
    return model.ByteCountHumanReadable(self.BalanceByteCount)
}


type SubscriptionEndedTemplate struct {
    BaseTemplate
}

func (self *SubscriptionEndedTemplate) Name() string {
    return "subscription_ended"
}

type SendPaymentTemplate struct {
    BaseTemplate
}

func (self *SendPaymentTemplate) Name() string {
    return "subscription_send_payment"
}


// TODO we can clean this up so all public functions are in the interface
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

type AWSMessageSender struct {}

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
    return sendAccountEmail(emailAddress, subject, bodyHtml, bodyText, sendOpts...)
}


func RenderEmailTemplate(template Template) (subject string, bodyHtml string, bodyText string, returnErr error) {
    if subjectBytes, err := emailTemplates.ReadFile(fmt.Sprintf("email_templates/%s.subject.txt", template.Name())); err == nil {
        if subjectTemplate, err := texttemplate.New("subject").Parse(string(subjectBytes)); err == nil {
            subjectOut := &strings.Builder{}
            if err := subjectTemplate.Funcs(TemplateFuncs(template)).Execute(subjectOut, template); err == nil {
                subject = subjectOut.String()
            } else {
                returnErr = err
                return
            }
        } else {
            returnErr = err
            return
        }
    } else {
        returnErr = err
        return
    }

    if bodyHtmlBytes, err := emailTemplates.ReadFile(fmt.Sprintf("email_templates/%s.html", template.Name())); err == nil {
        if bodyHtmlTemplate, err := htmltemplate.New("body").Parse(string(bodyHtmlBytes)); err == nil {
            bodyHtmlOut := &strings.Builder{}
            if err := bodyHtmlTemplate.Funcs(TemplateFuncs(template)).Execute(bodyHtmlOut, template); err == nil {
                bodyHtml = bodyHtmlOut.String()
            } else {
                returnErr = err
                return
            }     
        } else {
            returnErr = err
            return
        }
    } else {
        returnErr = err
        return
    }

    if bodyTextBytes, err := emailTemplates.ReadFile(fmt.Sprintf("email_templates/%s.txt", template.Name())); err == nil {
        if bodyTextTemplate, err := texttemplate.New("body").Parse(string(bodyTextBytes)); err == nil {
            bodyTextOut := &strings.Builder{}
            if err := bodyTextTemplate.Funcs(TemplateFuncs(template)).Execute(bodyTextOut, template); err == nil {
                bodyText = bodyTextOut.String()
            } else {
                returnErr = err
                return
            }
        } else {
            returnErr = err
            return
        }
    } else {
        returnErr = err
        return
    }

    return
}


func SendAccountSms(phoneNumber string, template Template) error {
    bodyText, err := RenderSmsTemplate(template)
    if err != nil {
        return err
    }
    return sendAccountSms(phoneNumber, bodyText)
}


func RenderSmsTemplate(template Template) (bodyText string, returnErr error) {
    if bodyTextBytes, err := emailTemplates.ReadFile(fmt.Sprintf("email_templates/%s.txt", template.Name())); err == nil {
        if bodyTextTemplate, err := texttemplate.New("body").Parse(string(bodyTextBytes)); err == nil {
            bodyTextOut := &strings.Builder{}
            if err := bodyTextTemplate.Funcs(TemplateFuncs(template)).Execute(bodyTextOut, template); err == nil {
                bodyText = bodyTextOut.String()
            } else {
                returnErr = err
                return
            }
        } else {
            returnErr = err
            return
        }
    } else {
        returnErr = err
        return
    }

    return
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
func sendAccountEmail(emailAddress string, subject string, bodyHtml string, bodyText string, sendOpts ...any) error {
	awsRegion := "us-west-1"
	charSet := "UTF-8"

    senderEmail := SenderEmail("no-reply@bringyour.com")
    for _, sendOpt := range sendOpts {
        switch v := sendOpt.(type) {
        case SendAccountEmailSenderEmail:
            senderEmail = &v
        case *SendAccountEmailSenderEmail:
            senderEmail = v
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
            CcAddresses: []*string{
            },
            ToAddresses: []*string{
                aws.String(emailAddress),
            },
        },
        Message: &ses.Message{
            Body: &ses.Body{
                Html: &ses.Content{
                    Charset: aws.String(charSet),
                    Data: aws.String(bodyHtml),
                },
                Text: &ses.Content{
                    Charset: aws.String(charSet),
                    Data: aws.String(bodyText),
                },
            },
            Subject: &ses.Content{
                Charset: aws.String(charSet),
                Data: aws.String(subject),
            },
        },
        Source: aws.String(senderEmail.SenderEmail),
            // Uncomment to use a configuration set
            //ConfigurationSetName: aws.String(ConfigurationSet),
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
    
    snsService := sns.New(awsSession)

	input := &sns.PublishInput{
		Message: &bodyText,
		PhoneNumber: &phoneNumber,
	}

	_, err = snsService.Publish(input)
	if err != nil {
		return err
	}

	return nil
}
