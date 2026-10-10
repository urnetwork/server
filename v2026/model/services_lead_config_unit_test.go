package model

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/session"
)

// Pure tests: no database or redis.

// The webhook is re-read from vault every servicesSalesConfigRefresh, so ops
// can add or rotate it without a restart, and an absent, empty or non-https
// value leaves leads stored but unposted.
func TestServicesSalesSlackWebhookUrlReloads(t *testing.T) {
	defer servicesSalesConfigCache.Store(nil)
	servicesSalesConfigCache.Store(nil)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	popFirst := server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte("slack_webhook_url: https://hooks.slack.com/services/T0/B0/first\n"))
	defer popFirst()
	connect.AssertEqual(t, servicesSalesSlackWebhookUrl(now), "https://hooks.slack.com/services/T0/B0/first")

	// a rotation inside the refresh window is not seen yet
	popSecond := server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte("slack_webhook_url: https://hooks.slack.com/services/T0/B0/second\n"))
	defer popSecond()
	connect.AssertEqual(t, servicesSalesSlackWebhookUrl(now.Add(servicesSalesConfigRefresh-time.Second)), "https://hooks.slack.com/services/T0/B0/first")

	// at the refresh it is
	connect.AssertEqual(t, servicesSalesSlackWebhookUrl(now.Add(servicesSalesConfigRefresh)), "https://hooks.slack.com/services/T0/B0/second")

	// a clock that moved backward re-reads rather than trusting the snapshot
	popThird := server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte("slack_webhook_url: https://hooks.slack.com/services/T0/B0/third\n"))
	defer popThird()
	connect.AssertEqual(t, servicesSalesSlackWebhookUrl(now.Add(-time.Second)), "https://hooks.slack.com/services/T0/B0/third")
}

func TestServicesSalesSlackWebhookUrlUnconfigured(t *testing.T) {
	defer servicesSalesConfigCache.Store(nil)
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)

	for _, config := range []string{
		"",
		"other_key: 1\n",
		"slack_webhook_url: \"\"\n",
		"slack_webhook_url: http://hooks.slack.com/services/T0/B0/x\n",
		"slack_webhook_url: hooks.slack.com/services/T0/B0/x\n",
	} {
		servicesSalesConfigCache.Store(nil)
		pop := server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte(config))
		connect.AssertEqual(t, servicesSalesSlackWebhookUrl(now), "")
		pop()
	}

	// with no sales.yml at all (this environment's vault has none, and no
	// override is pushed) the webhook is unset rather than an error
	servicesSalesConfigCache.Store(nil)
	if _, err := server.Vault.SimpleResource(servicesSalesVaultResource); err != nil {
		connect.AssertEqual(t, servicesSalesSlackWebhookUrl(now), "")
	} else {
		t.Logf("this environment has a sales.yml; the missing-file case is covered by the error branch only")
	}
}

// Without a webhook the lead is kept and nothing is posted: the notifier is
// never built, so no request can leave the server.
func TestNotifyServicesLeadUnconfiguredPostsNothing(t *testing.T) {
	defer servicesSalesConfigCache.Store(nil)
	pop := server.Vault.PushSimpleResource(servicesSalesVaultResource, []byte("slack_webhook_url: \"\"\n"))
	defer pop()
	servicesSalesConfigCache.Store(nil)

	var built atomic.Int32
	previousFactory := servicesLeadNotifierFactory
	servicesLeadNotifierFactory = func() *servicesLeadNotifier {
		built.Add(1)
		return previousFactory()
	}
	defer func() { servicesLeadNotifierFactory = previousFactory }()

	lead, refusal := newServicesLead(validServicesContactSalesArgs(), server.NewId(), server.NowUtc())
	connect.AssertEqual(t, refusal, "")
	notifyServicesLead(lead)
	connect.AssertEqual(t, built.Load(), int32(0))
}

// A request whose address cannot be classified is let through to the
// honeypot and validation rather than failing the form, and no redis call is
// made for it (this test has no redis).
func TestCheckServicesLeadRateLimitLetsAnUnclassifiableAddressThrough(t *testing.T) {
	ctx := context.Background()
	for _, address := range []string{"", "not-an-address"} {
		clientSession := session.Testing_CreateClientSession(ctx, nil)
		clientSession.ClientAddress = address
		connect.AssertEqual(t, checkServicesLeadRateLimit(ctx, clientSession, server.NowUtc()), nil)
	}
}

// The limits admit exactly the documented counts: the attempt script admits
// while the count including the attempt stays below the limit.
func TestServicesLeadRateLimitSettings(t *testing.T) {
	connect.AssertEqual(t, servicesLeadRateLimitSettings.AddressLimit, servicesLeadAddressAttemptsPerHour+1)
	connect.AssertEqual(t, servicesLeadRateLimitSettings.AddressLookback, time.Hour)
	connect.AssertEqual(t, servicesLeadRateLimitSettings.GlobalLimit, servicesLeadGlobalAttemptsPerMinute+1)
	connect.AssertEqual(t, servicesLeadRateLimitSettings.GlobalLookback, time.Minute)
	// address and global histories share one hash tag (one cluster slot)
	connect.AssertEqual(t, servicesLeadRateLimitHashTag, servicesLeadRateLimitSettings.KeyPrefix)
}

// postServicesLeadToSlack never returns the webhook url in its error, even for
// a transport failure whose *url.Error text would include it.
func TestPostServicesLeadToSlackErrorNeverContainsTheUrl(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	secretPath := "/services/T0SECRET/B0SECRET/xoxSECRETTOKEN"
	// a port nothing listens on, on a local address: the dial fails
	webhookUrl := "https://127.0.0.1:1" + secretPath
	err := postServicesLeadToSlack(ctx, &http.Client{Timeout: 2 * time.Second}, webhookUrl, "text")
	if err == nil {
		t.Fatal("expected a transport failure")
	}
	if strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), webhookUrl) {
		t.Fatalf("error leaked the webhook url: %q", err.Error())
	}

	// a malformed url refuses to build a request, also without echoing it
	err = postServicesLeadToSlack(ctx, &http.Client{}, "https://hooks.slack.com/\x7fSECRET", "text")
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("malformed url error = %v", err)
	}
}
