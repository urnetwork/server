package model

// QuotaRefusalError's status, retry hint and body (DESIGN.md 20.7), and when a refusal is
// an upgrade. Pure: no database.

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
)

func TestQuotaRefusalErrorStatus(t *testing.T) {
	// the router's convention for a declared status (router/handler_utils.go)
	declared := regexp.MustCompile(`^([45][0-9]{2})[ \t]+`)
	for _, c := range []struct {
		refusal           sidecar.QuotaRefusal
		status            int
		retryAfterSeconds int
	}{
		{refusal: sidecar.QuotaRefusal{LimitKey: "evs_drops_per_day", Limit: 20, Used: 20, RetryAfterMs: 1}, status: http.StatusTooManyRequests, retryAfterSeconds: 1},
		{refusal: sidecar.QuotaRefusal{LimitKey: "evs_drops_per_day", Limit: 20, Used: 20, RetryAfterMs: 1000}, status: http.StatusTooManyRequests, retryAfterSeconds: 1},
		{refusal: sidecar.QuotaRefusal{LimitKey: "evs_drops_per_day", Limit: 20, Used: 20, RetryAfterMs: 1001, Upgrade: true}, status: http.StatusTooManyRequests, retryAfterSeconds: 2},
		{refusal: sidecar.QuotaRefusal{LimitKey: "sidecar_clients", Limit: 20, Used: 20, Upgrade: true}, status: http.StatusPaymentRequired},
		{refusal: sidecar.QuotaRefusal{LimitKey: "directory_target_pending", Limit: 100, Used: 100}, status: http.StatusTooManyRequests},
	} {
		err := NewQuotaRefusalError(&c.refusal)
		connect.AssertEqual(t, err.HttpStatus(), c.status)
		connect.AssertEqual(t, err.RetryAfterSeconds(), c.retryAfterSeconds)
		match := declared.FindStringSubmatch(err.Error())
		if match == nil || match[1] != strconv.Itoa(c.status) {
			t.Errorf("%q does not declare %d", err.Error(), c.status)
		}
		body, marshalErr := json.Marshal(err.HttpErrorResultBody())
		connect.AssertEqual(t, marshalErr, nil)
		want, _ := json.Marshal(c.refusal)
		connect.AssertEqual(t, string(body), string(want))
	}
}

// Upgrade is true only when a higher tier lifts the limit: never for a sidecar_abuse:
// key, never past a network's own row, and never from the top tier.
func TestQuotaRefusalUpgrade(t *testing.T) {
	defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{"dns_entries": 5, "acl_approvers_two": 0, "directory_target_pending": 100})()
	defer Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{"dns_entries": 200, "acl_approvers_two": 0})()
	defer Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{"dns_entries": -1, "acl_approvers_two": 1})()

	for _, c := range []struct {
		tier      NetworkTier
		limitKey  string
		usedCount int64
		hasRow    bool
		upgrade   bool
	}{
		{tier: NetworkTierFree, limitKey: "dns_entries", usedCount: 5, upgrade: true},
		{tier: NetworkTierFree, limitKey: "dns_entries", usedCount: 5, hasRow: true, upgrade: false},
		{tier: NetworkTierPro, limitKey: "dns_entries", usedCount: 200, upgrade: true},
		{tier: NetworkTierTeam, limitKey: "dns_entries", usedCount: 1_000_000, upgrade: false},
		{tier: NetworkTierFree, limitKey: "acl_approvers_two", usedCount: 0, upgrade: true},
		{tier: NetworkTierPro, limitKey: "acl_approvers_two", usedCount: 0, upgrade: true},
		{tier: NetworkTierTeam, limitKey: "acl_approvers_two", usedCount: 1, upgrade: false},
		{tier: NetworkTierFree, limitKey: "directory_target_pending", usedCount: 100, upgrade: false},
		{tier: NetworkTierFree, limitKey: "no_such_limit", usedCount: 0, upgrade: false},
	} {
		if upgrade := quotaRefusalUpgrade(c.tier, c.limitKey, c.usedCount, c.hasRow); upgrade != c.upgrade {
			t.Errorf("%s %s at %d (row %t): upgrade %t, want %t", c.tier, c.limitKey, c.usedCount, c.hasRow, upgrade, c.upgrade)
		}
	}
	connect.AssertEqual(t, QuotaLimitLiftedByTier(NetworkTierPro, "dns_entries", 5), true)
	connect.AssertEqual(t, QuotaLimitLiftedByTier(NetworkTierPro, "acl_approvers_two", 0), false)
	connect.AssertEqual(t, QuotaLimitLiftedByTier(NetworkTierTeam, "acl_approvers_two", 0), true)
}
