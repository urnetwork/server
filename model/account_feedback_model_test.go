package model

import (
	"context"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestFeedback(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		feedback := FeedbackSendArgs{
			StarCount: 5,
			Uses:      FeedbackSendUses{},
			Needs:     FeedbackSendNeeds{},
		}

		// save feedback
		sendResult, err := FeedbackSend(feedback, userSession)
		assert.Equal(t, err, nil)

		// get feedback
		getResult, err := GetFeedbackById(&sendResult.FeedbackId, userSession)
		assert.Equal(t, err, nil)
		assert.Equal(t, getResult.NetworkId, networkId)

	})
}
