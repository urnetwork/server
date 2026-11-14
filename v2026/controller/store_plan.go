package controller

import (
	"strings"

	"github.com/urnetwork/server/v2026/model"
)

// The plan (monthly or yearly) a store purchase is on, from the store's own
// name for the plan. The sales notices and the onboarding trial outcomes read
// it. A name this server does not know is read by its words, and otherwise the
// plan is "", which the notice shows as plain "Pro": a plan is never guessed.

// The Play subscription's base plans: the product `supporter` sells the
// monthly plan as base plan `supporter-base` (P1M) and the yearly plan as
// `supporter-yearly` (P1Y), as read back from the Play Developer API when the
// store was set up (mmm/onboarding/STORES.md "Google Play"). The trial offers
// (`trial-14d`, and the welcome offer tagged `onboarding25`) are on the yearly
// base plan. The product id names no plan, so it cannot tell them apart.
var playBasePlanPlans = map[string]string{
	"supporter-base":   model.PlanMonthly,
	"supporter-yearly": model.PlanYearly,
}

// playPlanForSubscription is the plan of a Play purchase: the base plan of its
// line item for productId (any line item when none names it), else the
// product id's words. "supporter" has none.
func playPlanForSubscription(sub *PlaySubscription, productId string) string {
	plan := ""
	if sub != nil {
		for _, item := range sub.LineItems {
			if item == nil || item.OfferDetails == nil {
				continue
			}
			itemPlan := playPlanForBasePlan(item.OfferDetails.BasePlanId)
			if itemPlan == "" {
				continue
			}
			if item.ProductId == productId {
				return itemPlan
			}
			if plan == "" {
				plan = itemPlan
			}
		}
	}
	if plan == "" {
		plan = planForProductId(productId)
	}
	return plan
}

// playPlanForBasePlan is the plan of a Play base plan id, "" when neither the
// configured ids nor the id's words name one.
func playPlanForBasePlan(basePlanId string) string {
	if plan, ok := playBasePlanPlans[strings.ToLower(strings.TrimSpace(basePlanId))]; ok {
		return plan
	}
	return planForProductId(basePlanId)
}

// applePlanForProductId is the plan an App Store product sells: its name
// (supporter_monthly_26, supporter_yearly_26, supporter_yearly), and
// "supporter", the App Store's monthly plan from 2025-02
// (appleLegacySupporterProductIds). Only the App Store sold a product named
// "supporter" as monthly: Play's product of that name sells both plans.
func applePlanForProductId(productId string) string {
	if strings.EqualFold(productId, "supporter") {
		return model.PlanMonthly
	}
	return planForProductId(productId)
}
