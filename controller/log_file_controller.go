package controller

import (
	"fmt"
	"io"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	awsSession "github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type UploadLogFileArgs struct {
	// OriginalFilename string
	FeedbackId  *server.Id
	ContentType string
	NetworkId   server.Id
	UserId      server.Id
	ClientId    *server.Id
	Now         time.Time
}

type UploadLogFileResult struct{}

var logBucket = sync.OnceValue(func() string {
	c := server.Config.RequireSimpleResource("aws.yml").Parse()
	return c["aws"].(map[string]any)["feedback_log_bucket"].(string)
})

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

	if feedback.NetworkId != session.ByJwt.NetworkId {
		return nil, fmt.Errorf("%d Feedback does not belong to your network.", 403)
	}

	bucket := strings.TrimSpace(logBucket())
	if bucket == "" {
		return nil, fmt.Errorf("%d Missing log file bucket configuration.", 500)
	}

	key := buildLogKey(uploadFile, session)

	// pull this from config?
	awsRegion := "us-west-1"

	awsSess, err := awsSession.NewSession(&aws.Config{Region: aws.String(awsRegion)})
	if err != nil {
		return nil, err
	}

	uploader := s3manager.NewUploader(awsSess)
	cr := &countingReader{r: body}
	_, err = uploader.UploadWithContext(session.Ctx, &s3manager.UploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        cr,
		ContentType: aws.String(uploadFile.ContentType),
	})
	if err != nil {
		return nil, err
	}

	return &UploadLogFileResult{}, nil
}

/**
 * output will look like logs/<network_x>/<feedback_id>
 */
func buildLogKey(uploadFile UploadLogFileArgs, sess *session.ClientSession) string {

	parts := []string{
		"logs",
		"network_" + sess.ByJwt.NetworkId.String(),
	}
	if uploadFile.FeedbackId != nil {
		parts = append(parts, "feedback_"+uploadFile.FeedbackId.String())
	}
	return path.Join(parts...) + ".zip"
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
