package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/server/v2026"
)

// The sales Slack channel's incoming webhook: vault `sales.yml`, key
// `slack_webhook_url`. Two features post to it: the Services contact-sales
// leads (services_lead_model.go) and the payment notices
// (sales_payment_notice.go). Without the key nothing is posted.
//
// The webhook url is a credential: it is never logged, and no error returned
// here contains it.

const salesWebhookVaultResource = "sales.yml"

const salesWebhookConfigRefresh = 60 * time.Second

const (
	salesWebhookAttemptCount   = 3
	salesWebhookAttemptTimeout = 10 * time.Second
)

var salesWebhookRetryDelays = []time.Duration{2 * time.Second, 6 * time.Second}

type salesWebhookConfig struct {
	SlackWebhookUrl string `yaml:"slack_webhook_url"`
}

// parseSalesWebhookConfig returns the webhook url, or "" when the key is
// absent or is not an https url.
func parseSalesWebhookConfig(data []byte) string {
	var config salesWebhookConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return ""
	}
	webhookUrl := strings.TrimSpace(config.SlackWebhookUrl)
	parsed, err := url.Parse(webhookUrl)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return ""
	}
	return webhookUrl
}

type salesWebhookConfigSnapshot struct {
	loadTime        time.Time
	slackWebhookUrl string
}

var salesWebhookConfigCache atomic.Pointer[salesWebhookConfigSnapshot]

// salesWebhookUrl re-reads vault every `salesWebhookConfigRefresh`, so ops can
// add or rotate the webhook without a restart.
func salesWebhookUrl(now time.Time) string {
	if snapshot := salesWebhookConfigCache.Load(); snapshot != nil && now.Sub(snapshot.loadTime) < salesWebhookConfigRefresh && !now.Before(snapshot.loadTime) {
		return snapshot.slackWebhookUrl
	}
	slackWebhookUrl := ""
	if resource, err := server.Vault.SimpleResource(salesWebhookVaultResource); err == nil {
		if data, err := resource.BytesE(); err == nil {
			slackWebhookUrl = parseSalesWebhookConfig(data)
		}
	}
	salesWebhookConfigCache.Store(&salesWebhookConfigSnapshot{
		loadTime:        now,
		slackWebhookUrl: slackWebhookUrl,
	})
	return slackWebhookUrl
}

// slackEscape escapes the three characters Slack treats as control
// characters, so a field cannot mention a channel (<!channel>), a user or
// render a disguised link.
func slackEscape(value string) string {
	value = strings.ReplaceAll(value, "&", "&amp;")
	value = strings.ReplaceAll(value, "<", "&lt;")
	value = strings.ReplaceAll(value, ">", "&gt;")
	return value
}

// errSalesWebhookStatus is a non-2xx webhook answer.
type errSalesWebhookStatus struct {
	statusCode int
}

func (self *errSalesWebhookStatus) Error() string {
	return fmt.Sprintf("slack webhook answered %d", self.statusCode)
}

// postSalesWebhook posts one message, with link and media unfurling off. The
// returned error never contains the webhook url: a *url.Error, whose text
// includes the url, is unwrapped to its cause.
func postSalesWebhook(ctx context.Context, httpClient *http.Client, webhookUrl string, text string) error {
	body, err := json.Marshal(map[string]any{
		"text":         text,
		"unfurl_links": false,
		"unfurl_media": false,
	})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookUrl, bytes.NewReader(body))
	if err != nil {
		return errors.New("slack webhook request could not be built")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			return urlErr.Err
		}
		return errors.New("slack webhook request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || 300 <= response.StatusCode {
		return &errSalesWebhookStatus{statusCode: response.StatusCode}
	}
	return nil
}

// salesWebhookNotifier posts a message with bounded retries: at most
// `salesWebhookAttemptCount` attempts of `salesWebhookAttemptTimeout` each.
// Replaceable in tests.
type salesWebhookNotifier struct {
	httpClient  *http.Client
	retryDelays []time.Duration
	sleep       func(context.Context, time.Duration) bool
}

func newSalesWebhookNotifier() *salesWebhookNotifier {
	return &salesWebhookNotifier{
		httpClient:  server.NewHttpClient(salesWebhookAttemptTimeout),
		retryDelays: salesWebhookRetryDelays,
		sleep:       sleepSalesWebhookRetry,
	}
}

func sleepSalesWebhookRetry(ctx context.Context, delay time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(delay):
		return true
	}
}

// Notify returns the last error after the final attempt.
func (self *salesWebhookNotifier) Notify(ctx context.Context, webhookUrl string, text string) error {
	var err error
	for attempt := 0; attempt < salesWebhookAttemptCount; attempt += 1 {
		if 0 < attempt {
			delay := self.retryDelays[min(attempt-1, len(self.retryDelays)-1)]
			if !self.sleep(ctx, delay) {
				return ctx.Err()
			}
		}
		attemptCtx, cancel := context.WithTimeout(ctx, salesWebhookAttemptTimeout)
		err = postSalesWebhook(attemptCtx, self.httpClient, webhookUrl, text)
		cancel()
		if err == nil {
			return nil
		}
		// a 4xx other than a rate limit will not change on retry
		var statusErr *errSalesWebhookStatus
		if errors.As(err, &statusErr) && statusErr.statusCode/100 == 4 && statusErr.statusCode != http.StatusTooManyRequests {
			return err
		}
	}
	return err
}
