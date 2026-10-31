package server

import (
	"errors"
	"strings"
	"testing"
)

// usdc_payout.yml parses strictly: an omitted switch keeps its default (no
// wallet requests, payments on), and typos, unknown values or a second
// document are refused rather than read as a switch.
func TestParseUsdcPayoutPolicy(t *testing.T) {
	cases := []struct {
		name           string
		data           string
		walletRequests bool
		payments       bool
		err            string
	}{
		{name: "empty keeps the defaults", data: "", walletRequests: false, payments: true},
		{name: "comments only keep the defaults", data: "# none\n", walletRequests: false, payments: true},
		{name: "both declared", data: "wallet_requests: on\npayments: off\n", walletRequests: true, payments: false},
		{name: "payments off alone", data: "payments: off\n", walletRequests: false, payments: false},
		{name: "quoted values", data: "wallet_requests: \"off\"\npayments: \"on\"\n", walletRequests: false, payments: true},
		{name: "unknown field", data: "wallet_request: on\n", err: "field wallet_request not found"},
		{name: "unknown value", data: "payments: yes\n", err: "payments must be on or off"},
		{name: "boolean value", data: "payments: false\n", err: "payments must be on or off"},
		{name: "second document", data: "payments: on\n---\npayments: off\n", err: "exactly one document required"},
		{name: "oversized", data: "# " + strings.Repeat("x", 5000) + "\n", err: "invalid size"},
	}
	for _, c := range cases {
		policy, err := ParseUsdcPayoutPolicy([]byte(c.data))
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%s: err = %v, want one containing %q", c.name, err, c.err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if policy.WalletRequestsOn() != c.walletRequests || policy.PaymentsOn() != c.payments {
			t.Errorf("%s: wallet requests %v, payments %v; want %v, %v", c.name, policy.WalletRequestsOn(), policy.PaymentsOn(), c.walletRequests, c.payments)
		}
	}
}

// The loader reads the declared resource; without one it uses the defaults,
// and an invalid one is an error rather than a guess.
func TestLoadUsdcPayoutPolicy(t *testing.T) {
	if _, err := Config.SimpleResource(UsdcPayoutPolicyResourceName); err == nil {
		t.Logf("this environment declares %s; the absent case is not observable", UsdcPayoutPolicyResourceName)
	} else if errors.Is(err, ErrResourceNotFound) {
		policy, err := LoadUsdcPayoutPolicy()
		if err != nil || policy.WalletRequestsOn() || !policy.PaymentsOn() {
			t.Fatalf("absent resource: %+v, %v; want no wallet requests and payments on", policy, err)
		}
	}

	func() {
		defer Config.PushSimpleResource(UsdcPayoutPolicyResourceName, []byte("wallet_requests: on\npayments: off\n"))()
		policy, err := LoadUsdcPayoutPolicy()
		if err != nil || !policy.WalletRequestsOn() || policy.PaymentsOn() {
			t.Fatalf("declared resource: %+v, %v; want wallet requests on and payments off", policy, err)
		}
	}()

	func() {
		defer Config.PushSimpleResource(UsdcPayoutPolicyResourceName, []byte("payments: maybe\n"))()
		if policy, err := LoadUsdcPayoutPolicy(); err == nil {
			t.Fatalf("invalid resource loaded as %+v", policy)
		}
	}()

	// a nil policy, as a failed load leaves no value, allows nothing
	var policy *UsdcPayoutPolicy
	if policy.WalletRequestsOn() || policy.PaymentsOn() {
		t.Fatal("a nil policy allowed wallet requests or payments")
	}
}
