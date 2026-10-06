// Every SES and SMS call is bounded by the send timeout. The tests point the
// sender at a local server standing in for the AWS endpoint: one that never
// answers must fail the send with a timeout once the bound passes, and one that
// answers must still deliver.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The longest a test waits for a send that should have stopped at its bound.
// It only guards a broken bound from hanging the suite.
const awsSendTimeoutTestGuard = 1 * time.Minute

// A local stand-in for an AWS endpoint that either never answers or answers
// with a fixed body, and keeps the last request it read.
type awsEndpointFixture struct {
	server      *httptest.Server
	received    chan *http.Request
	release     chan struct{}
	contentType string
	body        string
}

// Answers no request until the test ends.
func newSilentAwsEndpoint(t *testing.T) *awsEndpointFixture {
	return newAwsEndpoint(t, "", "")
}

// Answers every request with `body`. An empty body never answers.
func newAwsEndpoint(t *testing.T, contentType string, body string) *awsEndpointFixture {
	fixture := &awsEndpointFixture{
		received:    make(chan *http.Request, 16),
		release:     make(chan struct{}),
		contentType: contentType,
		body:        body,
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestBody, _ := io.ReadAll(r.Body)
		clone := r.Clone(context.Background())
		clone.Body = io.NopCloser(bytes.NewReader(requestBody))
		select {
		case fixture.received <- clone:
		default:
		}
		if fixture.body == "" {
			// hold the call until the client gives up or the test ends
			select {
			case <-fixture.release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", fixture.contentType)
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, fixture.body)
	}))
	t.Cleanup(func() {
		close(fixture.release)
		fixture.server.Close()
	})
	return fixture
}

// A sender pointed at the fixture with synthetic credentials.
func (self *awsEndpointFixture) sender(sendTimeout time.Duration) *AWSMessageSender {
	return &AWSMessageSender{
		sendTimeout: sendTimeout,
		endpoint:    self.server.URL,
		credentials: credentials.NewStaticCredentials("synthetic-access-key", "synthetic-secret-key", ""),
	}
}

// The request the endpoint read, or nil if none arrived.
func (self *awsEndpointFixture) lastRequest() *http.Request {
	select {
	case r := <-self.received:
		return r
	default:
		return nil
	}
}

// Sends on another goroutine and returns its error, failing the test if the
// send outlives the guard.
func sendWithGuard(t *testing.T, send func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- send()
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(awsSendTimeoutTestGuard):
		t.Fatalf("the send did not return within %s", awsSendTimeoutTestGuard)
		return nil
	}
}

// An email whose SES call never answers fails with a timeout at the bound,
// while the endpoint is still holding the call.
func TestAWSMessageSenderEmailStopsAtTheSendTimeout(t *testing.T) {
	endpoint := newSilentAwsEndpoint(t)
	sender := endpoint.sender(200 * time.Millisecond)
	before := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelEmail, "timeout"))

	startTime := time.Now()
	err := sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate(
			"timeout@synthetic.example",
			&AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"},
			SenderEmail("sender@synthetic.example"),
		)
	})
	if err == nil {
		t.Fatal("the send to an endpoint that never answers succeeded")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send error = %v, want a timeout", err)
	}
	if elapsed := time.Since(startTime); elapsed < 200*time.Millisecond {
		t.Fatalf("the send gave up after %s, before its bound", elapsed)
	}
	request := endpoint.lastRequest()
	if request == nil {
		t.Fatal("the endpoint saw no SES call")
	}
	if err := request.ParseForm(); err != nil || request.PostForm.Get("Action") != "SendEmail" {
		t.Fatalf("the endpoint saw %v (%v), want an SES SendEmail call", request.PostForm, err)
	}
	if delta := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelEmail, "timeout")) - before; delta != 1 {
		t.Fatalf("counted %v email timeouts, want 1", delta)
	}
}

