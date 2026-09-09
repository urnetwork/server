package model

import (
	"testing"

	"github.com/urnetwork/connect"
)

func testPriceTierConfig() *ProConfig {
	return &ProConfig{
		Pro: ProTier{PriceMonthlyUsd: 5, PriceYearlyUsd: 40},
		priceTiers: parseProPriceTiers([]proPriceTierYaml{
			{Name: "standard", YearlyUsd: 40, MonthlyUsd: 5, Countries: []string{"US", "ca", " de ", "JP", "IL"}},
			{Name: "regional", YearlyUsd: 4, MonthlyUsd: 0.5},
		}),
	}
}

// TestPriceTierForCountry pins the resolution rules: a named country gets its
// tier, every other known country the catch-all, and an unknown or malformed
// country the DEFAULT (first) tier -- never the cheaper catch-all.
func TestPriceTierForCountry(t *testing.T) {
	c := testPriceTierConfig()

	connect.AssertEqual(t, "standard", c.PriceTierForCountry("US").Name)
	connect.AssertEqual(t, "standard", c.PriceTierForCountry("us").Name)
	connect.AssertEqual(t, "standard", c.PriceTierForCountry(" ca ").Name)
	connect.AssertEqual(t, "standard", c.PriceTierForCountry("DE").Name)
	connect.AssertEqual(t, "standard", c.PriceTierForCountry("IL").Name)

	connect.AssertEqual(t, "regional", c.PriceTierForCountry("BR").Name)
	connect.AssertEqual(t, "regional", c.PriceTierForCountry("in").Name)
	connect.AssertEqual(t, "regional", c.PriceTierForCountry("TR").Name)
	connect.AssertEqual(t, "regional", c.PriceTierForCountry("MX").Name)

	// unknown -> default tier
	connect.AssertEqual(t, "standard", c.PriceTierForCountry("").Name)
	connect.AssertEqual(t, "standard", c.PriceTierForCountry("USA").Name)
	connect.AssertEqual(t, "standard", c.PriceTierForCountry("1X").Name)
	connect.AssertEqual(t, "standard", c.DefaultPriceTier().Name)

	connect.AssertEqual(t, 4.0, c.PriceTierForCountry("BR").PriceUsd(PlanYearly))
	connect.AssertEqual(t, 0.5, c.PriceTierForCountry("BR").PriceUsd(PlanMonthly))
	connect.AssertEqual(t, 40.0, c.PriceTierForCountry("US").PriceUsd(PlanYearly))
	connect.AssertEqual(t, 0.0, c.PriceTierForCountry("US").PriceUsd("weekly"))

	connect.AssertEqual(t, true, c.PriceTierByName("regional") != nil)
	connect.AssertEqual(t, true, c.PriceTierByName("premium") == nil)
	connect.AssertEqual(t, 5, len(c.PriceTierByName("standard").Countries))
	connect.AssertEqual(t, true, c.PriceTierByName("regional").CatchAll())
}

// TestPriceTiersWithoutTiers: a pro.yml that predates price_tiers still yields
// one standard tier at pro.price_usd, and an absent pro.yml a zero-priced one.
func TestPriceTiersWithoutTiers(t *testing.T) {
	c := &ProConfig{Pro: ProTier{PriceMonthlyUsd: 5, PriceYearlyUsd: 40}}
	tiers := c.PriceTiers()
	connect.AssertEqual(t, 1, len(tiers))
	connect.AssertEqual(t, PriceTierStandard, tiers[0].Name)
	connect.AssertEqual(t, 40.0, tiers[0].YearlyUsd)
	connect.AssertEqual(t, 5.0, tiers[0].MonthlyUsd)
	connect.AssertEqual(t, "standard", c.PriceTierForCountry("BR").Name)

	absent := &ProConfig{}
	connect.AssertEqual(t, 0.0, absent.PriceTierForCountry("US").YearlyUsd)
}

func TestNormalizeCountryCode(t *testing.T) {
	connect.AssertEqual(t, "US", NormalizeCountryCode("us"))
	connect.AssertEqual(t, "GB", NormalizeCountryCode(" gb\n"))
	connect.AssertEqual(t, "", NormalizeCountryCode("USA"))
	connect.AssertEqual(t, "", NormalizeCountryCode("U"))
	connect.AssertEqual(t, "", NormalizeCountryCode("1A"))
	connect.AssertEqual(t, "", NormalizeCountryCode(""))
}

// TestPriceTierForPrice recovers the tier from a quoted price, including the
// welcome-offer price at the configured discount.
func TestPriceTierForPrice(t *testing.T) {
	c := testPriceTierConfig()
	connect.AssertEqual(t, "standard", c.PriceTierForPrice(PlanYearly, 40, 25))
	connect.AssertEqual(t, "standard", c.PriceTierForPrice(PlanYearly, 30, 25))
	connect.AssertEqual(t, "regional", c.PriceTierForPrice(PlanYearly, 4, 25))
	connect.AssertEqual(t, "regional", c.PriceTierForPrice(PlanYearly, 3, 25))
	connect.AssertEqual(t, "regional", c.PriceTierForPrice(PlanMonthly, 0.5, 25))
	connect.AssertEqual(t, "standard", c.PriceTierForPrice(PlanMonthly, 5.004, 25))
	connect.AssertEqual(t, "", c.PriceTierForPrice(PlanYearly, 30, 0))
	connect.AssertEqual(t, "", c.PriceTierForPrice(PlanYearly, 12, 25))
}

// TestProPriceTiersParse pins the yaml validation: names required and unique,
// country codes must be alpha-2.
func TestProPriceTiersParse(t *testing.T) {
	mustPanic := func(name string, ys []proPriceTierYaml) {
		defer func() {
			if r := recover(); r == nil {
				t.Errorf("%s: expected a panic", name)
			}
		}()
		parseProPriceTiers(ys)
	}
	mustPanic("no name", []proPriceTierYaml{{YearlyUsd: 1}})
	mustPanic("duplicate", []proPriceTierYaml{{Name: "a"}, {Name: "a"}})
	mustPanic("bad country", []proPriceTierYaml{{Name: "a", Countries: []string{"USA"}}})

	tiers := parseProPriceTiers([]proPriceTierYaml{{Name: "a", Countries: []string{"us", "US", "ca"}}})
	connect.AssertEqual(t, 2, len(tiers[0].Countries))
	connect.AssertEqual(t, "US", tiers[0].Countries[0])
}
