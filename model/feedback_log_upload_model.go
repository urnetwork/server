package model

import (
	"context"
	"time"

	"github.com/urnetwork/server"
)

// at most one log upload per network per rate period
const FeedbackLogUploadRatePeriod = 5 * time.Minute

type CreateFeedbackLogUploadArgs struct {
	FeedbackId  server.Id
	NetworkId   server.Id
	UserId      server.Id
	ClientId    *server.Id
	ContentType string
	Now         time.Time
}

// CreateFeedbackLogUpload records the metadata row for one upload attempt.
// `allowed` is false when the network already has an upload in the current
// rate bucket, in which case no row is written.
func CreateFeedbackLogUpload(
	ctx context.Context,
	createFeedbackLogUpload CreateFeedbackLogUploadArgs,
) (feedbackLogUploadId server.Id, allowed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		feedbackLogUploadId = server.NewId()
		rateBucket := createFeedbackLogUpload.Now.Unix() / int64(FeedbackLogUploadRatePeriod/time.Second)
		tag, err := tx.Exec(
			ctx,
			`
				INSERT INTO feedback_log_upload
				(
					feedback_log_upload_id,
					feedback_id,
					network_id,
					user_id,
					client_id,
					upload_time,
					rate_bucket,
					content_type
				)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
				ON CONFLICT (network_id, rate_bucket) DO NOTHING
			`,
			&feedbackLogUploadId,
			&createFeedbackLogUpload.FeedbackId,
			&createFeedbackLogUpload.NetworkId,
			&createFeedbackLogUpload.UserId,
			createFeedbackLogUpload.ClientId,
			createFeedbackLogUpload.Now.UTC(),
			rateBucket,
			createFeedbackLogUpload.ContentType,
		)
		server.Raise(err)
		allowed = tag.RowsAffected() == 1
	})
	return
}

// FinishFeedbackLogUpload records how many bytes the upload body carried.
// `complete` is false when the body was cut off by an error or exceeded the
// upload size limit.
func FinishFeedbackLogUpload(
	ctx context.Context,
	feedbackLogUploadId server.Id,
	byteCount int64,
	complete bool,
) {
	server.Tx(ctx, func(tx server.PgTx) {
		_, err := tx.Exec(
			ctx,
			`
				UPDATE feedback_log_upload
				SET byte_count = $2, complete = $3
				WHERE feedback_log_upload_id = $1
			`,
			&feedbackLogUploadId,
			byteCount,
			complete,
		)
		server.Raise(err)
	})
}

type FeedbackLogUpload struct {
	FeedbackLogUploadId server.Id
	FeedbackId          server.Id
	NetworkId           server.Id
	UserId              server.Id
	ClientId            *server.Id
	UploadTime          time.Time
	ContentType         string
	ByteCount           int64
	Complete            bool
}

func GetFeedbackLogUploads(
	ctx context.Context,
	networkId server.Id,
) (feedbackLogUploads []*FeedbackLogUpload) {
	server.Tx(ctx, func(tx server.PgTx) {
		result, err := tx.Query(
			ctx,
			`
				SELECT
					feedback_log_upload_id,
					feedback_id,
					network_id,
					user_id,
					client_id,
					upload_time,
					content_type,
					byte_count,
					complete
				FROM feedback_log_upload
				WHERE network_id = $1
				ORDER BY upload_time DESC
			`,
			&networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				feedbackLogUpload := &FeedbackLogUpload{}
				server.Raise(
					result.Scan(
						&feedbackLogUpload.FeedbackLogUploadId,
						&feedbackLogUpload.FeedbackId,
						&feedbackLogUpload.NetworkId,
						&feedbackLogUpload.UserId,
						&feedbackLogUpload.ClientId,
						&feedbackLogUpload.UploadTime,
						&feedbackLogUpload.ContentType,
						&feedbackLogUpload.ByteCount,
						&feedbackLogUpload.Complete,
					),
				)
				feedbackLogUploads = append(feedbackLogUploads, feedbackLogUpload)
			}
		})
	})
	return
}
