// Synthetic refusal bodies exercise quota and credential-echo boundaries;
// these tests never contact a provider or authorize a competition review.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Bounded private evidence retains the provider's actionable refusal taxonomy.
func TestWinnerModelRefusalRetainsQuotaIdentityWithoutApproval(t *testing.T) {
	content := []byte(`{"error":{"type":"insufficient_quota","code":"credit_balance_exhausted","message":"synthetic credits exhausted"}}`)
	response := &http.Response{StatusCode: 429, Header: http.Header{"X-Request-Id": []string{"synthetic-request"}}, Body: io.NopCloser(bytes.NewReader(content))}
	refusal := readWinnerModelRefusal(response, "synthetic-private-key", defaultWinnerModelSettings())
	digest := sha256.Sum256(content)
	if refusal.StatusCode != 429 || refusal.Code != "credit_balance_exhausted" || refusal.Type != "insufficient_quota" || refusal.RequestId != "synthetic-request" || !refusal.BodyComplete || refusal.CapturedBodySha256 != hex.EncodeToString(digest[:]) || refusal.CapturedBodyBytes != len(content) {
		t.Fatalf("quota refusal identity lost: %+v", refusal)
	}
	if strings.Contains(refusal.Error(), "synthetic credits") {
		t.Fatal("provider text escaped into ordinary error logs")
	}
}

// Redaction must decode escaped strings before checking for a credential echo.
func TestWinnerModelRefusalRedactsEscapedCredentialEcho(t *testing.T) {
	key := "synthetic-private-key"
	content := []byte(`{"error":{"code":"credit_balance_exhausted","message":"synthetic-\u0070rivate-key"},"synthetic-private-key":["synthetic-private-key"]}`)
	response := &http.Response{StatusCode: 401, Header: http.Header{"X-Request-Id": []string{key}}, Body: io.NopCloser(bytes.NewReader(content))}
	refusal := readWinnerModelRefusal(response, key, defaultWinnerModelSettings())
	if bytes.Contains(refusal.PrivateRedactedBody, []byte(key)) || bytes.Contains(refusal.PrivateRedactedBody, []byte(`\u0070rivate`)) || !bytes.Contains(refusal.PrivateRedactedBody, []byte("[redacted]")) || refusal.RequestId != "[redacted]" {
		t.Fatal("private refusal contains credential material")
	}
}

// Oversized and malformed replies retain only an explicitly partial/raw digest,
// never an unparsed body that might contain encoded credentials.
func TestWinnerModelRefusalBoundsOversizedAndMalformedReplies(t *testing.T) {
	settings := defaultWinnerModelSettings()
	settings.MaximumRefusalBytes = 8
	reader := strings.NewReader(strings.Repeat("x", 100))
	refusal := readWinnerModelRefusal(&http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(reader)}, "synthetic-key", settings)
	if refusal.BodyComplete || refusal.CapturedBodyBytes != 8 || reader.Len() != 91 || len(refusal.PrivateRedactedBody) != 0 {
		t.Fatal("oversized reply exceeded capture boundary")
	}
	settings = defaultWinnerModelSettings()
	refusal = readWinnerModelRefusal(&http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("invalid JSON"))}, "synthetic-key", settings)
	if !refusal.BodyComplete || len(refusal.PrivateRedactedBody) != 0 || refusal.Code != "" {
		t.Fatal("malformed provider body was interpreted as a judgment")
	}
}

// The actual model-execution boundary emits no review result and makes one
// attempt only when the provider refuses the request.
func TestWinnerModelExecutionReturnsTypedQuotaRefusalWithoutRetry(t *testing.T) {
	request, _ := winnerModelFixture()
	calls := 0
	client := &http.Client{Transport: winnerRefusalTransport(func(message *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"insufficient_quota","code":"credit_balance_exhausted"}}`))}, nil
	})}
	result, err := executeWinnerModel(context.Background(), client, "synthetic-private-key", request, defaultWinnerModelSettings())
	var refusal *winnerReviewProviderRefusal
	if result != nil || !errors.As(err, &refusal) || refusal.Code != "credit_balance_exhausted" || calls != 1 {
		t.Fatalf("provider refusal became a result or retry: calls=%d result=%v err=%v", calls, result, err)
	}
}

// The independent test-model owner has the same refusal boundary: unavailable
// credits cannot trigger tests, invent test evidence or reset a provider retry.
func TestSourceTestModelReturnsTypedQuotaRefusalWithoutExecuting(t *testing.T) {
	calls, executions := 0, 0
	client := &http.Client{Transport: winnerRefusalTransport(func(message *http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 429, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"insufficient_quota","code":"credit_balance_exhausted"}}`))}, nil
	})}
	evidence, err := executeSourceTestModel(context.Background(), client, "synthetic-private-key", sourceTestPlanFixture(), defaultSourceTestModelSettings(), func(context.Context) (sourceTestExecution, error) {
		executions++
		return sourceTestExecution{}, errors.New("must not execute")
	})
	var refusal *winnerReviewProviderRefusal
	if evidence != nil || !errors.As(err, &refusal) || refusal.Code != "credit_balance_exhausted" || calls != 1 || executions != 0 {
		t.Fatalf("quota refusal became test execution/evidence: calls=%d executions=%d evidence=%v err=%v", calls, executions, evidence, err)
	}
}

// Implements a deterministic in-process transport, never a real HTTP endpoint.
type winnerRefusalTransport func(*http.Request) (*http.Response, error)

func (self winnerRefusalTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return self(request)
}
