package controller

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/go-playground/assert/v2"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func testBody() io.ReadCloser {
	data := []byte("for testing log file upload")
	return io.NopCloser(bytes.NewReader(data))
}

func TestLogFileShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(func() {

		// create feedback
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		feedback := model.FeedbackSendArgs{
			StarCount: 5,
			Uses:      model.FeedbackSendUses{},
			Needs:     model.FeedbackSendNeeds{},
		}

		// save feedback
		sendResult, err := model.FeedbackSend(feedback, userSession)
		assert.Equal(t, err, nil)

		// different network tries to submit log file with this feedback id

		networkIdB := server.NewId()
		clientIdB := server.NewId()

		userSessionB := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkIdB,
			ClientId:  &clientIdB,
		})

		uploadArgs := UploadLogFileArgs{
			FeedbackId:  &sendResult.FeedbackId,
			ContentType: "text/plain",
			NetworkId:   userSessionB.ByJwt.NetworkId,
			UserId:      userSessionB.ByJwt.UserId,
			ClientId:    userSessionB.ByJwt.ClientId,
			Now:         server.NowUtc(),
		}

		// upload should be blocked
		_, err = UploadLogFile(
			userSessionB,
			testBody(),
			uploadArgs,
		)
		assert.NotEqual(t, err, nil)

	})
}
