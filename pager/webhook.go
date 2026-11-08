package pager

// A generic HTTPS event client for the pager service: each Trigger and Resolve
// is one JSON post with a stable dedup key, which the service uses to open one
// page per key.
//
// plan-OPS-pager-service has not named the service, and §2.8 lists no ext row
// for it (I-18), so nothing may leave the site yet: NewWebhookSink accepts only
// a loopback endpoint and refuses every other with ErrNoPagerVendor. Once the
// service is chosen, webhookEvent takes its field names, the §2.8 row lands,
// and that check narrows to https.
//
// The endpoint is a credential. It is never logged, put in a page, or put in
// an error. net/http errors embed the request URL, so a failed post is reduced
// to a fixed class. A redirect is refused, since it would send a page to
// another host, and any answer outside 2xx is a failure for the caller to
// retry: the dedup key makes a repeat harmless, so unlike a Slack post even a
// timeout is safe to send again. Summaries get the same address scrubbing as
// the process's stdout and stderr.
//
// WebhookSink is safe for concurrent use.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

// The endpoint is empty.
var ErrNoWebhookUrl = errors.New("no pager webhook is configured")

// The endpoint is not an absolute http or https URL free of a user part.
var ErrBadWebhookUrl = errors.New("the pager webhook is not an absolute URL")

// The endpoint is not loopback and no pager service has been chosen.
var ErrNoPagerVendor = errors.New("no pager service is chosen yet (plan-OPS-pager-service), so pages may not leave the site")

const (
	webhookActionTrigger = "trigger"
	webhookActionResolve = "resolve"
)

// Settings for NewWebhookSink. Zero values take the defaults.
type WebhookSinkSettings struct {
	// the event endpoint; a credential
	Url string
	// limit on one post
	PostTimeout time.Duration
	// how much of an answer's body is read before the connection is reused;
	// the body itself is never used
	ResponseDrainBytes int64
}

// The production limits. An event service answers with a short status body,
// so a few KiB drains it and keeps the connection.
func DefaultWebhookSinkSettings() WebhookSinkSettings {
	return WebhookSinkSettings{
		PostTimeout:        10 * time.Second,
		ResponseDrainBytes: 4 * 1024,
	}
}

// Fills unset limits from DefaultWebhookSinkSettings.
func (self WebhookSinkSettings) withDefaults() WebhookSinkSettings {
	defaults := DefaultWebhookSinkSettings()
	if self.PostTimeout <= 0 {
		self.PostTimeout = defaults.PostTimeout
	}
	if self.ResponseDrainBytes <= 0 {
		self.ResponseDrainBytes = defaults.ResponseDrainBytes
	}
	return self
}

// One event on the wire. These field names are the part that waits for the
// service's own.
type webhookEvent struct {
	Action     string `json:"action"`
	DedupKey   string `json:"dedup_key"`
	Class      string `json:"class,omitempty"`
	Summary    string `json:"summary,omitempty"`
	Time       string `json:"time,omitempty"`
	BothAtOnce bool   `json:"both_at_once,omitempty"`
}

// Posts pages to the pager service (see the file comment).
type WebhookSink struct {
	webhookUrl         string
	responseDrainBytes int64
	httpClient         *http.Client
}

// The error says why a sink cannot be built and never holds the endpoint.
func NewWebhookSink(settings WebhookSinkSettings) (*WebhookSink, error) {
	settings = settings.withDefaults()
	webhookUrl := strings.TrimSpace(settings.Url)
	if webhookUrl == "" {
		return nil, ErrNoWebhookUrl
	}
	// url.Parse errors embed their input, so only the verdict is used
	parsed, err := url.Parse(webhookUrl)
	if err != nil || parsed.Host == "" || parsed.User != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, ErrBadWebhookUrl
	}
	hostname := parsed.Hostname()
	address := net.ParseIP(hostname)
	if hostname != "localhost" && (address == nil || !address.IsLoopback()) {
		return nil, ErrNoPagerVendor
	}
	return &WebhookSink{
		webhookUrl:         webhookUrl,
		responseDrainBytes: settings.ResponseDrainBytes,
		httpClient: &http.Client{
			Transport: server.NewHttpTransport(),
			Timeout:   settings.PostTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (self *WebhookSink) Trigger(ctx context.Context, page Page) error {
	if err := page.Validate(); err != nil {
		return err
	}
	return self.post(ctx, webhookEvent{
		Action:     webhookActionTrigger,
		DedupKey:   page.DedupKey,
		Class:      page.Class,
		Summary:    server.ScrubAddrsText(page.Summary),
		Time:       page.Time.UTC().Format(time.RFC3339),
		BothAtOnce: page.BothAtOnce,
	})
}

func (self *WebhookSink) Resolve(ctx context.Context, dedupKey string) error {
	if err := ValidateDedupKey(dedupKey); err != nil {
		return err
	}
	return self.post(ctx, webhookEvent{Action: webhookActionResolve, DedupKey: dedupKey})
}

// Posts one event once. The error holds only a fixed class.
func (self *WebhookSink) post(ctx context.Context, event webhookEvent) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return errors.New("pager: a webhook event did not encode")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, self.webhookUrl, bytes.NewReader(payload))
	if err != nil {
		return errors.New("pager: a webhook request did not build")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := self.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("pager: webhook post failed (%s)", webhookErrorClass(err))
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, self.responseDrainBytes))
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		return fmt.Errorf("pager: webhook post failed (http-%d)", response.StatusCode)
	}
	return nil
}

// A fixed class: the error text is never used, since net/http errors embed
// the endpoint.
func webhookErrorClass(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "canceled"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "dns"
	}
	var certificateErr *tls.CertificateVerificationError
	var recordErr tls.RecordHeaderError
	if errors.As(err, &certificateErr) || errors.As(err, &recordErr) {
		return "tls"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return "network"
	}
	return "transport"
}
