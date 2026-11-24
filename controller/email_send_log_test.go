package controller

// A failed email or SMS send logs no address (I-13). The error of a failed send
// can carry the recipient's address or number, as SES's and Pinpoint's
// messages do, so each line about one names the failure's kind and the
// network, code or channel it already has, as the account message outbox's
// lines do: the AWS sender's own lines, the epoch earnings run's warning, the
// verification and password reset code sends. Before, each of those lines
// printed the send's error.
//
// Each test reads what its sends log from a subprocess of this test binary
// (TestEmailSendLogChild), with glog writing every line to stderr at
// verbosity 1, so the capture reads the real sink without changing this
// process's logging or racing another test's. Every capture must hold the
// line it expects, so a line that went missing cannot pass for a clean one,
// and every error the senders return must name the address, so it is the
// lines that keep the address out.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/credentials"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The environment variable that names the scenario TestEmailSendLogChild runs.
const emailSendLogChildEnv = "URNETWORK_TEST_EMAIL_SEND_LOG_CHILD"

// The lines around a scenario's sends in the subprocess's output, and the
// prefix of each fact it reports after them.
const (
	emailSendLogCaptureStart = "synthetic-email-send-log-capture-start"
	emailSendLogCaptureEnd   = "synthetic-email-send-log-capture-end"
	emailSendLogFactPrefix   = "synthetic-email-send-log-fact "
)

// The synthetic recipients' domain, which no captured line may name.
const emailSendLogDomain = "email-send-log.example"

// Runs the scenario in a subprocess of this test binary, glog writing every
// line to stderr at verbosity 1, and returns what the subprocess wrote
// between the scenario's start and end, and the facts it reported after.
func emailSendLogOutput(t *testing.T, scenario string) (string, map[string]string) {
	t.Helper()
	command := exec.CommandContext(
		t.Context(),
		os.Args[0],
		"-test.run=^TestEmailSendLogChild$",
		"-test.count=1",
		// the subprocess ends by then, whatever becomes of this process
		"-test.timeout=5m",
		// exactly the flag server's init looks for, so it leaves glog's flags
		// to these
		"-logtostderr",
		"-v=1",
	)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, emailSendLogChildEnv+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env, emailSendLogChildEnv+"="+scenario)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("the %s subprocess failed: %v\n%s", scenario, err, output)
	}
	start := []byte(emailSendLogCaptureStart + "\n")
	end := []byte(emailSendLogCaptureEnd + "\n")
	begin := bytes.Index(output, start)
	finish := bytes.Index(output, end)
	if begin < 0 || finish < begin+len(start) || bytes.Count(output, start) != 1 || bytes.Count(output, end) != 1 {
		t.Fatalf("the %s subprocess did not run its capture once:\n%s", scenario, output)
	}
	facts := map[string]string{}
	for _, line := range strings.Split(string(output[finish+len(end):]), "\n") {
		if fact, ok := strings.CutPrefix(line, emailSendLogFactPrefix); ok {
			key, value, _ := strings.Cut(fact, "=")
			facts[key] = value
		}
	}
	return string(output[begin+len(start) : finish]), facts
}

// Fails the test if the capture names any of the values.
func requireEmailSendLogOmits(t *testing.T, capture string, values ...string) {
	t.Helper()
	for _, value := range values {
		if strings.Contains(capture, value) {
			t.Fatalf("a line names %q:\n%s", value, capture)
		}
	}
}

// Fails the test unless a line of the capture ends with the message, after
// glog's header.
func requireEmailSendLogLine(t *testing.T, capture string, message string) {
	t.Helper()
	for _, line := range strings.Split(capture, "\n") {
		if strings.HasSuffix(line, "] "+message) {
			return
		}
	}
	t.Fatalf("no line says %q:\n%s", message, capture)
}

// Fails the test unless some line of the capture holds the text.
func requireEmailSendLogText(t *testing.T, capture string, text string) {
	t.Helper()
	if !strings.Contains(capture, text) {
		t.Fatalf("no line holds %q:\n%s", text, capture)
	}
}

// A sender whose every send fails with an error naming its recipient, as an
// SES rejection does: one that is `context.DeadlineExceeded` for a recipient
// in timeoutUserAuths, else a rejection. It keeps the errors it returned.
type emailSendLogTestSender struct {
	timeoutUserAuths map[string]bool

	stateLock sync.Mutex
	errs      []error
}

func (self *emailSendLogTestSender) SendAccountMessageTemplate(userAuth string, template Template, sendOpts ...any) error {
	err := fmt.Errorf("MessageRejected: Email address is not verified. The following identities failed the check in region US-WEST-1: %s", userAuth)
	if self.timeoutUserAuths[userAuth] {
		err = fmt.Errorf("email send to %s timed out: %w", userAuth, context.DeadlineExceeded)
	}
	func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.errs = append(self.errs, err)
	}()
	return err
}

