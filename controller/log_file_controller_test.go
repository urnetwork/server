package controller

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/lifecycle"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/jwt"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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

// A stored log file's key names its network and feedback under the logs/
// namespace.
func TestFeedbackLogKey(t *testing.T) {
	networkId := server.RequireParseId("0199a000-0000-7000-8000-00000000000a")
	feedbackId := server.RequireParseId("0199a000-0000-7000-8000-00000000000b")

	connect.AssertEqual(
		t,
		FeedbackLogKey(networkId, feedbackId),
		"logs/network_0199a000-0000-7000-8000-00000000000a/feedback_0199a000-0000-7000-8000-00000000000b.zip",
	)
}

// Log storage is on only with a `feedback_log_bucket` on a minio authority. A
// minio blob store alone (as the stats pipeline configures it) is not enough,
// and the local blob backend never stores log files.
func TestFeedbackLogStoreIsOffUnlessAMinioBucketIsSet(t *testing.T) {
	if _, present := server.LoadBlobStoreConfig(); !present {
		_, ok := LoadFeedbackLogStore()
		connect.AssertEqual(t, ok, false)
	}

	offMinioYmls := []string{
		`
authority: minio.example.com:9000
bucket: stats
access_key: access
secret_key: secret
`,
		`
authority: minio.example.com:9000
bucket: stats
access_key: access
secret_key: secret
feedback_log_bucket: "  "
`,
		`
authority: local
path: /tmp/blob
feedback_log_bucket: feedback-logs
`,
	}
	for _, minioYml := range offMinioYmls {
		pop := server.Vault.PushSimpleResource("minio.yml", []byte(minioYml))
		_, ok := LoadFeedbackLogStore()
		pop()
		connect.AssertEqual(t, ok, false)
	}

	pop := server.Vault.PushSimpleResource("minio.yml", []byte(`
authority: minio.example.com:9000
bucket: stats
access_key: access
secret_key: secret
feedback_log_bucket: feedback-logs
`))
	defer pop()
	store, ok := LoadFeedbackLogStore()
	connect.AssertEqual(t, ok, true)
	connect.AssertEqual(t, store.Bucket(), "feedback-logs")
	connect.AssertEqual(t, store.Prefix(), "logs")
	connect.AssertEqual(t, store.Authority(), "minio.example.com:9000")
}

// With a feedback log bucket set, an upload is stored in that bucket at the
// feedback's key, and the metadata row and the rate limit are unchanged.
// Without one the body is discarded and minio is never contacted.
func TestLogFileUploadStorageIsGatedByFeedbackLogBucket(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		fake := newFakeMinio(t)

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
			ContentType: "",
			NetworkId:   userSession.ByJwt.NetworkId,
			UserId:      userSession.ByJwt.UserId,
			ClientId:    userSession.ByJwt.ClientId,
			Now:         now,
		}

		// a minio blob store without a feedback log bucket: discarded
		pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "")))
		result, err := UploadLogFile(userSession, testBody(), uploadArgs)
		pop()
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)
		connect.AssertEqual(t, fake.RequestCount(), 0)

		// with a feedback log bucket: stored at the feedback's key
		pop = server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "feedback-logs")))
		defer pop()

		uploadArgs.Now = now.Add(model.FeedbackLogUploadRatePeriod)
		result, err = UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		writes := fake.Writes()
		connect.AssertEqual(t, len(writes), 1)
		connect.AssertEqual(t, writes[0].path, "/feedback-logs/"+FeedbackLogKey(networkId, feedbackId))
		connect.AssertEqual(t, writes[0].contentType, "application/zip")
		connect.AssertEqual(t, string(writes[0].body), "for testing log file upload")

		uploads := model.GetFeedbackLogUploads(ctx, networkId)
		connect.AssertEqual(t, len(uploads), 2)
		connect.AssertEqual(t, uploads[0].FeedbackId, feedbackId)
		connect.AssertEqual(t, uploads[0].ByteCount, int64(27))
		connect.AssertEqual(t, uploads[0].Complete, true)

		// a second upload in the same rate bucket is rejected before anything is stored
		result, err = UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)
		connect.AssertEqual(t, len(fake.Writes()), 1)

	})
}

