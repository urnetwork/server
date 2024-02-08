package controller


import (
    texttemplate "text/template"
    htmltemplate "html/template"
    "net/url"
    "fmt"
    "embed"
    "strings"
    "time"

    "github.com/aws/aws-sdk-go/aws"
    "github.com/aws/aws-sdk-go/aws/session"
    "github.com/aws/aws-sdk-go/service/ses"
    "github.com/aws/aws-sdk-go/service/sns"
    // "github.com/aws/aws-sdk-go/aws/awserr"

	// "bringyour.com/bringyour"
    "bringyour.com/bringyour/model"
)


// IMPORTANT this controller is for account messages only
// marketing messages are sent via a separate channel


//go:embed email_templates/*
var emailTemplates embed.FS


type Template interface {
    Name() string
    Funcs() texttemplate.FuncMap
}


type AuthPasswordResetTemplate struct {
    ResetCode string
}

func (self *AuthPasswordResetTemplate) Name() string {
    return "auth_password_reset"
}

func (self *AuthPasswordResetTemplate) Funcs() texttemplate.FuncMap {
    return texttemplate.FuncMap{}
}

func (self *AuthPasswordResetTemplate) ResetCodeUrlEncoded() string {
    return url.QueryEscape(self.ResetCode)
}


type AuthPasswordSetTemplate struct {
}

func (self *AuthPasswordSetTemplate) Name() string {
    return "auth_password_set"
}

func (self *AuthPasswordSetTemplate) Funcs() texttemplate.FuncMap {
    return texttemplate.FuncMap{}
}


type AuthVerifyTemplate struct {
    VerifyCode string
}

func (self *AuthVerifyTemplate) Name() string {
    return "auth_verify"
}

func (self *AuthVerifyTemplate) Funcs() texttemplate.FuncMap {
    return texttemplate.FuncMap{
        "CopyrightYear": self.CopyrightYear,
    }
}

func (self *AuthVerifyTemplate) CopyrightYear() string {
    year, _, _ := time.Now().Date()
    return fmt.Sprintf("%d", year)
}


type NetworkWelcomeTemplate struct {
}

func (self *NetworkWelcomeTemplate) Name() string {
    return "network_welcome"
}

func (self *NetworkWelcomeTemplate) Funcs() texttemplate.FuncMap {
    return texttemplate.FuncMap{
        "CopyrightYear": self.CopyrightYear,
    }
}

func (self *NetworkWelcomeTemplate) CopyrightYear() string {
    year, _, _ := time.Now().Date()
    return fmt.Sprintf("%d", year)
}


type SubscriptionBalanceTransferCodeTemplate struct {
    Code string
}

func (self *SubscriptionBalanceTransferCodeTemplate) Name() string {
    return "network_welcome"
}

func (self *SubscriptionBalanceTransferCodeTemplate) Funcs() texttemplate.FuncMap {
    return texttemplate.FuncMap{
        "CodeUrlEncoded": self.CodeUrlEncoded,
    }
}

func (self *SubscriptionBalanceTransferCodeTemplate) CodeUrlEncoded() string {
    return url.QueryEscape(self.Code)
}


func SendAccountMessageTemplate(userAuth string, template Template) error {
    normalUserAuth, userAuthType := model.NormalUserAuth(userAuth)

    switch userAuthType {
    case model.UserAuthTypeEmail:
        return SendAccountEmailTemplate(normalUserAuth, template)
    case model.UserAuthTypePhone:
        return SendAccountSms(normalUserAuth, template)
    default:
        return fmt.Errorf("Unknown user auth: %s", userAuthType)
    }
}


func SendAccountEmailTemplate(emailAddress string, template Template) error {
    subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
    if err != nil {
        return err
    }
    return sendAccountEmail(emailAddress, subject, bodyHtml, bodyText)
}


func RenderEmailTemplate(template Template) (subject string, bodyHtml string, bodyText string, returnErr error) {
    if subjectBytes, err := emailTemplates.ReadFile(fmt.Sprintf("email_templates/%s.subject.txt", template.Name())); err == nil {
        if subjectTemplate, err := texttemplate.New("subject").Parse(string(subjectBytes)); err == nil {
            subjectOut := &strings.Builder{}
            if err := subjectTemplate.Funcs(template.Funcs()).Execute(subjectOut, template); err == nil {
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
            if err := bodyHtmlTemplate.Funcs(template.Funcs()).Execute(bodyHtmlOut, template); err == nil {
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
            if err := bodyTextTemplate.Funcs(template.Funcs()).Execute(bodyTextOut, template); err == nil {
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
            if err := bodyTextTemplate.Funcs(template.Funcs()).Execute(bodyTextOut, template); err == nil {
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


// https://docs.aws.amazon.com/sdk-for-go/api/aws/session/
// https://docs.aws.amazon.com/sdk-for-go/v1/developer-guide/ses-example-send-email.html
// https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_SendEmail.html
func sendAccountEmail(emailAddress string, subject string, bodyHtml string, bodyText string) error {
	awsRegion := "us-west-1"
	charSet := "UTF-8"
	senderEmailAddress := "no-reply@bringyour.com"

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
        Source: aws.String(senderEmailAddress),
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