// Fails the test unless the sender returned one error for each address, each
// naming its address, so a line that printed an error would print it.
func (self *emailSendLogTestSender) requireErrorsNameTheirAddresses(t testing.TB, userAuths ...string) {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.errs) != len(userAuths) {
		t.Fatalf("the sender failed %d sends, want %d", len(self.errs), len(userAuths))
	}
	for _, userAuth := range userAuths {
		named := false
		for _, err := range self.errs {
			named = named || strings.Contains(err.Error(), userAuth)
		}
		if !named {
			t.Fatalf("no send error named %s", userAuth)
		}
	}
}

// A local stand-in for an AWS endpoint that refuses every call, as SES or
// Pinpoint refuses a message: status 400 with the content type, headers and
// body given. It counts the calls it read.
type emailSendLogAwsEndpoint struct {
	server    *httptest.Server
	callCount atomic.Int32
}

func newEmailSendLogAwsEndpoint(t testing.TB, header http.Header, body string) *emailSendLogAwsEndpoint {
	endpoint := &emailSendLogAwsEndpoint{}
	endpoint.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		endpoint.callCount.Add(1)
		for key, values := range header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, body)
	}))
	t.Cleanup(endpoint.server.Close)
	return endpoint
}

// A sender pointed at the endpoint, with synthetic credentials and its own
// email config, so no email.yml is read.
func (self *emailSendLogAwsEndpoint) sender() *AWSMessageSender {
	return &AWSMessageSender{
		sendTimeout: 30 * time.Second,
		emailConfig: &EmailConfig{CompanySenderEmail: "sender@synthetic-company.example"},
		endpoint:    self.server.URL,
		credentials: credentials.NewStaticCredentials("synthetic-access-key", "synthetic-secret-key", ""),
	}
}

// The AWS sender's lines about an SES rejection naming the recipient name the
// channel, the kind and the SDK's code: the bounded report and, at verbosity
// 1, the line for every failure, which printed the error.
func TestAwsMessageSenderEmailRefusalLogsNoAddress(t *testing.T) {
	capture, _ := emailSendLogOutput(t, "aws-email")
	requireEmailSendLogText(t, capture, "[aws]account email send failed (MessageRejected); 0 more failed sends since the last report")
	requireEmailSendLogLine(t, capture, "[aws]account email send failed (MessageRejected)")
	requireEmailSendLogOmits(t, capture, "@"+emailSendLogDomain)
}

// The same for a Pinpoint refusal naming the phone number.
func TestAwsMessageSenderSmsRefusalLogsNoNumber(t *testing.T) {
	capture, _ := emailSendLogOutput(t, "aws-sms")
	requireEmailSendLogText(t, capture, "[aws]account sms send failed (ValidationException); 0 more failed sends since the last report")
	requireEmailSendLogLine(t, capture, "[aws]account sms send failed (ValidationException)")
	requireEmailSendLogOmits(t, capture, "5555550100", "555 555 0100")
}

// The epoch earnings run's warning about each failed send names the network
// and the kind, a rejection or a timeout, and never the address its error
// names.
func TestEpochEarningsSendFailureLogsNoAddress(t *testing.T) {
	capture, facts := emailSendLogOutput(t, "epoch-earnings")
	if facts["failed"] == "" || facts["timeout"] == "" {
		t.Fatalf("the subprocess reported no networks: %v", facts)
	}
	requireEmailSendLogLine(t, capture, fmt.Sprintf("[st]epoch 9 earnings email 1 of 1 to network %s: send failed", facts["failed"]))
	requireEmailSendLogLine(t, capture, fmt.Sprintf("[st]epoch 9 earnings email 1 of 1 to network %s: send timeout", facts["timeout"]))
	requireEmailSendLogOmits(t, capture, "@epoch-earnings.example")
}

// A verification code's failed send names its kind, at verbosity 1.
func TestAuthVerifySendFailureLogsNoAddress(t *testing.T) {
	capture, _ := emailSendLogOutput(t, "auth-verify")
	requireEmailSendLogLine(t, capture, "[auth]verification code send failed")
	requireEmailSendLogOmits(t, capture, "@"+emailSendLogDomain)
}

// A password reset code's send that timed out names its kind, at verbosity 1.
func TestAuthPasswordResetSendFailureLogsNoAddress(t *testing.T) {
	capture, _ := emailSendLogOutput(t, "auth-reset")
	requireEmailSendLogLine(t, capture, "[auth]password reset code send timeout")
	requireEmailSendLogOmits(t, capture, "@"+emailSendLogDomain)
}

