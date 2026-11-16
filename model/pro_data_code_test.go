package model

import (
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
)

// Each pro.yml data pack names the rails that sell it (data_code.skus[].rails).
// These tests pin the rules every rail relies on, without the environment's
// pro.yml: a pack is sold on exactly the rails it lists, and nowhere by
// omission.

// proTestDataCodeSkus parses a data_code section the way Pro() does.
func proTestDataCodeSkus(t *testing.T, text string) []ProDataCodeSku {
	t.Helper()
	var y proDataCodeYaml
	if err := yaml.Unmarshal([]byte(text), &y); err != nil {
		t.Fatal(err)
	}
	skus := []ProDataCodeSku{}
	for _, s := range y.Skus {
		skus = append(skus, ProDataCodeSku{Sku: s.Sku, Data: mustParseByteCount(s.Data), PriceUsd: s.PriceUsd, Rails: s.Rails})
	}
	return normalizeDataCodeSkus(skus)
}

func proTestPanics(f func()) (panicked bool) {
	defer func() {
		panicked = recover() != nil
	}()
	f()
	return
}

// The 256 GiB pack is sold over x402 and with Gamma, and refused by card and
// with crypto, because its rails say so.
func TestDataCodeSkuIsSoldOnlyOnItsRails(t *testing.T) {
	skus := proTestDataCodeSkus(t, `
skus:
  - sku: data_1tib
    data: 1tib
    price_usd: 3
    rails: [card, crypto, x402, gamma]
  - sku: data_256gib
    data: 256gib
    price_usd: 0.75
    rails: [x402, gamma]
`)
	c := &ProConfig{DataCodeSkus: skus}
	pack := c.DataCodeSku("data_256gib")
	connect.AssertEqual(t, pack.Data, 256*Gib)
	connect.AssertEqual(t, pack.PriceUsd, 0.75)
	connect.AssertEqual(t, pack.SoldOn(DataRailX402), true)
	connect.AssertEqual(t, pack.SoldOn(DataRailGamma), true)
	connect.AssertEqual(t, pack.SoldOn(DataRailCard), false)
	connect.AssertEqual(t, pack.SoldOn(DataRailCrypto), false)

	onRail := func(rail string) []string {
		ids := []string{}
		for _, sku := range c.DataCodeSkusOn(rail) {
			ids = append(ids, sku.Sku)
		}
		return ids
	}
	connect.AssertEqual(t, onRail(DataRailCard), []string{"data_1tib"})
	connect.AssertEqual(t, onRail(DataRailCrypto), []string{"data_1tib"})
	connect.AssertEqual(t, onRail(DataRailX402), []string{"data_1tib", "data_256gib"})
	connect.AssertEqual(t, onRail(DataRailGamma), []string{"data_1tib", "data_256gib"})

	connect.AssertEqual(t, c.DataCodeSku("data_10tib") == nil, true)
	var missing *ProDataCodeSku
	connect.AssertEqual(t, missing.SoldOn(DataRailX402), false)
}

// A pro.yml written before rails (the one production reads until the config
// release) sells its 1 TiB and 10 TiB exactly as before: as data_1tib and
// data_10tib, on every rail. Any other amount in it has no id and is not
// sold, so a new pack can never reach card checkout by being listed without
// rails.
func TestDataCodeSkusFromBeforeRailsKeepTheirRails(t *testing.T) {
	skus := proTestDataCodeSkus(t, `
skus:
  - data: 1tib
    price_usd: 3
  - data: 10tib
    price_usd: 20
  - data: 256gib
    price_usd: 0.75
`)
	every := []string{DataRailCard, DataRailCrypto, DataRailX402, DataRailGamma}
	connect.AssertEqual(t, skus, []ProDataCodeSku{
		{Sku: "data_1tib", Data: Tib, PriceUsd: 3, Rails: every},
		{Sku: "data_10tib", Data: 10 * Tib, PriceUsd: 20, Rails: every},
	})
}

