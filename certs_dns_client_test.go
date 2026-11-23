package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"
)

// certs' dns client against a stand-in for the dns api (TLS-M5-16): each call
// carries the bearer to its route and nowhere else, a refusal reads as the
// solver expects it, and a client with no credential is not built.

// A synthetic bearer: 256 zero bits in base64url. No real credential.
const certsDnsTestBearer = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

// The dns api's two certs routes, answering what the test sets.
type certsDnsTestApi struct {
	stateLock sync.Mutex
	// what the next call answers, with its Retry-After when not empty
	status     int
	body       string
	retryAfter string
	// what the calls carried
	calls []certsDnsTestCall
}

type certsDnsTestCall struct {
	method        string
	path          string
	query         string
	authorization string
	body          string
}

func (self *certsDnsTestApi) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.calls = append(self.calls, certsDnsTestCall{
		method:        r.Method,
		path:          r.URL.Path,
		query:         r.URL.RawQuery,
		authorization: r.Header.Get("Authorization"),
		body:          string(body),
	})
	if self.status == http.StatusFound {
		http.Redirect(w, r, "http://127.0.0.1:1/elsewhere", http.StatusFound)
		return
	}
	if self.retryAfter != "" {
		w.Header().Set("Retry-After", self.retryAfter)
	}
	w.WriteHeader(self.status)
	io.WriteString(w, self.body)
}

func (self *certsDnsTestApi) answer(status int, body string) {
	self.answerRetryAfter(status, body, "")
}

func (self *certsDnsTestApi) answerRetryAfter(status int, body string, retryAfter string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.status = status
	self.body = body
	self.retryAfter = retryAfter
}

func (self *certsDnsTestApi) lastCall(t *testing.T) certsDnsTestCall {
	t.Helper()
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if len(self.calls) == 0 {
		t.Fatal("the api was not called")
	}
	return self.calls[len(self.calls)-1]
}

func newCertsDnsTestClient(t *testing.T) (*CertsDnsClient, *certsDnsTestApi) {
	t.Helper()
	api := &certsDnsTestApi{status: http.StatusOK}
	testServer := httptest.NewServer(api)
	t.Cleanup(testServer.Close)
	settings := DefaultCertsDnsClientSettings()
	settings.BaseUrl = testServer.URL
	settings.Root = "root.example"
	client, err := NewCertsDnsClient(settings, certsDnsTestBearer, nil)
	if err != nil {
		t.Fatal(err)
	}
	return client, api
}