// An SMS whose Pinpoint call never answers fails with a timeout at the bound.
func TestAWSMessageSenderSmsStopsAtTheSendTimeout(t *testing.T) {
	endpoint := newSilentAwsEndpoint(t)
	sender := endpoint.sender(200 * time.Millisecond)
	before := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelSms, "timeout"))

	err := sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate("+15555550100", &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send error = %v, want a timeout", err)
	}
	request := endpoint.lastRequest()
	if request == nil || request.Header.Get("X-Amz-Target") != "PinpointSMSVoiceV2.SendTextMessage" {
		t.Fatalf("the endpoint saw %v, want a Pinpoint SendTextMessage call", request)
	}
	if delta := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelSms, "timeout")) - before; delta != 1 {
		t.Fatalf("counted %v SMS timeouts, want 1", delta)
	}
}

// An email whose SES call answers is sent, with the recipient and sender the
// caller gave.
func TestAWSMessageSenderEmailSendsWhenSesAnswers(t *testing.T) {
	endpoint := newAwsEndpoint(t, "text/xml", `<SendEmailResponse xmlns="http://ses.amazonaws.com/doc/2010-12-01/">
  <SendEmailResult>
    <MessageId>synthetic-message-id</MessageId>
  </SendEmailResult>
  <ResponseMetadata>
    <RequestId>synthetic-request-id</RequestId>
  </ResponseMetadata>
</SendEmailResponse>`)
	sender := endpoint.sender(5 * time.Second)
	before := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelEmail, "sent"))

	err := sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate(
			"answered@synthetic.example",
			&AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"},
			SenderEmail("sender@synthetic.example"),
		)
	})
	if err != nil {
		t.Fatalf("send error = %v, want none", err)
	}
	request := endpoint.lastRequest()
	if request == nil {
		t.Fatal("the endpoint saw no SES call")
	}
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	}
	form := request.PostForm
	if form.Get("Action") != "SendEmail" ||
		form.Get("Destination.ToAddresses.member.1") != "answered@synthetic.example" ||
		form.Get("Source") != "sender@synthetic.example" {
		t.Fatalf("SES call = %v, want SendEmail to the recipient from the sender", url.Values(form))
	}
	if delta := testutil.ToFloat64(accountMessageSendCounter.WithLabelValues(accountMessageChannelEmail, "sent")) - before; delta != 1 {
		t.Fatalf("counted %v sent emails, want 1", delta)
	}
}

// An SMS whose Pinpoint call answers is sent to the stripped phone number.
func TestAWSMessageSenderSmsSendsWhenPinpointAnswers(t *testing.T) {
	endpoint := newAwsEndpoint(t, "application/x-amz-json-1.0", `{"MessageId":"synthetic-message-id"}`)
	sender := endpoint.sender(5 * time.Second)

	err := sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate("+1 555 555 0100", &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
	})
	if err != nil {
		t.Fatalf("send error = %v, want none", err)
	}
	request := endpoint.lastRequest()
	if request == nil || request.Header.Get("X-Amz-Target") != "PinpointSMSVoiceV2.SendTextMessage" {
		t.Fatalf("the endpoint saw %v, want a Pinpoint SendTextMessage call", request)
	}
	var input struct {
		DestinationPhoneNumber string
		MessageType            string
	}
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		t.Fatal(err)
	}
	if input.DestinationPhoneNumber != "+15555550100" || input.MessageType != "TRANSACTIONAL" {
		t.Fatalf("Pinpoint call = %+v, want a transactional text to +15555550100", input)
	}
}

// The configured bound falls back to the default and is clamped.
func TestEmailConfigSendTimeout(t *testing.T) {
	cases := []struct {
		sendTimeoutSeconds int
		sendTimeout        time.Duration
	}{
		{sendTimeoutSeconds: 0, sendTimeout: DefaultAccountMessageSendTimeout},
		{sendTimeoutSeconds: -1, sendTimeout: DefaultAccountMessageSendTimeout},
		{sendTimeoutSeconds: 30, sendTimeout: 30 * time.Second},
		{sendTimeoutSeconds: 3600, sendTimeout: maxAccountMessageSendTimeout},
	}
	for _, c := range cases {
		emailConfig := &EmailConfig{SendTimeoutSeconds: c.sendTimeoutSeconds}
		if sendTimeout := emailConfig.SendTimeout(); sendTimeout != c.sendTimeout {
			t.Errorf("send_timeout_seconds %d: send timeout = %s, want %s", c.sendTimeoutSeconds, sendTimeout, c.sendTimeout)
		}
	}
}