// With storage on, an upload over the size cap is refused and never stored,
// and the next upload within the cap is stored.
func TestLogFileUploadOverMaxSizeIsNotStored(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		fake := newFakeMinio(t)
		pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "feedback-logs")))
		defer pop()

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
			ContentType: "application/zip",
			NetworkId:   userSession.ByJwt.NetworkId,
			UserId:      userSession.ByJwt.UserId,
			ClientId:    userSession.ByJwt.ClientId,
			Now:         now,
		}

		oversizeBody := io.NopCloser(io.LimitReader(discardableReader{}, LogFileMaxByteCount+1))

		result, err := UploadLogFile(userSession, oversizeBody, uploadArgs)
		// minio is never contacted for an oversize body
		connect.AssertEqual(t, fake.RequestCount(), 0)
		connect.AssertEqual(t, err, nil)
		connect.AssertNotEqual(t, result.Error, nil)

		uploads := model.GetFeedbackLogUploads(ctx, networkId)
		connect.AssertEqual(t, len(uploads), 1)
		connect.AssertEqual(t, uploads[0].ByteCount, LogFileMaxByteCount+1)
		connect.AssertEqual(t, uploads[0].Complete, false)

		uploadArgs.Now = now.Add(model.FeedbackLogUploadRatePeriod)
		result, err = UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, nil)

		writes := fake.Writes()
		connect.AssertEqual(t, len(writes), 1)
		connect.AssertEqual(t, string(writes[0].body), "for testing log file upload")

	})
}

// A failed store is reported to the client as a server error and the upload is
// recorded as incomplete; the body is not silently dropped.
func TestLogFileUploadStoreFailureIsReported(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {

		ctx := context.Background()
		fake := newFakeMinio(t)
		fake.DenyWrites()
		pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "feedback-logs")))
		defer pop()

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

		_, err := UploadLogFile(userSession, testBody(), uploadArgs)
		connect.AssertNotEqual(t, err, nil)
		connect.AssertEqual(t, strings.HasPrefix(err.Error(), "503 "), true)

		uploads := model.GetFeedbackLogUploads(ctx, networkId)
		connect.AssertEqual(t, len(uploads), 1)
		connect.AssertEqual(t, uploads[0].ByteCount, int64(27))
		connect.AssertEqual(t, uploads[0].Complete, false)

	})
}

// Stored log files expire after seven days: one rule over the whole logs/
// namespace, which holds every stored key.
func TestFeedbackLogRetentionRules(t *testing.T) {
	connect.AssertEqual(t, FeedbackLogRetention, 7*24*time.Hour)

	store := server.NewLocalBlobStore(t.TempDir(), FeedbackLogKeyPrefix)
	rules := feedbackLogLifecycleRules(store)
	connect.AssertEqual(t, len(rules), 1)
	connect.AssertEqual(t, rules[0].KeyPrefix, "logs/")
	connect.AssertEqual(t, rules[0].TTL, 7*24*time.Hour)

	key := FeedbackLogKey(server.NewId(), server.NewId())
	connect.AssertEqual(t, strings.HasPrefix(key, rules[0].KeyPrefix), true)
}

// ApplyFeedbackLogRetention sets the rule as minio ILM on the feedback log
// bucket, and leaves minio alone while log storage is off.
func TestApplyFeedbackLogRetentionSetsBucketLifecycle(t *testing.T) {
	ctx := context.Background()
	fake := newFakeMinio(t)

	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "")))
	ApplyFeedbackLogRetention(ctx)
	pop()
	connect.AssertEqual(t, fake.RequestCount(), 0)

	pop = server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "feedback-logs")))
	defer pop()
	ApplyFeedbackLogRetention(ctx)

	writes := fake.Writes()
	connect.AssertEqual(t, len(writes), 1)
	connect.AssertEqual(t, strings.Trim(writes[0].path, "/"), "feedback-logs")
	connect.AssertEqual(t, writes[0].query.Has("lifecycle"), true)

	config := lifecycle.NewConfiguration()
	err := xml.Unmarshal(writes[0].body, config)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(config.Rules), 1)
	connect.AssertEqual(t, config.Rules[0].ID, "urnetwork-ttl-logs")
	connect.AssertEqual(t, config.Rules[0].Status, "Enabled")
	connect.AssertEqual(t, config.Rules[0].RuleFilter.Prefix, "logs/")
	connect.AssertEqual(t, int(config.Rules[0].Expiration.Days), 7)
}

// A vault `minio.yml` that points the blob store at fake, with the given
// feedback log bucket (empty for none).
func fakeMinioYml(fake *fakeMinio, feedbackLogBucket string) string {
	return fmt.Sprintf(`
authority: %s
bucket: blob
prefix: blob
access_key: access
secret_key: secret
feedback_log_bucket: %q
`, fake.Authority(), feedbackLogBucket)
}

// A minimal S3 endpoint for the minio client behind the feedback
// log store; the repo has no local minio fixture. It answers bucket location
// reads (the client asks before its first request to a bucket), serves retained
// lifecycle state, accepts object and lifecycle writes, and records them.
type fakeMinio struct {
	server *httptest.Server

	stateLock           sync.Mutex
	requestCount        int
	writes              []fakeMinioWrite
	denyWrites          bool
	lifecycleConfigs    map[string][]byte
	afterLifecycleRead  func(context.Context, string) error
	afterLifecycleWrite func(string)
}

// One write the fake accepted.
type fakeMinioWrite struct {
	path        string
	query       url.Values
	contentType string
	// the written bytes, without the client's aws-chunked signing framing
	body []byte
}