func TestCertsDnsClientRefusesToBuildWithoutACredentialOrASafeOrigin(t *testing.T) {
	good := DefaultCertsDnsClientSettings()
	good.BaseUrl = "https://dns.example.test"
	if _, err := NewCertsDnsClient(good, certsDnsTestBearer, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCertsDnsClient(good, "", nil); !errors.Is(err, ErrCertsDnsClientRefused) {
		t.Fatalf("a client with no credential answered %v", err)
	}
	if _, err := NewCertsDnsClient(nil, certsDnsTestBearer, nil); !errors.Is(err, ErrCertsDnsClientRefused) {
		t.Fatalf("a client with no settings answered %v", err)
	}
	for _, baseUrl := range []string{
		"",
		"dns.example.test",
		"http://dns.example.test",
		"http://localhost.example.test",
		"ftp://dns.example.test",
		"https://dns.example.test/path",
		"https://user:secret@dns.example.test",
		"https://dns.example.test?root=other",
	} {
		settings := DefaultCertsDnsClientSettings()
		settings.BaseUrl = baseUrl
		if _, err := NewCertsDnsClient(settings, certsDnsTestBearer, nil); !errors.Is(err, ErrCertsDnsClientRefused) {
			t.Fatalf("origin %q answered %v", baseUrl, err)
		}
	}
	zero := DefaultCertsDnsClientSettings()
	zero.BaseUrl = good.BaseUrl
	zero.ReadyTimeout = 0
	if _, err := NewCertsDnsClient(zero, certsDnsTestBearer, nil); !errors.Is(err, ErrCertsDnsClientRefused) {
		t.Fatalf("a zero timeout answered %v", err)
	}
}

func TestCertsDnsClientWritesAndRemovesAChallenge(t *testing.T) {
	client, api := newCertsDnsTestClient(t)
	ctx := context.Background()

	api.answer(http.StatusOK, `{"serial":41}`)
	serial, err := client.WriteAcmeChallenge(ctx, "acme.root.example", []string{"one", "two"})
	if err != nil || serial != 41 {
		t.Fatalf("the write: %d, %v", serial, err)
	}
	call := api.lastCall(t)
	if call.method != http.MethodPost || call.path != dnsservice.DnsAcmeChallengePath || call.authorization != "Bearer "+certsDnsTestBearer || call.query != "root=root.example" {
		t.Fatalf("the write went as %s %s?%s", call.method, call.path, call.query)
	}
	request := &dnsservice.DnsChallengeRequest{}
	if err := json.Unmarshal([]byte(call.body), request); err != nil || request.Name != "acme.root.example" || len(request.Values) != 2 || request.Root != "root.example" {
		t.Fatalf("the write's body: %s", call.body)
	}

	// a removal sends no values, and a removal of nothing answers serial zero
	api.answer(http.StatusOK, `{"serial":0}`)
	if err := client.RemoveAcmeChallenge(ctx, "acme.root.example"); err != nil {
		t.Fatal(err)
	}
	request = &dnsservice.DnsChallengeRequest{}
	if err := json.Unmarshal([]byte(api.lastCall(t).body), request); err != nil || len(request.Values) != 0 {
		t.Fatalf("the removal's body: %s", api.lastCall(t).body)
	}
	// a write of no values would be a removal, and a write with no serial is no write
	if _, err := client.WriteAcmeChallenge(ctx, "acme.root.example", nil); !errors.Is(err, certsservice.ErrCertsDnsWriteRefused) {
		t.Fatalf("a write of no values answered %v", err)
	}
	if _, err := client.WriteAcmeChallenge(ctx, "acme.root.example", []string{"one"}); err == nil {
		t.Fatal("a write answered serial zero and passed")
	}

	for status, want := range map[int]error{
		http.StatusServiceUnavailable: certsservice.ErrCertsDnsRestoring,
		http.StatusBadRequest:         certsservice.ErrCertsDnsWriteRefused,
		http.StatusTooManyRequests:    certsservice.ErrCertsDnsWriteRateLimited,
		http.StatusForbidden:          ErrCertsDnsCredentialRefused,
		http.StatusUnauthorized:       ErrCertsDnsCredentialRefused,
	} {
		api.answer(status, "refused")
		if _, err := client.WriteAcmeChallenge(ctx, "acme.root.example", []string{"one"}); !errors.Is(err, want) {
			t.Fatalf("status %d answered %v", status, err)
		}
	}
	for _, body := range []string{"", "not json", strings.Repeat("x", 70*1024)} {
		api.answer(http.StatusOK, body)
		if _, err := client.WriteAcmeChallenge(ctx, "acme.root.example", []string{"one"}); err == nil {
			t.Fatalf("an answer of %d bytes passed", len(body))
		}
	}
	// no error names the bearer
	api.answer(http.StatusInternalServerError, "")
	if _, err := client.WriteAcmeChallenge(ctx, "acme.root.example", []string{"one"}); err == nil || strings.Contains(err.Error(), certsDnsTestBearer) {
		t.Fatalf("a 500 answered %v", err)
	}
}

// dns's 429 on a challenge write is an error of its own (QD-1204): it names the name written and the 429's
// Retry-After in delay seconds, none for a header that is absent or not delay seconds, and it is a refusal
// of the path as a 400 is, while a 400 is not the cap's. Its text names neither the name nor the bearer.
func TestCertsDnsClientAnswersTheCapAsARefusalOfItsOwn(t *testing.T) {
	client, api := newCertsDnsTestClient(t)
	ctx := context.Background()
	name := "capped-name.root.example"
	for _, c := range []struct {
		retryAfter string
		want       time.Duration
	}{
		{retryAfter: "1200", want: 1200 * time.Second},
		{retryAfter: " 7 ", want: 7 * time.Second},
		{retryAfter: "", want: 0},
		{retryAfter: "Wed, 21 Oct 2026 07:28:00 GMT", want: 0},
		{retryAfter: "-5", want: 0},
		{retryAfter: "1.5", want: 0},
	} {
		api.answerRetryAfter(http.StatusTooManyRequests, "too many challenges", c.retryAfter)
		_, err := client.WriteAcmeChallenge(ctx, name, []string{"one", "two"})
		var rateLimit *certsservice.CertsDnsWriteRateLimitError
		if !errors.As(err, &rateLimit) || !errors.Is(err, certsservice.ErrCertsDnsWriteRateLimited) || !errors.Is(err, certsservice.ErrCertsDnsWriteRefused) {
			t.Fatalf("Retry-After %q: %v", c.retryAfter, err)
		}
		if rateLimit.Name != name || rateLimit.RetryAfter != c.want {
			t.Errorf("Retry-After %q read as %q, %v", c.retryAfter, rateLimit.Name, rateLimit.RetryAfter)
		}
		if strings.Contains(err.Error(), name) || strings.Contains(err.Error(), certsDnsTestBearer) {
			t.Errorf("the refusal's text names the name or the bearer: %v", err)
		}
	}
	api.answer(http.StatusBadRequest, "challenge refused")
	if _, err := client.WriteAcmeChallenge(ctx, name, []string{"one"}); !errors.Is(err, certsservice.ErrCertsDnsWriteRefused) || errors.Is(err, certsservice.ErrCertsDnsWriteRateLimited) {
		t.Fatalf("a 400 answered %v", err)
	}
}

func TestCertsDnsClientReadsReadinessAndNeverGuessesReady(t *testing.T) {
	client, api := newCertsDnsTestClient(t)
	ctx := context.Background()

	for answer, clean := range map[certsservice.CertsDnsReadiness]bool{
		certsservice.CertsDnsReady:         true,
		certsservice.CertsDnsStale:         false,
		certsservice.CertsDnsCanaryFailing: false,
		certsservice.CertsDnsWaiting:       false,
	} {
		api.answer(http.StatusOK, `{"answer":"`+string(answer)+`","serial":50,"restore_serial":3}`)
		readiness, err := client.Ready(ctx, 41)
		if err != nil || readiness != answer {
			t.Fatalf("ready answered %q, %v, want %q", readiness, err, answer)
		}
		call := api.lastCall(t)
		if call.method != http.MethodGet || call.path != dnsservice.DnsReadyPath || call.query != "root=root.example&serial=41" || call.authorization != "Bearer "+certsDnsTestBearer {
			t.Fatalf("ready went as %s %s?%s", call.method, call.path, call.query)
		}
		if isClean, err := client.CanaryClean(ctx, 41); err != nil || isClean != clean {
			t.Fatalf("the canary at %q: %v, %v", answer, isClean, err)
		}
	}
	// an answer this client does not know, an empty one and an error are never clean
	for _, body := range []string{`{"answer":"fine"}`, `{}`, `ready`} {
		api.answer(http.StatusOK, body)
		if readiness, err := client.Ready(ctx, 41); err == nil {
			t.Fatalf("answer %s read as %q", body, readiness)
		}
		if isClean, err := client.CanaryClean(ctx, 41); err == nil || isClean {
			t.Fatalf("answer %s read the canary as %v, %v", body, isClean, err)
		}
	}
	api.answer(http.StatusServiceUnavailable, "")
	if _, err := client.Ready(ctx, 41); !errors.Is(err, certsservice.ErrCertsDnsRestoring) {
		t.Fatalf("a 503 answered %v", err)
	}

	// a redirect is not followed, so the bearer goes nowhere else
	api.answer(http.StatusFound, "")
	callCount := len(api.calls)
	if _, err := client.Ready(ctx, 41); err == nil {
		t.Fatal("a redirect read as an answer")
	}
	if len(api.calls) != callCount+1 {
		t.Fatalf("a redirect made %d calls", len(api.calls)-callCount)
	}
}
