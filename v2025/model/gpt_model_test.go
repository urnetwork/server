package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestCompletePrivacyPolicy(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		completePrivacyPolicy := NewCompletePrivacyPolicy(
			"My Test Service",
			[]string{"https://testservice.com"},
			"this is the privacy policy",
			[]string{"https://testservice.com/privacy"},
		)

		SetCompletePrivacyPolicy(ctx, completePrivacyPolicy)

		completePrivacyPolicy2, err := GetCompletePrivacyPolicy(ctx, "My Test Service")
		assert.Equal(t, err, nil)
		assert.Equal(t, completePrivacyPolicy.PrivacyPolicyId, completePrivacyPolicy2.PrivacyPolicyId)
		assert.Equal(t, completePrivacyPolicy.ServiceName, completePrivacyPolicy2.ServiceName)
		assert.Equal(t, completePrivacyPolicy.ServiceUrls, completePrivacyPolicy2.ServiceUrls)
		assert.Equal(t, completePrivacyPolicy.PrivacyPolicyText, completePrivacyPolicy2.PrivacyPolicyText)
		assert.Equal(t, completePrivacyPolicy.ExtractedUrls, completePrivacyPolicy2.ExtractedUrls)
		assert.Equal(t, completePrivacyPolicy.CreateTime, completePrivacyPolicy2.CreateTime)
		assert.Equal(t, completePrivacyPolicy.Pending, completePrivacyPolicy2.Pending)
	})
}

func TestGptBeMyPrivacyAgent(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx := context.Background()

		networkId := server.NewId()
		userId := server.NewId()
		networkName := "test"
		guestMode := false

		byJwt := jwt.NewByJwt(
			networkId,
			userId,
			networkName,
			guestMode,
		)

		clientSession := session.Testing_CreateClientSession(
			ctx,
			byJwt,
		)

		beMyPrivacyAgent := &GptBeMyPrivacyAgentArgs{
			CountryOfResidence:  "test",
			RegionOfResidence:   "test",
			CorrespondenceEmail: "test",
			Consent:             true,
			EmailText: &GptBeMyPrivacyAgentEmail{
				To:      "test",
				Subject: "test",
				Body:    "test",
			},
			ServiceName: "test",
			ServiceUser: "test",
		}

		result, err := SetGptBeMyPrivacyAgentPending(beMyPrivacyAgent, clientSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, result.Accepted, true)
	})
}
