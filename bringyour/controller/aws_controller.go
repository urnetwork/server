package controller


// note marketing messages are sent via a separate channel


func SendAccountMessage(networkId ulid.ULID, bodyHtml string, bodyText string) {

}

func sendAccountEmail(emailAddress string, bodyHtml string) {
	// amazon simple email
}

func sendAccountSms(phoneNumber string, bodyText string) {
	// amazon simple sms
}
