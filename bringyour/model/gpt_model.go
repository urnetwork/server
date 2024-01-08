package model

import (
	// "fmt"
	// "strings"

	"bringyour.com/bringyour/session"
	"bringyour.com/bringyour"
)


type GptBeMyPrivacyAgentArgs struct {
	CountryOfResidence string `json:"country_of_residence"`
	RegionOfResidence string `json:"region_of_residence"`
	CorrespondenceEmail string `json:"correspondence_email"`
	Consent bool `json:"consent"`
	EmailText *GptBeMyPrivacyAgentEmail `json:"email_text"`
	ServiceName string `json:"service_name"`
	ServiceUser string `json:"service_user"`
}

type GptBeMyPrivacyAgentEmail struct {
	To string `json:"to"`
	Subject string `json:"subject"`
	Body string `json:"body"`
}

type GptBeMyPrivacyAgentResult struct {
	Accepted bool `json:"accepted"`
}

func GptBeMyPrivacyAgent(
	beMyPrivacyAgent *GptBeMyPrivacyAgentArgs,
	clientSession *session.ClientSession,
) (*GptBeMyPrivacyAgentResult, error) {
	accepted := true

	bringyour.Raise(bringyour.Tx(clientSession.Ctx, func(tx bringyour.PgTx) {
		privacyAgentRequestId := bringyour.NewId()

		bringyour.RaisePgResult(tx.Exec(
			clientSession.Ctx,
			`
				INSERT INTO privacy_agent_request (
					privacy_agent_request_id,
		            country_of_residence,
		            region_of_residence,
		            correspondence_email,
		            consent,
		            email_text_to,
		            email_text_subject,
		            email_text_body,
		            service_name,
		            service_user
				) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
			`,
			privacyAgentRequestId,
			beMyPrivacyAgent.CountryOfResidence,
			beMyPrivacyAgent.RegionOfResidence,
			beMyPrivacyAgent.CorrespondenceEmail,
			beMyPrivacyAgent.Consent,
			beMyPrivacyAgent.EmailText.To,
			beMyPrivacyAgent.EmailText.Subject,
			beMyPrivacyAgent.EmailText.Body,
			beMyPrivacyAgent.ServiceName,
			beMyPrivacyAgent.ServiceUser,
		))
	}))

	return &GptBeMyPrivacyAgentResult{
		Accepted: accepted,
	}, nil
}