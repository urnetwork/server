package model

import (
	"context"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/session"
)

func TestFeedback(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

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
		connect.AssertEqual(t, err, nil)

		// get feedback
		getResult, err := GetFeedbackById(&sendResult.FeedbackId, userSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, getResult.NetworkId, networkId)

	})
}