// A running fake, closed when the test ends.
func newFakeMinio(t testing.TB) *fakeMinio {
	fake := &fakeMinio{}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

// The host:port a `minio.yml` names to reach the fake.
func (self *fakeMinio) Authority() string {
	return strings.TrimPrefix(self.server.URL, "http://")
}

// The requests the fake has answered.
func (self *fakeMinio) RequestCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.requestCount
}

// The writes the fake has accepted, oldest first.
func (self *fakeMinio) Writes() []fakeMinioWrite {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.writes)
}

// Makes every later write fail with a non-retryable access error.
func (self *fakeMinio) DenyWrites() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.denyWrites = true
}

// Counts one request and returns whether writes are denied.
func (self *fakeMinio) countRequest() (denyWrites bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.requestCount += 1
	return self.denyWrites
}

// Keeps an accepted write, and a lifecycle write as the bucket's lifecycle.
func (self *fakeMinio) recordWrite(write fakeMinioWrite) {
	self.stateLock.Lock()
	self.writes = append(self.writes, write)
	var afterWrite func(string)
	bucket := strings.Trim(write.path, "/")
	if write.query.Has("lifecycle") {
		if self.lifecycleConfigs == nil {
			self.lifecycleConfigs = map[string][]byte{}
		}
		self.lifecycleConfigs[bucket] = bytes.Clone(write.body)
		afterWrite = self.afterLifecycleWrite
	}
	self.stateLock.Unlock()
	if afterWrite != nil {
		afterWrite(bucket)
	}
}

// Answers one S3 request the way minio would for the requests the store sends.
func (self *fakeMinio) serve(w http.ResponseWriter, r *http.Request) {
	denyWrites := self.countRequest()
	query := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && query.Has("location"):
		writeFakeMinioXml(w, http.StatusOK, `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/"></LocationConstraint>`)
	case r.Method == http.MethodGet && query.Has("lifecycle"):
		bucket := strings.Trim(r.URL.Path, "/")
		self.stateLock.Lock()
		current := bytes.Clone(self.lifecycleConfigs[bucket])
		afterRead := self.afterLifecycleRead
		self.stateLock.Unlock()
		if afterRead != nil {
			if err := afterRead(r.Context(), bucket); err != nil {
				writeFakeMinioXml(w, http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>lifecycle read refused</Message></Error>`)
				return
			}
		}
		if len(current) == 0 {
			writeFakeMinioXml(w, http.StatusNotFound, `<Error><Code>NoSuchLifecycleConfiguration</Code><Message>none</Message></Error>`)
		} else {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write(current)
		}
	case r.Method == http.MethodPut && denyWrites:
		writeFakeMinioXml(w, http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>denied</Message></Error>`)
	case r.Method == http.MethodPut:
		// removes the aws-chunked framing the minio client signs a streaming
		// upload with: chunks of "<hex size>;chunk-signature=<sig>\r\n<data>\r\n",
		// ended by a zero-size chunk (and any trailer after it)
		decodeAwsChunked := func(body []byte) ([]byte, error) {
			decoded := []byte{}
			for {
				header, rest, ok := bytes.Cut(body, []byte("\r\n"))
				if !ok {
					return nil, errors.New("aws-chunked header is truncated")
				}
				sizeHex, _, _ := bytes.Cut(header, []byte(";"))
				size, err := strconv.ParseUint(string(sizeHex), 16, 31)
				if err != nil {
					return nil, err
				}
				if size == 0 {
					return decoded, nil
				}
				if uint64(len(rest)) < size+2 || !bytes.Equal(rest[size:size+2], []byte("\r\n")) {
					return nil, errors.New("aws-chunked data is truncated")
				}
				decoded = append(decoded, rest[:size]...)
				body = rest[size+2:]
			}
		}

		body, err := io.ReadAll(r.Body)
		if err == nil && strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			body, err = decodeAwsChunked(body)
		}
		if err != nil {
			writeFakeMinioXml(w, http.StatusBadRequest, `<Error><Code>InvalidRequest</Code><Message>bad body</Message></Error>`)
			return
		}
		self.recordWrite(fakeMinioWrite{
			path:        r.URL.Path,
			query:       query,
			contentType: r.Header.Get("Content-Type"),
			body:        body,
		})
		w.Header().Set("ETag", `"stored"`)
		w.WriteHeader(http.StatusOK)
	default:
		writeFakeMinioXml(w, http.StatusNotImplemented, `<Error><Code>NotImplemented</Code><Message>not faked</Message></Error>`)
	}
}

// Answers with an S3 xml document.
func writeFakeMinioXml(w http.ResponseWriter, statusCode int, body string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(statusCode)
	io.WriteString(w, `<?xml version="1.0" encoding="UTF-8"?>`+body)
}
