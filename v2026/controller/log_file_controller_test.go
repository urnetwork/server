package controller

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

func testBody() io.ReadCloser {
	data := []byte("for testing log file upload")
	return io.NopCloser(bytes.NewReader(data))
}

func testFeedback(t testing.TB, userSession *session.ClientSession) server.Id {
	feedback := model.FeedbackSendArgs{
		StarCount: 5,
		Uses:      model.FeedbackSendUses{},
		Needs:     model.FeedbackSendNeeds{},
	}
	sendResult, err := model.FeedbackSend(feedback, userSession)
	connect.AssertEqual(t, err, nil)
	return sendResult.FeedbackId
}

func TestLogFileShouldFail(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		// create feedback
		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		feedbackId := testFeedback(t, userSession)

		// different network tries to submit log file with this feedback id

		networkIdB := server.NewId()
		clientIdB := server.NewId()

		userSessionB := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkIdB,
			ClientId:  &clientIdB,
		})

		uploadArgs := UploadLogFileArgs{
			FeedbackId:  &feedbackId,
			ContentType: "text/plain",
			NetworkId:   userSessionB.ByJwt.NetworkId,
			UserId:      userSessionB.ByJwt.UserId,
			ClientId:    userSessionB.ByJwt.ClientId,
			Now:         server.NowUtc(),
		}

		// upload should be blocked
		_, err := UploadLogFile(
			userSessionB,
			testBody(),
			uploadArgs,
		)
		connect.AssertNotEqual(t, err, nil)

	})
}

func TestLogFileUpload(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		feedbackId := testFeedback(t, userSession)

		now := server.NowUtc()

		uploadArgs := UploadLogFileArgs{
			FeedbackId:  &feedbackId,
			ContentType: "text/plain",
			NetworkId:   userSession.ByJwt.NetworkId,
			UserId:      userSession.ByJwt.UserId,
			ClientId:    userSession.ByJwt.ClientId,
			Now:         now,
		}

		result, err := UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		// the upload metadata is retained without the file content
		uploads := model.GetFeedbackLogUploads(ctx, networkId)
		connect.AssertEqual(t, len(uploads), 1)
		connect.AssertEqual(t, uploads[0].FeedbackId, feedbackId)
		connect.AssertEqual(t, uploads[0].NetworkId, networkId)
		connect.AssertEqual(t, *uploads[0].ClientId, clientId)
		connect.AssertEqual(t, uploads[0].ContentType, "text/plain")
		connect.AssertEqual(t, uploads[0].ByteCount, int64(27))
		connect.AssertEqual(t, uploads[0].Complete, true)

		// a second upload in the same rate bucket is rejected
		result, err = UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

		uploads = model.GetFeedbackLogUploads(ctx, networkId)
		connect.AssertEqual(t, len(uploads), 1)

		// after the rate period the upload is allowed again
		uploadArgs.Now = now.Add(model.FeedbackLogUploadRatePeriod)
		result, err = UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		uploads = model.GetFeedbackLogUploads(ctx, networkId)
		connect.AssertEqual(t, len(uploads), 2)

		// another network is rate limited independently
		networkIdB := server.NewId()
		clientIdB := server.NewId()

		userSessionB := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkIdB,
			ClientId:  &clientIdB,
		})

		feedbackIdB := testFeedback(t, userSessionB)

		uploadArgsB := UploadLogFileArgs{
			FeedbackId:  &feedbackIdB,
			ContentType: "text/plain",
			NetworkId:   userSessionB.ByJwt.NetworkId,
			UserId:      userSessionB.ByJwt.UserId,
			ClientId:    userSessionB.ByJwt.ClientId,
			Now:         now,
		}

		result, err = UploadLogFile(userSessionB, testBody(), uploadArgsB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

	})
}

func TestLogFileUploadMaxSize(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()

		networkId := server.NewId()
		clientId := server.NewId()

		userSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		})

		feedbackId := testFeedback(t, userSession)

		uploadArgs := UploadLogFileArgs{
			FeedbackId:  &feedbackId,
			ContentType: "application/zip",
			NetworkId:   userSession.ByJwt.NetworkId,
			UserId:      userSession.ByJwt.UserId,
			ClientId:    userSession.ByJwt.ClientId,
			Now:         server.NowUtc(),
		}

		oversizeBody := io.NopCloser(io.LimitReader(discardableReader{}, LogFileMaxByteCount+1))

		result, err := UploadLogFile(userSession, oversizeBody, uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

		uploads := model.GetFeedbackLogUploads(ctx, networkId)
		connect.AssertEqual(t, len(uploads), 1)
		connect.AssertEqual(t, uploads[0].ByteCount, LogFileMaxByteCount+1)
		connect.AssertEqual(t, uploads[0].Complete, false)

		// the rate bucket was consumed by the oversize attempt
		result, err = UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

	})
}

// an endless reader for upload bodies whose content does not matter
type discardableReader struct{}

func (discardableReader) Read(p []byte) (int, error) {
	return len(p), nil
}
