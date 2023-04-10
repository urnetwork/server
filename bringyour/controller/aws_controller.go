package controller


import (
	"bringyour.com/bringyour/ulid"
)


// note marketing messages are sent via a separate channel


func SendAccountMessage(networkId ulid.ULID, bodyHtml string, bodyText string) {
	// fixme
}

func sendAccountEmail(emailAddress string, bodyHtml string) {
	// amazon simple email
	// fixme
}

func sendAccountSms(phoneNumber string, bodyText string) {
	// amazon simple sms
	// fixme
}
