package controller

// The settings answer's notice of the automatic Gamma charges already sent
// when their switch was turned off (SetGammaSettings): plain words, the
// amount of each, and that each can still be charged once.

import (
	"testing"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

func TestGammaSettingsChargingNoticeSaysEachChargeIsChargedOnce(t *testing.T) {
	connect.AssertEqual(t, gammaSettingsChargingNotice(nil), "")

	renewal := &model.GammaAutomaticChargeInFlight{
		IntentId:    server.NewId(),
		Purpose:     model.GammaPurposeRenew,
		Item:        model.SolanaPlanMonthly,
		Amount:      model.UsdToNanoCents(5),
		ChargeState: model.GammaChargePending,
	}
	connect.AssertEqual(
		t,
		gammaSettingsChargingNotice([]*model.GammaAutomaticChargeInFlight{renewal}),
		"The renewal of your plan (URnetwork Pro monthly, 5 gamma) was already being charged when you turned this off. A payment already sent to the chain can't be recalled, so it can still be charged this once, but none after it while the switch is off.",
	)

	topup := &model.GammaAutomaticChargeInFlight{
		IntentId:    server.NewId(),
		Purpose:     model.GammaPurposeTopup,
		Item:        StripeItemData1Tib,
		Amount:      model.UsdToNanoCents(3),
		ChargeState: model.GammaChargeSpent,
	}
	seat := &model.GammaAutomaticChargeInFlight{
		IntentId:    server.NewId(),
		Purpose:     model.GammaPurposeSeat,
		Item:        model.SolanaPlanYearly,
		Amount:      model.UsdToNanoCents(40),
		ChargeState: model.GammaChargePending,
	}
	connect.AssertEqual(
		t,
		gammaSettingsChargingNotice([]*model.GammaAutomaticChargeInFlight{renewal, topup, seat}),
		"These automatic charges were already being made when you turned them off: the renewal of your plan (URnetwork Pro monthly, 5 gamma), the data top-up (URnetwork 1 TiB data, 3 gamma) and the upgrade to Pro at your client limit (URnetwork Pro yearly, 40 gamma). A payment already sent to the chain can't be recalled, so each can still be charged this once, but none after it while its switch is off.",
	)
}
