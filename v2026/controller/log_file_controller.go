package controller

import (
	"fmt"
	"io"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Log file storage is disabled: the upload body is drained and discarded, and
// only per-upload metadata is kept (`model.CreateFeedbackLogUpload`). When
// storage is re-enabled it should write to minio, not the db or aws s3, keyed
// logs/network_<network_id>/feedback_<feedback_id>.zip
const LogFileMaxByteCount = int64(100 * 1024 * 1024)

type UploadLogFileArgs struct {
	// OriginalFilename string
	FeedbackId  *server.Id
	ContentType string
	NetworkId   server.Id
	UserId      server.Id
	ClientId    *server.Id
	Now         time.Time
}

type UploadLogFileError struct {
	Message string `json:"message"`
}

type UploadLogFileResult struct {
	Error *UploadLogFileError `json:"error,omitempty"`
}

func UploadLogFile(
	session *session.ClientSession,
	body io.ReadCloser,
	uploadFile UploadLogFileArgs,
) (*UploadLogFileResult, error) {

	defer body.Close()

	feedback, err := model.GetFeedbackById(uploadFile.FeedbackId, session)
	if err != nil {
		return nil, err
	}
	if feedback == nil {
		return nil, fmt.Errorf("%d Feedback not found.", 404)
	}

	if feedback.NetworkId != session.ByJwt.NetworkId {
		return nil, fmt.Errorf("%d Feedback does not belong to your network.", 403)
	}

	feedbackLogUploadId, allowed := model.CreateFeedbackLogUpload(
		session.Ctx,
		model.CreateFeedbackLogUploadArgs{
			FeedbackId:  feedback.FeedbackId,
			NetworkId:   session.ByJwt.NetworkId,
			UserId:      session.ByJwt.UserId,
			ClientId:    session.ByJwt.ClientId,
			ContentType: uploadFile.ContentType,
			Now:         uploadFile.Now,
		},
	)
	if !allowed {
		return &UploadLogFileResult{
			Error: &UploadLogFileError{
				Message: fmt.Sprintf("Rate limited. One log upload per network per %s.", model.FeedbackLogUploadRatePeriod),
			},
		}, nil
	}

	byteCount, err := io.Copy(io.Discard, io.LimitReader(body, LogFileMaxByteCount+1))
	complete := err == nil && byteCount <= LogFileMaxByteCount
	model.FinishFeedbackLogUpload(session.Ctx, feedbackLogUploadId, byteCount, complete)
	if err != nil {
		return nil, err
	}
	if !complete {
		return &UploadLogFileResult{
			Error: &UploadLogFileError{
				Message: fmt.Sprintf("Log file exceeds the maximum upload size %dmb.", LogFileMaxByteCount/(1024*1024)),
			},
		}, nil
	}

	return &UploadLogFileResult{}, nil
}
