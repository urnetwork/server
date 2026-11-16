// SPDX-License-Identifier: MPL-2.0

package reengagement

// Onboarding/retention evaluation 2026-11-10 (C4): the free grant is 30 GiB a
// day (config/all/pro.yml free.data: 30gib), and the re-engagement emails
// that remind of it ("Your free plan hasn't changed: {daily_data_gb} of data
// every day") said "30 GB".

import (
	"testing"
	"time"
)

func TestReengagementDailyDataIsInGiB(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	last := now.Add(-35 * 24 * time.Hour)
	for _, tv := range [][2]string{
		{TemplateR1News, VariantLapsed}, {TemplateR3Keep, VariantDefault}, {TemplateN1Nudge, VariantNotNeeded},
		{TemplateW1Winback, VariantDefault}, {TemplateW1Winback, VariantOffer},
	} {
		p := BuildParams(ParamsInput{
			Template: tv[0], Variant: tv[1], Locale: "en", Location: time.UTC, Now: now,
			SiteUrl: "https://ur.io", Token: "T", DailyDataGb: 30, LastConnectAt: &last, ProEndedAt: &last,
		})
		if got := p["daily_data_gb"]; got != "30 GiB" {
			t.Errorf("%s/%s: daily_data_gb %q", tv[0], tv[1], got)
		}
	}
}
