package model

import (
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// The block as mmm/GIFTOFPRIVACY.md §10.1 publishes it, switched on.
const giftConfigTestYaml = `
gift:
  enabled: true
  season: winter-2026
  pro_duration: 8760h
  redeemable_for: 8760h
  max_quantity: 20
  tiers:
    - min_quantity: 1
      unit_price_usd: 39.99
    - min_quantity: 2
      unit_price_usd: 29.99
    - min_quantity: 5
      unit_price_usd: 23.99
  crypto_max_invoice_usd: 500
`

func giftConfigFromTestYaml(t testing.TB, text string) *GiftConfig {
	t.Helper()
	var y struct {
		Gift *giftConfigYaml `yaml:"gift"`
	}
	if err := yaml.Unmarshal([]byte(text), &y); err != nil {
		t.Fatal(err)
	}
	if y.Gift == nil {
		return &GiftConfig{}
	}
	return parseGiftConfig(*y.Gift)
}

// The published block parses to its prices in cents, a year of Pro and a year
// to redeem, and every quantity from 1 to 20 has its tier's price: 1 at $39.99,
// 2-4 at $29.99, 5-20 at $23.99. Four gifts cost a cent more than five.
func TestGiftConfigParsesThePublishedBlock(t *testing.T) {
	c := giftConfigFromTestYaml(t, giftConfigTestYaml)
	if !c.Enabled || c.Season != "winter-2026" || c.MaxQuantity != 20 || c.CryptoMaxInvoiceUsd != 500 {
		t.Fatalf("parsed %+v", c)
	}
	if c.ProDuration != 8760*time.Hour || c.RedeemableFor != 8760*time.Hour || c.ProDays() != 365 || c.RedeemableDays() != 365 {
		t.Fatalf("durations %s %s", c.ProDuration, c.RedeemableFor)
	}
	want := []GiftTier{{1, 3999}, {2, 2999}, {5, 2399}}
	if len(c.Tiers) != len(want) {
		t.Fatalf("tiers %+v", c.Tiers)
	}
	for i := range want {
		if c.Tiers[i] != want[i] {
			t.Fatalf("tier %d is %+v, want %+v", i, c.Tiers[i], want[i])
		}
	}
	for quantity, price := range map[int]int64{
		-1: 0, 0: 0, 1: 3999, 2: 2999, 4: 2999, 5: 2399, 20: 2399, 21: 0,
	} {
		if got := c.UnitPriceCents(quantity); got != price {
			t.Errorf("UnitPriceCents(%d) = %d, want %d", quantity, got, price)
		}
	}
	if four, five := 4*c.UnitPriceCents(4), 5*c.UnitPriceCents(5); four != 11996 || five != 11995 {
		t.Fatalf("four cost %d and five %d", four, five)
	}
}

// No block, and no pro.yml, is inert: switched off, nothing priced, no Pro to
// give. A nil config prices nothing either.
func TestGiftConfigAbsentIsInert(t *testing.T) {
	for _, c := range []*GiftConfig{
		giftConfigFromTestYaml(t, "pro:\n  concurrent_clients: 1\n"),
		parseGiftConfig(giftConfigYaml{}),
		nil,
	} {
		if c != nil && (c.Enabled || c.ProDuration != 0 || c.RedeemableFor != 0 || c.MaxQuantity != 0) {
			t.Fatalf("absent block parsed %+v", c)
		}
		for _, quantity := range []int{0, 1, 5, 20} {
			if price := c.UnitPriceCents(quantity); price != 0 {
				t.Fatalf("absent block prices %d gifts at %d", quantity, price)
			}
		}
		if c.ProDays() != 0 || c.RedeemableDays() != 0 {
			t.Fatal("absent block gives days")
		}
	}
}

// A block switched on that cannot sell every quantity it allows, or would sell
// codes that grant nothing or expire on arrival, stays switched off; its
// values are still read.
func TestGiftConfigUnsellableBlockStaysOff(t *testing.T) {
	for name, text := range map[string]string{
		"no year":        "gift:\n  enabled: true\n  redeemable_for: 8760h\n  max_quantity: 2\n  tiers:\n    - min_quantity: 1\n      unit_price_usd: 39.99\n",
		"no redeem time": "gift:\n  enabled: true\n  pro_duration: 8760h\n  max_quantity: 2\n  tiers:\n    - min_quantity: 1\n      unit_price_usd: 39.99\n",
		"no limit":       "gift:\n  enabled: true\n  pro_duration: 8760h\n  redeemable_for: 8760h\n  tiers:\n    - min_quantity: 1\n      unit_price_usd: 39.99\n",
		"no single gift": "gift:\n  enabled: true\n  pro_duration: 8760h\n  redeemable_for: 8760h\n  max_quantity: 4\n  tiers:\n    - min_quantity: 2\n      unit_price_usd: 29.99\n",
		"a free tier":    "gift:\n  enabled: true\n  pro_duration: 8760h\n  redeemable_for: 8760h\n  max_quantity: 4\n  tiers:\n    - min_quantity: 1\n      unit_price_usd: 39.99\n    - min_quantity: 2\n      unit_price_usd: 0\n",
	} {
		c := giftConfigFromTestYaml(t, text)
		if c.Enabled {
			t.Errorf("%s: an unsellable block is switched on: %+v", name, c)
		}
	}
	c := giftConfigFromTestYaml(t, "gift:\n  enabled: false\n  pro_duration: 8760h\n  redeemable_for: 8760h\n  max_quantity: 2\n  tiers:\n    - min_quantity: 1\n      unit_price_usd: 39.99\n")
	if c.Enabled || c.UnitPriceCents(2) != 3999 {
		t.Fatalf("a block switched off parsed %+v", c)
	}
}

// A malformed block panics, as the rest of pro.yml does.
func TestGiftConfigMalformedPanics(t *testing.T) {
	for name, text := range map[string]string{
		"bad duration":      "gift:\n  pro_duration: a year\n",
		"negative duration": "gift:\n  redeemable_for: -1h\n",
		"negative limit":    "gift:\n  max_quantity: -1\n",
		"zero tier":         "gift:\n  tiers:\n    - min_quantity: 0\n      unit_price_usd: 1\n",
		"falling tiers":     "gift:\n  tiers:\n    - min_quantity: 2\n      unit_price_usd: 1\n    - min_quantity: 2\n      unit_price_usd: 1\n",
		"negative price":    "gift:\n  tiers:\n    - min_quantity: 1\n      unit_price_usd: -1\n",
		"negative invoice":  "gift:\n  crypto_max_invoice_usd: -1\n",
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: a malformed block parsed", name)
				}
			}()
			giftConfigFromTestYaml(t, text)
		}()
	}
}

// A test's config replaces pro.yml's until it is restored.
func TestGiftConfigTestingOverride(t *testing.T) {
	before := Gift()
	restore := Testing_SetGiftConfig(Testing_GiftConfig())
	if c := Gift(); !c.Enabled || c.UnitPriceCents(1) != 3999 {
		restore()
		t.Fatalf("the override reads %+v", c)
	}
	restore()
	if Gift() != before {
		t.Fatal("the override was not restored")
	}
}
