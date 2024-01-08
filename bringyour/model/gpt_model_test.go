package model


import (
    "context"
    "testing"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour/jwt"
    "bringyour.com/bringyour/session"
    "bringyour.com/bringyour"
)


func TestGptBeMyPrivacyAgent(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    networkId := bringyour.NewId()
    userId := bringyour.NewId()
    networkName := "test"

    byJwt := jwt.NewByJwt(
        networkId,
        userId,
        networkName,
    )

    clientSession := session.NewLocalClientSession(
        ctx,
        byJwt,
    )

    beMyPrivacyAgent := &GptBeMyPrivacyAgentArgs{
        CountryOfResidence: "test",
        RegionOfResidence: "test",
        CorrespondenceEmail: "test",
        Consent: true,
        EmailText: &GptBeMyPrivacyAgentEmail {
            To: "test",
            Subject: "test",
            Body: "test",
        },
        ServiceName: "test",
        ServiceUser: "test",
    }

    result, err := GptBeMyPrivacyAgent(beMyPrivacyAgent, clientSession)
    assert.Equal(t, err, nil)
    assert.Equal(t, result.Accepted, true)
})}

