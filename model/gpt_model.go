package model

import (
	"context"
	"fmt"

	// "strings"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

type CompletePrivacyPolicy struct {
	PrivacyPolicyId   server.Id
	ServiceName       string
	ServiceUrls       []string
	CreateTime        time.Time
	Pending           bool
	PrivacyPolicyText string
	ExtractedUrls     []string
}

func NewCompletePrivacyPolicyPending(
	serviceName string,
	serviceUrls []string,
) *CompletePrivacyPolicy {
	return &CompletePrivacyPolicy{
		ServiceName: serviceName,
		ServiceUrls: serviceUrls,
		CreateTime:  server.CodecTime(server.NowUtc()),
		Pending:     true,
	}
}

func NewCompletePrivacyPolicy(
	serviceName string,
	serviceUrls []string,
	privacyPolicyText string,
	extractedUrls []string,
) *CompletePrivacyPolicy {
	return &CompletePrivacyPolicy{
		ServiceName:       serviceName,
		ServiceUrls:       serviceUrls,
		CreateTime:        server.CodecTime(server.NowUtc()),
		Pending:           false,
		PrivacyPolicyText: privacyPolicyText,
		ExtractedUrls:     extractedUrls,
	}
}

func GetCompletePrivacyPolicy(
	ctx context.Context,
	serviceName string,
) (completePrivacyPolicy *CompletePrivacyPolicy, returnErr error) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT privacy_policy_id
				FROM latest_complete_privacy_policy
				WHERE service_name = $1
			`,
			serviceName,
		)

		exists := false
		var privacyPolicyId server.Id
		server.WithPgResult(result, err, func() {
			if result.Next() {
				exists = true
				server.Raise(result.Scan(&privacyPolicyId))
			}
		})

		if !exists {
			returnErr = fmt.Errorf("Privacy policy does not exist for %s", serviceName)
			return
		}

		completePrivacyPolicy = &CompletePrivacyPolicy{
			PrivacyPolicyId: privacyPolicyId,
			ServiceName:     serviceName,
		}

		result, err = conn.Query(
			ctx,
			`
				SELECT
					create_time,
					privacy_policy_text,
					pending
				FROM complete_privacy_policy
				WHERE privacy_policy_id = $1
			`,
			privacyPolicyId,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(
					&completePrivacyPolicy.CreateTime,
					&completePrivacyPolicy.PrivacyPolicyText,
					&completePrivacyPolicy.Pending,
				))
			}
		})

		result, err = conn.Query(
			ctx,
			`
				SELECT service_url
				FROM complete_privacy_policy_service_url
				WHERE privacy_policy_id = $1
			`,
			privacyPolicyId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var serviceUrl string
				server.Raise(result.Scan(&serviceUrl))
				completePrivacyPolicy.ServiceUrls = append(
					completePrivacyPolicy.ServiceUrls,
					serviceUrl,
				)
			}
		})

		result, err = conn.Query(
			ctx,
			`
				SELECT extracted_url
				FROM complete_privacy_policy_extracted_url
				WHERE privacy_policy_id = $1
			`,
			privacyPolicyId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var extractedUrl string
				server.Raise(result.Scan(&extractedUrl))
				completePrivacyPolicy.ExtractedUrls = append(
					completePrivacyPolicy.ExtractedUrls,
					extractedUrl,
				)
			}
		})
	})

	return
}

func SetCompletePrivacyPolicy(ctx context.Context, completePrivacyPolicy *CompletePrivacyPolicy) {
	server.Tx(ctx, func(tx server.PgTx) {
		completePrivacyPolicy.PrivacyPolicyId = server.NewId()

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO complete_privacy_policy (
					privacy_policy_id,
					create_time,
					service_name,
					pending,
					privacy_policy_text
				) VALUES ($1, $2, $3, $4, $5)
			`,
			completePrivacyPolicy.PrivacyPolicyId,
			completePrivacyPolicy.CreateTime,
			completePrivacyPolicy.ServiceName,
			completePrivacyPolicy.Pending,
			completePrivacyPolicy.PrivacyPolicyText,
		))

		for _, serviceUrl := range completePrivacyPolicy.ServiceUrls {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO complete_privacy_policy_service_url (
						privacy_policy_id,
						service_url
					) VALUES ($1, $2)
				`,
				completePrivacyPolicy.PrivacyPolicyId,
				serviceUrl,
			))

		}

		for _, extractedUrl := range completePrivacyPolicy.ExtractedUrls {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO complete_privacy_policy_extracted_url (
						privacy_policy_id,
						extracted_url
					) VALUES ($1, $2)
				`,
				completePrivacyPolicy.PrivacyPolicyId,
				extractedUrl,
			))
		}

		server.RaisePgResult(tx.Exec(
			ctx,
			`
				INSERT INTO latest_complete_privacy_policy (
					service_name,
					privacy_policy_id
				) VALUES ($2, $1)
				ON CONFLICT (service_name) DO UPDATE
				SET privacy_policy_id = $1
			`,
			completePrivacyPolicy.PrivacyPolicyId,
			completePrivacyPolicy.ServiceName,
		))
	})
}

type GptBeMyPrivacyAgentArgs struct {
	CountryOfResidence  string                    `json:"country_of_residence"`
	RegionOfResidence   string                    `json:"region_of_residence"`
	CorrespondenceEmail string                    `json:"correspondence_email"`
	Consent             bool                      `json:"consent"`
	EmailText           *GptBeMyPrivacyAgentEmail `json:"email_text"`
	ServiceName         string                    `json:"service_name"`
	ServiceUser         string                    `json:"service_user"`
}

type GptBeMyPrivacyAgentEmail struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

type GptBeMyPrivacyAgentResult struct {
	Accepted bool `json:"accepted"`
}

func SetGptBeMyPrivacyAgentPending(
	beMyPrivacyAgent *GptBeMyPrivacyAgentArgs,
	clientSession *session.ClientSession,
) (*GptBeMyPrivacyAgentResult, error) {
	accepted := true

	server.Tx(clientSession.Ctx, func(tx server.PgTx) {
		privacyAgentRequestId := server.NewId()

		server.RaisePgResult(tx.Exec(
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
	})

	return &GptBeMyPrivacyAgentResult{
		Accepted: accepted,
	}, nil
}
