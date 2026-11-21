// The export's admission set for the SN Top board (sn_top_admission_model.go)
// goes through Redis: what the export publishes is what the fold reads, the
// next export folds its decisions into the published tracks, and with nothing
// published the fold reads no set. The shadow board, its states and a
// network's unit status round-trip too (sn_top_unit_rule_model.go). In the
// fixture's isolated Redis database.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestSnTopAdmissionPublishedSetReadsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		none, err := ReadSnTopAdmission(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)

		admitted, online, neither := server.NewId(), server.NewId(), server.NewId()
		ru, evidence := "ru", true
		measuredAt := server.NowUtc().Truncate(time.Millisecond)
		location := server.NewId()
		egresses := map[server.Id]*clientScoreEgress{
			admitted: {egressQuality: &evidence},
			online:   {publishedCountryCode: &ru, egressQuality: &evidence},
		}
		publishSnTopAdmission(
			ctx,
			measuredAt,
			map[server.Id]map[server.Id]*ClientScore{
				location: {
					admitted: {ClientId: admitted, PassesMinimums: map[string]bool{RankModeSpeed: true}, Online: true},
					online:   {ClientId: online, PassesMinimums: map[string]bool{}, Online: true},
					neither:  {ClientId: neither},
				},
			},
			egresses,
			map[server.Id]providerEgressDecision{online: {reason: ProviderExcludedLowSuccess, online: true}},
		)

		read, err := ReadSnTopAdmission(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, read.MeasuredAt, measuredAt)
		connect.AssertEqual(t, read.Fresh(measuredAt.Add(time.Minute)), true)
		connect.AssertEqual(t, len(read.Providers), 3)
		connect.AssertEqual(t, read.Providers[admitted].AdmittedTrack.Counts, true)
		connect.AssertEqual(t, read.Providers[online].CountryCode, "ru")
		connect.AssertEqual(t, read.Providers[online].Reason, SnTopUnitReasonUrl)
		connect.AssertEqual(t, read.Providers[neither].AdmittedTrack.Counts, false)

		// the next export: the admitted provider fails once and still counts
		publishSnTopAdmission(
			ctx,
			measuredAt.Add(5*time.Minute),
			map[server.Id]map[server.Id]*ClientScore{
				location: {admitted: {ClientId: admitted, PassesMinimums: map[string]bool{}, Online: true}},
			},
			egresses,
			map[server.Id]providerEgressDecision{admitted: {reason: ProviderExcludedLowSuccess, online: true}},
		)
		next, err := ReadSnTopAdmission(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, next.Providers[admitted].Admitted, false)
		connect.AssertEqual(t, next.Providers[admitted].AdmittedTrack.Counts, true)
	})
}

func TestSnTopShadowAndUnitsRoundTrip(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		board, err := GetSnTopShadowBoard(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, board == nil, true)

		networkId, clientId := server.NewId(), server.NewId()
		computedAt := server.NowUtc().Truncate(time.Second)
		SetSnTopShadow(ctx, &SnTopShadowBoard{
			TempoIndex:      1124,
			SettlementEpoch: SnTopUnitRuleFirstEpoch - 1,
			ComputedAt:      computedAt,
			RuleFirstEpoch:  SnTopUnitRuleFirstEpoch,
			TotalRanked:     1,
			Rows:            []SnTopShadowRow{{NetworkId: networkId, Position: 1, Prefixes: 3, LivePosition: 4, LivePrefixes: 9, WithheldProviders: 6}},
		}, map[server.Id]SnTopUnitState{networkId: {Ema: 2.5, SoloEma: 2.5, LastTempoIndex: 1124}})
		board, err = GetSnTopShadowBoard(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, board.TempoIndex, int64(1124))
		connect.AssertEqual(t, board.ComputedAt.Equal(computedAt), true)
		connect.AssertEqual(t, board.Rows[0], SnTopShadowRow{NetworkId: networkId, Position: 1, Prefixes: 3, LivePosition: 4, LivePrefixes: 9, WithheldProviders: 6})
		tempoIndex, states, err := GetSnTopShadowStates(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, tempoIndex, int64(1124))
		connect.AssertEqual(t, states[networkId].Ema, 2.5)

		SetSnTopNetworkUnits(ctx, map[server.Id]*SnTopNetworkUnits{networkId: {
			TempoIndex:        1124,
			CountedProviders:  2,
			WithheldByReason:  map[string]int{SnTopUnitReasonUdp: 1},
			WithheldProviders: []SnTopProviderUnit{{ClientId: clientId, Reason: SnTopUnitReasonUdp}},
		}})
		units, err := GetSnTopNetworkUnits(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, units.CountedProviders, 2)
		connect.AssertEqual(t, units.WithheldProviders, []SnTopProviderUnit{{ClientId: clientId, Reason: SnTopUnitReasonUdp}})
		missing, err := GetSnTopNetworkUnits(ctx, server.NewId())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, missing == nil, true)
	})
}