// A normal run skips this helper: the tests above run it in a subprocess, one
// scenario each, so what it logs reaches the real sink of a process of its own
// and no other test's logging is changed or read.
func TestEmailSendLogChild(t *testing.T) {
	scenario := os.Getenv(emailSendLogChildEnv)
	if scenario == "" {
		t.Skip("subprocess-only email send log capture")
	}
	captureStart := func() {
		glog.Flush()
		fmt.Fprintln(os.Stderr, emailSendLogCaptureStart)
	}
	captureEnd := func() {
		glog.Flush()
		fmt.Fprintln(os.Stderr, emailSendLogCaptureEnd)
	}
	fact := func(key string, value string) {
		fmt.Fprintf(os.Stderr, "%s%s=%s\n", emailSendLogFactPrefix, key, value)
	}

	switch scenario {
	case "aws-email":
		address := "rejected@" + emailSendLogDomain
		endpoint := newEmailSendLogAwsEndpoint(
			t,
			http.Header{"Content-Type": {"text/xml"}},
			`<ErrorResponse xmlns="http://ses.amazonaws.com/doc/2010-12-01/">
  <Error>
    <Type>Sender</Type>
    <Code>MessageRejected</Code>
    <Message>Email address is not verified. The following identities failed the check in region US-WEST-1: `+address+`</Message>
  </Error>
  <RequestId>synthetic-request-id</RequestId>
</ErrorResponse>`,
		)
		captureStart()
		err := endpoint.sender().SendAccountMessageTemplate(address, &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		captureEnd()
		if err == nil || !strings.Contains(err.Error(), address) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send error = %v, want SES's rejection naming %s", err, address)
		}
		if callCount := endpoint.callCount.Load(); callCount != 1 {
			t.Fatalf("the endpoint read %d calls, want 1", callCount)
		}

	case "aws-sms":
		endpoint := newEmailSendLogAwsEndpoint(
			t,
			http.Header{
				"Content-Type":     {"application/x-amz-json-1.0"},
				"X-Amzn-Errortype": {"ValidationException"},
			},
			`{"__type":"ValidationException","message":"1 validation error detected: Value '+15555550100' at 'destinationPhoneNumber' failed to satisfy constraint"}`,
		)
		captureStart()
		err := endpoint.sender().SendAccountMessageTemplate("+1 555 555 0100", &AuthVerifyTemplate{VerifyCode: "SYNTHETIC1"})
		captureEnd()
		if err == nil || !strings.Contains(err.Error(), "+15555550100") || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("send error = %v, want Pinpoint's refusal naming the number", err)
		}
		if callCount := endpoint.callCount.Load(); callCount != 1 {
			t.Fatalf("the endpoint read %d calls, want 1", callCount)
		}

	case "auth-verify":
		address := "verify@" + emailSendLogDomain
		sender := &emailSendLogTestSender{}
		captureStart()
		result, err := authVerifySend(AuthVerifySendArgs{UserAuth: address}, verifySendTestSession(), createCodeOk, sender)
		captureEnd()
		if err != nil || result.Error == nil || result.Error.Code != model.AuthVerifySendErrorCodeSendFailed {
			t.Fatalf("verify send = %+v, %v; want a failed send", result, err)
		}
		sender.requireErrorsNameTheirAddresses(t, address)

	case "auth-reset":
		address := "reset@" + emailSendLogDomain
		sender := &emailSendLogTestSender{timeoutUserAuths: map[string]bool{address: true}}
		captureStart()
		result, err := authPasswordReset(AuthPasswordResetArgs{UserAuth: address}, verifySendTestSession(), resetCreateCodeOk, sender)
		captureEnd()
		if err != nil || result.Error == nil || result.Error.Code != model.AuthVerifySendErrorCodeSendFailed {
			t.Fatalf("password reset = %+v, %v; want a failed send", result, err)
		}
		sender.requireErrorsNameTheirAddresses(t, address)

	case "epoch-earnings":
		(&server.TestEnv{ApplyDbMigrations: true, RerunCount: 0}).Run(t, func(t testing.TB) {
			f := newEpochEarningsTestFixture(t)
			failedNetworkId, _, failedAddress := newEpochEarningsTestNetwork(t, f.ctx, "failed")
			timeoutNetworkId, _, timeoutAddress := newEpochEarningsTestNetwork(t, f.ctx, "timeout")
			f.finalize(9, map[server.Id]int{
				failedNetworkId:  3000,
				timeoutNetworkId: 2000,
			})
			sender := &emailSendLogTestSender{timeoutUserAuths: map[string]bool{timeoutAddress: true}}
			captureStart()
			sent, skipped := f.send(9, sender)
			captureEnd()
			if sent != 0 || skipped != 2 {
				t.Fatalf("sent %d and skipped %d, want both sends failed and skipped", sent, skipped)
			}
			sender.requireErrorsNameTheirAddresses(t, failedAddress, timeoutAddress)
			fact("failed", failedNetworkId.String())
			fact("timeout", timeoutNetworkId.String())
		})

	default:
		t.Fatalf("unknown email send log scenario %q", scenario)
	}
}
