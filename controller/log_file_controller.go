package controller

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Log file storage is off unless ops sets `feedback_log_bucket` in the vault
// `minio.yml` (see LoadFeedbackLogStore). While it is off the upload body is
// drained and discarded, and only per-upload metadata is kept
// (`model.CreateFeedbackLogUpload`, which is also the rate limiter). While it
// is on the body is written to that minio bucket, not the db or aws s3, keyed
// logs/network_<network_id>/feedback_<feedback_id>.zip, and expires after
// FeedbackLogRetention. Either way the metadata row and the rate limit are the
// same. Apps upload logs only when the user sends feedback with logs.
const LogFileMaxByteCount = int64(100 * 1024 * 1024)

// FeedbackLogKeyPrefix is the object-key namespace of stored log files in the
// feedback log bucket.
const FeedbackLogKeyPrefix = "logs"

// FeedbackLogRetention is how long a stored log file is kept. The retention is
// a code-owned minio ILM rule (ApplyFeedbackLogRetention), the same as for the
// stats sample streams.
const FeedbackLogRetention = 7 * 24 * time.Hour

// the sdk uploads one zip of its log files (sdk `DeviceLocal.UploadLogs`)
const feedbackLogContentType = "application/zip"

// FeedbackLogKey is the object key of the log file stored for a feedback.
func FeedbackLogKey(networkId server.Id, feedbackId server.Id) string {
	return fmt.Sprintf(
		"%s/network_%s/feedback_%s.zip",
		FeedbackLogKeyPrefix,
		networkId.String(),
		feedbackId.String(),
	)
}

// LoadFeedbackLogStore returns the store that keeps uploaded log files, or
// ok=false when log storage is off. It is on only when the vault `minio.yml`
// names a `feedback_log_bucket` and a minio authority: the store uses the blob
// store's minio endpoint and credentials, in that bucket. A local blob backend
// never stores log files.
func LoadFeedbackLogStore() (store server.BlobStore, ok bool) {
	config, present := server.LoadBlobStoreConfig()
	if !present || config.Local || config.FeedbackLogBucket == "" {
		return nil, false
	}
	logConfig := *config
	logConfig.Bucket = config.FeedbackLogBucket
	logConfig.Prefix = FeedbackLogKeyPrefix
	store, err := server.NewBlobStore(&logConfig)
	if err != nil {
		glog.Infof("[log]feedback log store disabled: %s\n", err)
		return nil, false
	}
	return store, true
}

// feedbackLogLifecycleRules expires every stored log file after
// FeedbackLogRetention.
func feedbackLogLifecycleRules(store server.BlobStore) []server.BlobLifecycleRule {
	return []server.BlobLifecycleRule{
		{
			KeyPrefix: store.Prefix() + "/",
			TTL:       FeedbackLogRetention,
		},
	}
}

// ApplyFeedbackLogRetention installs the log file retention on the feedback log
// bucket (minio ILM; see `server.BlobStore.SetLifecycle`), like
// `stats.ApplyStreamRetention` does for the sample streams. Call once at
// taskworker init. Best-effort: it logs and never fails, and does nothing while
// log storage is off.
func ApplyFeedbackLogRetention(ctx context.Context) {
	store, ok := LoadFeedbackLogStore()
	if !ok {
		glog.Infof("[log]feedback log retention: log storage is off\n")
		return
	}
	if err := store.SetLifecycle(ctx, feedbackLogLifecycleRules(store)); err != nil {
		glog.Infof("[log]feedback log retention apply err=%s (not enforced by this process)\n", err)
		return
	}
	glog.Infof("[log]feedback log retention %s set -> %s/%s\n", FeedbackLogRetention, store.Authority(), store.Bucket())
}

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

	var byteCount int64
	var complete bool
	if store, ok := LoadFeedbackLogStore(); ok {
		byteCount, complete, err = storeLogFile(
			session.Ctx,
			store,
			FeedbackLogKey(feedback.NetworkId, feedback.FeedbackId),
			body,
		)
	} else {
		byteCount, err = io.Copy(io.Discard, io.LimitReader(body, LogFileMaxByteCount+1))
		complete = err == nil && byteCount <= LogFileMaxByteCount
	}
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

// storeLogFile stages the upload body in a temporary file, because the store
// uploads from a file, and puts it at key when the body fits the size cap. An
// oversize body is never stored. complete is false when the body was cut off by
// an error, exceeded the cap, or could not be stored.
func storeLogFile(
	ctx context.Context,
	store server.BlobStore,
	key string,
	body io.Reader,
) (byteCount int64, complete bool, err error) {
	temporary, err := os.CreateTemp("", "urnetwork-feedback-log-*.zip")
	if err != nil {
		glog.Infof("[log]feedback log stage err=%s\n", err)
		return 0, false, fmt.Errorf("%d Log file storage is unavailable.", 503)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)

	byteCount, err = io.Copy(temporary, io.LimitReader(body, LogFileMaxByteCount+1))
	if closeErr := temporary.Close(); err == nil && closeErr != nil {
		glog.Infof("[log]feedback log stage err=%s\n", closeErr)
		return byteCount, false, fmt.Errorf("%d Log file storage is unavailable.", 503)
	}
	if err != nil {
		// the body was cut off
		return byteCount, false, err
	}
	if LogFileMaxByteCount < byteCount {
		return byteCount, false, nil
	}

	if err := store.Put(ctx, key, temporaryPath, feedbackLogContentType); err != nil {
		glog.Infof("[log]feedback log store err=%s\n", err)
		return byteCount, false, fmt.Errorf("%d Log file storage is unavailable.", 503)
	}
	return byteCount, true, nil
}