// A pack is sold on the rails it names and no others: no rails is no sale, a
// rail named twice counts once, and a rail this server does not know is not
// one it sells on -- including the stores, which sell UR Pro only.
func TestDataCodeSkuRailsAreExplicit(t *testing.T) {
	skus := proTestDataCodeSkus(t, `
skus:
  - sku: data_1tib
    data: 1tib
    price_usd: 3
    rails: []
  - sku: data_2tib
    data: 2tib
    price_usd: 6
  - sku: data_3tib
    data: 3tib
    price_usd: 9
    rails: [gamma, apple, play, store, google, stripe, gamma]
`)
	connect.AssertEqual(t, len(skus), 3)
	connect.AssertEqual(t, skus[0].Rails, []string{})
	connect.AssertEqual(t, skus[1].Rails, []string{})
	connect.AssertEqual(t, skus[2].Rails, []string{DataRailGamma})
	c := &ProConfig{DataCodeSkus: skus}
	for _, rail := range []string{DataRailCard, DataRailCrypto, DataRailX402} {
		connect.AssertEqual(t, len(c.DataCodeSkusOn(rail)), 0)
	}
	connect.AssertEqual(t, len(c.DataCodeSkusOn(DataRailGamma)), 1)
	for _, store := range []string{"apple", "play", "store", "google"} {
		connect.AssertEqual(t, len(c.DataCodeSkusOn(store)), 0)
	}
}

// A data pack's id always starts with data_, so a credit can tell a pack from
// a plan by the id alone; one id is one price; and a pack that lists rails
// names its id. pro.yml that breaks these does not load.
func TestDataCodeSkuIdsAreUnambiguous(t *testing.T) {
	connect.AssertEqual(t, proTestPanics(func() {
		normalizeDataCodeSkus([]ProDataCodeSku{{Sku: "yearly", Data: Tib, PriceUsd: 3, Rails: []string{DataRailGamma}}})
	}), true)
	connect.AssertEqual(t, proTestPanics(func() {
		normalizeDataCodeSkus([]ProDataCodeSku{{Sku: DataCodeSkuPrefix, Data: Tib, PriceUsd: 3, Rails: []string{DataRailGamma}}})
	}), true)
	connect.AssertEqual(t, proTestPanics(func() {
		normalizeDataCodeSkus([]ProDataCodeSku{
			{Sku: "data_1tib", Data: Tib, PriceUsd: 3, Rails: []string{DataRailCard}},
			{Sku: "data_1tib", Data: Tib, PriceUsd: 1, Rails: []string{DataRailX402}},
		})
	}), true)
	// a legacy entry and a new one for the same pack are one id twice
	connect.AssertEqual(t, proTestPanics(func() {
		normalizeDataCodeSkus([]ProDataCodeSku{
			{Data: Tib, PriceUsd: 3},
			{Sku: "data_1tib", Data: Tib, PriceUsd: 3, Rails: []string{DataRailCard}},
		})
	}), true)
	connect.AssertEqual(t, proTestPanics(func() {
		normalizeDataCodeSkus([]ProDataCodeSku{{Data: 256 * Gib, PriceUsd: 0.75, Rails: []string{DataRailX402}}})
	}), true)
	// in order, with no panic, when the ids are distinct
	connect.AssertEqual(t, proTestPanics(func() {
		normalizeDataCodeSkus([]ProDataCodeSku{
			{Sku: "data_1tib", Data: Tib, PriceUsd: 3, Rails: []string{DataRailCard}},
			{Sku: " data_256gib ", Data: 256 * Gib, PriceUsd: 0.75, Rails: []string{" x402 "}},
		})
	}), false)
	skus := normalizeDataCodeSkus([]ProDataCodeSku{{Sku: " data_256gib ", Data: 256 * Gib, PriceUsd: 0.75, Rails: []string{" x402 "}}})
	connect.AssertEqual(t, skus[0].Sku, "data_256gib")
	connect.AssertEqual(t, skus[0].Rails, []string{DataRailX402})
}
