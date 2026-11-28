package model

import (
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
)

// A synthetic contract input: closed a week before now on its two reports,
// paid by the payer network to the provider's client, with these escrows in
// the funding order.
func contractLegacySettlementTestInput(now time.Time, payerNetworkId server.Id, providerClientId server.Id, providerNetworkId server.Id,
	sourceUsed ByteCount, destinationUsed ByteCount, escrows ...contractDeadlineEscrow) *contractLegacySettlementInput {
	outcome := ContractOutcomeSettled
	closeTime := now.Add(-7 * 24 * time.Hour)
	return &contractLegacySettlementInput{
		found:     true,
		outcome:   &outcome,
		closeTime: &closeTime,
		reports: []contractSettlementReport{
			{party: ContractPartyDestination, byteCount: destinationUsed},
			{party: ContractPartySource, byteCount: sourceUsed},
		},
		escrows:         escrows,
		grantEndTimes:   map[server.Id]time.Time{},
		participants:    []ContractParticipant{{ClientId: providerClientId, NetworkId: providerNetworkId}},
		originNetworkId: payerNetworkId,
		payerNetworkId:  &payerNetworkId,
	}
}

// An unsettled escrow on a grant of the network (nil for a grant that no
// longer exists).
func contractLegacySettlementTestEscrowOf(balanceId server.Id, networkId *server.Id, amount ByteCount, start ByteCount, revenue NanoCents) contractDeadlineEscrow {
	return contractDeadlineEscrow{
		contractSettlementEscrow: contractSettlementEscrow{balanceId: balanceId, amount: amount, start: start, revenue: revenue},
		networkId:                networkId,
		available:                start,
	}
}

// The ordinary arithmetic decides a settle: the used mean of the reports is
// charged from the grants in the funding order, the provider's sweep carries
// those bytes and half their revenue, and every unsettled escrow is flagged
// with what it was charged. A grant that no longer exists absorbs its share
// without a charge or a sweep.
func TestContractLegacySettlementDecidesOrdinarySettlement(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	settings := DefaultContractLegacySettlementSettings()
	payerNetworkId, providerNetworkId, providerClientId := server.NewId(), server.NewId(), server.NewId()
	firstBalanceId, missingBalanceId, laterBalanceId := server.NewId(), server.NewId(), server.NewId()
	input := contractLegacySettlementTestInput(now, payerNetworkId, providerClientId, providerNetworkId, 1000, 1400,
		contractLegacySettlementTestEscrowOf(firstBalanceId, &payerNetworkId, 700, 10000, 2000),
		contractLegacySettlementTestEscrowOf(missingBalanceId, nil, 300, 0, 0),
		contractLegacySettlementTestEscrowOf(laterBalanceId, &payerNetworkId, 900, 10000, 2000),
	)
	decision := decideContractLegacySettlement(input, settings, now)
	if decision.category != ContractLegacySettlementSettle {
		t.Fatalf("category %s, want settle", decision.category)
	}
	// used 1200: 700 from the first grant, 300 absorbed by the missing one,
	// 200 from the later grant
	charged := map[server.Id]ByteCount{}
	for _, charge := range decision.charges {
		charged[charge.balanceId] += charge.byteCount
	}
	if len(charged) != 2 || charged[firstBalanceId] != 700 || charged[laterBalanceId] != 200 {
		t.Fatalf("charges %+v, want 700 and 200 on the two existing grants", decision.charges)
	}
	if len(decision.payouts) != 3 || decision.payouts[missingBalanceId].payoutByteCount != 0 ||
		decision.payouts[firstBalanceId].payoutByteCount != 700 || decision.payouts[laterBalanceId].payoutByteCount != 200 {
		t.Fatalf("flag payouts %+v, want every escrow with what it was charged", decision.payouts)
	}
	if len(decision.sweeps) != 2 {
		t.Fatalf("sweeps %+v, want one per funding grant to the provider network", decision.sweeps)
	}
	var sweepByteCount ByteCount
	var sweepNanoCents NanoCents
	for key, sweep := range decision.sweeps {
		if key.networkId != providerNetworkId || key.balanceId == missingBalanceId {
			t.Fatalf("sweep %+v pays %s from %s", sweep, key.networkId, key.balanceId)
		}
		sweepByteCount += sweep.payoutByteCount
		sweepNanoCents += sweep.payout
	}
	// half of 2000 * 700/10000 and of 2000 * 200/10000
	if sweepByteCount != 900 || sweepNanoCents != 70+20 {
		t.Fatalf("sweeps carry %d bytes and %d nano cents, want 900 and 90", sweepByteCount, sweepNanoCents)
	}
}

