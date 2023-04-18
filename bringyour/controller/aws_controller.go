package controller


import (
	"bringyour.com/bringyour/ulid"

	"github.com/aws/aws-sdk-go/aws"
    "github.com/aws/aws-sdk-go/aws/session"
    "github.com/aws/aws-sdk-go/service/ses"
    "github.com/aws/aws-sdk-go/service/sns"
    // "github.com/aws/aws-sdk-go/aws/awserr"
)


// IMPORTANT this controller is for account messages only
// marketing messages are sent via a separate channel


func SendAccountMessage(networkId ulid.ULID, bodyHtml string, bodyText string) {
	// fixme
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
		Message: &phoneNumber,
		PhoneNumber: &phoneNumber,
	}

	_, err = snsService.Publish(input)
	if err != nil {
		return err
	}

	return nil
}
