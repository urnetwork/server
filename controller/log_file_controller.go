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
	"github.com/golang/glog"
	"github.com/urnetwork/server"
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
	out, err := uploader.UploadWithContext(session.Ctx, &s3manager.UploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(key),
		Body:        cr,
		ContentType: aws.String(uploadFile.ContentType),
	})
	if err != nil {
		return nil, err
	}

	glog.Infof("[log_upload] uploaded key=%s size=%d etag=%s\n", key, cr.n, aws.StringValue(out.ETag))

	return &UploadLogFileResult{}, nil
}

/**
 * output will look like logs/<year>/<month>/<day>/<time>/<network_x>/<user_y>/<client_z>/<feedback_id>
 */
func buildLogKey(uploadFile UploadLogFileArgs, sess *session.ClientSession) string {
	ts := uploadFile.Now.UTC().Format("2006/01/02/15_04_05")

	parts := []string{
		"logs",
		ts,
		"network_" + sess.ByJwt.NetworkId.String(),
		"user_" + sess.ByJwt.UserId.String(),
	}
	if sess.ByJwt.ClientId != nil {
		parts = append(parts, "client_"+sess.ByJwt.ClientId.String())
	}
	if uploadFile.FeedbackId != nil {
		parts = append(parts, "feedback_"+uploadFile.FeedbackId.String())
	}
	return path.Join(parts...)
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