// Each category is decided by its rule, first match first, and only a settle
// charges or sweeps.
func TestContractLegacySettlementDecidesEveryCategory(t *testing.T) {
	now := time.Date(2026, 11, 25, 0, 0, 0, 0, time.UTC)
	settings := DefaultContractLegacySettlementSettings()
	payerNetworkId, providerNetworkId, providerClientId := server.NewId(), server.NewId(), server.NewId()
	balanceId := server.NewId()
	base := func(sourceUsed ByteCount, destinationUsed ByteCount) *contractLegacySettlementInput {
		return contractLegacySettlementTestInput(now, payerNetworkId, providerClientId, providerNetworkId, sourceUsed, destinationUsed,
			contractLegacySettlementTestEscrowOf(balanceId, &payerNetworkId, 1000, 10000, 2000))
	}
	cases := []struct {
		name     string
		input    func() *contractLegacySettlementInput
		settings func(*ContractLegacySettlementSettings)
		category string
	}{
		{name: "missing", input: func() *contractLegacySettlementInput { return &contractLegacySettlementInput{} }, category: ContractLegacySettlementMissing},
		{name: "open", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.outcome = nil
			return input
		}, category: ContractLegacySettlementExcludedOpen},
		{name: "recent", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			closeTime := now.Add(-time.Hour)
			input.closeTime = &closeTime
			return input
		}, category: ContractLegacySettlementExcludedRecent},
		{name: "debit pending", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.journalPending = true
			return input
		}, category: ContractLegacySettlementExcludedDebitPending},
		{name: "already settled", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.escrows[0].settled = true
			return input
		}, category: ContractLegacySettlementAlreadySettled},
		{name: "swept", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.sweepCount = 1
			return input
		}, category: ContractLegacySettlementFlagSwept},
		{name: "one report", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.reports = input.reports[1:]
			return input
		}, category: ContractLegacySettlementFlagQuarantine},
		{name: "reports beyond the escrow", input: func() *contractLegacySettlementInput { return base(1500, 1500) }, category: ContractLegacySettlementFlagQuarantine},
		{name: "zero", input: func() *contractLegacySettlementInput { return base(0, 0) }, category: ContractLegacySettlementFlagZero},
		{name: "unfunded", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.escrows[0].networkId = nil
			return input
		}, category: ContractLegacySettlementFlagUnfunded},
		{name: "another network's grant", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			otherNetworkId := server.NewId()
			input.escrows[0].networkId = &otherNetworkId
			return input
		}, category: ContractLegacySettlementFlagUnfunded},
		{name: "live grant", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.grantEndTimes[balanceId] = now.Add(time.Hour)
			return input
		}, category: ContractLegacySettlementHoldLiveGrant},
		{name: "live grant charged", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.grantEndTimes[balanceId] = now.Add(time.Hour)
			return input
		}, settings: func(settings *ContractLegacySettlementSettings) { settings.ChargeLiveGrants = true }, category: ContractLegacySettlementSettle},
		{name: "expired grant", input: func() *contractLegacySettlementInput {
			input := base(100, 100)
			input.grantEndTimes[balanceId] = now.Add(-time.Hour)
			return input
		}, category: ContractLegacySettlementSettle},
	}
	for _, c := range cases {
		caseSettings := DefaultContractLegacySettlementSettings()
		if c.settings != nil {
			c.settings(caseSettings)
		} else {
			caseSettings = settings
		}
		decision := decideContractLegacySettlement(c.input(), caseSettings, now)
		if decision.category != c.category {
			t.Errorf("%s: category %s, want %s", c.name, decision.category, c.category)
			continue
		}
		// a held contract reports what a settle would write
		writes := 0 < len(decision.charges) || 0 < len(decision.sweeps)
		if writes != (c.category == ContractLegacySettlementSettle || c.category == ContractLegacySettlementHoldLiveGrant) {
			t.Errorf("%s: charges %+v sweeps %+v", c.name, decision.charges, decision.sweeps)
		}
		flags := 0 < len(decision.payouts)
		wantFlags := false
		for _, category := range contractLegacySettlementWriteCategories {
			wantFlags = wantFlags || category == c.category
		}
		if flags != wantFlags {
			t.Errorf("%s: flag payouts %+v, want flags %t", c.name, decision.payouts, wantFlags)
		}
		for _, payout := range decision.payouts {
			if c.category != ContractLegacySettlementSettle && payout.payoutByteCount != 0 {
				t.Errorf("%s: a flag-only category records a payout %+v", c.name, payout)
			}
		}
	}
}

// The token is equal for equal plans and changes with any category's totals
// or with the settings that decide categories.
func TestContractLegacySettlementTokenFollowsTheTotals(t *testing.T) {
	settings := DefaultContractLegacySettlementSettings()
	bound := server.NewId()
	plan := func(contracts int64, chargeByteCount ByteCount) *ContractLegacySettlementReport {
		report := newContractLegacySettlementReport(&bound, time.Time{})
		report.Categories[ContractLegacySettlementSettle].Contracts = contracts
		report.Categories[ContractLegacySettlementSettle].ChargeByteCount = chargeByteCount
		return report
	}
	token := contractLegacySettlementToken(plan(3, 100), settings)
	if token != contractLegacySettlementToken(plan(3, 100), settings) || len(token) != 16 {
		t.Fatalf("token %q is not stable", token)
	}
	if token == contractLegacySettlementToken(plan(4, 100), settings) || token == contractLegacySettlementToken(plan(3, 101), settings) {
		t.Fatal("the token does not follow the totals")
	}
	charging := DefaultContractLegacySettlementSettings()
	charging.ChargeLiveGrants = true
	if token == contractLegacySettlementToken(plan(3, 100), charging) {
		t.Fatal("the token does not follow the live grant setting")
	}
	unbounded := newContractLegacySettlementReport(nil, time.Time{})
	unbounded.Categories[ContractLegacySettlementSettle].Contracts = 3
	unbounded.Categories[ContractLegacySettlementSettle].ChargeByteCount = 100
	if token == contractLegacySettlementToken(unbounded, settings) {
		t.Fatal("the token does not follow the bound")
	}
}
