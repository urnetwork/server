package controller

import (
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server/model"
)

func TestPlayPlanForSubscription(t *testing.T) {
	start := time.Date(2026, 11, 8, 4, 7, 27, 0, time.UTC)
	sub := playPenguinSubscription(start, start.Add(14*24*time.Hour))
	connect.AssertEqual(t, playPlanForSubscription(sub, "supporter"), model.PlanYearly)
	// the RTDN names the product; a line item of another product does not
	// hide the purchase's plan
	sub.LineItems = append([]*PlaySubscriptionPurchaseLineItem{{
		ProductId:    "monthly_transfer_300gib",
		OfferDetails: &PlayOfferDetails{BasePlanId: "monthly-300gib"},
	}}, sub.LineItems...)
	connect.AssertEqual(t, playPlanForSubscription(sub, "supporter"), model.PlanYearly)
	connect.AssertEqual(t, playPlanForSubscription(nil, "supporter"), "")
	connect.AssertEqual(t, playPlanForBasePlan(" Supporter-Base "), model.PlanMonthly)
	// without a base plan, only a product id that says its plan names one
	connect.AssertEqual(t, playPlanForSubscription(&PlaySubscription{}, "supporter"), "")
	connect.AssertEqual(t, playPlanForSubscription(&PlaySubscription{}, "pro_yearly"), model.PlanYearly)
}

func TestApplePlanForProductId(t *testing.T) {
	connect.AssertEqual(t, applePlanForProductId("supporter_monthly_26"), model.PlanMonthly)
	connect.AssertEqual(t, applePlanForProductId("supporter_yearly_26"), model.PlanYearly)
	connect.AssertEqual(t, applePlanForProductId("supporter_yearly"), model.PlanYearly)
	// the App Store's monthly plan from 2025-02
	connect.AssertEqual(t, applePlanForProductId("supporter"), model.PlanMonthly)
	connect.AssertEqual(t, applePlanForProductId("pro"), "")
}
