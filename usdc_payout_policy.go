// The switches for what remains of the legacy USDC (Circle) provider payouts.
// Provider earnings after the sn.yml cutoff pay out through the subnet; these
// switches never change which asset an obligation is owed, so they live apart
// from sn.yml, whose fields are bound into the provider earning boundary.
package server

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// The config resource of the switches.
const UsdcPayoutPolicyResourceName = "usdc_payout.yml"

const (
	UsdcPayoutOn  = "on"
	UsdcPayoutOff = "off"
)

// wallet_requests: ask a network to connect a payout wallet when a USDC payout
// is withheld for want of one (the missing-wallet email or SMS).
//
// payments: send USDC payouts to Circle. Off never reserves or submits an
// attempt and stops rescheduling the payments it holds; a payment Circle
// already has keeps its status check, so no sent transfer goes untracked.
type UsdcPayoutPolicy struct {
	WalletRequests string `yaml:"wallet_requests" json:"wallet_requests"`
	Payments       string `yaml:"payments" json:"payments"`
}

// Without usdc_payout.yml, no network is asked to connect a wallet (owner,
// 2026-10-26: "let's stop asking people to connect their wallets now") and
// payments run as before; payments turn off only by declaration.
func DefaultUsdcPayoutPolicy() *UsdcPayoutPolicy {
	return &UsdcPayoutPolicy{
		WalletRequests: UsdcPayoutOff,
		Payments:       UsdcPayoutOn,
	}
}

// Whether a withheld payout may ask its network to connect a wallet.
func (self *UsdcPayoutPolicy) WalletRequestsOn() bool {
	return self != nil && self.WalletRequests == UsdcPayoutOn
}

// Whether USDC payouts may be submitted to Circle.
func (self *UsdcPayoutPolicy) PaymentsOn() bool {
	return self != nil && self.Payments == UsdcPayoutOn
}

// Strict parsing refuses typos and unknown values instead of guessing a
// switch. An omitted field keeps its default.
func ParseUsdcPayoutPolicy(data []byte) (*UsdcPayoutPolicy, error) {
	if len(data) > 4*1024 {
		return nil, errors.New("usdc_payout.yml: invalid size")
	}
	policy := DefaultUsdcPayoutPolicy()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(policy); err != nil && err != io.EOF {
		return nil, fmt.Errorf("usdc_payout.yml: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, errors.New("usdc_payout.yml: exactly one document required")
	}
	for name, value := range map[string]string{"wallet_requests": policy.WalletRequests, "payments": policy.Payments} {
		if value != UsdcPayoutOn && value != UsdcPayoutOff {
			return nil, fmt.Errorf("usdc_payout.yml: %s must be on or off", name)
		}
	}
	return policy, nil
}

// The declared switches, or the defaults when usdc_payout.yml is absent. An
// existing file that cannot be read or parsed is an error: a caller that would
// send money treats the error as off.
func LoadUsdcPayoutPolicy() (*UsdcPayoutPolicy, error) {
	resource, err := Config.SimpleResource(UsdcPayoutPolicyResourceName)
	if err != nil {
		if errors.Is(err, ErrResourceNotFound) {
			return DefaultUsdcPayoutPolicy(), nil
		}
		return nil, fmt.Errorf("usdc payout switches unavailable: %w", err)
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("usdc payout switches unreadable: %w", err)
	}
	return ParseUsdcPayoutPolicy(data)
}
